package artifact_test

import (
	"strings"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/audit"
)

func TestMembersVisibility(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice := e.user(t, "alice")
	e.create(t, alice, "demo")

	a, err := e.svc.SetVisibility(e.ctx, alice, "demo", "members")
	if err != nil {
		t.Fatal(err)
	}
	if a.Visibility != artifact.Members || !a.Visibility.Gated() {
		t.Fatalf("members: %+v", a)
	}
	p, err := e.svc.ResolveVersion(e.ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if p.Visibility != artifact.Members || !p.Servable() {
		t.Fatalf("a pinned version of a members artifact resolves as %+v", p)
	}
	for _, v := range []artifact.Visibility{artifact.Public, artifact.Unlisted} {
		if v.Gated() {
			t.Errorf("%s is gated", v)
		}
	}
}

// An empty visibility on update is refused, not read as public: it would
// open a members page to everyone.
func TestUpdateEmptyVisibility(t *testing.T) {
	e := newEnv(t, artifact.Options{DefaultVisibility: artifact.Members})
	alice := e.user(t, "alice")
	e.create(t, alice, "demo")
	empty := artifact.Visibility("")
	_, _, err := e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "demo", Visibility: &empty})
	wantErr(t, "empty visibility", err, artifact.ErrInvalidArgument)
	a, _, err := e.svc.Get(e.ctx, alice, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if a.Visibility != artifact.Members {
		t.Fatalf("visibility = %q, want members", a.Visibility)
	}
}

func TestDefaultVisibility(t *testing.T) {
	e := newEnv(t, artifact.Options{DefaultVisibility: artifact.Members})
	alice := e.user(t, "alice")
	if a := e.create(t, alice, "demo"); a.Visibility != artifact.Members {
		t.Fatalf("created without a visibility: %q, want members", a.Visibility)
	}
	a, _, err := e.svc.Create(e.ctx, alice, artifact.CreateRequest{
		Slug: "open", Visibility: artifact.Public, Files: files("index.html", "x"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.Visibility != artifact.Public {
		t.Fatalf("an explicit public became %q", a.Visibility)
	}
}

func TestShareLinks(t *testing.T) {
	e := newEnv(t, artifact.Options{MaxShareLinkTTL: 30 * 24 * time.Hour})
	alice := e.user(t, "alice")
	e.create(t, alice, "demo")
	e.create(t, alice, "other")

	l, token, err := e.svc.CreateShareLink(e.ctx, alice, "demo", "for the client", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !artifact.IsLinkToken(token) || !strings.HasPrefix(token, artifact.LinkTokenPrefix+l.Prefix+"_") {
		t.Fatalf("token %q does not match prefix %q", token, l.Prefix)
	}
	if !l.ExpiresAt.Equal(start.Add(artifact.DefaultShareLinkTTL)) || l.Label != "for the client" || l.CreatedBy != "alice" {
		t.Fatalf("link = %+v", l)
	}
	ev := e.rec.Events[len(e.rec.Events)-1]
	if ev.Action != audit.ShareLinkCreate || ev.Slug != "demo" || ev.Details["prefix"] != l.Prefix {
		t.Fatalf("audit = %+v", ev)
	}
	if strings.Contains(strings.Join([]string{ev.Details["prefix"], ev.Details["expires_at"]}, " "), token) {
		t.Fatal("the audit trail holds the token")
	}

	got, err := e.svc.OpenShareLink(e.ctx, "demo", token)
	if err != nil {
		t.Fatal(err)
	}
	if got.Uses != 1 || got.LastUsedAt == nil {
		t.Fatalf("after one open: %+v", got)
	}
	for _, tc := range []struct{ name, slug, token string }{
		{"other artifact", "other", token},
		{"malformed", "demo", "kzs_short"},
		{"empty", "demo", ""},
		{"unknown", "demo", artifact.LinkTokenPrefix + l.Prefix + "_" + strings.Repeat("a", 52)},
	} {
		_, err := e.svc.CheckShareLink(e.ctx, tc.slug, tc.token)
		wantErr(t, tc.name, err, artifact.ErrLinkNotFound)
	}

	links, err := e.svc.ListShareLinks(e.ctx, alice, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 || links[0].Prefix != l.Prefix || links[0].Uses != 1 {
		t.Fatalf("ListShareLinks = %+v", links)
	}

	if err := e.svc.RevokeShareLink(e.ctx, alice, "demo", l.Prefix); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.CheckShareLink(e.ctx, "demo", token)
	wantErr(t, "revoked link", err, artifact.ErrLinkNotFound)
	wantErr(t, "revoke unknown", e.svc.RevokeShareLink(e.ctx, alice, "demo", "zzzzzzzz"), artifact.ErrLinkNotFound)

	// Expiry: a one-hour link stops at the hour.
	_, short, err := e.svc.CreateShareLink(e.ctx, alice, "demo", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(time.Hour - time.Second)
	if _, err := e.svc.CheckShareLink(e.ctx, "demo", short); err != nil {
		t.Fatalf("before expiry: %v", err)
	}
	e.clock.Advance(time.Second)
	_, err = e.svc.CheckShareLink(e.ctx, "demo", short)
	wantErr(t, "expired link", err, artifact.ErrLinkNotFound)

	_, _, err = e.svc.CreateShareLink(e.ctx, alice, "demo", "", 31*24*time.Hour)
	wantErr(t, "beyond the maximum", err, artifact.ErrInvalidArgument)
	_, _, err = e.svc.CreateShareLink(e.ctx, alice, "demo", "bad\x00label", 0)
	wantErr(t, "control character in the label", err, artifact.ErrInvalidArgument)
	_, _, err = e.svc.CreateShareLink(e.ctx, alice, "missing", "", 0)
	wantErr(t, "unknown slug", err, artifact.ErrNotFound)
	_, _, err = e.svc.CreateShareLink(e.ctx, artifact.Actor{}, "demo", "", 0)
	wantErr(t, "anonymous", err, artifact.ErrForbidden)
}

// The default lifetime never exceeds the configured maximum.
func TestShareLinkDefaultCappedByMaximum(t *testing.T) {
	e := newEnv(t, artifact.Options{MaxShareLinkTTL: 24 * time.Hour})
	alice := e.user(t, "alice")
	e.create(t, alice, "demo")
	l, _, err := e.svc.CreateShareLink(e.ctx, alice, "demo", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !l.ExpiresAt.Equal(start.Add(24 * time.Hour)) {
		t.Fatalf("ExpiresAt = %v, want a day", l.ExpiresAt)
	}
}

// Managing links needs the same rights as changing the artifact.
func TestShareLinkPolicy(t *testing.T) {
	e := newEnv(t, artifact.Options{WritePolicy: artifact.WriteOwner})
	alice := e.user(t, "alice")
	bob := e.user(t, "bob")
	e.create(t, alice, "demo")
	l, _, err := e.svc.CreateShareLink(e.ctx, alice, "demo", "", 0)
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = e.svc.CreateShareLink(e.ctx, bob, "demo", "", 0)
	wantErr(t, "create by another member", err, artifact.ErrForbidden)
	_, err = e.svc.ListShareLinks(e.ctx, bob, "demo")
	wantErr(t, "list by another member", err, artifact.ErrForbidden)
	wantErr(t, "revoke by another member", e.svc.RevokeShareLink(e.ctx, bob, "demo", l.Prefix), artifact.ErrForbidden)

	admin := artifact.Actor{Handle: "root", Admin: true}
	if err := e.svc.RevokeShareLink(e.ctx, admin, "demo", l.Prefix); err != nil {
		t.Fatalf("admin revoke: %v", err)
	}

	// A deleted artifact's links cannot be managed until it is restored.
	if err := e.svc.Delete(e.ctx, alice, "demo", false); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.ListShareLinks(e.ctx, alice, "demo")
	wantErr(t, "list on a deleted artifact", err, artifact.ErrNotFound)
}

// Under the shared policy anyone may change a live artifact, but an
// offline one stays hidden from members who are not involved with it.
func TestShareLinkOfflineArtifact(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice := e.user(t, "alice")
	bob := e.user(t, "bob")
	e.create(t, alice, "demo")
	if _, err := e.svc.SetVisibility(e.ctx, alice, "demo", artifact.Unpublished); err != nil {
		t.Fatal(err)
	}
	_, _, err := e.svc.CreateShareLink(e.ctx, bob, "demo", "", 0)
	wantErr(t, "stranger on an unpublished artifact", err, artifact.ErrNotFound)
	if _, _, err := e.svc.CreateShareLink(e.ctx, alice, "demo", "", 0); err != nil {
		t.Fatalf("owner on an unpublished artifact: %v", err)
	}
}
