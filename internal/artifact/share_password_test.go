package artifact_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/audit"
)

// plainPasswords stands in for user.Service: the rules here are the
// artifact service's, not the hash's.
type plainPasswords struct{}

func (plainPasswords) HashPassword(_ context.Context, password string) (string, error) {
	return "hash:" + password, nil
}

func (plainPasswords) VerifyPassword(_ context.Context, hash, password string) (bool, error) {
	return hash == "hash:"+password, nil
}

func TestShareLinkPassword(t *testing.T) {
	e := newEnv(t, artifact.Options{Passwords: plainPasswords{}})
	alice := e.user(t, "alice")
	e.create(t, alice, "demo")

	l, token, err := e.svc.CreateShareLink(e.ctx, alice, "demo", "", 0, "open sesame please")
	if err != nil {
		t.Fatal(err)
	}
	if !l.HasPassword {
		t.Errorf("CreateShareLink with a password = %+v, want HasPassword", l)
	}
	if ev := e.rec.Events[len(e.rec.Events)-1]; ev.Action != audit.ShareLinkCreate || ev.Details["password"] != "true" {
		t.Errorf("audit = %+v, want share_link.create with password true", ev)
	}
	for _, ev := range e.rec.Events {
		for _, v := range ev.Details {
			if strings.Contains(v, "sesame") {
				t.Fatalf("the audit trail holds the password: %+v", ev)
			}
		}
	}

	// The URL's token alone opens nothing, and gives no cookie.
	got, cookie, err := e.svc.OpenShareLink(e.ctx, "demo", token)
	if err != nil || cookie != "" || !got.HasPassword || got.Uses != 0 {
		t.Fatalf("OpenShareLink of a link with a password = %+v, %q, %v; want the link, no cookie, no use", got, cookie, err)
	}
	for _, tc := range []struct{ name, cookie string }{
		{"the token alone", token},
		{"a made-up proof", token + ".aaaaaaaa"},
		{"an empty proof", token + "."},
	} {
		_, err := e.svc.CheckShareLink(e.ctx, "demo", tc.cookie)
		wantErr(t, "CheckShareLink with "+tc.name, err, artifact.ErrLinkNotFound)
	}

	_, _, err = e.svc.UnlockShareLink(e.ctx, "demo", token, "open says me")
	wantErr(t, "UnlockShareLink with a wrong password", err, artifact.ErrForbidden)
	got, cookie, err = e.svc.UnlockShareLink(e.ctx, "demo", token, "open sesame please")
	if err != nil {
		t.Fatal(err)
	}
	if got.Uses != 1 || !strings.HasPrefix(cookie, token+".") {
		t.Fatalf("UnlockShareLink = %+v, %q; want one use and a cookie of the token and a proof", got, cookie)
	}
	if _, err := e.svc.CheckShareLink(e.ctx, "demo", cookie); err != nil {
		t.Errorf("CheckShareLink with the unlocked cookie: %v", err)
	}
	if links, err := e.svc.ListShareLinks(e.ctx, alice, "demo"); err != nil || len(links) != 1 || !links[0].HasPassword {
		t.Errorf("ListShareLinks = %+v, %v; want the link with HasPassword", links, err)
	}

	// The cookie ends with the link.
	if err := e.svc.RevokeShareLink(e.ctx, alice, "demo", l.Prefix); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.CheckShareLink(e.ctx, "demo", cookie)
	wantErr(t, "CheckShareLink with the cookie of a revoked link", err, artifact.ErrLinkNotFound)
	_, _, err = e.svc.UnlockShareLink(e.ctx, "demo", token, "open sesame please")
	wantErr(t, "UnlockShareLink of a revoked link", err, artifact.ErrLinkNotFound)

	_, short, err := e.svc.CreateShareLink(e.ctx, alice, "demo", "", time.Hour, "open sesame please")
	if err != nil {
		t.Fatal(err)
	}
	_, shortCookie, err := e.svc.UnlockShareLink(e.ctx, "demo", short, "open sesame please")
	if err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(time.Hour)
	_, err = e.svc.CheckShareLink(e.ctx, "demo", shortCookie)
	wantErr(t, "CheckShareLink with the cookie of an expired link", err, artifact.ErrLinkNotFound)
}

func TestShareLinkPasswordRules(t *testing.T) {
	e := newEnv(t, artifact.Options{Passwords: plainPasswords{}})
	alice := e.user(t, "alice")
	e.create(t, alice, "demo")
	for _, tc := range []struct {
		name     string
		password string
		ok       bool
	}{
		{"the shortest", "123456789012", true},
		{"too short", "12345678901", false},
		{"the longest", strings.Repeat("é", artifact.ShareLinkPasswordMax), true},
		{"too long", strings.Repeat("é", artifact.ShareLinkPasswordMax+1), false},
		{"not UTF-8", "\xff\xfe\xfd\xfc\xfb\xfa\xf9\xf8", false},
	} {
		_, _, err := e.svc.CreateShareLink(e.ctx, alice, "demo", "", 0, tc.password)
		if (err == nil) != tc.ok || (err != nil && (!errors.Is(err, artifact.ErrInvalidArgument) || !errors.Is(err, artifact.ErrLinkPassword))) {
			t.Errorf("%s: CreateShareLink = %v, want ok %t or invalid_argument marked as the password's", tc.name, err, tc.ok)
		}
		if artifact.CodeOf(err) != "" && artifact.CodeOf(err) != "invalid_argument" {
			t.Errorf("%s: CreateShareLink code = %q, want invalid_argument", tc.name, artifact.CodeOf(err))
		}
	}

	// A link with no password opens with its token, and an unlock of it
	// opens nothing.
	_, token, err := e.svc.CreateShareLink(e.ctx, alice, "demo", "", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = e.svc.UnlockShareLink(e.ctx, "demo", token, "anything at all")
	wantErr(t, "UnlockShareLink of a link without a password", err, artifact.ErrLinkNotFound)

	none := newEnv(t, artifact.Options{})
	bob := none.user(t, "bob")
	none.create(t, bob, "demo")
	_, _, err = none.svc.CreateShareLink(none.ctx, bob, "demo", "", 0, "open sesame please")
	wantErr(t, "CreateShareLink with a password and nothing to hash it", err, artifact.ErrLinkPassword)
	_, _, err = none.svc.CreateShareLink(none.ctx, bob, "demo", "bad\x00label", 0, "")
	if errors.Is(err, artifact.ErrLinkPassword) {
		t.Errorf("a bad label's refusal = %v, which says it is the password's", err)
	}
}
