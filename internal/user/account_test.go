package user_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/audit"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/user"
)

// cheap keeps the tests fast: the real parameters cost 64 MiB a hash.
var cheap = user.Argon2{Memory: 64, Time: 1, Threads: 1}

const goodPassword = "correct horse battery"

func accounts(t *testing.T, opts user.Options) *fixture {
	t.Helper()
	opts.LocalAccounts = true
	if opts.Argon2 == (user.Argon2{}) {
		opts.Argon2 = cheap
	}
	return newFixture(t, opts)
}

func asAdmin(ctx context.Context, handle string) context.Context {
	return auth.ContextWithPrincipal(ctx, auth.Principal{Handle: handle, Admin: true, Scopes: auth.AllScopes})
}

// invite invites handle and sets their password through the link.
func (f *fixture) invite(t *testing.T, handle, email string) *user.User {
	t.Helper()
	ctx := context.Background()
	_, _, token, err := f.svc.Invite(ctx, user.InviteRequest{Handle: handle, Email: email})
	if err != nil {
		t.Fatalf("Invite(%q): %v", handle, err)
	}
	u, err := f.svc.Redeem(ctx, token, user.RedeemRequest{Password: goodPassword})
	if err != nil {
		t.Fatalf("Redeem for %q: %v", handle, err)
	}
	return u
}

func (f *fixture) actions() []string {
	out := make([]string, 0, len(f.audit.Events))
	for _, e := range f.audit.Events {
		out = append(out, e.Action)
	}
	return out
}

func TestInviteSetsPassword(t *testing.T) {
	ctx := context.Background()
	f := accounts(t, user.Options{})
	u, l, token, err := f.svc.Invite(ctx, user.InviteRequest{Handle: "ana", Email: "Ana@Example.com", DisplayName: "Ana"})
	if err != nil {
		t.Fatal(err)
	}
	if u.Role != user.RoleMember || u.Email != "ana@example.com" || l.Purpose != user.PurposeInvite ||
		!l.ExpiresAt.Equal(t0.Add(user.DefaultInviteTTL)) || !user.IsLinkToken(token) || l.Hash == token {
		t.Fatalf("user = %+v, link = %+v, token = %q", u, l, token)
	}
	if _, err := f.svc.CheckPassword(ctx, "ana", goodPassword); !errors.Is(err, user.ErrBadPassword) {
		t.Errorf("sign-in before the invite is used: err = %v, want ErrBadPassword", err)
	}
	if acc := f.account(t, "ana"); !acc.Invited || acc.HasPassword {
		t.Errorf("account before use = %+v, want invited", acc)
	}

	// Opening the link changes nothing, however often.
	for range 3 {
		got, who, err := f.svc.Link(ctx, token)
		if err != nil || got.ID != l.ID || who.Handle != "ana" {
			t.Fatalf("Link = %+v, %+v, %v", got, who, err)
		}
	}
	if _, err := f.svc.Redeem(ctx, token, user.RedeemRequest{Password: "too short"}); !errors.Is(err, user.ErrWeakPassword) {
		t.Fatalf("short password: err = %v, want ErrWeakPassword", err)
	}
	if _, err := f.svc.Redeem(ctx, token, user.RedeemRequest{Password: goodPassword}); err != nil {
		t.Fatalf("Redeem after a refused password: %v", err)
	}
	if _, err := f.svc.Redeem(ctx, token, user.RedeemRequest{Password: goodPassword}); !errors.Is(err, user.ErrLinkNotFound) {
		t.Errorf("second use: err = %v, want ErrLinkNotFound", err)
	}
	if _, _, err := f.svc.Link(ctx, token); !errors.Is(err, user.ErrLinkNotFound) {
		t.Errorf("Link after use: err = %v, want ErrLinkNotFound", err)
	}

	for _, login := range []string{"ana", " ANA ", "ana@example.com", "Ana@Example.COM"} {
		got, err := f.svc.CheckPassword(ctx, login, goodPassword)
		if err != nil || got.Handle != "ana" {
			t.Errorf("CheckPassword(%q) = %+v, %v", login, got, err)
		}
	}
	acc := f.account(t, "ana")
	if acc.Invited || !acc.HasPassword || acc.PasswordLoginAt == nil || !acc.LastActive().Equal(t0) {
		t.Errorf("account after sign-in = %+v", acc)
	}
	want := []string{audit.UserCreate, audit.InviteCreate, audit.InviteUse, audit.PasswordSet}
	if got := f.actions(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("audit = %v, want %v", got, want)
	}
	for _, e := range f.audit.Events {
		for k, v := range e.Details {
			if strings.Contains(v, token) || strings.Contains(v, goodPassword) {
				t.Errorf("audit %s detail %s holds a secret", e.Action, k)
			}
		}
	}
}

func (f *fixture) account(t *testing.T, handle string) user.Account {
	t.Helper()
	all, err := f.svc.Accounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range all {
		if a.Handle == handle {
			return a
		}
	}
	t.Fatalf("no account for %q in %+v", handle, all)
	return user.Account{}
}

type ownersFunc func(context.Context) ([]artifact.OwnerUsage, error)

func (f ownersFunc) OwnerUsage(ctx context.Context) ([]artifact.OwnerUsage, error) { return f(ctx) }

func TestPeople(t *testing.T) {
	f := accounts(t, user.Options{})
	f.invite(t, "ana", "ana@example.com")
	f.mustCreate(t, "bob", "", user.RoleAdmin)
	owners := ownersFunc(func(context.Context) ([]artifact.OwnerUsage, error) {
		return []artifact.OwnerUsage{{Handle: "ana", Artifacts: 2, Bytes: 300}}, nil
	})

	people, err := f.svc.People(t.Context(), owners)
	if err != nil {
		t.Fatalf("People: %v", err)
	}
	type row struct {
		Handle      string
		HasPassword bool
		Usage       artifact.OwnerUsage
	}
	got := make([]row, 0, len(people))
	for _, p := range people {
		if p.Account.Handle != p.User.Handle {
			t.Errorf("People: %q has the account of %q", p.User.Handle, p.Account.Handle)
		}
		got = append(got, row{p.User.Handle, p.Account.HasPassword, p.Usage})
	}
	want := []row{
		{"ana", true, artifact.OwnerUsage{Handle: "ana", Artifacts: 2, Bytes: 300}},
		{"bob", false, artifact.OwnerUsage{Handle: "bob"}},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("People (-want +got):\n%s", diff)
	}

	failed := errors.New("usage unavailable")
	_, err = f.svc.People(t.Context(), ownersFunc(func(context.Context) ([]artifact.OwnerUsage, error) { return nil, failed }))
	if !errors.Is(err, failed) {
		t.Errorf("People with failing owners: err = %v, want %v", err, failed)
	}
}

func TestCheckPasswordRefusals(t *testing.T) {
	ctx := context.Background()
	f := accounts(t, user.Options{})
	f.invite(t, "ana", "ana@example.com")
	f.mustCreate(t, "keyonly", "keys@example.com", "")
	f.invite(t, "gone", "")
	f.mustCreate(t, "boss", "", user.RoleAdmin)
	if _, err := f.svc.UpdateUser(ctx, "gone", user.UpdateUserRequest{Disabled: new(true)}); err != nil {
		t.Fatal(err)
	}
	for name, tt := range map[string]struct{ login, password string }{
		"wrong password":    {"ana", goodPassword + "!"},
		"empty password":    {"ana", ""},
		"unknown handle":    {"nobody", goodPassword},
		"unknown email":     {"nobody@example.com", goodPassword},
		"empty login":       {"", goodPassword},
		"no password set":   {"keyonly", goodPassword},
		"no password, mail": {"keys@example.com", ""},
		"disabled":          {"gone", goodPassword},
		"oversized":         {"ana", strings.Repeat("x", 5000)},
		"oversized login":   {strings.Repeat("a", 300), goodPassword},
	} {
		if _, err := f.svc.CheckPassword(ctx, tt.login, tt.password); !errors.Is(err, user.ErrBadPassword) {
			t.Errorf("%s: err = %v, want ErrBadPassword", name, err)
		}
	}
}

func TestPasswordHashRenewed(t *testing.T) {
	ctx := context.Background()
	f := accounts(t, user.Options{})
	f.invite(t, "ana", "")
	before, err := f.store.PasswordHash(ctx, "ana")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(before, "$argon2id$v=19$m=64,t=1,p=1$") {
		t.Fatalf("hash = %q, want the argon2id PHC form with the configured parameters", before)
	}
	if _, err := f.svc.CheckPassword(ctx, "ana", goodPassword); err != nil {
		t.Fatal(err)
	}
	if same, _ := f.store.PasswordHash(ctx, "ana"); same != before {
		t.Error("a hash with the current parameters was rewritten")
	}

	f.opts.Argon2 = user.Argon2{Memory: 128, Time: 2, Threads: 1}
	svc := f.restart()
	if _, err := svc.CheckPassword(ctx, "ana", "wrong wrong wrong"); !errors.Is(err, user.ErrBadPassword) {
		t.Fatal(err)
	}
	if same, _ := f.store.PasswordHash(ctx, "ana"); same != before {
		t.Error("a refused sign-in renewed the hash")
	}
	if _, err := svc.CheckPassword(ctx, "ana", goodPassword); err != nil {
		t.Fatalf("sign-in against the old parameters: %v", err)
	}
	after, _ := f.store.PasswordHash(ctx, "ana")
	if !strings.HasPrefix(after, "$argon2id$v=19$m=128,t=2,p=1$") {
		t.Errorf("hash after sign-in = %q, want the new parameters", after)
	}
	if _, err := svc.CheckPassword(ctx, "ana", goodPassword); err != nil {
		t.Errorf("sign-in against the renewed hash: %v", err)
	}
}

func TestDefaultArgon2(t *testing.T) {
	if p := user.DefaultArgon2; p.Memory != 64<<10 || p.Time != 3 || p.Threads != 4 {
		t.Errorf("DefaultArgon2 = %+v, want RFC 9106's second option", p)
	}
}

func TestPasswordPolicy(t *testing.T) {
	ctx := context.Background()
	f := accounts(t, user.Options{MinPasswordLength: 10})
	_, _, token, err := f.svc.Invite(ctx, user.InviteRequest{Handle: "ana"})
	if err != nil {
		t.Fatal(err)
	}
	for name, pw := range map[string]string{
		"short":         "ninechars",
		"long":          strings.Repeat("x", user.MaxPasswordLength+1),
		"not utf-8":     "0123456789\xff\xfe",
		"long in kanji": strings.Repeat("飾", user.MaxPasswordLength+1),
	} {
		if _, err := f.svc.Redeem(ctx, token, user.RedeemRequest{Password: pw}); !errors.Is(err, user.ErrWeakPassword) {
			t.Errorf("%s: err = %v, want ErrWeakPassword", name, err)
		}
	}
	// Length counts characters, and nothing else is asked of a password.
	if _, err := f.svc.Redeem(ctx, token, user.RedeemRequest{Password: strings.Repeat("飾", 10)}); err != nil {
		t.Errorf("ten characters of three bytes each: %v", err)
	}
}

func TestLinkLifetimes(t *testing.T) {
	ctx := context.Background()
	f := accounts(t, user.Options{})
	_, invite, inviteToken, err := f.svc.Invite(ctx, user.InviteRequest{Handle: "ana"})
	if err != nil {
		t.Fatal(err)
	}
	f.invite(t, "ben", "")
	reset, resetToken, err := f.svc.IssueLink(ctx, "ben", user.PurposeReset, false)
	if err != nil {
		t.Fatal(err)
	}
	setup, _, err := accounts(t, user.Options{}).svc.SetupLink(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]time.Duration{
		"invite": invite.ExpiresAt.Sub(t0), "reset": reset.ExpiresAt.Sub(t0), "setup": setup.ExpiresAt.Sub(t0),
	} {
		want := map[string]time.Duration{"invite": 7 * 24 * time.Hour, "reset": 24 * time.Hour, "setup": time.Hour}[name]
		if got != want {
			t.Errorf("%s link lives %s, want %s", name, got, want)
		}
	}

	f.clock.Advance(24*time.Hour - time.Nanosecond)
	if _, _, err := f.svc.Link(ctx, resetToken); err != nil {
		t.Errorf("reset link just before its expiry: %v", err)
	}
	f.clock.Advance(time.Nanosecond)
	if _, err := f.svc.Redeem(ctx, resetToken, user.RedeemRequest{Password: goodPassword}); !errors.Is(err, user.ErrLinkNotFound) {
		t.Errorf("reset link at its expiry: err = %v, want ErrLinkNotFound", err)
	}
	links, err := f.svc.Links(ctx)
	if err != nil || len(links) != 1 || links[0].ID != invite.ID {
		t.Errorf("Links = %+v, %v; want the invite alone", links, err)
	}
	f.clock.Advance(6 * 24 * time.Hour)
	if _, _, err := f.svc.Link(ctx, inviteToken); !errors.Is(err, user.ErrLinkNotFound) {
		t.Errorf("invite at its expiry: err = %v, want ErrLinkNotFound", err)
	}
	if acc := f.account(t, "ana"); !acc.Invited {
		t.Errorf("account with a lapsed invite = %+v, want still invited", acc)
	}
}

func TestLinkReplacedAndRevoked(t *testing.T) {
	ctx := context.Background()
	f := accounts(t, user.Options{})
	_, first, firstToken, err := f.svc.Invite(ctx, user.InviteRequest{Handle: "ana"})
	if err != nil {
		t.Fatal(err)
	}
	second, secondToken, err := f.svc.IssueLink(ctx, "ana", user.PurposeInvite, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.Link(ctx, firstToken); !errors.Is(err, user.ErrLinkNotFound) {
		t.Errorf("replaced link: err = %v, want ErrLinkNotFound", err)
	}
	// A reset link is another purpose, and leaves the invite alone.
	if _, _, err := f.svc.IssueLink(ctx, "ana", user.PurposeReset, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.Link(ctx, secondToken); err != nil {
		t.Errorf("invite after a reset link was made: %v", err)
	}
	if err := f.svc.RevokeLink(ctx, first.ID); !errors.Is(err, user.ErrLinkNotFound) {
		t.Errorf("revoking a replaced link: err = %v, want ErrLinkNotFound", err)
	}
	if err := f.svc.RevokeLink(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Redeem(ctx, secondToken, user.RedeemRequest{Password: goodPassword}); !errors.Is(err, user.ErrLinkNotFound) {
		t.Errorf("revoked link: err = %v, want ErrLinkNotFound", err)
	}
	if e := f.lastEvent(t); e.Action != audit.InviteRevoke || e.Details["handle"] != "ana" {
		t.Errorf("last event = %+v", e)
	}
	for _, bad := range []string{"", "kzl_short", "kzk_" + strings.Repeat("a", 52), user.LinkPrefix + strings.Repeat("A", 52)} {
		if _, _, err := f.svc.Link(ctx, bad); !errors.Is(err, user.ErrLinkNotFound) {
			t.Errorf("Link(%q): err = %v, want ErrLinkNotFound", bad, err)
		}
	}
}

func TestLinkOfDisabledPerson(t *testing.T) {
	ctx := context.Background()
	f := accounts(t, user.Options{})
	f.mustCreate(t, "boss", "", user.RoleAdmin)
	f.invite(t, "ana", "")
	_, token, err := f.svc.IssueLink(ctx, "ana", user.PurposeReset, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.UpdateUser(ctx, "ana", user.UpdateUserRequest{Disabled: new(true)}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.Link(ctx, token); !errors.Is(err, user.ErrLinkNotFound) {
		t.Errorf("Link: err = %v, want ErrLinkNotFound", err)
	}
	if _, err := f.svc.Redeem(ctx, token, user.RedeemRequest{Password: goodPassword}); !errors.Is(err, user.ErrLinkNotFound) {
		t.Errorf("Redeem: err = %v, want ErrLinkNotFound", err)
	}
	if _, _, err := f.svc.IssueLink(ctx, "ana", user.PurposeReset, false); !errors.Is(err, user.ErrDisabled) {
		t.Errorf("IssueLink: err = %v, want ErrDisabled", err)
	}
}

func TestResetEndsSessions(t *testing.T) {
	ctx := context.Background()
	var ended, revoked []string
	f := accounts(t, user.Options{
		EndSessions: func(_ context.Context, handle string, keep int64) error {
			ended = append(ended, handle)
			if keep != 0 {
				t.Errorf("a reset keeps session %d", keep)
			}
			return nil
		},
		RevokeGrants: func(_ context.Context, handle string) error { revoked = append(revoked, handle); return nil },
	})
	f.invite(t, "ana", "")
	_, secret := f.mustIssue(t, "ana", nil, 0)
	if len(ended)+len(revoked) != 0 {
		t.Fatalf("an invite ended sessions %v and revoked grants %v", ended, revoked)
	}

	_, token, err := f.svc.IssueLink(ctx, "ana", user.PurposeReset, false)
	if err != nil {
		t.Fatal(err)
	}
	const next = "another long password"
	if _, err := f.svc.Redeem(ctx, token, user.RedeemRequest{Password: next}); err != nil {
		t.Fatal(err)
	}
	if len(ended) != 1 || len(revoked) != 0 || !f.authenticates(secret) {
		t.Errorf("plain reset: ended %v, revoked %v, key works %v; want sessions only", ended, revoked, f.authenticates(secret))
	}
	if _, err := f.svc.CheckPassword(ctx, "ana", goodPassword); !errors.Is(err, user.ErrBadPassword) {
		t.Errorf("old password after a reset: err = %v", err)
	}
	if _, err := f.svc.CheckPassword(ctx, "ana", next); err != nil {
		t.Errorf("new password after a reset: %v", err)
	}

	_, token, err = f.svc.IssueLink(ctx, "ana", user.PurposeReset, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Redeem(ctx, token, user.RedeemRequest{Password: goodPassword}); err != nil {
		t.Fatal(err)
	}
	if len(revoked) != 1 || f.authenticates(secret) {
		t.Errorf("reset with revoke-all: revoked %v, key works %v", revoked, f.authenticates(secret))
	}
}

func TestChangePassword(t *testing.T) {
	ctx := context.Background()
	var kept []int64
	f := accounts(t, user.Options{EndSessions: func(_ context.Context, _ string, keep int64) error {
		kept = append(kept, keep)
		return nil
	}})
	f.invite(t, "ana", "ana@example.com")
	const next = "a new long password"
	if err := f.svc.ConfirmPassword(ctx, "ana", "not the password"); !errors.Is(err, user.ErrBadPassword) {
		t.Errorf("wrong current password: err = %v", err)
	}
	if err := f.svc.ConfirmPassword(ctx, "ana@example.com", goodPassword); !errors.Is(err, user.ErrBadPassword) {
		t.Errorf("current password confirmed by email: err = %v", err)
	}
	if err := f.svc.ChangePasswordConfirmed(ctx, "ana", "short", 7); !errors.Is(err, user.ErrWeakPassword) {
		t.Errorf("weak new password: err = %v", err)
	}
	if len(kept) != 0 {
		t.Fatalf("a refused change ended sessions: %v", kept)
	}
	if err := f.svc.ConfirmPassword(ctx, "ana", goodPassword); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.ChangePasswordConfirmed(ctx, "ana", next, 7); err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 || kept[0] != 7 {
		t.Errorf("sessions kept = %v, want the one the change was made in", kept)
	}
	if _, err := f.svc.CheckPassword(ctx, "ana", next); err != nil {
		t.Error(err)
	}
	if e := f.lastEvent(t); e.Action != audit.PasswordSet || e.Details["via"] != "change" || e.Actor != "ana" {
		t.Errorf("last event = %+v", e)
	}
}

func TestSetup(t *testing.T) {
	ctx := context.Background()
	f := accounts(t, user.Options{DefaultQuota: 1 << 30})
	_, oldToken, err := f.svc.SetupLink(ctx)
	if err != nil {
		t.Fatal(err)
	}
	l, token, err := f.svc.SetupLink(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.Link(ctx, oldToken); !errors.Is(err, user.ErrLinkNotFound) {
		t.Errorf("replaced setup link: err = %v, want ErrLinkNotFound", err)
	}
	got, who, err := f.svc.Link(ctx, token)
	if err != nil || who != nil || got.Purpose != user.PurposeSetup || got.ID != l.ID {
		t.Fatalf("Link = %+v, %+v, %v", got, who, err)
	}
	if _, err := f.svc.Redeem(ctx, token, user.RedeemRequest{Handle: "Not Valid", Password: goodPassword}); !errors.Is(err, user.ErrInvalidHandle) {
		t.Fatalf("invalid handle: err = %v", err)
	}
	if _, err := f.svc.Redeem(ctx, token, user.RedeemRequest{Handle: "admin", Password: goodPassword}); !errors.Is(err, user.ErrInvalidHandle) {
		t.Fatalf("reserved handle: err = %v", err)
	}
	u, err := f.svc.Redeem(ctx, token, user.RedeemRequest{Handle: "peter", Email: "Peter@Example.com", DisplayName: " Peter ", Password: goodPassword})
	if err != nil {
		t.Fatal(err)
	}
	if u.Role != user.RoleAdmin || u.Email != "peter@example.com" || u.DisplayName != "Peter" || u.QuotaBytes != 1<<30 || u.ID == 0 {
		t.Errorf("first user = %+v", u)
	}
	if _, err := f.svc.CheckPassword(ctx, "peter", goodPassword); err != nil {
		t.Error(err)
	}
	if _, _, err := f.svc.SetupLink(ctx); !errors.Is(err, user.ErrSetupDone) {
		t.Errorf("SetupLink with a user: err = %v, want ErrSetupDone", err)
	}
	want := []string{audit.InviteCreate, audit.InviteCreate, audit.UserCreate, audit.InviteUse, audit.PasswordSet}
	if got := f.actions(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("audit = %v, want %v", got, want)
	}
}

func TestSetupLinkDeadOnceAUserExists(t *testing.T) {
	ctx := context.Background()
	f := accounts(t, user.Options{})
	_, token, err := f.svc.SetupLink(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.mustCreate(t, "ana", "", "")
	if _, _, err := f.svc.Link(ctx, token); !errors.Is(err, user.ErrSetupDone) {
		t.Errorf("Link: err = %v, want ErrSetupDone", err)
	}
	if _, err := f.svc.Redeem(ctx, token, user.RedeemRequest{Handle: "eve", Password: goodPassword}); !errors.Is(err, user.ErrSetupDone) {
		t.Errorf("Redeem: err = %v, want ErrSetupDone", err)
	}
	if _, err := f.svc.GetUser(ctx, "eve"); !errors.Is(err, user.ErrNotFound) {
		t.Errorf("eve exists: %v", err)
	}
}

// Two people race for a fresh instance with the one link: one becomes its
// admin and the other gets nothing.
func TestSetupClaimedOnce(t *testing.T) {
	ctx := context.Background()
	f := accounts(t, user.Options{})
	_, token, err := f.svc.SetupLink(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const racers = 8
	var (
		wg   sync.WaitGroup
		errs [racers]error
	)
	for i := range racers {
		wg.Go(func() {
			_, errs[i] = f.svc.Redeem(ctx, token, user.RedeemRequest{Handle: "racer" + string(rune('a'+i)), Password: goodPassword})
		})
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case !errors.Is(err, user.ErrLinkNotFound) && !errors.Is(err, user.ErrSetupDone):
			t.Errorf("loser: err = %v", err)
		}
	}
	users, err := f.svc.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if won != 1 || len(users) != 1 || users[0].Role != user.RoleAdmin {
		t.Errorf("%d winners and users %+v, want one admin", won, users)
	}
}

func TestInviteAdminNeedsAdmin(t *testing.T) {
	ctx := context.Background()
	f := accounts(t, user.Options{})
	req := user.InviteRequest{Handle: "ana", Role: user.RoleAdmin}
	member := auth.ContextWithPrincipal(ctx, auth.Principal{Handle: "ben", Scopes: auth.MemberScopes})
	for name, c := range map[string]context.Context{"nobody": ctx, "a member": member} {
		if _, _, _, err := f.svc.Invite(c, req); !errors.Is(err, user.ErrInvalidRole) {
			t.Errorf("%s inviting an admin: err = %v, want ErrInvalidRole", name, err)
		}
	}
	if _, err := f.svc.GetUser(ctx, "ana"); !errors.Is(err, user.ErrNotFound) {
		t.Errorf("a refused invite created the user: %v", err)
	}
	u, l, _, err := f.svc.Invite(asAdmin(ctx, "boss"), req)
	if err != nil || u.Role != user.RoleAdmin || l.CreatedBy != "boss" {
		t.Errorf("an admin inviting an admin = %+v, %+v, %v", u, l, err)
	}
}

func TestLocalAccountsOff(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, user.Options{Argon2: cheap})
	f.mustCreate(t, "ana", "", "")
	if _, _, _, err := f.svc.Invite(ctx, user.InviteRequest{Handle: "ben"}); !errors.Is(err, user.ErrLocalOff) {
		t.Errorf("Invite: err = %v, want ErrLocalOff", err)
	}
	if _, err := f.svc.GetUser(ctx, "ben"); !errors.Is(err, user.ErrNotFound) {
		t.Errorf("a refused invite created the user: %v", err)
	}
	if _, _, err := f.svc.IssueLink(ctx, "ana", user.PurposeReset, false); !errors.Is(err, user.ErrLocalOff) {
		t.Errorf("IssueLink: err = %v, want ErrLocalOff", err)
	}
	if _, _, err := f.svc.SetupLink(ctx); !errors.Is(err, user.ErrLocalOff) {
		t.Errorf("SetupLink: err = %v, want ErrLocalOff", err)
	}
	if _, err := f.svc.CheckPassword(ctx, "ana", goodPassword); !errors.Is(err, user.ErrBadPassword) {
		t.Errorf("CheckPassword: err = %v, want ErrBadPassword", err)
	}
}

func TestLastAdminGuard(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, user.Options{})
	f.mustCreate(t, "boss", "", user.RoleAdmin)
	f.mustCreate(t, "ana", "", "")
	for name, req := range map[string]user.UpdateUserRequest{
		"disable": {Disabled: new(true)},
		"demote":  {Role: ptr(user.RoleMember)},
	} {
		if _, err := f.svc.UpdateUser(ctx, "boss", req); !errors.Is(err, user.ErrLastAdmin) {
			t.Errorf("%s the last admin: err = %v, want ErrLastAdmin", name, err)
		}
	}
	u, err := f.svc.GetUser(ctx, "boss")
	if err != nil || u.Role != user.RoleAdmin || u.DisabledAt != nil {
		t.Fatalf("the last admin after refused changes = %+v, %v", u, err)
	}
	// What leaves them an admin is not guarded.
	if _, err := f.svc.UpdateUser(ctx, "boss", user.UpdateUserRequest{QuotaBytes: new(int64(5)), Role: ptr(user.RoleAdmin)}); err != nil {
		t.Errorf("changing the last admin's quota: %v", err)
	}
	if _, err := f.svc.UpdateUser(ctx, "ana", user.UpdateUserRequest{Disabled: new(true)}); err != nil {
		t.Errorf("disabling a member: %v", err)
	}

	// A disabled admin does not count as one.
	f.mustCreate(t, "second", "", user.RoleAdmin)
	if _, err := f.svc.UpdateUser(ctx, "second", user.UpdateUserRequest{Disabled: new(true)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.UpdateUser(ctx, "boss", user.UpdateUserRequest{Role: ptr(user.RoleMember)}); !errors.Is(err, user.ErrLastAdmin) {
		t.Errorf("demoting the only enabled admin: err = %v, want ErrLastAdmin", err)
	}
	if _, err := f.svc.UpdateUser(ctx, "second", user.UpdateUserRequest{Disabled: new(false)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.UpdateUser(ctx, "boss", user.UpdateUserRequest{Role: ptr(user.RoleMember)}); err != nil {
		t.Errorf("demoting one of two admins: %v", err)
	}
}

func TestSelfGuard(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, user.Options{})
	f.mustCreate(t, "boss", "", user.RoleAdmin)
	f.mustCreate(t, "second", "", user.RoleAdmin)
	self := asAdmin(ctx, "boss")
	for name, req := range map[string]user.UpdateUserRequest{
		"disable": {Disabled: new(true)},
		"demote":  {Role: ptr(user.RoleMember)},
	} {
		if _, err := f.svc.UpdateUser(self, "boss", req); !errors.Is(err, user.ErrSelf) {
			t.Errorf("%s oneself: err = %v, want ErrSelf", name, err)
		}
	}
	if _, err := f.svc.UpdateUser(self, "boss", user.UpdateUserRequest{DisplayName: new("Boss"), Role: ptr(user.RoleAdmin), Disabled: new(false)}); err != nil {
		t.Errorf("changing one's own name: %v", err)
	}
	if _, err := f.svc.UpdateUser(self, "second", user.UpdateUserRequest{Role: ptr(user.RoleMember)}); err != nil {
		t.Errorf("demoting another admin: %v", err)
	}
	// The socket and the admin token are no person.
	local := auth.ContextWithPrincipal(ctx, auth.Principal{Handle: "admin", Admin: true, Source: auth.SourceLocal})
	if _, err := f.svc.UpdateUser(local, "second", user.UpdateUserRequest{Role: ptr(user.RoleAdmin)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.UpdateUser(local, "boss", user.UpdateUserRequest{Disabled: new(true)}); err != nil {
		t.Errorf("the socket disabling one of two admins: %v", err)
	}
}

func TestKeyCap(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, user.Options{MaxKeys: 3})
	f.mustCreate(t, "ana", "", "")
	f.mustCreate(t, "ben", "", "")
	first, _ := f.mustIssue(t, "ana", nil, 0)
	short, _ := f.mustIssue(t, "ana", nil, time.Hour)
	f.mustIssue(t, "ana", nil, 0)

	refused := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, user.ErrTooManyKeys) {
			t.Errorf("%s at the cap: err = %v, want ErrTooManyKeys", what, err)
		}
	}
	_, _, err := f.svc.IssueKey(ctx, user.IssueKeyRequest{Handle: "ana"})
	refused("an admin issuing", err)
	_, _, err = f.svc.IssueKey(ctx, user.IssueKeyRequest{Handle: "ana", With: first.Prefix})
	refused("a key issuing", err)
	_, _, err = f.svc.IssueKey(ctx, user.IssueKeyRequest{Handle: "ana", Session: true})
	refused("a session issuing", err)

	// A rotation replaces a key: the one it replaces is not a key more,
	// though both are live for the overlap. That is one over the cap, once.
	next, _, _, err := f.svc.RotateKey(ctx, user.KeyRef{Handle: "ana", Prefix: first.Prefix}, time.Hour)
	if err != nil {
		t.Fatalf("rotating with an overlap at the cap: %v", err)
	}
	_, _, _, err = f.svc.RotateKey(ctx, user.KeyRef{Handle: "ana", Prefix: next.Prefix}, time.Hour)
	refused("rotating again inside the overlap", err)
	_, _, err = f.svc.IssueKey(ctx, user.IssueKeyRequest{Handle: "ana"})
	refused("issuing inside the overlap", err)

	// The cap is each person's own, and counts live keys only.
	f.mustIssue(t, "ben", nil, 0)
	f.clock.Advance(time.Hour)
	if _, _, err := f.svc.IssueKey(ctx, user.IssueKeyRequest{Handle: "ana"}); err != nil {
		t.Errorf("issuing once %s and the rotated key expired: %v", short.Prefix, err)
	}
	_, _, err = f.svc.IssueKey(ctx, user.IssueKeyRequest{Handle: "ana"})
	refused("issuing again", err)
	keys, _ := f.svc.ListKeys(ctx, "ana")
	if err := f.svc.RevokeKey(ctx, user.KeyRef{Handle: "ana", Prefix: keys[0].Prefix}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.IssueKey(ctx, user.IssueKeyRequest{Handle: "ana"}); err != nil {
		t.Errorf("issuing after a revoke: %v", err)
	}
}

func TestDefaultKeyCap(t *testing.T) {
	if user.DefaultMaxKeys != 50 {
		t.Errorf("DefaultMaxKeys = %d, want 50", user.DefaultMaxKeys)
	}
}

func TestSessionKeys(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, user.Options{})
	f.mustCreate(t, "boss", "", user.RoleAdmin)
	f.mustCreate(t, "ana", "", "")

	// An admin's own session issues what a member's does.
	for _, handle := range []string{"boss", "ana"} {
		k, _ := f.issue(t, user.IssueKeyRequest{Handle: handle, Session: true})
		if strings.Join(k.Scopes, " ") != strings.Join(auth.MemberScopes, " ") || k.IssuedBy != "" {
			t.Errorf("%s's session key = %+v, want the member scopes and no issuer", handle, k)
		}
		_, _, err := f.svc.IssueKey(ctx, user.IssueKeyRequest{Handle: handle, Session: true, Scopes: auth.AllScopes})
		if !errors.Is(err, user.ErrKeyTooWide) {
			t.Errorf("%s's session asking for admin: err = %v, want ErrKeyTooWide", handle, err)
		}
	}
	narrow, _ := f.issue(t, user.IssueKeyRequest{Handle: "ana", Session: true, Scopes: []string{auth.ScopeRead}, Slugs: []string{"docs-*"}})
	if len(narrow.Scopes) != 1 || len(narrow.Slugs) != 1 {
		t.Errorf("narrowed session key = %+v", narrow)
	}

	// A session opened with a key is bounded by it, as the routes are.
	_, _, err := f.svc.IssueKey(ctx, user.IssueKeyRequest{Handle: "ana", Session: true, With: narrow.Prefix, Scopes: []string{auth.ScopeWrite}})
	if !errors.Is(err, user.ErrKeyTooWide) {
		t.Errorf("a restricted key's session widening scopes: err = %v, want ErrKeyTooWide", err)
	}
	_, _, err = f.svc.IssueKey(ctx, user.IssueKeyRequest{Handle: "ana", Session: true, With: narrow.Prefix, Slugs: []string{"other-*"}})
	if !errors.Is(err, user.ErrKeyTooWide) {
		t.Errorf("a restricted key's session widening slugs: err = %v, want ErrKeyTooWide", err)
	}
	child, _ := f.issue(t, user.IssueKeyRequest{Handle: "ana", Session: true, With: narrow.Prefix})
	if child.IssuedBy != narrow.Prefix || len(child.Scopes) != 1 || len(child.Slugs) != 1 {
		t.Errorf("key from a restricted key's session = %+v", child)
	}

	// The page rotates any key but one that holds admin, and revokes any.
	adminKey, _ := f.issue(t, user.IssueKeyRequest{Handle: "boss", Scopes: auth.AllScopes})
	if _, _, _, err := f.svc.RotateKey(ctx, user.KeyRef{Handle: "boss", Prefix: adminKey.Prefix, Session: true}, 0); !errors.Is(err, user.ErrKeyTooWide) {
		t.Errorf("a session rotating an admin key: err = %v, want ErrKeyTooWide", err)
	}
	if _, _, _, err := f.svc.RotateKey(ctx, user.KeyRef{Handle: "ana", Prefix: narrow.Prefix, Session: true}, 0); err != nil {
		t.Errorf("a session rotating a member key: %v", err)
	}
	if err := f.svc.RevokeKey(ctx, user.KeyRef{Handle: "boss", Prefix: adminKey.Prefix, Session: true}); err != nil {
		t.Errorf("a session revoking an admin key: %v", err)
	}
	// Another person's prefix is not found, never forbidden.
	if err := f.svc.RevokeKey(ctx, user.KeyRef{Handle: "ana", Prefix: adminKey.Prefix, Session: true}); !errors.Is(err, user.ErrKeyNotFound) {
		t.Errorf("revoking another person's key: err = %v, want ErrKeyNotFound", err)
	}
	if _, _, _, err := f.svc.RotateKey(ctx, user.KeyRef{Handle: "ana", Prefix: adminKey.Prefix, Session: true}, 0); !errors.Is(err, user.ErrKeyNotFound) {
		t.Errorf("rotating another person's key: err = %v, want ErrKeyNotFound", err)
	}
}

func TestKeyStatusScopes(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, user.Options{})
	f.mustCreate(t, "boss", "", user.RoleAdmin)
	f.mustCreate(t, "second", "", user.RoleAdmin)
	k, _ := f.issue(t, user.IssueKeyRequest{Handle: "boss", Scopes: auth.AllScopes})
	st, err := f.svc.KeyStatus(ctx, k.Prefix)
	if err != nil || len(st.Scopes) != len(auth.AllScopes) {
		t.Fatalf("KeyStatus = %+v, %v", st, err)
	}
	if _, err := f.svc.UpdateUser(ctx, "boss", user.UpdateUserRequest{Role: ptr(user.RoleMember)}); err != nil {
		t.Fatal(err)
	}
	st, err = f.svc.KeyStatus(ctx, k.Prefix)
	if err != nil || strings.Join(st.Scopes, " ") != strings.Join(auth.MemberScopes, " ") {
		t.Errorf("KeyStatus after a demotion = %+v, %v; want the member scopes", st, err)
	}
}

// Disabling a person ends their links for good: one that was pending works
// no better after they are enabled again.
func TestDisableRevokesLinks(t *testing.T) {
	ctx := context.Background()
	f := accounts(t, user.Options{})
	f.mustCreate(t, "boss", "", user.RoleAdmin)
	_, _, invite, err := f.svc.Invite(ctx, user.InviteRequest{Handle: "ana"})
	if err != nil {
		t.Fatal(err)
	}
	for _, disabled := range []bool{true, false} {
		if _, err := f.svc.UpdateUser(ctx, "ana", user.UpdateUserRequest{Disabled: new(disabled)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := f.svc.Link(ctx, invite); !errors.Is(err, user.ErrLinkNotFound) {
		t.Errorf("an invite from before the disable, after the enable: err = %v, want ErrLinkNotFound", err)
	}
	if _, err := f.svc.Redeem(ctx, invite, user.RedeemRequest{Password: goodPassword}); !errors.Is(err, user.ErrLinkNotFound) {
		t.Errorf("Redeem: err = %v, want ErrLinkNotFound", err)
	}
	if links, _ := f.svc.Links(ctx); len(links) != 0 {
		t.Errorf("links still listed: %+v", links)
	}
}

// An invite that was passed over, because the person set their password
// another way, must not set it again for whoever finds the link later.
func TestPassedOverLinkIsDead(t *testing.T) {
	ctx := context.Background()
	var ended int
	f := accounts(t, user.Options{EndSessions: func(context.Context, string, int64) error { ended++; return nil }})
	_, _, invite, err := f.svc.Invite(ctx, user.InviteRequest{Handle: "ana"})
	if err != nil {
		t.Fatal(err)
	}
	_, reset, err := f.svc.IssueLink(ctx, "ana", user.PurposeReset, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Redeem(ctx, reset, user.RedeemRequest{Password: goodPassword}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Redeem(ctx, invite, user.RedeemRequest{Password: "an attacker's password"}); !errors.Is(err, user.ErrLinkNotFound) {
		t.Fatalf("the invite after the reset was used: err = %v, want ErrLinkNotFound", err)
	}
	if _, err := f.svc.CheckPassword(ctx, "ana", goodPassword); err != nil {
		t.Errorf("ana's password after the stale invite was tried: %v", err)
	}
	// Nor is a new invite made for someone who has a password: a reset is
	// what ends their sessions.
	if _, _, err := f.svc.IssueLink(ctx, "ana", user.PurposeInvite, false); !errors.Is(err, user.ErrHasPassword) {
		t.Errorf("an invite for a person with a password: err = %v, want ErrHasPassword", err)
	}
	if _, _, err := f.svc.IssueLink(ctx, "ana", user.PurposeReset, false); err != nil {
		t.Errorf("a reset link for a person with a password: %v", err)
	}
}

// Changing a password is no sign-in, and ends a reset link made before it.
func TestChangePasswordSideEffects(t *testing.T) {
	ctx := context.Background()
	f := accounts(t, user.Options{})
	f.invite(t, "ana", "")
	if acc := f.account(t, "ana"); acc.PasswordLoginAt != nil {
		t.Fatalf("a sign-in is recorded before any: %+v", acc)
	}
	_, reset, err := f.svc.IssueLink(ctx, "ana", user.PurposeReset, false)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := f.store.PasswordHash(ctx, "ana")
	if err := f.svc.ConfirmPassword(ctx, "ana", "not her password"); !errors.Is(err, user.ErrBadPassword) {
		t.Fatal(err)
	}
	if after, _ := f.store.PasswordHash(ctx, "ana"); after != before {
		t.Error("a refused confirmation rewrote the hash")
	}
	if err := f.svc.ConfirmPassword(ctx, "ana", goodPassword); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.ChangePasswordConfirmed(ctx, "ana", "a new long password", 0); err != nil {
		t.Fatal(err)
	}
	if acc := f.account(t, "ana"); acc.PasswordLoginAt != nil || acc.LastActive() != nil {
		t.Errorf("changing the password counted as a sign-in: %+v", acc)
	}
	if _, _, err := f.svc.Link(ctx, reset); !errors.Is(err, user.ErrLinkNotFound) {
		t.Errorf("a reset link from before the change: err = %v, want ErrLinkNotFound", err)
	}
}

// An invite either makes the person and their link, or neither.
func TestInviteIsAtomic(t *testing.T) {
	ctx := context.Background()
	f := accounts(t, user.Options{})
	f.mustCreate(t, "ana", "ana@example.com", "")
	events := len(f.audit.Events)
	if _, _, _, err := f.svc.Invite(ctx, user.InviteRequest{Handle: "ben", Email: "ANA@example.com"}); !errors.Is(err, user.ErrEmailTaken) {
		t.Fatalf("an invite with a taken email: err = %v", err)
	}
	if _, err := f.svc.GetUser(ctx, "ben"); !errors.Is(err, user.ErrNotFound) {
		t.Error("the refused invite left a person without a link")
	}
	if links, _ := f.svc.Links(ctx); len(links) != 0 || len(f.audit.Events) != events {
		t.Errorf("the refused invite left links %+v or audit events", links)
	}
	if _, _, _, err := f.svc.Invite(ctx, user.InviteRequest{Handle: "ben"}); err != nil {
		t.Errorf("the handle is free for the next invite: %v", err)
	}
}

func TestHandleFor(t *testing.T) {
	ctx := context.Background()
	f := accounts(t, user.Options{})
	f.mustCreate(t, "ana", "ana@example.com", "")
	for login, want := range map[string]string{"ana": "ana", " ANA ": "ana", "Ana@Example.com": "ana", "nobody": "", "x@example.com": "", "": ""} {
		if got := f.svc.HandleFor(ctx, login); got != want {
			t.Errorf("HandleFor(%q) = %q, want %q", login, got, want)
		}
	}
}

func TestAccountKey(t *testing.T) {
	ctx := context.Background()
	f := accounts(t, user.Options{})
	f.mustCreate(t, "ana", "ana@example.com", "")
	long := strings.Repeat("x", user.MaxLoginLen+10)
	for login, want := range map[string]string{
		"ana":             "ana",
		"Ana@Example.com": "ana",
		"Nobody":          "nobody",
		long:              long[:user.MaxLoginLen],
	} {
		if got := f.svc.AccountKey(ctx, login); got != want {
			t.Errorf("AccountKey(%q) = %q, want %q", login, got, want)
		}
	}
}

func TestActiveUser(t *testing.T) {
	ctx := context.Background()
	f := accounts(t, user.Options{})
	f.mustCreate(t, "ana", "ana@example.com", "")
	if u, err := f.svc.ActiveUser(ctx, "ana"); err != nil || u.Handle != "ana" {
		t.Errorf("ActiveUser(ana) = %v, %v; want ana", u, err)
	}
	if _, err := f.svc.ActiveUser(ctx, "nobody"); !errors.Is(err, user.ErrNotFound) {
		t.Errorf("ActiveUser(nobody): err = %v, want ErrNotFound", err)
	}
	if _, err := f.svc.UpdateUser(ctx, "ana", user.UpdateUserRequest{Disabled: new(true)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ActiveUser(ctx, "ana"); !errors.Is(err, user.ErrNotFound) {
		t.Errorf("ActiveUser of a disabled person: err = %v, want ErrNotFound", err)
	}
}
