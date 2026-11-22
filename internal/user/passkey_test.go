package user_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/PeterBooker/kazari/internal/user"
)

func (f *fixture) addPasskey(t *testing.T, handle string, credID byte) *user.Passkey {
	t.Helper()
	ctx := context.Background()
	id, err := f.svc.PasskeyUserID(ctx, handle)
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.svc.AddPasskey(ctx, handle, user.NewPasskey{UserID: id, CredentialID: []byte{credID}, Record: []byte("{}")})
	if err != nil {
		t.Fatalf("AddPasskey: %v", err)
	}
	return p
}

func TestPasskeyUserID(t *testing.T) {
	ctx := context.Background()
	f := accounts(t, user.Options{})
	f.invite(t, "ana", "")
	first, err := f.svc.PasskeyUserID(ctx, "ana")
	if err != nil || len(first) != 32 {
		t.Fatalf("PasskeyUserID = %x, %v", first, err)
	}
	// Not stored until a passkey is: another page gets another.
	again, _ := f.svc.PasskeyUserID(ctx, "ana")
	if bytes.Equal(first, again) {
		t.Error("an unused user handle was kept")
	}
	if _, err := f.svc.AddPasskey(ctx, "ana", user.NewPasskey{UserID: first, CredentialID: []byte{1}, Record: []byte("{}")}); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.svc.PasskeyUserID(ctx, "ana"); !bytes.Equal(got, first) {
		t.Errorf("user handle after the first passkey = %x, want %x", got, first)
	}
	// A page opened before the first passkey was added made its passkey for
	// a handle that lost.
	if _, err := f.svc.AddPasskey(ctx, "ana", user.NewPasskey{UserID: again, CredentialID: []byte{2}, Record: []byte("{}")}); !errors.Is(err, user.ErrPasskeyStale) {
		t.Errorf("passkey for a stale handle: err = %v, want ErrPasskeyStale", err)
	}
	if _, err := f.svc.AddPasskey(ctx, "ana", user.NewPasskey{UserID: first, CredentialID: []byte{1}, Record: []byte("{}")}); !errors.Is(err, user.ErrPasskeyExists) {
		t.Errorf("the same passkey twice: err = %v, want ErrPasskeyExists", err)
	}
	if _, err := f.svc.PasskeyUserID(ctx, "nobody"); !errors.Is(err, user.ErrNotFound) {
		t.Errorf("unknown person: err = %v", err)
	}
}

func TestPasskeysOfAnAccount(t *testing.T) {
	ctx := context.Background()
	f := accounts(t, user.Options{})
	f.invite(t, "ana", "")
	f.invite(t, "ben", "")
	p := f.addPasskey(t, "ana", 1)
	f.addPasskey(t, "ana", 2)
	if acc := f.account(t, "ana"); acc.Passkeys != 2 || acc.PasskeyLoginAt != nil {
		t.Errorf("account = %+v", acc)
	}
	if err := f.svc.PasskeyUsed(ctx, p.ID, []byte(`{"n":1}`)); err != nil {
		t.Fatal(err)
	}
	if acc := f.account(t, "ana"); acc.PasskeyLoginAt == nil || !acc.LastActive().Equal(t0) {
		t.Errorf("account after a sign-in = %+v", acc)
	}
	got, _, _, err := f.svc.PasskeyByCredential(ctx, []byte{1})
	if err != nil || string(got.Record) != `{"n":1}` {
		t.Errorf("PasskeyByCredential = %+v, %v", got, err)
	}

	if err := f.svc.RemovePasskey(ctx, "ben", p.ID); !errors.Is(err, user.ErrPasskeyNotFound) {
		t.Errorf("removing another person's passkey: err = %v", err)
	}
	if err := f.svc.RemovePasskey(ctx, "ana", p.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := f.svc.PasskeyByCredential(ctx, []byte{1}); !errors.Is(err, user.ErrPasskeyNotFound) {
		t.Errorf("removed passkey: err = %v", err)
	}
	if err := f.svc.PasskeyUsed(ctx, p.ID, nil); err != nil {
		t.Errorf("a sign-in with a passkey removed meanwhile: %v", err)
	}

	if _, err := f.svc.UpdateUser(ctx, "ana", user.UpdateUserRequest{Disabled: new(true)}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := f.svc.PasskeyByCredential(ctx, []byte{2}); !errors.Is(err, user.ErrPasskeyNotFound) {
		t.Errorf("passkey of a disabled person: err = %v", err)
	}
	id, _ := f.svc.PasskeyUserID(ctx, "ana")
	if _, err := f.svc.AddPasskey(ctx, "ana", user.NewPasskey{UserID: id, CredentialID: []byte{3}, Record: []byte("{}")}); !errors.Is(err, user.ErrDisabled) {
		t.Errorf("adding to a disabled person: err = %v", err)
	}
}

func TestResetRevokeAllEndsPasskeys(t *testing.T) {
	ctx := context.Background()
	f := accounts(t, user.Options{})
	f.invite(t, "ana", "")
	f.addPasskey(t, "ana", 1)

	_, token, err := f.svc.IssueLink(ctx, "ana", user.PurposeReset, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Redeem(ctx, token, user.RedeemRequest{Password: goodPassword}); err != nil {
		t.Fatal(err)
	}
	if n := f.account(t, "ana").Passkeys; n != 1 {
		t.Errorf("a plain reset left %d passkeys", n)
	}
	_, token, err = f.svc.IssueLink(ctx, "ana", user.PurposeReset, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Redeem(ctx, token, user.RedeemRequest{Password: goodPassword}); err != nil {
		t.Fatal(err)
	}
	if n := f.account(t, "ana").Passkeys; n != 0 {
		t.Errorf("a reset with revoke-all left %d passkeys", n)
	}
}

func TestPasskeysLocalOff(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, user.Options{})
	if _, err := f.svc.PasskeyUserID(ctx, "ana"); !errors.Is(err, user.ErrLocalOff) {
		t.Errorf("PasskeyUserID: err = %v", err)
	}
	if _, _, _, err := f.svc.PasskeyByCredential(ctx, []byte{1}); !errors.Is(err, user.ErrPasskeyNotFound) {
		t.Errorf("PasskeyByCredential: err = %v", err)
	}
}

func TestChangePasswordConfirmed(t *testing.T) {
	ctx := context.Background()
	var kept []int64
	f := accounts(t, user.Options{EndSessions: func(_ context.Context, _ string, keep int64) error {
		kept = append(kept, keep)
		return nil
	}})
	f.invite(t, "ana", "")
	_, reset, err := f.svc.IssueLink(ctx, "ana", user.PurposeReset, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.ChangePasswordConfirmed(ctx, "ana", "short", 7); !errors.Is(err, user.ErrWeakPassword) {
		t.Errorf("weak password: err = %v", err)
	}
	const next = "a new long password"
	if err := f.svc.ChangePasswordConfirmed(ctx, "ana", next, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CheckPassword(ctx, "ana", next); err != nil {
		t.Errorf("new password: %v", err)
	}
	if len(kept) != 1 || kept[0] != 7 {
		t.Errorf("sessions ended keeping %v, want [7]", kept)
	}
	if _, _, err := f.svc.Link(ctx, reset); !errors.Is(err, user.ErrLinkNotFound) {
		t.Errorf("reset link after the change: err = %v", err)
	}
	if err := f.svc.ChangePasswordConfirmed(ctx, "nobody", next, 0); !errors.Is(err, user.ErrNotFound) {
		t.Errorf("unknown person: err = %v", err)
	}
	if _, err := f.svc.UpdateUser(ctx, "ana", user.UpdateUserRequest{Disabled: new(true)}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.ChangePasswordConfirmed(ctx, "ana", next, 0); !errors.Is(err, user.ErrDisabled) {
		t.Errorf("disabled person: err = %v", err)
	}
}
