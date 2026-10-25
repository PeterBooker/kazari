package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"kazari/internal/oauth"
)

// The browser sign-in half of oauth.Store: pending sign-ins, sessions,
// view codes and view sessions.

// CreateSignIn implements oauth.Store.
func (s *OAuthStore) CreateSignIn(ctx context.Context, r *oauth.SignInRequest) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO signin_request
		(id, binding, nonce, upstream_verifier, host, path, nonce_hash, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Binding, r.Nonce, r.UpstreamVerifier, r.Host, r.Path, r.NonceHash, toNanos(r.ExpiresAt))
	if err != nil {
		return fmt.Errorf("sqlite: create sign-in: %w", err)
	}
	return nil
}

// GetSignIn implements oauth.Store.
func (s *OAuthStore) GetSignIn(ctx context.Context, id string) (*oauth.SignInRequest, error) {
	r := oauth.SignInRequest{ID: id}
	var expires int64
	err := s.db.QueryRowContext(ctx, `SELECT binding, nonce, upstream_verifier, host, path, nonce_hash, expires_at
		FROM signin_request WHERE id = ?`, id).Scan(
		&r.Binding, &r.Nonce, &r.UpstreamVerifier, &r.Host, &r.Path, &r.NonceHash, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		err = oauth.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get sign-in: %w", err)
	}
	r.ExpiresAt = fromNanos(expires)
	return &r, nil
}

// DeleteSignIn implements oauth.Store.
func (s *OAuthStore) DeleteSignIn(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM signin_request WHERE id = ?`, id)
	if err == nil {
		err = oauthAffected(res)
	}
	if err != nil {
		return fmt.Errorf("sqlite: delete sign-in: %w", err)
	}
	return nil
}

// oauthAffected maps a statement that changed no row to oauth.ErrNotFound.
func oauthAffected(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return oauth.ErrNotFound
	}
	return nil
}

// CreateSession implements oauth.Store.
func (s *OAuthStore) CreateSession(ctx context.Context, sess *oauth.Session) error {
	err := inTx(ctx, s.db, func(tx *sql.Tx) error {
		uid, err := oauthUserID(ctx, tx, sess.UserHandle)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO signin_session (token_hash, user_id, key_prefix, created_at, expires_at)
			VALUES (?, ?, ?, ?, ?)`, sess.Hash, uid, sess.KeyPrefix, toNanos(sess.CreatedAt), toNanos(sess.ExpiresAt))
		if err != nil {
			return err
		}
		sess.ID, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return fmt.Errorf("sqlite: create session: %w", err)
	}
	return nil
}

const sessionSelect = `SELECT s.id, s.token_hash, u.handle, s.key_prefix, s.created_at, s.expires_at
	FROM signin_session s JOIN user u ON u.id = s.user_id`

func (s *OAuthStore) getSession(ctx context.Context, where string, arg any) (*oauth.Session, error) {
	var (
		sess             oauth.Session
		created, expires int64
	)
	err := s.db.QueryRowContext(ctx, sessionSelect+" WHERE "+where, arg).Scan(
		&sess.ID, &sess.Hash, &sess.UserHandle, &sess.KeyPrefix, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		err = oauth.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get session: %w", err)
	}
	sess.CreatedAt = fromNanos(created)
	sess.ExpiresAt = fromNanos(expires)
	return &sess, nil
}

// GetSession implements oauth.Store.
func (s *OAuthStore) GetSession(ctx context.Context, hash string) (*oauth.Session, error) {
	return s.getSession(ctx, "s.token_hash = ?", hash)
}

// GetSessionByID implements oauth.Store.
func (s *OAuthStore) GetSessionByID(ctx context.Context, id int64) (*oauth.Session, error) {
	return s.getSession(ctx, "s.id = ?", id)
}

// DeleteSession implements oauth.Store. Its codes and view sessions go by
// cascade.
func (s *OAuthStore) DeleteSession(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM signin_session WHERE id = ?`, id); err != nil {
		return fmt.Errorf("sqlite: delete session: %w", err)
	}
	return nil
}

// CreateViewCode implements oauth.Store.
func (s *OAuthStore) CreateViewCode(ctx context.Context, c *oauth.ViewCode) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO signin_code (code_hash, session_id, host, path, nonce_hash, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)`, c.Hash, c.SessionID, c.Host, c.Path, c.NonceHash, toNanos(c.ExpiresAt))
	if err != nil {
		return fmt.Errorf("sqlite: create view code: %w", err)
	}
	return nil
}

// ConsumeViewCode implements oauth.Store.
func (s *OAuthStore) ConsumeViewCode(ctx context.Context, hash string, at time.Time) (*oauth.ViewCode, error) {
	var c *oauth.ViewCode
	err := inTx(ctx, s.db, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE signin_code SET used_at = ? WHERE code_hash = ? AND used_at IS NULL`, toNanos(at), hash)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		var (
			v       oauth.ViewCode
			expires int64
			used    sql.NullInt64
		)
		err = tx.QueryRowContext(ctx, `SELECT session_id, host, path, nonce_hash, expires_at, used_at
			FROM signin_code WHERE code_hash = ?`, hash).Scan(&v.SessionID, &v.Host, &v.Path, &v.NonceHash, &expires, &used)
		if errors.Is(err, sql.ErrNoRows) {
			return oauth.ErrNotFound
		}
		if err != nil {
			return err
		}
		v.Hash = hash
		v.ExpiresAt = fromNanos(expires)
		v.UsedAt = ptrTime(used)
		c = &v
		if n == 0 {
			return oauth.ErrCodeUsed
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("sqlite: consume view code: %w", err)
	}
	return c, nil
}

// CreateViewSession implements oauth.Store.
func (s *OAuthStore) CreateViewSession(ctx context.Context, v *oauth.ViewSession) error {
	res, err := s.db.ExecContext(ctx, `INSERT INTO view_session (token_hash, session_id, host, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?)`, v.Hash, v.SessionID, v.Host, toNanos(v.CreatedAt), toNanos(v.ExpiresAt))
	if err == nil {
		v.ID, err = res.LastInsertId()
	}
	if err != nil {
		return fmt.Errorf("sqlite: create view session: %w", err)
	}
	return nil
}

// GetViewSession implements oauth.Store.
func (s *OAuthStore) GetViewSession(ctx context.Context, hash string) (*oauth.ViewSession, error) {
	var (
		v                oauth.ViewSession
		created, expires int64
	)
	err := s.db.QueryRowContext(ctx, `SELECT v.id, v.session_id, v.host, v.created_at, v.expires_at, u.handle, s.key_prefix
		FROM view_session v
		JOIN signin_session s ON s.id = v.session_id
		JOIN user u ON u.id = s.user_id
		WHERE v.token_hash = ?`, hash).Scan(&v.ID, &v.SessionID, &v.Host, &created, &expires, &v.UserHandle, &v.KeyPrefix)
	if errors.Is(err, sql.ErrNoRows) {
		err = oauth.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get view session: %w", err)
	}
	v.Hash = hash
	v.CreatedAt = fromNanos(created)
	v.ExpiresAt = fromNanos(expires)
	return &v, nil
}
