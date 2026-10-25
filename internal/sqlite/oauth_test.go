package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"kazari/internal/oauth"
	"kazari/internal/user"
)

var oauthT0 = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

func oauthTestStores(t *testing.T) *OAuthStore {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "kazari.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	users := NewUserStore(db)
	for _, h := range []string{"alice", "bob"} {
		if err := users.CreateUser(ctx, &user.User{Handle: h, Role: user.RoleMember, CreatedAt: oauthT0}); err != nil {
			t.Fatal(err)
		}
	}
	s := NewOAuthStore(db)
	if err := s.UpsertClient(ctx, &oauth.Client{ID: "dcr_test", Name: "Test", RedirectURIs: []string{"https://app.example/cb"}, CreatedAt: oauthT0}); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestOAuthStoreClient(t *testing.T) {
	ctx := context.Background()
	s := oauthTestStores(t)
	fetched := oauthT0.Add(time.Minute)
	want := &oauth.Client{ID: "https://client.example/meta.json", Name: "CIMD",
		RedirectURIs: []string{"http://localhost/cb", "http://127.0.0.1/cb"}, CreatedAt: oauthT0, FetchedAt: &fetched}
	if err := s.UpsertClient(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetClient(ctx, want.ID)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("GetClient (-want +got):\n%s", diff)
	}

	refetched := oauthT0.Add(2 * time.Hour)
	update := &oauth.Client{ID: want.ID, Name: "Renamed", RedirectURIs: []string{"https://x.example/cb"},
		CreatedAt: oauthT0.Add(time.Hour), FetchedAt: &refetched}
	if err := s.UpsertClient(ctx, update); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetClient(ctx, want.ID)
	if err != nil {
		t.Fatal(err)
	}
	update.CreatedAt = oauthT0
	if diff := cmp.Diff(update, got); diff != "" {
		t.Errorf("GetClient after upsert (-want +got):\n%s", diff)
	}

	if _, err := s.GetClient(ctx, "nope"); !errors.Is(err, oauth.ErrNotFound) {
		t.Errorf("GetClient(nope) err = %v, want ErrNotFound", err)
	}
}

func TestOAuthStoreRequest(t *testing.T) {
	ctx := context.Background()
	s := oauthTestStores(t)
	want := &oauth.Request{ID: "req1", ClientID: "dcr_test", RedirectURI: "https://app.example/cb",
		Scopes: []string{"artifacts:read", "artifacts:write"}, CodeChallenge: "chal", Resource: "https://mcp.example/",
		ClientState: "cs", Nonce: "n", UpstreamState: "us", UpstreamVerifier: "uv", ExpiresAt: oauthT0.Add(10 * time.Minute)}
	if err := s.CreateRequest(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetRequest(ctx, "req1")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("GetRequest (-want +got):\n%s", diff)
	}

	if err := s.BindRequest(ctx, "req1", "ghost"); !errors.Is(err, user.ErrNotFound) {
		t.Errorf("BindRequest(ghost) err = %v, want user.ErrNotFound", err)
	}
	if err := s.BindRequest(ctx, "req1", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := s.BindRequest(ctx, "req1", "bob"); !errors.Is(err, oauth.ErrNotFound) {
		t.Errorf("second BindRequest err = %v, want ErrNotFound", err)
	}
	if err := s.BindRequest(ctx, "missing", "bob"); !errors.Is(err, oauth.ErrNotFound) {
		t.Errorf("BindRequest(missing) err = %v, want ErrNotFound", err)
	}
	got, err = s.GetRequest(ctx, "req1")
	if err != nil {
		t.Fatal(err)
	}
	if got.UserHandle != "alice" {
		t.Errorf("UserHandle = %q, want alice", got.UserHandle)
	}

	if err := s.DeleteRequest(ctx, "req1"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRequest(ctx, "req1"); err != nil {
		t.Errorf("second DeleteRequest: %v", err)
	}
	if _, err := s.GetRequest(ctx, "req1"); !errors.Is(err, oauth.ErrNotFound) {
		t.Errorf("GetRequest after delete err = %v, want ErrNotFound", err)
	}
}

func TestOAuthStoreConsumeCode(t *testing.T) {
	ctx := context.Background()
	s := oauthTestStores(t)
	c := &oauth.Code{Hash: "h1", ClientID: "dcr_test", UserHandle: "alice", Scopes: []string{"artifacts:read"},
		RedirectURI: "https://app.example/cb", CodeChallenge: "chal", Resource: "r", ExpiresAt: oauthT0.Add(5 * time.Minute)}
	if err := s.CreateCode(ctx, c); err != nil {
		t.Fatal(err)
	}
	used := oauthT0.Add(time.Second)
	got, err := s.ConsumeCode(ctx, "h1", used)
	if err != nil {
		t.Fatal(err)
	}
	want := *c
	want.UsedAt = &used
	if diff := cmp.Diff(&want, got); diff != "" {
		t.Errorf("ConsumeCode (-want +got):\n%s", diff)
	}
	if _, err := s.ConsumeCode(ctx, "h1", used); !errors.Is(err, oauth.ErrCodeUsed) {
		t.Errorf("second ConsumeCode err = %v, want ErrCodeUsed", err)
	}
	if _, err := s.ConsumeCode(ctx, "nope", used); !errors.Is(err, oauth.ErrNotFound) {
		t.Errorf("ConsumeCode(nope) err = %v, want ErrNotFound", err)
	}
}

func TestOAuthStoreConsumeCodeConcurrent(t *testing.T) {
	ctx := context.Background()
	s := oauthTestStores(t)
	if err := s.CreateCode(ctx, &oauth.Code{Hash: "h", ClientID: "dcr_test", UserHandle: "alice",
		RedirectURI: "https://app.example/cb", CodeChallenge: "c", ExpiresAt: oauthT0.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	const n = 8
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
	)
	for range n {
		wg.Go(func() {
			_, err := s.ConsumeCode(ctx, "h", oauthT0)
			switch {
			case err == nil:
				mu.Lock()
				wins++
				mu.Unlock()
			case !errors.Is(err, oauth.ErrCodeUsed):
				t.Errorf("ConsumeCode: %v", err)
			}
		})
	}
	wg.Wait()
	if wins != 1 {
		t.Errorf("%d of %d concurrent ConsumeCode calls succeeded, want 1", wins, n)
	}
}

func TestOAuthStoreKeyPrefix(t *testing.T) {
	ctx := context.Background()
	s := oauthTestStores(t)
	c := &oauth.Code{Hash: "hk", ClientID: "dcr_test", UserHandle: "alice", RedirectURI: "https://app.example/cb",
		CodeChallenge: "chal", KeyPrefix: "abcdefgh", ExpiresAt: oauthT0.Add(5 * time.Minute)}
	if err := s.CreateCode(ctx, c); err != nil {
		t.Fatal(err)
	}
	got, err := s.ConsumeCode(ctx, "hk", oauthT0)
	if err != nil {
		t.Fatal(err)
	}
	if got.KeyPrefix != "abcdefgh" {
		t.Errorf("code KeyPrefix = %q, want abcdefgh", got.KeyPrefix)
	}

	a1, r1 := oauthPair("alice", nil, oauthT0, 0)
	a1.KeyPrefix, r1.KeyPrefix = "abcdefgh", "abcdefgh"
	if err := s.CreateTokens(ctx, a1, r1); err != nil {
		t.Fatal(err)
	}
	a2, r2 := oauthPair("alice", nil, oauthT0, 1)
	a2.KeyPrefix, r2.KeyPrefix = "abcdefgh", "abcdefgh"
	if err := s.Rotate(ctx, r1.ID, oauthT0, a2, r2); err != nil {
		t.Fatal(err)
	}
	for _, tok := range []*oauth.Token{a1, a2, r2} {
		got, err := s.GetToken(ctx, tok.Hash)
		if err != nil {
			t.Fatal(err)
		}
		if got.KeyPrefix != "abcdefgh" {
			t.Errorf("%s token %d KeyPrefix = %q, want abcdefgh", got.Kind, got.ID, got.KeyPrefix)
		}
	}
	grants, err := s.ListGrants(ctx, "alice", oauthT0)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 || grants[0].KeyPrefix != "abcdefgh" {
		t.Errorf("ListGrants = %+v, want one grant with the key prefix", grants)
	}
}

func oauthPair(handle string, scopes []string, now time.Time, n int) (*oauth.Token, *oauth.Token) {
	mk := func(kind string, ttl time.Duration) *oauth.Token {
		return &oauth.Token{Hash: kind + "-" + handle + "-" + string(rune('a'+n)), Kind: kind, ClientID: "dcr_test",
			UserHandle: handle, Scopes: scopes, Resource: "r", CreatedAt: now, ExpiresAt: now.Add(ttl)}
	}
	return mk(oauth.KindAccess, time.Hour), mk(oauth.KindRefresh, 30*24*time.Hour)
}

func TestOAuthStoreTokens(t *testing.T) {
	ctx := context.Background()
	s := oauthTestStores(t)
	scopes := []string{"artifacts:read"}

	a1, r1 := oauthPair("alice", scopes, oauthT0, 0)
	if err := s.CreateTokens(ctx, a1, r1); err != nil {
		t.Fatal(err)
	}
	if r1.ID == 0 || a1.ID == 0 || r1.FamilyID != r1.ID || a1.FamilyID != r1.ID {
		t.Fatalf("ids: access %d refresh %d families %d %d", a1.ID, r1.ID, a1.FamilyID, r1.FamilyID)
	}
	got, err := s.GetToken(ctx, r1.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(r1, got); diff != "" {
		t.Errorf("GetToken (-want +got):\n%s", diff)
	}
	if _, err := s.GetToken(ctx, "nope"); !errors.Is(err, oauth.ErrNotFound) {
		t.Errorf("GetToken(nope) err = %v, want ErrNotFound", err)
	}

	t1 := oauthT0.Add(time.Hour)
	a2, r2 := oauthPair("alice", scopes, t1, 1)
	if err := s.Rotate(ctx, r1.ID, t1, a2, r2); err != nil {
		t.Fatal(err)
	}
	if r2.FamilyID != r1.ID || a2.FamilyID != r1.ID {
		t.Errorf("rotated family = %d/%d, want %d", a2.FamilyID, r2.FamilyID, r1.ID)
	}
	old, err := s.GetToken(ctx, r1.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if old.RevokedAt == nil || !old.RevokedAt.Equal(t1) || old.ReplacedBy == nil || *old.ReplacedBy != r2.ID {
		t.Errorf("old refresh token = revoked %v replaced %v, want %v and %d", old.RevokedAt, old.ReplacedBy, t1, r2.ID)
	}
	a3, r3 := oauthPair("alice", scopes, t1, 2)
	if err := s.Rotate(ctx, r1.ID, t1, a3, r3); !errors.Is(err, oauth.ErrNotFound) {
		t.Errorf("Rotate of rotated token err = %v, want ErrNotFound", err)
	}
	if _, err := s.GetToken(ctx, r3.Hash); !errors.Is(err, oauth.ErrNotFound) {
		t.Errorf("failed Rotate left its tokens behind: %v", err)
	}
	if err := s.Rotate(ctx, a2.ID, t1, a3, r3); !errors.Is(err, oauth.ErrNotFound) {
		t.Errorf("Rotate of access token err = %v, want ErrNotFound", err)
	}

	a4, r4 := oauthPair("alice", scopes, t1.Add(time.Minute), 3)
	if err := s.CreateTokens(ctx, a4, r4); err != nil {
		t.Fatal(err)
	}
	a5, r5 := oauthPair("bob", scopes, t1, 4)
	if err := s.CreateTokens(ctx, a5, r5); err != nil {
		t.Fatal(err)
	}

	grantIDs := func(handle string, now time.Time) []int64 {
		t.Helper()
		gs, err := s.ListGrants(ctx, handle, now)
		if err != nil {
			t.Fatal(err)
		}
		var ids []int64
		for _, g := range gs {
			ids = append(ids, g.ID)
		}
		return ids
	}
	if diff := cmp.Diff([]int64{r4.ID, r2.ID}, grantIDs("alice", t1)); diff != "" {
		t.Errorf("ListGrants(alice) (-want +got):\n%s", diff)
	}
	if got := grantIDs("alice", oauthT0.Add(60*24*time.Hour)); got != nil {
		t.Errorf("ListGrants after expiry = %v, want none", got)
	}

	if err := s.RevokeGrant(ctx, r2.ID, t1); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{a1.Hash, a2.Hash, r2.Hash} {
		tok, err := s.GetToken(ctx, h)
		if err != nil {
			t.Fatal(err)
		}
		if tok.RevokedAt == nil {
			t.Errorf("token %s not revoked by RevokeGrant", h)
		}
	}
	if diff := cmp.Diff([]int64{r4.ID}, grantIDs("alice", t1)); diff != "" {
		t.Errorf("ListGrants after revoke (-want +got):\n%s", diff)
	}
	if err := s.RevokeGrant(ctx, a4.ID, t1); !errors.Is(err, oauth.ErrNotFound) {
		t.Errorf("RevokeGrant(access token) err = %v, want ErrNotFound", err)
	}

	if err := s.RevokeFamily(ctx, r4.FamilyID, t1); err != nil {
		t.Fatal(err)
	}
	if got := grantIDs("alice", t1); got != nil {
		t.Errorf("ListGrants after RevokeFamily = %v", got)
	}
	if err := s.RevokeAllForUser(ctx, "alice", t1); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]int64{r5.ID}, grantIDs("bob", t1)); diff != "" {
		t.Errorf("ListGrants(bob) (-want +got):\n%s", diff)
	}
	if err := s.RevokeAllForUser(ctx, "bob", t1); err != nil {
		t.Fatal(err)
	}
	if got := grantIDs("bob", t1); got != nil {
		t.Errorf("ListGrants(bob) after RevokeAllForUser = %v", got)
	}
}

func TestOAuthStoreDeleteExpired(t *testing.T) {
	ctx := context.Background()
	s := oauthTestStores(t)
	if err := s.CreateRequest(ctx, &oauth.Request{ID: "old", ClientID: "dcr_test", RedirectURI: "u", CodeChallenge: "c", ExpiresAt: oauthT0}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRequest(ctx, &oauth.Request{ID: "new", ClientID: "dcr_test", RedirectURI: "u", CodeChallenge: "c", ExpiresAt: oauthT0.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateCode(ctx, &oauth.Code{Hash: "old", ClientID: "dcr_test", UserHandle: "alice", RedirectURI: "u", CodeChallenge: "c", ExpiresAt: oauthT0}); err != nil {
		t.Fatal(err)
	}
	// r1 is rotated into r2 so replaced_by references survive the delete.
	a1, r1 := oauthPair("alice", nil, oauthT0.Add(-time.Hour), 0)
	if err := s.CreateTokens(ctx, a1, r1); err != nil {
		t.Fatal(err)
	}
	a2, r2 := oauthPair("alice", nil, oauthT0.Add(-time.Hour), 1)
	if err := s.Rotate(ctx, r1.ID, oauthT0.Add(-time.Hour), a2, r2); err != nil {
		t.Fatal(err)
	}

	n, err := s.DeleteExpired(ctx, oauthT0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 { // request "old", code "old", access tokens a1 and a2
		t.Errorf("DeleteExpired = %d, want 4", n)
	}
	if _, err := s.GetRequest(ctx, "old"); !errors.Is(err, oauth.ErrNotFound) {
		t.Errorf("expired request kept: %v", err)
	}
	if _, err := s.GetRequest(ctx, "new"); err != nil {
		t.Errorf("live request deleted: %v", err)
	}
	if _, err := s.GetToken(ctx, a1.Hash); !errors.Is(err, oauth.ErrNotFound) {
		t.Errorf("expired access token kept: %v", err)
	}
	if _, err := s.GetToken(ctx, r1.Hash); err != nil {
		t.Errorf("live refresh token deleted: %v", err)
	}

	n, err = s.DeleteExpired(ctx, oauthT0.Add(31*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 { // request "new", r1, r2
		t.Errorf("second DeleteExpired = %d, want 3", n)
	}
}
