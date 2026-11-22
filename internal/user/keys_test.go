package user_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/audit"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/user"
)

const day = 24 * time.Hour

func (f *fixture) issue(t *testing.T, req user.IssueKeyRequest) (*user.APIKey, string) {
	t.Helper()
	k, secret, err := f.svc.IssueKey(context.Background(), req)
	if err != nil {
		t.Fatalf("IssueKey(%+v): %v", req, err)
	}
	return k, secret
}

func (f *fixture) key(t *testing.T, prefix string) *user.APIKey {
	t.Helper()
	k, _, err := f.store.GetKey(context.Background(), prefix)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func (f *fixture) authenticates(secret string) bool {
	_, err := f.svc.Authenticate(context.Background(), secret)
	return err == nil
}

func TestIssueRestrictedKey(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, user.Options{})
	f.mustCreate(t, "ana", "", "")
	f.mustCreate(t, "root", "", user.RoleAdmin)

	k, secret := f.issue(t, user.IssueKeyRequest{Handle: "ana", Label: "weekly",
		Scopes: []string{auth.ScopeWrite, auth.ScopeRead}, Slugs: []string{" weekly-* ", "report", "weekly-*", ""}, ExpiresIn: 90 * day})
	if diff := cmp.Diff([]string{"weekly-*", "report"}, k.Slugs); diff != "" {
		t.Errorf("Slugs (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(k, f.key(t, k.Prefix)); diff != "" {
		t.Errorf("stored key (-issued +stored):\n%s", diff)
	}
	p, err := f.svc.Authenticate(ctx, secret)
	if err != nil {
		t.Fatal(err)
	}
	want := auth.Principal{Handle: "ana", Role: "member", Scopes: []string{auth.ScopeRead, auth.ScopeWrite}, Source: auth.SourceAPIKey,
		KeyID: k.Prefix, Slugs: []string{"weekly-*", "report"}, ExpiresAt: new(t0.Add(90 * day))}
	if diff := cmp.Diff(want, p); diff != "" {
		t.Errorf("Principal (-want +got):\n%s", diff)
	}
	st, err := f.svc.KeyStatus(ctx, k.Prefix)
	if err != nil || !cmp.Equal(st.Slugs, k.Slugs) {
		t.Errorf("KeyStatus = %+v, %v; want the key's slugs", st, err)
	}
	if ev := f.lastEvent(t); ev.Action != audit.KeyIssue || ev.Details["slugs"] != "weekly-* report" {
		t.Errorf("audit event = %+v, want the slugs", ev)
	}

	many := make([]string, artifact.MaxSlugPatterns+1)
	for i := range many {
		many[i] = fmt.Sprintf("page-%d", i)
	}
	for name, slugs := range map[string][]string{
		"every slug":      {"*"},
		"upper case":      {"Weekly"},
		"inner wildcard":  {"week*ly"},
		"double hyphen":   {"a--b"},
		"too many":        many,
		"like wildcard":   {"weekly_%"},
		"one bad of many": {"fine", "no good"},
	} {
		_, _, err := f.svc.IssueKey(ctx, user.IssueKeyRequest{Handle: "ana", Slugs: slugs})
		if !errors.Is(err, user.ErrInvalidKey) {
			t.Errorf("%s: err = %v, want ErrInvalidKey", name, err)
		}
	}
	if _, _, err := f.svc.IssueKey(ctx, user.IssueKeyRequest{Handle: "ana", Slugs: many[:artifact.MaxSlugPatterns]}); err != nil {
		t.Errorf("%d patterns: %v", artifact.MaxSlugPatterns, err)
	}
	// A restricted admin key would open the admin API, where no slug is checked.
	_, _, err = f.svc.IssueKey(ctx, user.IssueKeyRequest{Handle: "root", Scopes: auth.AllScopes, Slugs: []string{"weekly-*"}})
	if !errors.Is(err, user.ErrInvalidScope) {
		t.Errorf("restricted key with admin: err = %v, want ErrInvalidScope", err)
	}
}

func TestMaxKeyTTL(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, user.Options{MaxKeyTTL: 90 * day})
	f.mustCreate(t, "ana", "", "")

	k, _ := f.issue(t, user.IssueKeyRequest{Handle: "ana"})
	if k.ExpiresAt == nil || !k.ExpiresAt.Equal(t0.Add(90*day)) {
		t.Errorf("no expiry asked: ExpiresAt = %v, want the limit", k.ExpiresAt)
	}
	k, _ = f.issue(t, user.IssueKeyRequest{Handle: "ana", ExpiresIn: 30 * day})
	if !k.ExpiresAt.Equal(t0.Add(30 * day)) {
		t.Errorf("shorter expiry asked: ExpiresAt = %v", k.ExpiresAt)
	}
	k, _ = f.issue(t, user.IssueKeyRequest{Handle: "ana", ExpiresIn: 90 * day})
	if !k.ExpiresAt.Equal(t0.Add(90 * day)) {
		t.Errorf("the limit asked: ExpiresAt = %v", k.ExpiresAt)
	}
	_, _, err := f.svc.IssueKey(ctx, user.IssueKeyRequest{Handle: "ana", ExpiresIn: 90*day + time.Second})
	if !errors.Is(err, user.ErrInvalidKey) || !strings.Contains(err.Error(), "90d") {
		t.Errorf("longer expiry asked: err = %v, want ErrInvalidKey naming 90d", err)
	}

	// The limit binds new keys only.
	open := newFixture(t, user.Options{})
	open.mustCreate(t, "ana", "", "")
	old, secret := open.issue(t, user.IssueKeyRequest{Handle: "ana"})
	open.opts.MaxKeyTTL = day
	limited := open.restart()
	open.clock.Advance(400 * day)
	if _, err := limited.Authenticate(ctx, secret); err != nil || old.ExpiresAt != nil {
		t.Errorf("key from before the limit: %v, expires %v", err, old.ExpiresAt)
	}
}

func TestIssueKeyWithKey(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, user.Options{})
	f.mustCreate(t, "ana", "", "")
	f.mustCreate(t, "ben", "", "")
	f.mustCreate(t, "root", "", user.RoleAdmin)
	laptop, _ := f.issue(t, user.IssueKeyRequest{Handle: "ana", Label: "laptop"})
	bens, _ := f.issue(t, user.IssueKeyRequest{Handle: "ben"})

	child, secret := f.issue(t, user.IssueKeyRequest{Handle: "ana", Label: "agent", With: laptop.Prefix,
		Scopes: []string{auth.ScopeRead, auth.ScopeWrite}, Slugs: []string{"weekly-*"}, ExpiresIn: 30 * day})
	if child.IssuedBy != laptop.Prefix || f.key(t, child.Prefix).IssuedBy != laptop.Prefix {
		t.Errorf("IssuedBy = %q, want %q", child.IssuedBy, laptop.Prefix)
	}
	if ev := f.lastEvent(t); ev.Details["issued_by"] != laptop.Prefix {
		t.Errorf("audit event = %+v, want issued_by", ev)
	}
	if !f.authenticates(secret) {
		t.Error("issued key does not authenticate")
	}

	// A key issued with a narrow key takes what that key has when nothing is named.
	heir, _ := f.issue(t, user.IssueKeyRequest{Handle: "ana", With: child.Prefix})
	want := &user.APIKey{ID: heir.ID, UserHandle: "ana", Prefix: heir.Prefix, Hash: heir.Hash,
		Scopes: []string{auth.ScopeRead, auth.ScopeWrite}, Slugs: []string{"weekly-*"}, IssuedBy: child.Prefix,
		ExpiresAt: child.ExpiresAt, CreatedAt: t0}
	if diff := cmp.Diff(want, heir); diff != "" {
		t.Errorf("key issued with a narrow key (-want +got):\n%s", diff)
	}
	if _, _, err := f.svc.IssueKey(ctx, user.IssueKeyRequest{Handle: "ana", With: child.Prefix,
		Slugs: []string{"weekly-2026-*", "weekly-report"}, ExpiresIn: 30 * day}); err != nil {
		t.Errorf("narrower slugs and the same expiry: %v", err)
	}

	admin, _ := f.issue(t, user.IssueKeyRequest{Handle: "root", Scopes: auth.AllScopes})
	fromAdmin, _ := f.issue(t, user.IssueKeyRequest{Handle: "root", With: admin.Prefix})
	if diff := cmp.Diff(auth.MemberScopes, fromAdmin.Scopes); diff != "" {
		t.Errorf("scopes of a key issued with an admin key (-want +got):\n%s", diff)
	}

	for _, tt := range []struct {
		name string
		req  user.IssueKeyRequest
		want error
	}{
		{"a scope the key lacks", user.IssueKeyRequest{Handle: "ana", With: child.Prefix, Scopes: []string{auth.ScopeDelete}}, user.ErrKeyTooWide},
		{"the share scope the key lacks", user.IssueKeyRequest{Handle: "ana", With: child.Prefix, Scopes: []string{auth.ScopeRead, auth.ScopeShare}}, user.ErrKeyTooWide},
		{"wider slugs", user.IssueKeyRequest{Handle: "ana", With: child.Prefix, Slugs: []string{"week*"}}, user.ErrKeyTooWide},
		{"other slugs", user.IssueKeyRequest{Handle: "ana", With: child.Prefix, Slugs: []string{"weekly-a", "other"}}, user.ErrKeyTooWide},
		{"a later expiry", user.IssueKeyRequest{Handle: "ana", With: child.Prefix, ExpiresIn: 30*day + time.Second}, user.ErrKeyTooWide},
		{"admin from an admin key", user.IssueKeyRequest{Handle: "root", With: admin.Prefix, Scopes: auth.AllScopes}, user.ErrKeyTooWide},
		{"admin for a member", user.IssueKeyRequest{Handle: "ana", With: laptop.Prefix, Scopes: []string{auth.ScopeAdmin}}, user.ErrInvalidScope},
		{"another user's key", user.IssueKeyRequest{Handle: "ana", With: bens.Prefix}, user.ErrKeyNotFound},
		{"a key for another handle", user.IssueKeyRequest{Handle: "ben", With: laptop.Prefix}, user.ErrKeyNotFound},
		{"an unknown key", user.IssueKeyRequest{Handle: "ana", With: "zzzzzzzz"}, user.ErrKeyNotFound},
	} {
		if _, _, err := f.svc.IssueKey(ctx, tt.req); !errors.Is(err, tt.want) {
			t.Errorf("%s: err = %v, want %v", tt.name, err, tt.want)
		}
	}

	if err := f.svc.RevokeKey(ctx, user.KeyRef{Handle: "ana", Prefix: laptop.Prefix}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.IssueKey(ctx, user.IssueKeyRequest{Handle: "ana", With: laptop.Prefix}); !errors.Is(err, user.ErrKeyNotFound) {
		t.Errorf("issuing with a revoked key: err = %v, want ErrKeyNotFound", err)
	}
}

func TestIssueKeyWithKeyAfterDemotion(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, user.Options{})
	f.mustCreate(t, "root", "", user.RoleAdmin)
	f.mustCreate(t, "second", "", user.RoleAdmin)
	admin, secret := f.issue(t, user.IssueKeyRequest{Handle: "root", Scopes: auth.AllScopes})
	member := user.RoleMember
	if _, err := f.svc.UpdateUser(ctx, "root", user.UpdateUserRequest{Role: &member}); err != nil {
		t.Fatal(err)
	}
	p, err := f.svc.Authenticate(ctx, secret)
	if err != nil || p.Admin || p.HasScope(auth.ScopeAdmin) {
		t.Fatalf("demoted admin's key: %+v, %v", p, err)
	}
	child, _ := f.issue(t, user.IssueKeyRequest{Handle: "root", With: admin.Prefix})
	if diff := cmp.Diff(auth.MemberScopes, child.Scopes); diff != "" {
		t.Errorf("scopes (-want +got):\n%s", diff)
	}
	rotated, _, _, err := f.svc.RotateKey(ctx, user.KeyRef{Handle: "root", Prefix: admin.Prefix}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(auth.MemberScopes, rotated.Scopes); diff != "" {
		t.Errorf("scopes of the rotated key (-want +got):\n%s", diff)
	}
}

func TestRevokeKeyCascade(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, user.Options{})
	f.mustCreate(t, "ana", "", "")
	laptop, laptopKey := f.issue(t, user.IssueKeyRequest{Handle: "ana", Label: "laptop"})
	other, otherKey := f.issue(t, user.IssueKeyRequest{Handle: "ana", Label: "other"})
	agent, agentKey := f.issue(t, user.IssueKeyRequest{Handle: "ana", With: laptop.Prefix, ExpiresIn: day})
	sub, subKey := f.issue(t, user.IssueKeyRequest{Handle: "ana", With: agent.Prefix})
	gone, _ := f.issue(t, user.IssueKeyRequest{Handle: "ana", With: laptop.Prefix})
	f.clock.Advance(time.Minute)
	if err := f.svc.RevokeKey(ctx, user.KeyRef{Handle: "ana", Prefix: gone.Prefix}); err != nil {
		t.Fatal(err)
	}
	earlier := f.key(t, gone.Prefix).RevokedAt

	// An expiry does not cascade: a key outlives the key it was issued
	// with only when a rotation ended that one early.
	short, shortKey := f.issue(t, user.IssueKeyRequest{Handle: "ana", ExpiresIn: 7 * day})
	kept, keptKey := f.issue(t, user.IssueKeyRequest{Handle: "ana", With: short.Prefix})
	if _, _, _, err := f.svc.RotateKey(ctx, user.KeyRef{Handle: "ana", Prefix: short.Prefix}, 0); err != nil {
		t.Fatal(err)
	}
	if f.authenticates(shortKey) || !f.authenticates(keptKey) {
		t.Errorf("after the issuing key expired: issuer live = %t, issued live = %t; want false, true",
			f.authenticates(shortKey), f.authenticates(keptKey))
	}

	f.clock.Advance(time.Minute)
	events := len(f.audit.Events)
	if err := f.svc.RevokeKey(ctx, user.KeyRef{Handle: "ana", Prefix: laptop.Prefix}); err != nil {
		t.Fatal(err)
	}
	for name, secret := range map[string]string{"the key": laptopKey, "a key issued with it": agentKey, "a key issued with that": subKey} {
		if f.authenticates(secret) {
			t.Errorf("%s still authenticates after the revoke", name)
		}
	}
	if !f.authenticates(otherKey) || !f.authenticates(keptKey) {
		t.Error("the revoke reached a key that was not issued with the revoked one")
	}
	now := f.clock.Now()
	for _, prefix := range []string{laptop.Prefix, agent.Prefix, sub.Prefix} {
		if at := f.key(t, prefix).RevokedAt; at == nil || !at.Equal(now) {
			t.Errorf("key %s revoked at %v, want %v", prefix, at, now)
		}
	}
	if at := f.key(t, gone.Prefix).RevokedAt; !at.Equal(*earlier) {
		t.Errorf("an earlier revocation moved from %v to %v", earlier, at)
	}
	if f.key(t, other.Prefix).RevokedAt != nil || f.key(t, kept.Prefix).RevokedAt != nil {
		t.Error("an unrelated key was revoked")
	}

	var revoked []string
	for _, ev := range f.audit.Events[events:] {
		if ev.Action != audit.KeyRevoke {
			t.Errorf("unexpected audit event %+v", ev)
		}
		revoked = append(revoked, ev.Details["prefix"]+" by "+ev.Details["issued_by"])
	}
	wantRevoked := []string{laptop.Prefix + " by ", agent.Prefix + " by " + laptop.Prefix, sub.Prefix + " by " + laptop.Prefix}
	if diff := cmp.Diff(wantRevoked, revoked, cmpopts.SortSlices(func(a, b string) bool { return a < b })); diff != "" {
		t.Errorf("revoke events (-want +got):\n%s", diff)
	}
}

func TestRevokeKeyWithKey(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, user.Options{})
	f.mustCreate(t, "ana", "", "")
	f.mustCreate(t, "ben", "", "")
	laptop, laptopKey := f.issue(t, user.IssueKeyRequest{Handle: "ana"})
	reader, _ := f.issue(t, user.IssueKeyRequest{Handle: "ana", Scopes: []string{auth.ScopeRead}})
	agent, agentKey := f.issue(t, user.IssueKeyRequest{Handle: "ana", Scopes: []string{auth.ScopeRead, auth.ScopeWrite}, Slugs: []string{"weekly-*"}})
	narrower, _ := f.issue(t, user.IssueKeyRequest{Handle: "ana", Scopes: []string{auth.ScopeRead}, Slugs: []string{"weekly-a"}})
	bens, _ := f.issue(t, user.IssueKeyRequest{Handle: "ben"})

	for _, tt := range []struct {
		name string
		ref  user.KeyRef
		want error
	}{
		{"a wider key with a restricted one", user.KeyRef{Handle: "ana", Prefix: laptop.Prefix, With: agent.Prefix}, user.ErrKeyTooWide},
		{"a wider key with a read-only one", user.KeyRef{Handle: "ana", Prefix: laptop.Prefix, With: reader.Prefix}, user.ErrKeyTooWide},
		{"an unrestricted key with a restricted one", user.KeyRef{Handle: "ana", Prefix: reader.Prefix, With: narrower.Prefix}, user.ErrKeyTooWide},
		{"another user's key", user.KeyRef{Handle: "ana", Prefix: bens.Prefix, With: laptop.Prefix}, user.ErrKeyNotFound},
		{"with another user's key", user.KeyRef{Handle: "ana", Prefix: agent.Prefix, With: bens.Prefix}, user.ErrKeyNotFound},
		{"an unknown key", user.KeyRef{Handle: "ana", Prefix: "zzzzzzzz", With: laptop.Prefix}, user.ErrKeyNotFound},
	} {
		if err := f.svc.RevokeKey(ctx, tt.ref); !errors.Is(err, tt.want) {
			t.Errorf("revoking %s: err = %v, want %v", tt.name, err, tt.want)
		}
	}
	if !f.authenticates(laptopKey) || !f.authenticates(agentKey) {
		t.Fatal("a refused revoke revoked a key")
	}

	if err := f.svc.RevokeKey(ctx, user.KeyRef{Handle: "ana", Prefix: narrower.Prefix, With: agent.Prefix}); err != nil {
		t.Errorf("revoking a narrower key: %v", err)
	}
	if err := f.svc.RevokeKey(ctx, user.KeyRef{Handle: "ana", Prefix: agent.Prefix, With: agent.Prefix}); err != nil {
		t.Errorf("a key revoking itself: %v", err)
	}
	if f.authenticates(agentKey) {
		t.Error("the key authenticates after revoking itself")
	}
	if err := f.svc.RevokeKey(ctx, user.KeyRef{Handle: "ana", Prefix: reader.Prefix, With: agent.Prefix}); !errors.Is(err, user.ErrKeyNotFound) {
		t.Errorf("revoking with a revoked key: err = %v, want ErrKeyNotFound", err)
	}
}

func TestRotateKey(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, user.Options{})
	f.mustCreate(t, "ana", "", "")
	old, oldKey := f.issue(t, user.IssueKeyRequest{Handle: "ana", Label: "weekly", Scopes: []string{auth.ScopeRead, auth.ScopeWrite},
		Slugs: []string{"weekly-*"}, ExpiresIn: 90 * day})
	f.clock.Advance(60 * day)
	now := f.clock.Now()

	k, secret, replaced, err := f.svc.RotateKey(ctx, user.KeyRef{Handle: "ana", Prefix: old.Prefix}, day)
	if err != nil {
		t.Fatal(err)
	}
	want := &user.APIKey{ID: k.ID, UserHandle: "ana", Prefix: k.Prefix, Hash: user.HashKey(secret), Label: "weekly",
		Scopes: []string{auth.ScopeRead, auth.ScopeWrite}, Slugs: []string{"weekly-*"},
		ExpiresAt: new(now.Add(90 * day)), CreatedAt: now}
	if diff := cmp.Diff(want, k); diff != "" {
		t.Errorf("new key (-want +got):\n%s", diff)
	}
	if k.Prefix == old.Prefix || secret == oldKey {
		t.Error("the rotation returned the old key")
	}
	if !replaced.ExpiresAt.Equal(now.Add(day)) || !f.key(t, old.Prefix).ExpiresAt.Equal(now.Add(day)) {
		t.Errorf("old key expires at %v, stored %v; want %v", replaced.ExpiresAt, f.key(t, old.Prefix).ExpiresAt, now.Add(day))
	}
	ev := f.lastEvent(t)
	wantDetails := map[string]string{"handle": "ana", "prefix": old.Prefix, "new_prefix": k.Prefix, "label": "weekly",
		"scopes": "artifacts:read artifacts:write", "slugs": "weekly-*",
		"expires_at": now.Add(90 * day).Format(time.RFC3339), "old_expires_at": now.Add(day).Format(time.RFC3339)}
	if ev.Action != audit.KeyRotate || !cmp.Equal(ev.Details, wantDetails) {
		t.Errorf("audit event = %+v, want %v", ev, wantDetails)
	}

	if !f.authenticates(oldKey) || !f.authenticates(secret) {
		t.Error("inside the overlap both keys must work")
	}
	f.clock.Advance(day - time.Second)
	if !f.authenticates(oldKey) {
		t.Error("the old key ended before the overlap did")
	}
	f.clock.Advance(time.Second)
	if f.authenticates(oldKey) || !f.authenticates(secret) {
		t.Errorf("when the overlap ends: old live = %t, new live = %t; want false, true", f.authenticates(oldKey), f.authenticates(secret))
	}
	if f.key(t, old.Prefix).RevokedAt != nil {
		t.Error("the rotation revoked the old key instead of letting it expire")
	}

	if _, _, _, err := f.svc.RotateKey(ctx, user.KeyRef{Handle: "ana", Prefix: old.Prefix}, 0); !errors.Is(err, user.ErrKeyNotFound) {
		t.Errorf("rotating an expired key: err = %v, want ErrKeyNotFound", err)
	}
	for _, overlap := range []time.Duration{-time.Second, user.MaxRotateOverlap + time.Second} {
		if _, _, _, err := f.svc.RotateKey(ctx, user.KeyRef{Handle: "ana", Prefix: k.Prefix}, overlap); !errors.Is(err, user.ErrInvalidKey) {
			t.Errorf("overlap %s: err = %v, want ErrInvalidKey", overlap, err)
		}
	}

	// Overlap 0 ends the old key at once; a key without an expiry gets
	// another without one, and an overlap never extends a key.
	forever, foreverKey := f.issue(t, user.IssueKeyRequest{Handle: "ana"})
	next, _, _, err := f.svc.RotateKey(ctx, user.KeyRef{Handle: "ana", Prefix: forever.Prefix}, 0)
	if err != nil || next.ExpiresAt != nil || f.authenticates(foreverKey) {
		t.Errorf("overlap 0: new key expires %v, old live = %t, err %v", next.ExpiresAt, f.authenticates(foreverKey), err)
	}
	ending, _ := f.issue(t, user.IssueKeyRequest{Handle: "ana", ExpiresIn: time.Hour})
	_, _, replaced, err = f.svc.RotateKey(ctx, user.KeyRef{Handle: "ana", Prefix: ending.Prefix}, day)
	if err != nil || !replaced.ExpiresAt.Equal(*ending.ExpiresAt) || !f.key(t, ending.Prefix).ExpiresAt.Equal(*ending.ExpiresAt) {
		t.Errorf("overlap past the old key's expiry: old expires %v, want %v (err %v)", replaced.ExpiresAt, ending.ExpiresAt, err)
	}
}

func TestRotateKeyWithKey(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, user.Options{})
	f.mustCreate(t, "ana", "", "")
	f.mustCreate(t, "root", "", user.RoleAdmin)
	laptop, _ := f.issue(t, user.IssueKeyRequest{Handle: "ana", ExpiresIn: 30 * day})
	agent, _ := f.issue(t, user.IssueKeyRequest{Handle: "ana", With: laptop.Prefix, Scopes: []string{auth.ScopeRead, auth.ScopeWrite},
		Slugs: []string{"weekly-*"}, ExpiresIn: 10 * day})
	long, _ := f.issue(t, user.IssueKeyRequest{Handle: "ana", Scopes: []string{auth.ScopeRead}, ExpiresIn: 365 * day})
	f.clock.Advance(5 * day)
	now := f.clock.Now()

	// With another key: the same lineage, and no later than that key.
	k, _, _, err := f.svc.RotateKey(ctx, user.KeyRef{Handle: "ana", Prefix: agent.Prefix, With: laptop.Prefix}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if k.IssuedBy != laptop.Prefix || !k.ExpiresAt.Equal(now.Add(10*day)) {
		t.Errorf("rotated with its issuer: issued by %q, expires %v; want %q, %v", k.IssuedBy, k.ExpiresAt, laptop.Prefix, now.Add(10*day))
	}
	renewed, _, _, err := f.svc.RotateKey(ctx, user.KeyRef{Handle: "ana", Prefix: long.Prefix, With: laptop.Prefix}, 0)
	if err != nil || !renewed.ExpiresAt.Equal(*laptop.ExpiresAt) {
		t.Errorf("rotating a longer-lived key: expires %v, want the acting key's %v (err %v)", renewed.ExpiresAt, laptop.ExpiresAt, err)
	}

	// With itself: a new secret, never more time.
	self, _, _, err := f.svc.RotateKey(ctx, user.KeyRef{Handle: "ana", Prefix: k.Prefix, With: k.Prefix}, time.Hour)
	if err != nil || !self.ExpiresAt.Equal(*k.ExpiresAt) || self.IssuedBy != laptop.Prefix {
		t.Errorf("a key rotating itself: expires %v, want %v; issued by %q (err %v)", self.ExpiresAt, k.ExpiresAt, self.IssuedBy, err)
	}

	if _, _, _, err := f.svc.RotateKey(ctx, user.KeyRef{Handle: "ana", Prefix: laptop.Prefix, With: self.Prefix}, 0); !errors.Is(err, user.ErrKeyTooWide) {
		t.Errorf("rotating a wider key: err = %v, want ErrKeyTooWide", err)
	}
	admin, _ := f.issue(t, user.IssueKeyRequest{Handle: "root", Scopes: auth.AllScopes})
	if _, _, _, err := f.svc.RotateKey(ctx, user.KeyRef{Handle: "root", Prefix: admin.Prefix, With: admin.Prefix}, 0); !errors.Is(err, user.ErrKeyTooWide) {
		t.Errorf("an admin key rotating itself: err = %v, want ErrKeyTooWide", err)
	}
	if next, _, _, err := f.svc.RotateKey(ctx, user.KeyRef{Handle: "root", Prefix: admin.Prefix}, 0); err != nil || !cmp.Equal(next.Scopes, auth.AllScopes) {
		t.Errorf("an admin rotating an admin key: %+v, %v", next, err)
	}

	limited := newFixture(t, user.Options{})
	limited.mustCreate(t, "ana", "", "")
	open, _ := limited.issue(t, user.IssueKeyRequest{Handle: "ana"})
	limited.opts.MaxKeyTTL = 30 * day
	next, _, _, err := limited.restart().RotateKey(ctx, user.KeyRef{Handle: "ana", Prefix: open.Prefix}, 0)
	if err != nil || next.ExpiresAt == nil || !next.ExpiresAt.Equal(t0.Add(30*day)) {
		t.Errorf("rotating a key from before the limit: expires %v, want the limit (err %v)", next.ExpiresAt, err)
	}
}

func TestKeySecretsStayOutOfTheAuditLog(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, user.Options{})
	f.mustCreate(t, "ana", "", "")
	laptop, laptopKey := f.issue(t, user.IssueKeyRequest{Handle: "ana", Label: "laptop"})
	_, agentKey := f.issue(t, user.IssueKeyRequest{Handle: "ana", With: laptop.Prefix, Slugs: []string{"weekly-*"}})
	_, rotatedKey, _, err := f.svc.RotateKey(ctx, user.KeyRef{Handle: "ana", Prefix: laptop.Prefix, With: laptop.Prefix}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.RevokeKey(ctx, user.KeyRef{Handle: "ana", Prefix: laptop.Prefix}); err != nil {
		t.Fatal(err)
	}
	if len(f.audit.Events) < 4 {
		t.Fatalf("%d audit events, want at least 4", len(f.audit.Events))
	}
	for _, ev := range f.audit.Events {
		row := fmt.Sprintf("%+v", ev)
		for _, key := range []string{laptopKey, agentKey, rotatedKey} {
			_, secret, _ := user.ParseKey(key)
			if strings.Contains(row, secret) || strings.Contains(row, user.HashKey(key)) {
				t.Errorf("audit event holds a key's secret or hash: %s", row)
			}
		}
	}
}
