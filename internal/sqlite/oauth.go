package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"agalma/internal/oauth"
	"agalma/internal/user"
)

// OAuthStore implements oauth.Store.
type OAuthStore struct {
	db *sql.DB
}

var _ oauth.Store = (*OAuthStore)(nil)

// NewOAuthStore returns an OAuth store backed by db.
func NewOAuthStore(db *DB) *OAuthStore {
	return &OAuthStore{db: db.DB}
}

func oauthJoinScopes(scopes []string) string { return strings.Join(scopes, " ") }

func oauthSplitScopes(s string) []string {
	if s == "" {
		return []string{}
	}
	return strings.Fields(s)
}

// oauthUserID returns the id of the user with handle, or user.ErrNotFound.
func oauthUserID(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, handle string) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx, `SELECT id FROM user WHERE handle = ?`, handle).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, user.ErrNotFound
	}
	return id, err
}

// UpsertClient inserts the client or replaces its name, redirect URIs and
// fetch time.
func (s *OAuthStore) UpsertClient(ctx context.Context, c *oauth.Client) error {
	uris, err := json.Marshal(c.RedirectURIs)
	if err != nil {
		return fmt.Errorf("sqlite: upsert client %q: %w", c.ID, err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO oauth_client (id, name, redirect_uris, created_at, fetched_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET name = excluded.name, redirect_uris = excluded.redirect_uris,
			fetched_at = excluded.fetched_at`,
		c.ID, c.Name, string(uris), toNanos(c.CreatedAt), nullNanos(c.FetchedAt))
	if err != nil {
		return fmt.Errorf("sqlite: upsert client %q: %w", c.ID, err)
	}
	return nil
}

// GetClient returns a client by id, or oauth.ErrNotFound.
func (s *OAuthStore) GetClient(ctx context.Context, id string) (*oauth.Client, error) {
	var (
		c       = oauth.Client{ID: id}
		uris    string
		created int64
		fetched sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx, `SELECT name, redirect_uris, created_at, fetched_at
		FROM oauth_client WHERE id = ?`, id).Scan(&c.Name, &uris, &created, &fetched)
	if errors.Is(err, sql.ErrNoRows) {
		err = oauth.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get client %q: %w", id, err)
	}
	if err := json.Unmarshal([]byte(uris), &c.RedirectURIs); err != nil {
		return nil, fmt.Errorf("sqlite: get client %q: redirect_uris: %w", id, err)
	}
	c.CreatedAt = fromNanos(created)
	c.FetchedAt = ptrTime(fetched)
	return &c, nil
}

// CreateRequest stores a pending authorization request. A non-empty
// UserHandle is bound at once.
func (s *OAuthStore) CreateRequest(ctx context.Context, r *oauth.Request) error {
	err := inTx(ctx, s.db, func(tx *sql.Tx) error {
		var uid sql.NullInt64
		if r.UserHandle != "" {
			id, err := oauthUserID(ctx, tx, r.UserHandle)
			if err != nil {
				return err
			}
			uid = sql.NullInt64{Int64: id, Valid: true}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO oauth_request
			(id, client_id, redirect_uri, scopes, code_challenge, resource, client_state,
			 nonce, upstream_state, upstream_verifier, user_id, expires_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ID, r.ClientID, r.RedirectURI, oauthJoinScopes(r.Scopes), r.CodeChallenge, r.Resource,
			r.ClientState, r.Nonce, r.UpstreamState, r.UpstreamVerifier, uid, toNanos(r.ExpiresAt))
		return err
	})
	if err != nil {
		return fmt.Errorf("sqlite: create oauth request: %w", err)
	}
	return nil
}

// GetRequest returns a request, expired or not, or oauth.ErrNotFound.
func (s *OAuthStore) GetRequest(ctx context.Context, id string) (*oauth.Request, error) {
	var (
		r       = oauth.Request{ID: id}
		scopes  string
		handle  sql.NullString
		expires int64
	)
	err := s.db.QueryRowContext(ctx, `SELECT r.client_id, r.redirect_uri, r.scopes, r.code_challenge,
			r.resource, r.client_state, r.nonce, r.upstream_state, r.upstream_verifier, u.handle, r.expires_at
		FROM oauth_request r LEFT JOIN user u ON u.id = r.user_id
		WHERE r.id = ?`, id).Scan(&r.ClientID, &r.RedirectURI, &scopes, &r.CodeChallenge,
		&r.Resource, &r.ClientState, &r.Nonce, &r.UpstreamState, &r.UpstreamVerifier, &handle, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		err = oauth.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get oauth request: %w", err)
	}
	r.Scopes = oauthSplitScopes(scopes)
	r.UserHandle = handle.String
	r.ExpiresAt = fromNanos(expires)
	return &r, nil
}

// BindRequest binds a user to a request that has none yet. It returns
// oauth.ErrNotFound when the request is missing or already bound, and
// user.ErrNotFound for an unknown handle.
func (s *OAuthStore) BindRequest(ctx context.Context, id, handle string) error {
	err := inTx(ctx, s.db, func(tx *sql.Tx) error {
		uid, err := oauthUserID(ctx, tx, handle)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE oauth_request SET user_id = ? WHERE id = ? AND user_id IS NULL`, uid, id)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			if err == nil {
				err = oauth.ErrNotFound
			}
			return err
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("sqlite: bind oauth request: %w", err)
	}
	return nil
}

// DeleteRequest removes a request; a missing one is not an error.
func (s *OAuthStore) DeleteRequest(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM oauth_request WHERE id = ?`, id); err != nil {
		return fmt.Errorf("sqlite: delete oauth request: %w", err)
	}
	return nil
}

// CreateCode stores an authorization code.
func (s *OAuthStore) CreateCode(ctx context.Context, c *oauth.Code) error {
	err := inTx(ctx, s.db, func(tx *sql.Tx) error {
		uid, err := oauthUserID(ctx, tx, c.UserHandle)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO oauth_code
			(code_hash, client_id, user_id, scopes, redirect_uri, code_challenge, resource, expires_at, used_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			c.Hash, c.ClientID, uid, oauthJoinScopes(c.Scopes), c.RedirectURI, c.CodeChallenge,
			c.Resource, toNanos(c.ExpiresAt), nullNanos(c.UsedAt))
		return err
	})
	if err != nil {
		return fmt.Errorf("sqlite: create oauth code: %w", err)
	}
	return nil
}

// ConsumeCode marks the code used at at and returns it. A second call for
// the same code returns oauth.ErrCodeUsed; an unknown code
// oauth.ErrNotFound.
func (s *OAuthStore) ConsumeCode(ctx context.Context, hash string, at time.Time) (*oauth.Code, error) {
	var c *oauth.Code
	err := inTx(ctx, s.db, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE oauth_code SET used_at = ? WHERE code_hash = ? AND used_at IS NULL`, toNanos(at), hash)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		c, err = oauthGetCode(ctx, tx, hash)
		if err != nil {
			return err
		}
		if n == 0 {
			return oauth.ErrCodeUsed
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("sqlite: consume oauth code: %w", err)
	}
	return c, nil
}

func oauthGetCode(ctx context.Context, tx *sql.Tx, hash string) (*oauth.Code, error) {
	var (
		c       = oauth.Code{Hash: hash}
		scopes  string
		expires int64
		used    sql.NullInt64
	)
	err := tx.QueryRowContext(ctx, `SELECT c.client_id, u.handle, c.scopes, c.redirect_uri,
			c.code_challenge, c.resource, c.expires_at, c.used_at
		FROM oauth_code c JOIN user u ON u.id = c.user_id
		WHERE c.code_hash = ?`, hash).Scan(&c.ClientID, &c.UserHandle, &scopes, &c.RedirectURI,
		&c.CodeChallenge, &c.Resource, &expires, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, oauth.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c.Scopes = oauthSplitScopes(scopes)
	c.ExpiresAt = fromNanos(expires)
	c.UsedAt = ptrTime(used)
	return &c, nil
}

// oauthInsertToken inserts t for the user uid and sets t.ID.
func oauthInsertToken(ctx context.Context, tx *sql.Tx, uid int64, t *oauth.Token) error {
	res, err := tx.ExecContext(ctx, `INSERT INTO oauth_token
		(token_hash, kind, client_id, user_id, scopes, resource, family_id, created_at, expires_at, revoked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.Hash, t.Kind, t.ClientID, uid, oauthJoinScopes(t.Scopes), t.Resource, t.FamilyID,
		toNanos(t.CreatedAt), toNanos(t.ExpiresAt), nullNanos(t.RevokedAt))
	if err != nil {
		return err
	}
	t.ID, err = res.LastInsertId()
	return err
}

// oauthInsertPair inserts refresh, then access, both in family (0 = start a
// new family at the refresh token's id).
func oauthInsertPair(ctx context.Context, tx *sql.Tx, family int64, access, refresh *oauth.Token) error {
	if refresh.UserHandle != access.UserHandle {
		return errors.New("access and refresh token belong to different users")
	}
	uid, err := oauthUserID(ctx, tx, refresh.UserHandle)
	if err != nil {
		return err
	}
	refresh.FamilyID = family
	if err := oauthInsertToken(ctx, tx, uid, refresh); err != nil {
		return err
	}
	if family == 0 {
		refresh.FamilyID = refresh.ID
		if _, err := tx.ExecContext(ctx, `UPDATE oauth_token SET family_id = id WHERE id = ?`, refresh.ID); err != nil {
			return err
		}
	}
	access.FamilyID = refresh.FamilyID
	return oauthInsertToken(ctx, tx, uid, access)
}

// CreateTokens stores a new access and refresh pair starting a new family.
func (s *OAuthStore) CreateTokens(ctx context.Context, access, refresh *oauth.Token) error {
	err := inTx(ctx, s.db, func(tx *sql.Tx) error {
		return oauthInsertPair(ctx, tx, 0, access, refresh)
	})
	if err != nil {
		return fmt.Errorf("sqlite: create oauth tokens: %w", err)
	}
	return nil
}

const oauthTokenColumns = `t.id, t.token_hash, t.kind, t.client_id, u.handle, t.scopes, t.resource,
	t.created_at, t.expires_at, t.revoked_at, t.replaced_by, t.family_id`

func oauthScanToken(s interface{ Scan(dest ...any) error }, t *oauth.Token) error {
	var (
		scopes           string
		created, expires int64
		revoked, repl    sql.NullInt64
	)
	if err := s.Scan(&t.ID, &t.Hash, &t.Kind, &t.ClientID, &t.UserHandle, &scopes, &t.Resource,
		&created, &expires, &revoked, &repl, &t.FamilyID); err != nil {
		return err
	}
	t.Scopes = oauthSplitScopes(scopes)
	t.CreatedAt = fromNanos(created)
	t.ExpiresAt = fromNanos(expires)
	t.RevokedAt = ptrTime(revoked)
	if repl.Valid {
		t.ReplacedBy = &repl.Int64
	}
	return nil
}

// GetToken returns a token by hash, revoked or expired included, or
// oauth.ErrNotFound.
func (s *OAuthStore) GetToken(ctx context.Context, hash string) (*oauth.Token, error) {
	var t oauth.Token
	row := s.db.QueryRowContext(ctx, `SELECT `+oauthTokenColumns+`
		FROM oauth_token t JOIN user u ON u.id = t.user_id WHERE t.token_hash = ?`, hash)
	if err := oauthScanToken(row, &t); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = oauth.ErrNotFound
		}
		return nil, fmt.Errorf("sqlite: get oauth token: %w", err)
	}
	return &t, nil
}

// Rotate revokes the live refresh token oldID, stores the new pair in its
// family and points oldID's replaced_by at the new refresh token.
func (s *OAuthStore) Rotate(ctx context.Context, oldID int64, at time.Time, access, refresh *oauth.Token) error {
	err := inTx(ctx, s.db, func(tx *sql.Tx) error {
		var family int64
		err := tx.QueryRowContext(ctx, `SELECT family_id FROM oauth_token
			WHERE id = ? AND kind = 'refresh' AND revoked_at IS NULL AND replaced_by IS NULL`, oldID).Scan(&family)
		if errors.Is(err, sql.ErrNoRows) {
			return oauth.ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := oauthInsertPair(ctx, tx, family, access, refresh); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE oauth_token SET revoked_at = ?, replaced_by = ?
			WHERE id = ? AND revoked_at IS NULL AND replaced_by IS NULL`, toNanos(at), refresh.ID, oldID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			if err == nil {
				err = oauth.ErrNotFound
			}
			return err
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("sqlite: rotate oauth token %d: %w", oldID, err)
	}
	return nil
}

// RevokeFamily revokes every live token of a family.
func (s *OAuthStore) RevokeFamily(ctx context.Context, familyID int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE oauth_token SET revoked_at = ? WHERE family_id = ? AND revoked_at IS NULL`, toNanos(at), familyID)
	if err != nil {
		return fmt.Errorf("sqlite: revoke oauth family %d: %w", familyID, err)
	}
	return nil
}

// ListGrants returns the user's live refresh tokens at now, newest first.
func (s *OAuthStore) ListGrants(ctx context.Context, handle string, now time.Time) ([]oauth.Token, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+oauthTokenColumns+`
		FROM oauth_token t JOIN user u ON u.id = t.user_id
		WHERE u.handle = ? AND t.kind = 'refresh' AND t.revoked_at IS NULL AND t.expires_at > ?
		ORDER BY t.created_at DESC, t.id DESC`, handle, toNanos(now))
	if err != nil {
		return nil, fmt.Errorf("sqlite: list oauth grants: %w", err)
	}
	defer rows.Close()
	var out []oauth.Token
	for rows.Next() {
		var t oauth.Token
		if err := oauthScanToken(rows, &t); err != nil {
			return nil, fmt.Errorf("sqlite: list oauth grants: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list oauth grants: %w", err)
	}
	return out, nil
}

// RevokeGrant revokes the family of refresh token id, or returns
// oauth.ErrNotFound when id is not a refresh token.
func (s *OAuthStore) RevokeGrant(ctx context.Context, id int64, at time.Time) error {
	err := inTx(ctx, s.db, func(tx *sql.Tx) error {
		var family int64
		err := tx.QueryRowContext(ctx,
			`SELECT family_id FROM oauth_token WHERE id = ? AND kind = 'refresh'`, id).Scan(&family)
		if errors.Is(err, sql.ErrNoRows) {
			return oauth.ErrNotFound
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE oauth_token SET revoked_at = ? WHERE family_id = ? AND revoked_at IS NULL`, toNanos(at), family)
		return err
	})
	if err != nil {
		return fmt.Errorf("sqlite: revoke oauth grant %d: %w", id, err)
	}
	return nil
}

// RevokeAllForUser revokes every live token of the user.
func (s *OAuthStore) RevokeAllForUser(ctx context.Context, handle string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE oauth_token SET revoked_at = ?
		WHERE revoked_at IS NULL AND user_id = (SELECT id FROM user WHERE handle = ?)`, toNanos(at), handle)
	if err != nil {
		return fmt.Errorf("sqlite: revoke oauth tokens of %q: %w", handle, err)
	}
	return nil
}

// DeleteExpired removes requests, codes and tokens whose expiry is at or
// before now, and returns how many rows went.
func (s *OAuthStore) DeleteExpired(ctx context.Context, now time.Time) (int, error) {
	var total int64
	err := inTx(ctx, s.db, func(tx *sql.Tx) error {
		for _, table := range []string{"oauth_request", "oauth_code", "oauth_token"} {
			res, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE expires_at <= ?`, toNanos(now))
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			total += n
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("sqlite: delete expired oauth rows: %w", err)
	}
	return int(total), nil
}
