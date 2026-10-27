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

func TestOAuthStoreSignIn(t *testing.T) {
	ctx := context.Background()
	s := oauthTestStores(t)
	want := &oauth.SignInRequest{ID: "v.state", Binding: "b", Nonce: "n", UpstreamVerifier: "pv",
		Host: "demo--v2", Path: "/report?x=1", NonceHash: "nh", ExpiresAt: oauthT0.Add(10 * time.Minute)}
	if err := s.CreateSignIn(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSignIn(ctx, "v.state")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("GetSignIn (-want +got):\n%s", diff)
	}
	if err := s.DeleteSignIn(ctx, "v.state"); err != nil {
		t.Fatal(err)
	}
	// The second delete loses, so two racing callbacks cannot both go on.
	if err := s.DeleteSignIn(ctx, "v.state"); !errors.Is(err, oauth.ErrNotFound) {
		t.Fatalf("second DeleteSignIn = %v, want ErrNotFound", err)
	}
	if _, err := s.GetSignIn(ctx, "v.state"); !errors.Is(err, oauth.ErrNotFound) {
		t.Fatalf("GetSignIn after delete = %v", err)
	}
}

func TestOAuthStoreSessions(t *testing.T) {
	ctx := context.Background()
	s := oauthTestStores(t)
	sess := &oauth.Session{Hash: "sh", UserHandle: "alice", KeyPrefix: "abcdefgh", CreatedAt: oauthT0, ExpiresAt: oauthT0.Add(12 * time.Hour)}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if sess.ID == 0 {
		t.Fatal("CreateSession set no ID")
	}
	if err := s.CreateSession(ctx, &oauth.Session{Hash: "x", UserHandle: "nobody", CreatedAt: oauthT0, ExpiresAt: oauthT0}); !errors.Is(err, user.ErrNotFound) {
		t.Fatalf("CreateSession for an unknown user = %v", err)
	}
	for _, get := range []func() (*oauth.Session, error){
		func() (*oauth.Session, error) { return s.GetSession(ctx, "sh") },
		func() (*oauth.Session, error) { return s.GetSessionByID(ctx, sess.ID) },
	} {
		got, err := get()
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(sess, got); diff != "" {
			t.Errorf("session (-want +got):\n%s", diff)
		}
	}

	code := &oauth.ViewCode{Hash: "ch", SessionID: sess.ID, Host: "demo", Path: "/", NonceHash: "nh", ExpiresAt: oauthT0.Add(time.Minute)}
	if err := s.CreateViewCode(ctx, code); err != nil {
		t.Fatal(err)
	}
	used := oauthT0.Add(time.Second)
	got, err := s.ConsumeViewCode(ctx, "ch", used)
	if err != nil {
		t.Fatal(err)
	}
	code.UsedAt = &used
	if diff := cmp.Diff(code, got); diff != "" {
		t.Errorf("ConsumeViewCode (-want +got):\n%s", diff)
	}
	if _, err := s.ConsumeViewCode(ctx, "ch", used); !errors.Is(err, oauth.ErrCodeUsed) {
		t.Fatalf("second ConsumeViewCode = %v, want ErrCodeUsed", err)
	}
	if _, err := s.ConsumeViewCode(ctx, "unknown", used); !errors.Is(err, oauth.ErrNotFound) {
		t.Fatalf("ConsumeViewCode unknown = %v, want ErrNotFound", err)
	}

	view := &oauth.ViewSession{Hash: "vh", SessionID: sess.ID, Host: "demo", CreatedAt: oauthT0, ExpiresAt: sess.ExpiresAt}
	if err := s.CreateViewSession(ctx, view); err != nil {
		t.Fatal(err)
	}
	gotView, err := s.GetViewSession(ctx, "vh")
	if err != nil {
		t.Fatal(err)
	}
	view.UserHandle, view.KeyPrefix = "alice", "abcdefgh"
	if diff := cmp.Diff(view, gotView); diff != "" {
		t.Errorf("GetViewSession (-want +got):\n%s", diff)
	}

	// Deleting the session ends its codes and view sessions.
	if err := s.CreateViewCode(ctx, &oauth.ViewCode{Hash: "ch2", SessionID: sess.ID, Host: "demo", Path: "/", NonceHash: "nh", ExpiresAt: oauthT0}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetViewSession(ctx, "vh"); !errors.Is(err, oauth.ErrNotFound) {
		t.Fatalf("view session after DeleteSession = %v", err)
	}
	if _, err := s.ConsumeViewCode(ctx, "ch2", used); !errors.Is(err, oauth.ErrNotFound) {
		t.Fatalf("code after DeleteSession = %v", err)
	}
	if err := s.DeleteSession(ctx, sess.ID); err != nil {
		t.Fatalf("DeleteSession of a missing session = %v", err)
	}
}

// Disabling a user (RevokeAllForUser) ends their browser sessions too.
func TestOAuthStoreRevokeAllEndsSessions(t *testing.T) {
	ctx := context.Background()
	s := oauthTestStores(t)
	for _, h := range []string{"alice", "bob"} {
		sess := &oauth.Session{Hash: "s-" + h, UserHandle: h, CreatedAt: oauthT0, ExpiresAt: oauthT0.Add(time.Hour)}
		if err := s.CreateSession(ctx, sess); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateViewSession(ctx, &oauth.ViewSession{Hash: "v-" + h, SessionID: sess.ID, Host: "demo",
			CreatedAt: oauthT0, ExpiresAt: oauthT0.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RevokeAllForUser(ctx, "alice", oauthT0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSession(ctx, "s-alice"); !errors.Is(err, oauth.ErrNotFound) {
		t.Fatalf("alice's session = %v, want gone", err)
	}
	if _, err := s.GetViewSession(ctx, "v-alice"); !errors.Is(err, oauth.ErrNotFound) {
		t.Fatalf("alice's view session = %v, want gone", err)
	}
	if _, err := s.GetViewSession(ctx, "v-bob"); err != nil {
		t.Fatalf("bob's view session: %v", err)
	}
}

func TestOAuthStoreConsumeViewCodeConcurrent(t *testing.T) {
	ctx := context.Background()
	s := oauthTestStores(t)
	sess := &oauth.Session{Hash: "sh", UserHandle: "alice", CreatedAt: oauthT0, ExpiresAt: oauthT0.Add(time.Hour)}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateViewCode(ctx, &oauth.ViewCode{Hash: "ch", SessionID: sess.ID, Host: "demo", Path: "/", NonceHash: "nh",
		ExpiresAt: oauthT0.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
	)
	for range 8 {
		wg.Go(func() {
			if _, err := s.ConsumeViewCode(ctx, "ch", oauthT0); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d redemptions of one code succeeded, want 1", wins)
	}
}

func TestOAuthStoreDeleteExpiredSignIns(t *testing.T) {
	ctx := context.Background()
	s := oauthTestStores(t)
	if err := s.CreateSignIn(ctx, &oauth.SignInRequest{ID: "v.old", ExpiresAt: oauthT0}); err != nil {
		t.Fatal(err)
	}
	old := &oauth.Session{Hash: "old", UserHandle: "alice", CreatedAt: oauthT0, ExpiresAt: oauthT0}
	live := &oauth.Session{Hash: "live", UserHandle: "alice", CreatedAt: oauthT0, ExpiresAt: oauthT0.Add(time.Hour)}
	for _, sess := range []*oauth.Session{old, live} {
		if err := s.CreateSession(ctx, sess); err != nil {
			t.Fatal(err)
		}
	}
	// An expired code and view session of the live session go; so does the
	// view session of the expired one.
	if err := s.CreateViewCode(ctx, &oauth.ViewCode{Hash: "c", SessionID: live.ID, ExpiresAt: oauthT0}); err != nil {
		t.Fatal(err)
	}
	for i, v := range []*oauth.ViewSession{
		{Hash: "v1", SessionID: live.ID, ExpiresAt: oauthT0},
		{Hash: "v2", SessionID: old.ID, ExpiresAt: oauthT0},
		{Hash: "v3", SessionID: live.ID, ExpiresAt: oauthT0.Add(time.Hour)},
	} {
		v.Host = "demo"
		v.CreatedAt = oauthT0.Add(time.Duration(i))
		if err := s.CreateViewSession(ctx, v); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.DeleteExpired(ctx, oauthT0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 { // sign-in, code, v1, v2, session "old"
		t.Errorf("DeleteExpired = %d, want 5", n)
	}
	if _, err := s.GetViewSession(ctx, "v3"); err != nil {
		t.Errorf("live view session deleted: %v", err)
	}
	if _, err := s.GetSession(ctx, "live"); err != nil {
		t.Errorf("live session deleted: %v", err)
	}
}
