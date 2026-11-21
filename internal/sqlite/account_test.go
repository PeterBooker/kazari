package sqlite

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/oauth"
	"github.com/PeterBooker/kazari/internal/user"
)

func TestUserStoreLastAdmin(t *testing.T) {
	ctx := context.Background()
	s := userTestStore(t)
	boss := userMustCreate(t, s, "boss", "", user.RoleAdmin)
	userMustCreate(t, s, "ana", "", user.RoleMember)

	demoted := *boss
	demoted.Role = user.RoleMember
	if err := s.UpdateUser(ctx, &demoted); !errors.Is(err, user.ErrLastAdmin) {
		t.Fatalf("demoting the last admin: err = %v, want ErrLastAdmin", err)
	}
	disabled := *boss
	disabled.DisabledAt = &userT0
	if err := s.UpdateUser(ctx, &disabled); !errors.Is(err, user.ErrLastAdmin) {
		t.Fatalf("disabling the last admin: err = %v, want ErrLastAdmin", err)
	}
	if got, _ := s.GetUser(ctx, "boss"); got.Role != user.RoleAdmin || got.DisabledAt != nil {
		t.Fatalf("the last admin after refused updates = %+v", got)
	}
	renamed := *boss
	renamed.DisplayName = "The Boss"
	if err := s.UpdateUser(ctx, &renamed); err != nil {
		t.Errorf("an update that keeps the last admin an admin: %v", err)
	}
	if err := s.UpdateUser(ctx, &user.User{Handle: "nobody", Role: user.RoleMember}); !errors.Is(err, user.ErrNotFound) {
		t.Errorf("updating an unknown user: err = %v, want ErrNotFound", err)
	}
}

// Two admins demoting each other at once must not both succeed.
func TestUserStoreLastAdminConcurrent(t *testing.T) {
	ctx := context.Background()
	s := userTestStore(t)
	a := userMustCreate(t, s, "ana", "", user.RoleAdmin)
	b := userMustCreate(t, s, "ben", "", user.RoleAdmin)
	var (
		wg   sync.WaitGroup
		errs [2]error
	)
	for i, u := range []*user.User{a, b} {
		wg.Go(func() {
			demoted := *u
			demoted.Role = user.RoleMember
			errs[i] = s.UpdateUser(ctx, &demoted)
		})
	}
	wg.Wait()
	refused := 0
	for _, err := range errs {
		if errors.Is(err, user.ErrLastAdmin) {
			refused++
		} else if err != nil {
			t.Fatal(err)
		}
	}
	counts, err := s.CountUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	admins := int64(0)
	for _, c := range counts {
		if c.Role == user.RoleAdmin && !c.Disabled {
			admins += c.Count
		}
	}
	if refused != 1 || admins != 1 {
		t.Errorf("%d demotions refused and %d admins left, want one of each", refused, admins)
	}
}

func TestUserStoreKeyCap(t *testing.T) {
	ctx := context.Background()
	s := userTestStore(t)
	userMustCreate(t, s, "ana", "", user.RoleMember)
	key := func(prefix string, expires *time.Time) *user.APIKey {
		return &user.APIKey{UserHandle: "ana", Prefix: prefix, Hash: "hash-" + prefix, CreatedAt: userT0, ExpiresAt: expires}
	}
	past, future := userT0.Add(-time.Hour), userT0.Add(time.Hour)
	for _, k := range []*user.APIKey{key("aaaaaaaa", nil), key("bbbbbbbb", &future), key("expired1", &past), key("expired2", &userT0)} {
		if err := s.CreateKey(ctx, k, 2); err != nil && k.ExpiresAt != nil && k.ExpiresAt.After(userT0) {
			t.Fatalf("CreateKey(%s) under the cap: %v", k.Prefix, err)
		}
	}
	if err := s.CreateKey(ctx, key("cccccccc", nil), 2); !errors.Is(err, user.ErrTooManyKeys) {
		t.Fatalf("a third live key at a cap of 2: err = %v, want ErrTooManyKeys", err)
	}
	if _, _, err := s.GetKey(ctx, "cccccccc"); !errors.Is(err, user.ErrKeyNotFound) {
		t.Error("a refused key was stored")
	}
	if err := s.CreateKey(ctx, key("cccccccc", nil), 0); err != nil {
		t.Errorf("no cap: %v", err)
	}
	if _, err := s.RevokeKey(ctx, "ana", "aaaaaaaa", userT0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RevokeKey(ctx, "ana", "bbbbbbbb", userT0); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateKey(ctx, key("dddddddd", nil), 2); err != nil {
		t.Errorf("after revoking two: %v", err)
	}
}

func TestUserStorePassword(t *testing.T) {
	ctx := context.Background()
	s := userTestStore(t)
	userMustCreate(t, s, "ana", "", user.RoleMember)
	if h, err := s.PasswordHash(ctx, "ana"); h != "" || err != nil {
		t.Fatalf("PasswordHash before one is set = %q, %v", h, err)
	}
	if h, err := s.PasswordHash(ctx, "nobody"); h != "" || err != nil {
		t.Fatalf("PasswordHash of an unknown user = %q, %v", h, err)
	}
	if err := s.SetPassword(ctx, "nobody", "hash", userT0); !errors.Is(err, user.ErrNotFound) {
		t.Errorf("SetPassword for an unknown user: err = %v, want ErrNotFound", err)
	}
	for _, hash := range []string{"first", "second"} {
		if err := s.SetPassword(ctx, "ana", hash, userT0); err != nil {
			t.Fatal(err)
		}
		if h, _ := s.PasswordHash(ctx, "ana"); h != hash {
			t.Errorf("PasswordHash = %q, want %q", h, hash)
		}
	}
	if err := s.TouchPassword(ctx, "nobody", userT0); err != nil {
		t.Errorf("TouchPassword for an unknown user: %v", err)
	}
	later := userT0.Add(time.Hour)
	if err := s.TouchPassword(ctx, "ana", later); err != nil {
		t.Fatal(err)
	}
	accounts, err := s.ListAccounts(ctx, later)
	if err != nil {
		t.Fatal(err)
	}
	want := []user.Account{{Handle: "ana", HasPassword: true, PasswordLoginAt: ptrTo(later.UTC())}}
	if diff := cmp.Diff(want, accounts); diff != "" {
		t.Errorf("ListAccounts (-want +got):\n%s", diff)
	}
}

func ptrTo[T any](v T) *T { return &v }

func TestUserStoreLinks(t *testing.T) {
	ctx := context.Background()
	s := userTestStore(t)
	userMustCreate(t, s, "ana", "", user.RoleMember)
	link := func(handle string, purpose user.Purpose, hash string, at time.Time) *user.Link {
		return &user.Link{UserHandle: handle, Purpose: purpose, Hash: hash, CreatedBy: "boss", CreatedAt: at, ExpiresAt: at.Add(time.Hour)}
	}
	if err := s.CreateLink(ctx, link("nobody", user.PurposeInvite, "h0", userT0)); !errors.Is(err, user.ErrNotFound) {
		t.Fatalf("a link for an unknown user: err = %v, want ErrNotFound", err)
	}
	if err := s.CreateLink(ctx, link("", user.PurposeSetup, "h-setup", userT0)); !errors.Is(err, user.ErrSetupDone) {
		t.Fatalf("a setup link with a user: err = %v, want ErrSetupDone", err)
	}
	first := link("ana", user.PurposeInvite, "h1", userT0)
	if err := s.CreateLink(ctx, first); err != nil || first.ID == 0 {
		t.Fatalf("CreateLink = %v, id %d", err, first.ID)
	}
	reset := link("ana", user.PurposeReset, "h2", userT0)
	reset.RevokeAll = true
	if err := s.CreateLink(ctx, reset); err != nil {
		t.Fatal(err)
	}
	second := link("ana", user.PurposeInvite, "h3", userT0.Add(time.Minute))
	if err := s.CreateLink(ctx, second); err != nil {
		t.Fatal(err)
	}

	got, u, err := s.GetLink(ctx, "h1")
	if err != nil || u.Handle != "ana" || got.RevokedAt == nil || !got.RevokedAt.Equal(second.CreatedAt) {
		t.Fatalf("the replaced invite = %+v, %+v, %v; want it revoked when the next was made", got, u, err)
	}
	if got, _, _ := s.GetLink(ctx, "h2"); got.RevokedAt != nil || !got.RevokeAll {
		t.Errorf("the reset link = %+v, want it live: another purpose replaces nothing", got)
	}
	if _, _, err := s.GetLink(ctx, "nope"); !errors.Is(err, user.ErrLinkNotFound) {
		t.Errorf("GetLink of an unknown hash: err = %v", err)
	}
	live, err := s.ListLinks(ctx, userT0.Add(2*time.Minute))
	if err != nil || len(live) != 2 || live[0].ID != second.ID || live[1].ID != reset.ID {
		t.Fatalf("ListLinks = %+v, %v; want the second invite, then the reset", live, err)
	}
	if live, _ := s.ListLinks(ctx, userT0.Add(time.Hour)); len(live) != 1 || live[0].ID != second.ID {
		t.Errorf("ListLinks at the reset's expiry = %+v, want the invite alone", live)
	}

	// Using a link sets the password and nothing uses it twice.
	if _, _, err := s.UseLink(ctx, "h1", userT0.Add(2*time.Minute), "pw", nil); !errors.Is(err, user.ErrLinkNotFound) {
		t.Errorf("using a replaced link: err = %v", err)
	}
	if _, _, err := s.UseLink(ctx, "h3", second.ExpiresAt, "pw", nil); !errors.Is(err, user.ErrLinkNotFound) {
		t.Errorf("using a link at its expiry: err = %v", err)
	}
	used, who, err := s.UseLink(ctx, "h3", userT0.Add(2*time.Minute), "pw", nil)
	if err != nil || who.Handle != "ana" || used.UsedAt == nil {
		t.Fatalf("UseLink = %+v, %+v, %v", used, who, err)
	}
	if h, _ := s.PasswordHash(ctx, "ana"); h != "pw" {
		t.Errorf("PasswordHash after UseLink = %q", h)
	}
	if _, _, err := s.UseLink(ctx, "h3", userT0.Add(3*time.Minute), "pw2", nil); !errors.Is(err, user.ErrLinkNotFound) {
		t.Errorf("using a link twice: err = %v", err)
	}
	if _, err := s.RevokeLink(ctx, second.ID, userT0.Add(3*time.Minute)); !errors.Is(err, user.ErrLinkNotFound) {
		t.Errorf("revoking a used link: err = %v", err)
	}
	revoked, err := s.RevokeLink(ctx, reset.ID, userT0.Add(3*time.Minute))
	if err != nil || revoked.UserHandle != "ana" || revoked.RevokedAt == nil {
		t.Fatalf("RevokeLink = %+v, %v", revoked, err)
	}

	// A disabled person's link sets nothing, and stays unused.
	third := link("ana", user.PurposeReset, "h4", userT0.Add(4*time.Minute))
	if err := s.CreateLink(ctx, third); err != nil {
		t.Fatal(err)
	}
	ana, _ := s.GetUser(ctx, "ana")
	ana.DisabledAt = &userT0
	if err := s.UpdateUser(ctx, ana); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.UseLink(ctx, "h4", userT0.Add(5*time.Minute), "pw3", nil); !errors.Is(err, user.ErrLinkNotFound) {
		t.Errorf("using a disabled person's link: err = %v", err)
	}
	if got, _, _ := s.GetLink(ctx, "h4"); got.UsedAt != nil {
		t.Error("a refused use marked the link used")
	}
	if h, _ := s.PasswordHash(ctx, "ana"); h != "pw" {
		t.Errorf("PasswordHash after a refused use = %q", h)
	}
}

func TestUserStoreSetup(t *testing.T) {
	ctx := context.Background()
	s := userTestStore(t)
	setup := &user.Link{Purpose: user.PurposeSetup, Hash: "s1", CreatedAt: userT0, ExpiresAt: userT0.Add(time.Hour)}
	if err := s.CreateLink(ctx, setup); err != nil {
		t.Fatal(err)
	}
	got, u, err := s.GetLink(ctx, "s1")
	if err != nil || u != nil || got.UserHandle != "" {
		t.Fatalf("GetLink of a setup link = %+v, %+v, %v", got, u, err)
	}
	if _, _, err := s.UseLink(ctx, "s1", userT0, "pw", nil); err == nil {
		t.Fatal("a setup link was used without a first user")
	}
	if got, _, _ := s.GetLink(ctx, "s1"); got.UsedAt != nil {
		t.Fatal("a failed use marked the setup link used")
	}
	first := &user.User{Handle: "peter", Email: "peter@example.com", Role: user.RoleAdmin, CreatedAt: userT0}
	_, who, err := s.UseLink(ctx, "s1", userT0, "pw", first)
	if err != nil || who.Handle != "peter" || who.ID == 0 {
		t.Fatalf("UseLink = %+v, %v", who, err)
	}
	stored, err := s.GetUser(ctx, "peter")
	if err != nil || stored.Role != user.RoleAdmin || stored.Email != "peter@example.com" {
		t.Fatalf("the first user = %+v, %v", stored, err)
	}
	if h, _ := s.PasswordHash(ctx, "peter"); h != "pw" {
		t.Errorf("PasswordHash of the first user = %q", h)
	}

	// A later setup link, however it came to exist, creates nobody.
	if _, err := s.db.ExecContext(ctx, `INSERT INTO invite (purpose, token_hash, created_at, expires_at) VALUES ('setup', 's2', ?, ?)`,
		toNanos(userT0), toNanos(userT0.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	second := &user.User{Handle: "eve", Role: user.RoleAdmin, CreatedAt: userT0}
	if _, _, err := s.UseLink(ctx, "s2", userT0, "pw", second); !errors.Is(err, user.ErrSetupDone) {
		t.Fatalf("a setup link once a user exists: err = %v, want ErrSetupDone", err)
	}
	if _, err := s.GetUser(ctx, "eve"); !errors.Is(err, user.ErrNotFound) {
		t.Errorf("eve exists: %v", err)
	}
}

func TestOAuthStoreUserSessions(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, t.TempDir()+"/kazari.db", Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	users, s := NewUserStore(db), NewOAuthStore(db)
	userMustCreate(t, users, "ana", "", user.RoleMember)
	userMustCreate(t, users, "ben", "", user.RoleMember)
	session := func(handle, hash, browser string, expires time.Time) int64 {
		t.Helper()
		sess := &oauth.Session{Hash: hash, Browser: browser, UserHandle: handle, CreatedAt: userT0, ExpiresAt: expires}
		if err := s.CreateSession(ctx, sess); err != nil {
			t.Fatal(err)
		}
		return sess.ID
	}
	later := userT0.Add(time.Hour)
	keep := session("ana", "a1", "laptop", later)
	session("ana", "a2", "laptop", later)
	session("ana", "a3", "phone", later)
	session("ana", "a4", "", later)
	session("ana", "a5", "old", userT0)
	bens := session("ben", "b1", "laptop", later)

	if n, err := s.CountSessions(ctx, "ana", userT0); err != nil || n != 4 {
		t.Fatalf("CountSessions = %d, %v; want the 4 that have not expired", n, err)
	}
	n, err := s.DeleteUserSessions(ctx, "ana", keep)
	if err != nil || n != 3 {
		t.Fatalf("DeleteUserSessions = %d, %v; want 3: the phone, the unbound one and the expired one", n, err)
	}
	for hash, want := range map[string]bool{"a1": true, "a2": true, "a3": false, "a4": false, "a5": false, "b1": true} {
		_, err := s.GetSession(ctx, hash)
		if (err == nil) != want {
			t.Errorf("session %s exists = %v, want %v", hash, err == nil, want)
		}
	}
	if n, err := s.DeleteUserSessions(ctx, "ana", 0); err != nil || n != 2 {
		t.Errorf("DeleteUserSessions keeping none = %d, %v; want 2", n, err)
	}
	if _, err := s.GetSessionByID(ctx, bens); err != nil {
		t.Errorf("another person's session went too: %v", err)
	}
	if n, err := s.DeleteUserSessions(ctx, "nobody", 0); err != nil || n != 0 {
		t.Errorf("DeleteUserSessions of an unknown user = %d, %v", n, err)
	}
}
