package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/oauth"
	"github.com/PeterBooker/kazari/internal/user"
)

// The 0009 migration splits artifacts:share out of artifacts:write: every
// stored credential must keep each right it had, and gain nothing else.
func TestMigrationShareScopeKeepsRights(t *testing.T) {
	ctx := context.Background()
	raw, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "kazari.db")+"?_pragma=foreign_keys(1)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	sub, err := fs.Sub(migrationFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	older := fstest.MapFS{}
	entries, err := fs.ReadDir(sub, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() >= "0009" {
			continue
		}
		b, err := fs.ReadFile(sub, e.Name())
		if err != nil {
			t.Fatal(err)
		}
		older[e.Name()] = &fstest.MapFile{Data: b}
	}
	if err := migrate(ctx, raw, older); err != nil {
		t.Fatal(err)
	}

	before := []string{
		"artifacts:read artifacts:write artifacts:delete",
		"artifacts:read artifacts:write artifacts:delete admin",
		"artifacts:read artifacts:write",
		"artifacts:write",
		"artifacts:write artifacts:delete",
		"artifacts:read",
		"artifacts:read artifacts:delete",
		"admin",
		"",
	}
	for _, q := range []string{
		`INSERT INTO user (id, handle, role, created_at) VALUES (1, 'alice', 'admin', 1)`,
		`INSERT INTO oauth_client (id, created_at) VALUES ('c', 1)`,
	} {
		if _, err := raw.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for i, scopes := range before {
		for _, q := range []string{
			`INSERT INTO api_key (user_id, prefix, hash, scopes, created_at) VALUES (1, 'k' || ?1, 'h', ?2, 1)`,
			`INSERT INTO oauth_request (id, client_id, redirect_uri, scopes, code_challenge, expires_at) VALUES ('r' || ?1, 'c', 'u', ?2, 'x', 1)`,
			`INSERT INTO oauth_code (code_hash, client_id, user_id, scopes, redirect_uri, code_challenge, expires_at) VALUES ('c' || ?1, 'c', 1, ?2, 'u', 'x', 1)`,
			`INSERT INTO oauth_token (token_hash, kind, client_id, user_id, scopes, created_at, expires_at) VALUES ('t' || ?1, 'refresh', 'c', 1, ?2, 1, 1)`,
		} {
			if _, err := raw.ExecContext(ctx, q, i, scopes); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
	}

	if err := migrate(ctx, raw, sub); err != nil {
		t.Fatal(err)
	}
	for table, key := range map[string]string{"api_key": "prefix", "oauth_request": "id", "oauth_code": "code_hash", "oauth_token": "token_hash"} {
		rows, err := raw.QueryContext(ctx, `SELECT scopes FROM `+table+` ORDER BY `+key)
		if err != nil {
			t.Fatal(err)
		}
		var after []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			after = append(after, s)
		}
		rows.Close()
		if len(after) != len(before) {
			t.Fatalf("%s: %d rows, want %d", table, len(after), len(before))
		}
		for i, was := range before {
			had, has := strings.Fields(was), strings.Fields(after[i])
			want := slices.Clone(had)
			if slices.Contains(had, "artifacts:write") {
				want = append(want, "artifacts:share")
			}
			slices.Sort(want)
			got := slices.Clone(has)
			slices.Sort(got)
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("%s: scopes %q became %q (-want +got):\n%s", table, was, after[i], diff)
			}
		}
	}

	var slugs string
	var issuedBy sql.NullInt64
	if err := raw.QueryRowContext(ctx, `SELECT slugs, issued_by FROM api_key WHERE prefix = 'k0'`).Scan(&slugs, &issuedBy); err != nil {
		t.Fatal(err)
	}
	if slugs != "" || issuedBy.Valid {
		t.Errorf("an existing key got slugs %q and issuer %v, want neither", slugs, issuedBy)
	}
	// In the order every new key is stored with.
	var first string
	if err := raw.QueryRowContext(ctx, `SELECT scopes FROM api_key WHERE prefix = 'k1'`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if want := "artifacts:read artifacts:write artifacts:share artifacts:delete admin"; first != want {
		t.Errorf("scopes = %q, want %q", first, want)
	}
}

func TestUserStoreKeyLineage(t *testing.T) {
	ctx := context.Background()
	s := userTestStore(t)
	userMustCreate(t, s, "alice", "", user.RoleMember)
	userMustCreate(t, s, "bob", "", user.RoleMember)
	mk := func(handle, prefix, issuedBy string, slugs ...string) *user.APIKey {
		t.Helper()
		k := &user.APIKey{UserHandle: handle, Prefix: prefix, Hash: "h-" + prefix, Scopes: []string{"artifacts:read"},
			Slugs: slugs, IssuedBy: issuedBy, CreatedAt: userT0}
		if err := s.CreateKey(ctx, k); err != nil {
			t.Fatalf("CreateKey(%s): %v", prefix, err)
		}
		return k
	}
	root := mk("alice", "rootroot", "")
	child := mk("alice", "childchi", "rootroot", "weekly-*", "report")
	mk("alice", "grandchi", "childchi")
	mk("alice", "sibling1", "rootroot")
	mk("alice", "unrelate", "")
	mk("bob", "bobsbobs", "")

	got, _, err := s.GetKey(ctx, "childchi")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(child, got); diff != "" {
		t.Errorf("GetKey (-want +got):\n%s", diff)
	}
	keys, err := s.ListKeys(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	issuers := map[string]string{}
	for _, k := range keys {
		issuers[k.Prefix] = k.IssuedBy
	}
	wantIssuers := map[string]string{"rootroot": "", "childchi": "rootroot", "grandchi": "childchi", "sibling1": "rootroot", "unrelate": ""}
	if diff := cmp.Diff(wantIssuers, issuers); diff != "" {
		t.Errorf("issuers (-want +got):\n%s", diff)
	}

	for name, k := range map[string]*user.APIKey{
		"an unknown issuer":      {UserHandle: "alice", Prefix: "orphan01", IssuedBy: "zzzzzzzz", CreatedAt: userT0},
		"another user's key":     {UserHandle: "alice", Prefix: "orphan02", IssuedBy: "bobsbobs", CreatedAt: userT0},
		"a key for another user": {UserHandle: "bob", Prefix: "orphan03", IssuedBy: "rootroot", CreatedAt: userT0},
	} {
		if err := s.CreateKey(ctx, k); !errors.Is(err, user.ErrKeyNotFound) {
			t.Errorf("CreateKey issued by %s: err = %v, want ErrKeyNotFound", name, err)
		}
		if _, _, err := s.GetKey(ctx, k.Prefix); !errors.Is(err, user.ErrKeyNotFound) {
			t.Errorf("CreateKey issued by %s left a key behind", name)
		}
	}

	early := userT0.Add(time.Hour)
	if issued, err := s.RevokeKey(ctx, "alice", "sibling1", early); err != nil || len(issued) != 0 {
		t.Fatalf("RevokeKey of a key that issued none = %v, %v", issued, err)
	}
	at := userT0.Add(2 * time.Hour)
	issued, err := s.RevokeKey(ctx, "alice", "rootroot", at)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"childchi", "grandchi"}, issued); diff != "" {
		t.Errorf("issued keys revoked (-want +got):\n%s", diff)
	}
	wantRevoked := map[string]*time.Time{"rootroot": &at, "childchi": &at, "grandchi": &at, "sibling1": &early, "unrelate": nil, "bobsbobs": nil}
	for prefix, want := range wantRevoked {
		k, _, err := s.GetKey(ctx, prefix)
		if err != nil {
			t.Fatal(err)
		}
		if (k.RevokedAt == nil) != (want == nil) || want != nil && !k.RevokedAt.Equal(*want) {
			t.Errorf("%s RevokedAt = %v, want %v", prefix, k.RevokedAt, want)
		}
	}
	if issued, err := s.RevokeKey(ctx, "alice", "rootroot", at.Add(time.Hour)); err != nil || len(issued) != 0 {
		t.Errorf("a second RevokeKey = %v, %v; want nothing more revoked", issued, err)
	}
	if k, _, _ := s.GetKey(ctx, "rootroot"); !k.RevokedAt.Equal(at) || k.ID != root.ID {
		t.Errorf("a second RevokeKey moved the time to %v", k.RevokedAt)
	}
}

func TestUserStoreRotateKey(t *testing.T) {
	ctx := context.Background()
	s := userTestStore(t)
	userMustCreate(t, s, "alice", "", user.RoleMember)
	userMustCreate(t, s, "bob", "", user.RoleMember)
	soon := userT0.Add(time.Hour)
	for _, k := range []*user.APIKey{
		{UserHandle: "alice", Prefix: "foreverk", CreatedAt: userT0},
		{UserHandle: "alice", Prefix: "soonsoon", ExpiresAt: &soon, CreatedAt: userT0},
	} {
		if err := s.CreateKey(ctx, k); err != nil {
			t.Fatal(err)
		}
	}
	expiry := func(prefix string) *time.Time {
		t.Helper()
		k, _, err := s.GetKey(ctx, prefix)
		if err != nil {
			t.Fatal(err)
		}
		return k.ExpiresAt
	}

	ends := userT0.Add(24 * time.Hour)
	next := &user.APIKey{UserHandle: "alice", Prefix: "nextnext", Slugs: []string{"weekly-*"}, CreatedAt: userT0}
	if err := s.RotateKey(ctx, "foreverk", next, ends); err != nil {
		t.Fatal(err)
	}
	if got := expiry("foreverk"); got == nil || !got.Equal(ends) {
		t.Errorf("old key expires at %v, want %v", got, ends)
	}
	if got, _, err := s.GetKey(ctx, "nextnext"); err != nil || got.ID != next.ID || next.ID == 0 || !cmp.Equal(got.Slugs, next.Slugs) {
		t.Errorf("new key = %+v, %v", got, err)
	}

	if err := s.RotateKey(ctx, "soonsoon", &user.APIKey{UserHandle: "alice", Prefix: "aftersoo", CreatedAt: userT0}, ends); err != nil {
		t.Fatal(err)
	}
	if got := expiry("soonsoon"); !got.Equal(soon) {
		t.Errorf("a key that ends sooner now expires at %v, want %v kept", got, soon)
	}

	for name, k := range map[string]struct{ prefix, handle string }{
		"an unknown key":     {"zzzzzzzz", "alice"},
		"another user's key": {"foreverk", "bob"},
	} {
		orphan := &user.APIKey{UserHandle: k.handle, Prefix: "orphanok", CreatedAt: userT0}
		if err := s.RotateKey(ctx, k.prefix, orphan, userT0); !errors.Is(err, user.ErrKeyNotFound) {
			t.Errorf("RotateKey of %s: err = %v, want ErrKeyNotFound", name, err)
		}
		if _, _, err := s.GetKey(ctx, "orphanok"); !errors.Is(err, user.ErrKeyNotFound) {
			t.Errorf("RotateKey of %s stored the new key", name)
		}
	}
	if got := expiry("foreverk"); !got.Equal(ends) {
		t.Errorf("a refused rotation moved the expiry to %v", got)
	}
}

func TestOAuthStoreReissue(t *testing.T) {
	ctx := context.Background()
	scopes := []string{"artifacts:read"}
	const window = 30 * time.Second
	t1 := oauthT0.Add(time.Hour)

	// rotated returns a store whose family was rotated once at t1: r1 to r2.
	rotated := func(t *testing.T) (s *OAuthStore, r1, r2 *oauth.Token) {
		t.Helper()
		s = oauthTestStores(t)
		a1, r1 := oauthPair("alice", scopes, oauthT0, 0)
		if err := s.CreateTokens(ctx, a1, r1); err != nil {
			t.Fatal(err)
		}
		a2, r2 := oauthPair("alice", scopes, t1, 1)
		if err := s.Rotate(ctx, r1.ID, t1, a2, r2); err != nil {
			t.Fatal(err)
		}
		return s, r1, r2
	}
	reissue := func(s *OAuthStore, oldID int64, at time.Time, n int) (*oauth.Token, *oauth.Token, error) {
		a, r := oauthPair("alice", scopes, at, n)
		return a, r, s.Reissue(ctx, oldID, at.Add(-window), a, r)
	}
	refused := func(t *testing.T, s *OAuthStore, r *oauth.Token, err error) {
		t.Helper()
		if !errors.Is(err, oauth.ErrNotFound) {
			t.Errorf("Reissue err = %v, want ErrNotFound", err)
		}
		if _, err := s.GetToken(ctx, r.Hash); !errors.Is(err, oauth.ErrNotFound) {
			t.Errorf("a refused Reissue stored its tokens: %v", err)
		}
	}

	t.Run("inside the window, once", func(t *testing.T) {
		s, r1, r2 := rotated(t)
		at := t1.Add(window - time.Nanosecond)
		a3, r3, err := reissue(s, r1.ID, at, 2)
		if err != nil {
			t.Fatal(err)
		}
		if r3.FamilyID != r1.FamilyID || a3.FamilyID != r1.FamilyID || r3.ID <= r2.ID {
			t.Errorf("reissued pair: family %d/%d, id %d; want family %d and an id above %d", a3.FamilyID, r3.FamilyID, r3.ID, r1.FamilyID, r2.ID)
		}
		for name, tok := range map[string]*oauth.Token{"the first successor": r2, "the reissued token": r3} {
			got, err := s.GetToken(ctx, tok.Hash)
			if err != nil || got.RevokedAt != nil || got.ReplacedBy != nil {
				t.Errorf("%s = %+v, %v; want it live and unrotated", name, got, err)
			}
		}
		old, _ := s.GetToken(ctx, r1.Hash)
		if old.ReplacedBy == nil || *old.ReplacedBy != r2.ID || !old.RevokedAt.Equal(t1) {
			t.Errorf("the reused token changed: %+v", old)
		}
		_, r4, err := reissue(s, r1.ID, at, 3)
		refused(t, s, r4, err)

		grants, err := s.ListGrants(ctx, "alice", at)
		if err != nil || len(grants) != 1 || grants[0].ID != r3.ID || grants[0].FamilyID != r1.FamilyID {
			t.Errorf("ListGrants = %+v, %v; want the family once, as its newest token", grants, err)
		}
	})
	t.Run("at the edge of the window", func(t *testing.T) {
		s, r1, _ := rotated(t)
		_, r3, err := reissue(s, r1.ID, t1.Add(window), 2)
		refused(t, s, r3, err)
	})
	t.Run("after the successor was rotated", func(t *testing.T) {
		s, r1, r2 := rotated(t)
		at := t1.Add(time.Second)
		a3, r3 := oauthPair("alice", scopes, at, 2)
		if err := s.Rotate(ctx, r2.ID, at, a3, r3); err != nil {
			t.Fatal(err)
		}
		_, r4, err := reissue(s, r1.ID, at, 3)
		refused(t, s, r4, err)
		// The token before last is inside the window, and its successor intact.
		if _, _, err := reissue(s, r2.ID, at, 4); err != nil {
			t.Errorf("Reissue for the last rotated token: %v", err)
		}
	})
	t.Run("in a revoked family", func(t *testing.T) {
		s, r1, _ := rotated(t)
		if err := s.RevokeFamily(ctx, r1.FamilyID, t1.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		_, r3, err := reissue(s, r1.ID, t1.Add(2*time.Second), 2)
		refused(t, s, r3, err)
	})
	t.Run("an unrotated token, an access token, an unknown id", func(t *testing.T) {
		s, r1, r2 := rotated(t)
		at := t1.Add(time.Second)
		for _, id := range []int64{r2.ID, r1.ID - 1, r2.ID - 1, 9999} {
			if id == r1.ID {
				continue
			}
			_, r3, err := reissue(s, id, at, 2)
			refused(t, s, r3, err)
		}
	})
	t.Run("after the successor expired", func(t *testing.T) {
		s, r1, r2 := rotated(t)
		a, r := oauthPair("alice", scopes, r2.ExpiresAt, 2)
		refused(t, s, r, s.Reissue(ctx, r1.ID, t1.Add(-time.Second), a, r))
	})
	t.Run("many at once", func(t *testing.T) {
		s, r1, _ := rotated(t)
		at := t1.Add(time.Second)
		const n = 8
		var (
			wg   sync.WaitGroup
			errs = make([]error, n)
		)
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _, errs[i] = reissue(s, r1.ID, at, 10+i)
			}()
		}
		wg.Wait()
		ok := 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case !errors.Is(err, oauth.ErrNotFound):
				t.Errorf("Reissue err = %v", err)
			}
		}
		if ok != 1 {
			t.Errorf("%d of %d concurrent Reissues succeeded, want 1", ok, n)
		}
	})
}
