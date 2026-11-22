package passkey_test

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/audit"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/passkey"
	"github.com/PeterBooker/kazari/internal/passkey/passkeytest"
	"github.com/PeterBooker/kazari/internal/sqlite"
	"github.com/PeterBooker/kazari/internal/user"
)

const (
	apex    = "https://art.example.com"
	mcp     = "https://mcp.art.example.com"
	rpID    = "art.example.com"
	artHost = "https://q3-report.art.example.com"
)

var t0 = time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)

type clock struct{ t time.Time }

func (c *clock) Now() time.Time { return c.t }

type fixture struct {
	t     *testing.T
	users *user.Service
	audit *audit.Memory
	clock *clock
	svc   *passkey.Service
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "kazari.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	f := &fixture{t: t, audit: &audit.Memory{}, clock: &clock{t: t0}}
	f.users = user.New(sqlite.NewUserStore(db), f.audit, user.Options{
		LocalAccounts: true, Now: f.clock.Now, Argon2: user.Argon2{Memory: 64, Time: 1, Threads: 1},
	})
	f.svc, err = passkey.New(f.users, passkey.Options{RPID: rpID, Origins: []string{apex, mcp}, Now: f.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) person(handle string) *user.User {
	f.t.Helper()
	admin := auth.ContextWithPrincipal(context.Background(), auth.Principal{Handle: "admin", Admin: true})
	u, err := f.users.CreateUser(admin, user.CreateUserRequest{Handle: handle, DisplayName: strings.ToUpper(handle)})
	if err != nil {
		f.t.Fatal(err)
	}
	return u
}

// add registers a passkey of u's held by a, from the apex.
func (f *fixture) add(u *user.User, a *passkeytest.Authenticator, name string) *user.Passkey {
	f.t.Helper()
	ctx := context.Background()
	c, err := f.svc.Registration(ctx, u)
	if err != nil {
		f.t.Fatal(err)
	}
	resp, err := a.Create(c.Options, apex)
	if err != nil {
		f.t.Fatal(err)
	}
	p, err := f.svc.Register(ctx, u, c.Token, resp, name)
	if err != nil {
		f.t.Fatalf("Register: %v", err)
	}
	return p
}

// signIn signs in with a from origin.
func (f *fixture) signIn(a *passkeytest.Authenticator, origin string) (*user.User, error) {
	f.t.Helper()
	c, err := f.svc.SignIn()
	if err != nil {
		f.t.Fatal(err)
	}
	resp, err := a.Get(c.Options, origin)
	if err != nil {
		f.t.Fatal(err)
	}
	return f.svc.Verify(context.Background(), c.Token, resp)
}

func TestRegisterAndSignIn(t *testing.T) {
	f := newFixture(t)
	ana := f.person("ana")
	var a passkeytest.Authenticator

	c, err := f.svc.Registration(context.Background(), ana)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"rp":{"name":"Kazari","id":"art.example.com"}`, `"name":"ana"`, `"displayName":"ANA"`,
		`"residentKey":"required"`, `"userVerification":"required"`} {
		if !strings.Contains(c.Options, want) {
			t.Errorf("registration options lack %s: %s", want, c.Options)
		}
	}
	p := f.add(ana, &a, "  Work   laptop ")
	if p.Name != "Work laptop" || p.UserHandle != "ana" {
		t.Fatalf("passkey = %+v", p)
	}
	if !slices.ContainsFunc(f.audit.Events, func(e audit.Event) bool { return e.Action == audit.PasskeyCreate && e.Subject == "ana" }) {
		t.Errorf("no %s event: %+v", audit.PasskeyCreate, f.audit.Events)
	}

	for _, origin := range []string{mcp, apex} {
		u, err := f.signIn(&a, origin)
		if err != nil || u.Handle != "ana" {
			t.Fatalf("sign-in from %s = %+v, %v", origin, u, err)
		}
	}
	keys, err := f.users.Passkeys(context.Background(), "ana")
	if err != nil || len(keys) != 1 || keys[0].LastUsedAt == nil || !keys[0].LastUsedAt.Equal(t0) {
		t.Fatalf("passkeys after sign-in = %+v, %v", keys, err)
	}

	// A second passkey keeps the user handle of the first.
	var phone passkeytest.Authenticator
	f.add(ana, &phone, "")
	if !slices.Equal(phone.Passkeys[0].UserID, a.Passkeys[0].UserID) {
		t.Error("the second passkey was made for another user handle")
	}
	if keys, _ := f.users.Passkeys(context.Background(), "ana"); len(keys) != 2 || keys[1].Name != "Passkey" {
		t.Errorf("passkeys = %+v", keys)
	}
}

// Script on an artifact host shares the relying party's domain, so it can
// ask a browser for an Kazari passkey; the answer names its origin.
func TestSignInRefusesOtherOrigins(t *testing.T) {
	f := newFixture(t)
	ana := f.person("ana")
	var a passkeytest.Authenticator
	f.add(ana, &a, "")
	for _, origin := range []string{artHost, "https://evil.example", "http://art.example.com"} {
		if u, err := f.signIn(&a, origin); !errors.Is(err, passkey.ErrRejected) {
			t.Errorf("sign-in from %s = %+v, %v; want ErrRejected", origin, u, err)
		}
	}
	c, err := f.svc.Registration(context.Background(), ana)
	if err != nil {
		t.Fatal(err)
	}
	var b passkeytest.Authenticator
	resp, err := b.Create(c.Options, artHost)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Register(context.Background(), ana, c.Token, resp, ""); !errors.Is(err, passkey.ErrRejected) {
		t.Errorf("registration from an artifact host: err = %v, want ErrRejected", err)
	}
}

func TestCeremonyTokens(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ana, ben := f.person("ana"), f.person("ben")
	var a passkeytest.Authenticator
	f.add(ana, &a, "")

	// A sign-in token works once.
	c, err := f.svc.SignIn()
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.Get(c.Options, mcp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Verify(ctx, c.Token, resp); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Verify(ctx, c.Token, resp); !errors.Is(err, passkey.ErrExpired) {
		t.Errorf("token used twice: err = %v, want ErrExpired", err)
	}

	// It expires.
	c, _ = f.svc.SignIn()
	resp, _ = a.Get(c.Options, mcp)
	f.clock.t = f.clock.t.Add(passkey.DefaultTTL + time.Second)
	if _, err := f.svc.Verify(ctx, c.Token, resp); !errors.Is(err, passkey.ErrExpired) {
		t.Errorf("expired token: err = %v, want ErrExpired", err)
	}

	// A registration token is ana's and no one else's, and no sign-in's.
	c, err = f.svc.Registration(ctx, ana)
	if err != nil {
		t.Fatal(err)
	}
	var other passkeytest.Authenticator
	resp, err = other.Create(c.Options, apex)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Register(ctx, ben, c.Token, resp, ""); !errors.Is(err, passkey.ErrExpired) {
		t.Errorf("ana's ceremony for ben: err = %v, want ErrExpired", err)
	}
	if _, err := f.svc.Verify(ctx, c.Token, resp); !errors.Is(err, passkey.ErrExpired) {
		t.Errorf("registration token at sign-in: err = %v, want ErrExpired", err)
	}
	for _, bad := range []string{"", "x", "a.b", strings.Replace(c.Token, ".", "x.", 1)} {
		if _, err := f.svc.Register(ctx, ana, bad, resp, ""); !errors.Is(err, passkey.ErrExpired) {
			t.Errorf("token %q: err = %v, want ErrExpired", bad, err)
		}
	}

	// Another process's tokens mean nothing here.
	restarted, err := passkey.New(f.users, passkey.Options{RPID: rpID, Origins: []string{apex, mcp}, Now: f.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	c, _ = f.svc.SignIn()
	resp, _ = a.Get(c.Options, mcp)
	if _, err := restarted.Verify(ctx, c.Token, resp); !errors.Is(err, passkey.ErrExpired) {
		t.Errorf("token of another process: err = %v, want ErrExpired", err)
	}
}

func TestSignInRefusals(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ana := f.person("ana")
	var a passkeytest.Authenticator
	p := f.add(ana, &a, "")

	// Without user verification.
	a.NoUV = true
	if _, err := f.signIn(&a, mcp); !errors.Is(err, passkey.ErrRejected) {
		t.Errorf("no user verification: err = %v, want ErrRejected", err)
	}
	a.NoUV = false

	// An answer for another challenge.
	c, _ := f.svc.SignIn()
	other, _ := f.svc.SignIn()
	resp, _ := a.Sign(a.Passkeys[0], passkeytest.Challenge(other.Options), mcp)
	if _, err := f.svc.Verify(ctx, c.Token, resp); !errors.Is(err, passkey.ErrRejected) {
		t.Errorf("answer to another challenge: err = %v, want ErrRejected", err)
	}

	// Garbage.
	c, _ = f.svc.SignIn()
	if _, err := f.svc.Verify(ctx, c.Token, `{"id":"x"}`); !errors.Is(err, passkey.ErrRejected) {
		t.Errorf("garbage answer: err = %v, want ErrRejected", err)
	}

	// A passkey that was removed.
	if err := f.users.RemovePasskey(ctx, "ana", p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.signIn(&a, mcp); !errors.Is(err, passkey.ErrRejected) {
		t.Errorf("removed passkey: err = %v, want ErrRejected", err)
	}
	if !slices.ContainsFunc(f.audit.Events, func(e audit.Event) bool { return e.Action == audit.PasskeyDelete }) {
		t.Errorf("no %s event", audit.PasskeyDelete)
	}

	// A disabled person.
	var b passkeytest.Authenticator
	f.add(ana, &b, "")
	if _, err := f.users.UpdateUser(ctx, "ana", user.UpdateUserRequest{Disabled: new(true)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.signIn(&b, mcp); !errors.Is(err, passkey.ErrRejected) {
		t.Errorf("disabled person: err = %v, want ErrRejected", err)
	}
}

// A security key counts its signatures; a count that goes back means a
// copy of it exists.
func TestSignInRefusesAClonedKey(t *testing.T) {
	f := newFixture(t)
	ana := f.person("ana")
	var a passkeytest.Authenticator
	f.add(ana, &a, "")
	a.Passkeys[0].Count = 10
	if _, err := f.signIn(&a, mcp); err != nil {
		t.Fatal(err)
	}
	a.Passkeys[0].Count = 3
	if _, err := f.signIn(&a, mcp); !errors.Is(err, passkey.ErrRejected) {
		t.Errorf("counter went back: err = %v, want ErrRejected", err)
	}
}

func TestRegistrationExcludesHeldPasskeys(t *testing.T) {
	f := newFixture(t)
	ana := f.person("ana")
	var a passkeytest.Authenticator
	f.add(ana, &a, "")
	c, err := f.svc.Registration(context.Background(), ana)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Create(c.Options, apex); !errors.Is(err, passkeytest.ErrExcluded) {
		t.Errorf("a second passkey on the same authenticator: err = %v, want ErrExcluded", err)
	}
}

func TestTooManyPasskeys(t *testing.T) {
	f := newFixture(t)
	ana := f.person("ana")
	for range user.MaxPasskeys {
		var a passkeytest.Authenticator
		f.add(ana, &a, "")
	}
	ctx := context.Background()
	c, _ := f.svc.Registration(ctx, ana)
	var a passkeytest.Authenticator
	resp, _ := a.Create(c.Options, apex)
	if _, err := f.svc.Register(ctx, ana, c.Token, resp, ""); !errors.Is(err, user.ErrTooManyPasskeys) {
		t.Errorf("passkey %d: err = %v, want ErrTooManyPasskeys", user.MaxPasskeys+1, err)
	}
}

func TestSupported(t *testing.T) {
	for domain, want := range map[string]bool{
		"art.example.com": true, "example.com": true, "artifacts.kazari.io": true, "kazari.test": true,
		"kazari.localhost": true, "localhost": false, "LOCALHOST": false, "github.io": false, "co.uk": false, "com": false,
	} {
		if got := passkey.Supported(domain); got != want {
			t.Errorf("Supported(%q) = %v, want %v", domain, got, want)
		}
	}
}

func TestConfirm(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ana, ben := f.person("ana"), f.person("ben")
	if _, ok, err := f.svc.Confirmation(ctx, ana); ok || err != nil {
		t.Fatalf("confirmation without a passkey: ok %v, err %v", ok, err)
	}
	var a, b passkeytest.Authenticator
	f.add(ana, &a, "")
	f.add(ben, &b, "")

	c, ok, err := f.svc.Confirmation(ctx, ana)
	if !ok || err != nil {
		t.Fatalf("Confirmation = %v, %v", ok, err)
	}
	if !strings.Contains(c.Options, `"allowCredentials":[{"type":"public-key","id":"`) || !strings.Contains(c.Options, `"userVerification":"required"`) {
		t.Errorf("confirmation options name no passkey of ana's: %s", c.Options)
	}
	resp, err := a.Get(c.Options, apex)
	if err != nil {
		t.Fatal(err)
	}
	f.clock.t = t0.Add(time.Minute)
	if err := f.svc.Confirm(ctx, ana, c.Token, resp); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if keys, _ := f.users.Passkeys(ctx, "ana"); keys[0].LastUsedAt == nil || !keys[0].LastUsedAt.Equal(t0.Add(time.Minute)) {
		t.Errorf("last use not kept: %+v", keys[0])
	}
	if err := f.svc.Confirm(ctx, ana, c.Token, resp); !errors.Is(err, passkey.ErrExpired) {
		t.Errorf("confirmation used twice: err = %v, want ErrExpired", err)
	}

	// Ben's passkey cannot confirm for ana, nor ana's ceremony for ben.
	c, _, _ = f.svc.Confirmation(ctx, ana)
	resp, _ = b.Sign(b.Passkeys[0], passkeytest.Challenge(c.Options), apex)
	if err := f.svc.Confirm(ctx, ana, c.Token, resp); !errors.Is(err, passkey.ErrRejected) {
		t.Errorf("another person's passkey: err = %v, want ErrRejected", err)
	}
	c, _, _ = f.svc.Confirmation(ctx, ana)
	resp, _ = b.Sign(b.Passkeys[0], passkeytest.Challenge(c.Options), apex)
	if err := f.svc.Confirm(ctx, ben, c.Token, resp); !errors.Is(err, passkey.ErrExpired) {
		t.Errorf("ana's ceremony for ben: err = %v, want ErrExpired", err)
	}

	// A sign-in's token confirms nothing, and a confirmation's signs no one in.
	s, _ := f.svc.SignIn()
	resp, _ = a.Get(s.Options, apex)
	if err := f.svc.Confirm(ctx, ana, s.Token, resp); !errors.Is(err, passkey.ErrExpired) {
		t.Errorf("sign-in token used to confirm: err = %v, want ErrExpired", err)
	}
	c, _, _ = f.svc.Confirmation(ctx, ana)
	resp, _ = a.Get(c.Options, mcp)
	if _, err := f.svc.Verify(ctx, c.Token, resp); !errors.Is(err, passkey.ErrExpired) {
		t.Errorf("confirmation token used to sign in: err = %v, want ErrExpired", err)
	}

	// Without user verification, or from another origin.
	c, _, _ = f.svc.Confirmation(ctx, ana)
	resp, _ = a.Get(c.Options, artHost)
	if err := f.svc.Confirm(ctx, ana, c.Token, resp); !errors.Is(err, passkey.ErrRejected) {
		t.Errorf("answer from an artifact host: err = %v, want ErrRejected", err)
	}
	a.NoUV = true
	c, _, _ = f.svc.Confirmation(ctx, ana)
	resp, _ = a.Get(c.Options, apex)
	if err := f.svc.Confirm(ctx, ana, c.Token, resp); !errors.Is(err, passkey.ErrRejected) {
		t.Errorf("no user verification: err = %v, want ErrRejected", err)
	}
}
