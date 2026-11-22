package user_test

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/PeterBooker/kazari/internal/audit"
	"github.com/PeterBooker/kazari/internal/user"
)

var codeShape = regexp.MustCompile(`^[a-z2-7]{4}-[a-z2-7]{4}-[a-z2-7]{4}-[a-z2-7]{4}$`)

func (f *fixture) codes(t *testing.T, handle string) []string {
	t.Helper()
	codes, err := f.svc.NewRecoveryCodes(context.Background(), handle)
	if err != nil {
		t.Fatalf("NewRecoveryCodes(%q): %v", handle, err)
	}
	return codes
}

func TestNewRecoveryCodes(t *testing.T) {
	ctx := context.Background()
	f := accounts(t, user.Options{})
	f.invite(t, "ana", "")
	if _, err := f.svc.NewRecoveryCodes(ctx, "nobody"); !errors.Is(err, user.ErrNotFound) {
		t.Fatalf("unknown person: err = %v, want ErrNotFound", err)
	}

	codes := f.codes(t, "ana")
	if len(codes) != user.RecoveryCodeCount {
		t.Fatalf("got %d codes, want %d", len(codes), user.RecoveryCodeCount)
	}
	seen := map[string]bool{}
	for _, c := range codes {
		if !codeShape.MatchString(c) || seen[c] {
			t.Errorf("code %q is malformed or repeated", c)
		}
		seen[c] = true
	}
	if n := f.account(t, "ana").RecoveryCodes; n != user.RecoveryCodeCount {
		t.Errorf("account counts %d codes", n)
	}
	if !slices.Contains(f.actions(), audit.RecoveryCreate) {
		t.Errorf("actions = %v, want %s", f.actions(), audit.RecoveryCreate)
	}

	// A new set ends the old one.
	fresh := f.codes(t, "ana")
	if _, err := f.svc.Recover(ctx, "ana", codes[0], "a brand new password"); !errors.Is(err, user.ErrBadRecoveryCode) {
		t.Errorf("code of the replaced set: err = %v, want ErrBadRecoveryCode", err)
	}
	if _, err := f.svc.Recover(ctx, "ana", fresh[0], "a brand new password"); err != nil {
		t.Errorf("code of the new set: %v", err)
	}
}

func TestRecover(t *testing.T) {
	ctx := context.Background()
	var ended []int64
	f := accounts(t, user.Options{EndSessions: func(_ context.Context, _ string, keep int64) error {
		ended = append(ended, keep)
		return nil
	}})
	f.invite(t, "ana", "ana@example.com")
	_, secret := f.mustIssue(t, "ana", nil, 0)
	codes := f.codes(t, "ana")
	_, reset, err := f.svc.IssueLink(ctx, "ana", user.PurposeReset, false)
	if err != nil {
		t.Fatal(err)
	}

	const next = "recovered long password"
	if _, err := f.svc.Recover(ctx, "ana", codes[0], "short"); !errors.Is(err, user.ErrWeakPassword) {
		t.Fatalf("weak password: err = %v", err)
	}
	// Typed by hand: upper case, spaces in place of hyphens, by email.
	typed := strings.ToUpper(strings.ReplaceAll(codes[0], "-", " "))
	u, err := f.svc.Recover(ctx, "Ana@Example.com", typed, next)
	if err != nil || u.Handle != "ana" {
		t.Fatalf("Recover = %+v, %v", u, err)
	}
	if _, err := f.svc.CheckPassword(ctx, "ana", next); err != nil {
		t.Errorf("new password: %v", err)
	}
	if _, err := f.svc.CheckPassword(ctx, "ana", goodPassword); !errors.Is(err, user.ErrBadPassword) {
		t.Errorf("old password still works: err = %v", err)
	}
	if len(ended) != 1 || ended[0] != 0 {
		t.Errorf("sessions ended %v, want every one once", ended)
	}
	if !f.authenticates(secret) {
		t.Error("a recovery revoked the person's key")
	}
	if _, _, err := f.svc.Link(ctx, reset); !errors.Is(err, user.ErrLinkNotFound) {
		t.Errorf("reset link made before the recovery: err = %v, want ErrLinkNotFound", err)
	}
	if n := f.account(t, "ana").RecoveryCodes; n != user.RecoveryCodeCount-1 {
		t.Errorf("codes left = %d", n)
	}
	if got := f.actions(); !slices.Contains(got, audit.RecoveryUse) {
		t.Errorf("actions = %v, want %s", got, audit.RecoveryUse)
	}

	// A code works once.
	if _, err := f.svc.Recover(ctx, "ana", codes[0], goodPassword); !errors.Is(err, user.ErrBadRecoveryCode) {
		t.Errorf("used code: err = %v, want ErrBadRecoveryCode", err)
	}
}

func TestRecoverRefusals(t *testing.T) {
	ctx := context.Background()
	f := accounts(t, user.Options{})
	f.invite(t, "ana", "")
	f.invite(t, "ben", "")
	anas := f.codes(t, "ana")
	f.codes(t, "ben")

	for _, c := range []struct{ name, login, code string }{
		{"unknown person", "nobody", anas[0]},
		{"another person's code", "ben", anas[0]},
		{"malformed code", "ana", "not-a-code"},
		{"empty code", "ana", ""},
		{"wrong code", "ana", "aaaa-aaaa-aaaa-aaaa"},
	} {
		if _, err := f.svc.Recover(ctx, c.login, c.code, "a brand new password"); !errors.Is(err, user.ErrBadRecoveryCode) {
			t.Errorf("%s: err = %v, want ErrBadRecoveryCode", c.name, err)
		}
	}

	if _, err := f.svc.UpdateUser(ctx, "ana", user.UpdateUserRequest{Disabled: new(true)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Recover(ctx, "ana", anas[1], "a brand new password"); !errors.Is(err, user.ErrBadRecoveryCode) {
		t.Errorf("disabled person: err = %v, want ErrBadRecoveryCode", err)
	}
	if _, err := f.svc.CheckPassword(ctx, "ben", goodPassword); err != nil {
		t.Errorf("ben's password changed: %v", err)
	}
}

func TestResetRevokeAllEndsRecoveryCodes(t *testing.T) {
	ctx := context.Background()
	f := accounts(t, user.Options{})
	f.invite(t, "ana", "")
	codes := f.codes(t, "ana")

	_, token, err := f.svc.IssueLink(ctx, "ana", user.PurposeReset, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Redeem(ctx, token, user.RedeemRequest{Password: goodPassword}); err != nil {
		t.Fatal(err)
	}
	if n := f.account(t, "ana").RecoveryCodes; n != user.RecoveryCodeCount {
		t.Errorf("a plain reset left %d codes", n)
	}

	_, token, err = f.svc.IssueLink(ctx, "ana", user.PurposeReset, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Redeem(ctx, token, user.RedeemRequest{Password: goodPassword}); err != nil {
		t.Fatal(err)
	}
	if n := f.account(t, "ana").RecoveryCodes; n != 0 {
		t.Errorf("a reset with revoke-all left %d codes", n)
	}
	if _, err := f.svc.Recover(ctx, "ana", codes[0], "a brand new password"); !errors.Is(err, user.ErrBadRecoveryCode) {
		t.Errorf("code after revoke-all: err = %v", err)
	}
}

func TestRecoveryLocalOff(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, user.Options{})
	if _, err := f.svc.NewRecoveryCodes(ctx, "ana"); !errors.Is(err, user.ErrLocalOff) {
		t.Errorf("NewRecoveryCodes: err = %v, want ErrLocalOff", err)
	}
	if _, err := f.svc.Recover(ctx, "ana", "aaaa-aaaa-aaaa-aaaa", goodPassword); !errors.Is(err, user.ErrLocalOff) {
		t.Errorf("Recover: err = %v, want ErrLocalOff", err)
	}
}
