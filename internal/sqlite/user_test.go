package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/user"
)

var userT0 = time.Date(2026, 10, 24, 12, 0, 0, 123456789, time.UTC)

func userTestStore(t *testing.T) *UserStore {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "kazari.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return NewUserStore(db)
}

func userMustCreate(t *testing.T, s *UserStore, handle, email string, role user.Role) *user.User {
	t.Helper()
	u := &user.User{Handle: handle, Email: email, DisplayName: "Name " + handle, Role: role,
		QuotaBytes: 1 << 20, CreatedAt: userT0}
	if err := s.CreateUser(context.Background(), u); err != nil {
		t.Fatalf("CreateUser(%q): %v", handle, err)
	}
	return u
}

func userMustKey(t *testing.T, s *UserStore, handle, prefix string, created time.Time) *user.APIKey {
	t.Helper()
	k := &user.APIKey{UserHandle: handle, Prefix: prefix, Hash: "hash-" + prefix, Label: "label " + prefix,
		Scopes: []string{"artifacts:read", "artifacts:write"}, CreatedAt: created}
	if err := s.CreateKey(context.Background(), k); err != nil {
		t.Fatalf("CreateKey(%q): %v", prefix, err)
	}
	return k
}

func TestUserStoreCreateGet(t *testing.T) {
	ctx := context.Background()
	s := userTestStore(t)
	disabled := userT0.Add(time.Hour)
	want := &user.User{Handle: "alice", Email: "alice@example.com", DisplayName: "Alice",
		Role: user.RoleAdmin, QuotaBytes: 42, DisabledAt: &disabled, CreatedAt: userT0}
	if err := s.CreateUser(ctx, want); err != nil {
		t.Fatal(err)
	}
	if want.ID == 0 {
		t.Fatal("CreateUser did not set ID")
	}
	got, err := s.GetUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("GetUser (-want +got):\n%s", diff)
	}

	// Users without an email do not collide on UNIQUE email.
	userMustCreate(t, s, "bob", "", user.RoleMember)
	userMustCreate(t, s, "carol", "", user.RoleMember)
	bob, err := s.GetUser(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if bob.Email != "" || bob.DisabledAt != nil {
		t.Errorf("bob = %+v, want empty email and not disabled", bob)
	}
}

func TestUserStoreCreateConflicts(t *testing.T) {
	ctx := context.Background()
	s := userTestStore(t)
	userMustCreate(t, s, "alice", "alice@example.com", user.RoleMember)
	tests := []struct {
		name   string
		handle string
		email  string
		want   error
	}{
		{"handle", "alice", "other@example.com", user.ErrHandleTaken},
		{"email", "alice2", "alice@example.com", user.ErrEmailTaken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := s.CreateUser(ctx, &user.User{Handle: tt.handle, Email: tt.email, Role: user.RoleMember, CreatedAt: userT0})
			if !errors.Is(err, tt.want) {
				t.Fatalf("CreateUser = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestUserStoreNotFound(t *testing.T) {
	ctx := context.Background()
	s := userTestStore(t)
	userMustCreate(t, s, "alice", "alice@example.com", user.RoleMember)
	tests := []struct {
		name string
		call func() error
		want error
	}{
		{"GetUser", func() error { _, err := s.GetUser(ctx, "nobody"); return err }, user.ErrNotFound},
		{"GetUserByEmail", func() error { _, err := s.GetUserByEmail(ctx, "x@example.com"); return err }, user.ErrNotFound},
		{"UpdateUser", func() error {
			return s.UpdateUser(ctx, &user.User{Handle: "nobody", Role: user.RoleMember})
		}, user.ErrNotFound},
		{"CreateKey", func() error {
			return s.CreateKey(ctx, &user.APIKey{UserHandle: "nobody", Prefix: "aaaaaaaa", CreatedAt: userT0})
		}, user.ErrNotFound},
		{"GetKey", func() error { _, _, err := s.GetKey(ctx, "aaaaaaaa"); return err }, user.ErrKeyNotFound},
		{"ListKeys", func() error { _, err := s.ListKeys(ctx, "nobody"); return err }, user.ErrNotFound},
		{"RevokeKey", func() error { return s.RevokeKey(ctx, "alice", "aaaaaaaa", userT0) }, user.ErrKeyNotFound},
		{"RevokeAllKeys", func() error { return s.RevokeAllKeys(ctx, "nobody", userT0) }, user.ErrNotFound},
		{"TouchKey", func() error { return s.TouchKey(ctx, "aaaaaaaa", userT0) }, user.ErrKeyNotFound},
		{"GetIdentity", func() error { _, err := s.GetIdentity(ctx, "iss", "sub"); return err }, user.ErrNotFound},
		{"LinkIdentity", func() error {
			return s.LinkIdentity(ctx, "nobody", user.Identity{Issuer: "iss", Subject: "sub", LastLoginAt: userT0})
		}, user.ErrNotFound},
		{"UnlinkIdentity", func() error { return s.UnlinkIdentity(ctx, "alice", "iss", "sub") }, user.ErrNotFound},
		{"ListIdentities", func() error { _, err := s.ListIdentities(ctx, "nobody"); return err }, user.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestUserStoreGetUserByEmailCaseInsensitive(t *testing.T) {
	ctx := context.Background()
	s := userTestStore(t)
	userMustCreate(t, s, "alice", "Alice@Example.com", user.RoleMember)
	got, err := s.GetUserByEmail(ctx, "aLICE@example.COM")
	if err != nil {
		t.Fatal(err)
	}
	if got.Handle != "alice" {
		t.Fatalf("handle = %q, want alice", got.Handle)
	}
}

func TestUserStoreListUsers(t *testing.T) {
	ctx := context.Background()
	s := userTestStore(t)
	got, err := s.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("empty store listed %d users", len(got))
	}
	for _, h := range []string{"carol", "alice", "bob"} {
		userMustCreate(t, s, h, "", user.RoleMember)
	}
	got, err = s.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var handles []string
	for _, u := range got {
		handles = append(handles, u.Handle)
	}
	if diff := cmp.Diff([]string{"alice", "bob", "carol"}, handles); diff != "" {
		t.Errorf("ListUsers handles (-want +got):\n%s", diff)
	}
}

func TestUserStoreUpdateUser(t *testing.T) {
	ctx := context.Background()
	s := userTestStore(t)
	u := userMustCreate(t, s, "alice", "alice@example.com", user.RoleMember)
	userMustCreate(t, s, "bob", "bob@example.com", user.RoleMember)

	disabled := userT0.Add(time.Minute)
	u.Email = "new@example.com"
	u.DisplayName = "New Name"
	u.Role = user.RoleAdmin
	u.QuotaBytes = 7
	u.DisabledAt = &disabled
	if err := s.UpdateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(u, got); diff != "" {
		t.Errorf("after update (-want +got):\n%s", diff)
	}

	u.Email = ""
	u.DisabledAt = nil
	if err := s.UpdateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.GetUser(ctx, "alice"); got.Email != "" || got.DisabledAt != nil {
		t.Errorf("after clear = %+v", got)
	}

	u.Email = "bob@example.com"
	if err := s.UpdateUser(ctx, u); !errors.Is(err, user.ErrEmailTaken) {
		t.Errorf("UpdateUser to taken email = %v, want ErrEmailTaken", err)
	}
}

func TestUserStoreKeys(t *testing.T) {
	ctx := context.Background()
	s := userTestStore(t)
	alice := userMustCreate(t, s, "alice", "alice@example.com", user.RoleAdmin)
	userMustCreate(t, s, "bob", "", user.RoleMember)

	exp := userT0.Add(24 * time.Hour)
	k1 := &user.APIKey{UserHandle: "alice", Prefix: "aaaaaaaa", Hash: "h1", Label: "laptop",
		Scopes: []string{"artifacts:read", "admin"}, ExpiresAt: &exp, CreatedAt: userT0}
	if err := s.CreateKey(ctx, k1); err != nil {
		t.Fatal(err)
	}
	if k1.ID == 0 {
		t.Fatal("CreateKey did not set ID")
	}
	k2 := userMustKey(t, s, "alice", "bbbbbbbb", userT0.Add(time.Second))
	userMustKey(t, s, "bob", "cccccccc", userT0)
	noScopes := &user.APIKey{UserHandle: "bob", Prefix: "dddddddd", Hash: "h4", CreatedAt: userT0}
	if err := s.CreateKey(ctx, noScopes); err != nil {
		t.Fatal(err)
	}

	if err := s.CreateKey(ctx, &user.APIKey{UserHandle: "bob", Prefix: "aaaaaaaa", CreatedAt: userT0}); err == nil {
		t.Error("CreateKey with a duplicate prefix succeeded")
	}

	gotK, gotU, err := s.GetKey(ctx, "aaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(k1, gotK); diff != "" {
		t.Errorf("GetKey key (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(alice, gotU); diff != "" {
		t.Errorf("GetKey user (-want +got):\n%s", diff)
	}
	if gotK, _, _ = s.GetKey(ctx, "dddddddd"); len(gotK.Scopes) != 0 {
		t.Errorf("empty scopes round-tripped as %q", gotK.Scopes)
	}

	keys, err := s.ListKeys(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]user.APIKey{*k2, *k1}, keys); diff != "" {
		t.Errorf("ListKeys newest first (-want +got):\n%s", diff)
	}

	if err := s.RevokeKey(ctx, "bob", "aaaaaaaa", userT0); !errors.Is(err, user.ErrKeyNotFound) {
		t.Errorf("RevokeKey by another user = %v, want ErrKeyNotFound", err)
	}
	r1 := userT0.Add(time.Hour)
	if err := s.RevokeKey(ctx, "alice", "aaaaaaaa", r1); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeKey(ctx, "alice", "aaaaaaaa", r1.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if gotK, _, _ = s.GetKey(ctx, "aaaaaaaa"); gotK.RevokedAt == nil || !gotK.RevokedAt.Equal(r1) {
		t.Errorf("RevokedAt = %v, want %v", gotK.RevokedAt, r1)
	}

	r2 := userT0.Add(2 * time.Hour)
	if err := s.RevokeAllKeys(ctx, "alice", r2); err != nil {
		t.Fatal(err)
	}
	want := map[string]time.Time{"aaaaaaaa": r1, "bbbbbbbb": r2}
	for prefix, at := range want {
		k, _, _ := s.GetKey(ctx, prefix)
		if k.RevokedAt == nil || !k.RevokedAt.Equal(at) {
			t.Errorf("%s RevokedAt = %v, want %v", prefix, k.RevokedAt, at)
		}
	}
	if k, _, _ := s.GetKey(ctx, "cccccccc"); k.RevokedAt != nil {
		t.Errorf("bob's key revoked by RevokeAllKeys(alice)")
	}

	used := userT0.Add(3 * time.Hour)
	if err := s.TouchKey(ctx, "cccccccc", used); err != nil {
		t.Fatal(err)
	}
	if k, _, _ := s.GetKey(ctx, "cccccccc"); k.LastUsedAt == nil || !k.LastUsedAt.Equal(used) {
		t.Errorf("LastUsedAt = %v, want %v", k.LastUsedAt, used)
	}

	if keys, err := s.ListKeys(ctx, "carol"); !errors.Is(err, user.ErrNotFound) || keys != nil {
		t.Errorf("ListKeys(unknown) = %v, %v", keys, err)
	}
}

func TestUserStoreIdentities(t *testing.T) {
	ctx := context.Background()
	s := userTestStore(t)
	alice := userMustCreate(t, s, "alice", "alice@example.com", user.RoleMember)
	bob := userMustCreate(t, s, "bob", "", user.RoleMember)

	ids, err := s.ListIdentities(ctx, "alice")
	if err != nil || len(ids) != 0 {
		t.Fatalf("ListIdentities before link = %v, %v", ids, err)
	}

	first := user.Identity{Issuer: "https://idp", Subject: "s1", EmailAtLogin: "alice@example.com", LastLoginAt: userT0}
	if err := s.LinkIdentity(ctx, "alice", first); err != nil {
		t.Fatal(err)
	}
	other := user.Identity{Issuer: "https://another", Subject: "s9", EmailAtLogin: "a@x", LastLoginAt: userT0}
	if err := s.LinkIdentity(ctx, "alice", other); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetIdentity(ctx, "https://idp", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(alice, got); diff != "" {
		t.Errorf("GetIdentity (-want +got):\n%s", diff)
	}

	again := first
	again.EmailAtLogin = "Alice@Example.com"
	again.LastLoginAt = userT0.Add(time.Hour)
	if err := s.LinkIdentity(ctx, "alice", again); err != nil {
		t.Fatal(err)
	}
	ids, err = s.ListIdentities(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]user.Identity{other, again}, ids); diff != "" {
		t.Errorf("ListIdentities (-want +got):\n%s", diff)
	}

	if err := s.LinkIdentity(ctx, "bob", other); err != nil {
		t.Fatal(err)
	}
	if got, err = s.GetIdentity(ctx, other.Issuer, other.Subject); err != nil || got.Handle != bob.Handle {
		t.Errorf("moved identity resolves to %v, %v; want bob", got, err)
	}

	if err := s.UnlinkIdentity(ctx, "bob", first.Issuer, first.Subject); !errors.Is(err, user.ErrNotFound) {
		t.Errorf("UnlinkIdentity of another user's identity = %v, want ErrNotFound", err)
	}
	if err := s.UnlinkIdentity(ctx, "alice", first.Issuer, first.Subject); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetIdentity(ctx, first.Issuer, first.Subject); !errors.Is(err, user.ErrNotFound) {
		t.Errorf("GetIdentity after unlink = %v, want ErrNotFound", err)
	}
}
