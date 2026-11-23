package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/PeterBooker/kazari/internal/signin"
)

// SignInStore implements signin.Store: pending sign-ins, sessions, view
// codes and view sessions.
type SignInStore struct {
	db *DB
}

var _ signin.Store = (*SignInStore)(nil)

// NewSignInStore returns a sign-in store backed by db.
func NewSignInStore(db *DB) *SignInStore {
	return &SignInStore{db: db}
}

// CreateSignIn implements signin.Store.
func (s *SignInStore) CreateSignIn(ctx context.Context, r *signin.Request) (err error) {
	ctx, end := s.db.op(ctx, "SignInStore.CreateSignIn")
	defer func() { end(err) }()
	_, err = s.db.ExecContext(ctx, `INSERT INTO signin_request
		(id, binding, nonce, upstream_verifier, host, path, nonce_hash, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Binding, r.Nonce, r.UpstreamVerifier, r.Host, r.Path, r.NonceHash, toNanos(r.ExpiresAt))
	if err != nil {
		return fmt.Errorf("sqlite: create sign-in: %w", err)
	}
	return nil
}

// GetSignIn implements signin.Store.
func (s *SignInStore) GetSignIn(ctx context.Context, id string) (_ *signin.Request, err error) {
	ctx, end := s.db.op(ctx, "SignInStore.GetSignIn")
	defer func() { end(err) }()
	r := signin.Request{ID: id}
	var expires int64
	err = s.db.QueryRowContext(ctx, `SELECT binding, nonce, upstream_verifier, host, path, nonce_hash, expires_at
		FROM signin_request WHERE id = ?`, id).Scan(
		&r.Binding, &r.Nonce, &r.UpstreamVerifier, &r.Host, &r.Path, &r.NonceHash, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		err = signin.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get sign-in: %w", err)
	}
	r.ExpiresAt = fromNanos(expires)
	return &r, nil
}

// DeleteSignIn implements signin.Store.
func (s *SignInStore) DeleteSignIn(ctx context.Context, id string) (err error) {
	ctx, end := s.db.op(ctx, "SignInStore.DeleteSignIn")
	defer func() { end(err) }()
	res, err := s.db.ExecContext(ctx, `DELETE FROM signin_request WHERE id = ?`, id)
	if err == nil {
		err = signInAffected(res)
	}
	if err != nil {
		return fmt.Errorf("sqlite: delete sign-in: %w", err)
	}
	return nil
}

// signInAffected maps a statement that changed no row to signin.ErrNotFound.
func signInAffected(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return signin.ErrNotFound
	}
	return nil
}

// CreateSession implements signin.Store.
func (s *SignInStore) CreateSession(ctx context.Context, sess *signin.Session) (err error) {
	ctx, end := s.db.op(ctx, "SignInStore.CreateSession")
	defer func() { end(err) }()
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
		uid, err := oauthUserID(ctx, tx, sess.UserHandle)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO signin_session (token_hash, browser, user_id, key_prefix, created_at, expires_at)
			VALUES (?, ?, ?, ?, ?, ?)`, sess.Hash, sess.Browser, uid, sess.KeyPrefix, toNanos(sess.CreatedAt), toNanos(sess.ExpiresAt))
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

const sessionSelect = `SELECT s.id, s.token_hash, s.browser, u.handle, s.key_prefix, s.created_at, s.expires_at,
	u.disabled_at IS NOT NULL
	FROM signin_session s JOIN user u ON u.id = s.user_id`

func (s *SignInStore) getSession(ctx context.Context, where string, arg any) (*signin.Session, error) {
	var (
		sess             signin.Session
		created, expires int64
	)
	err := s.db.QueryRowContext(ctx, sessionSelect+" WHERE "+where, arg).Scan(
		&sess.ID, &sess.Hash, &sess.Browser, &sess.UserHandle, &sess.KeyPrefix, &created, &expires, &sess.UserDisabled)
	if errors.Is(err, sql.ErrNoRows) {
		err = signin.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get session: %w", err)
	}
	sess.CreatedAt = fromNanos(created)
	sess.ExpiresAt = fromNanos(expires)
	return &sess, nil
}

// GetSession implements signin.Store.
func (s *SignInStore) GetSession(ctx context.Context, hash string) (_ *signin.Session, err error) {
	ctx, end := s.db.op(ctx, "SignInStore.GetSession")
	defer func() { end(err) }()
	return s.getSession(ctx, "s.token_hash = ?", hash)
}

// GetSessionByID implements signin.Store.
func (s *SignInStore) GetSessionByID(ctx context.Context, id int64) (_ *signin.Session, err error) {
	ctx, end := s.db.op(ctx, "SignInStore.GetSessionByID")
	defer func() { end(err) }()
	return s.getSession(ctx, "s.id = ?", id)
}

// DeleteSession implements signin.Store. The codes and view sessions go by
// cascade.
func (s *SignInStore) DeleteSession(ctx context.Context, id int64) (err error) {
	ctx, end := s.db.op(ctx, "SignInStore.DeleteSession")
	defer func() { end(err) }()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM signin_session WHERE id = ?
		OR browser IN (SELECT browser FROM signin_session WHERE id = ? AND browser != '')`, id, id); err != nil {
		return fmt.Errorf("sqlite: delete session: %w", err)
	}
	return nil
}

// DeleteUserSessions implements signin.Store. The codes and view sessions go
// by cascade.
func (s *SignInStore) DeleteUserSessions(ctx context.Context, handle string, keep int64) (_ int, err error) {
	ctx, end := s.db.op(ctx, "SignInStore.DeleteUserSessions")
	defer func() { end(err) }()
	res, err := s.db.ExecContext(ctx, `DELETE FROM signin_session
		WHERE user_id = (SELECT id FROM user WHERE handle = ?1) AND id <> ?2
		  AND (browser = '' OR browser NOT IN (SELECT browser FROM signin_session WHERE id = ?2))`, handle, keep)
	if err != nil {
		return 0, fmt.Errorf("sqlite: delete sessions of %q: %w", handle, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sqlite: delete sessions of %q: %w", handle, err)
	}
	return int(n), nil
}

// CountSessions implements signin.Store.
func (s *SignInStore) CountSessions(ctx context.Context, handle string, now time.Time) (_ int, err error) {
	ctx, end := s.db.op(ctx, "SignInStore.CountSessions")
	defer func() { end(err) }()
	var n int
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM signin_session
		WHERE user_id = (SELECT id FROM user WHERE handle = ?) AND expires_at > ?`, handle, toNanos(now)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("sqlite: count sessions of %q: %w", handle, err)
	}
	return n, nil
}

// CreateViewCode implements signin.Store.
func (s *SignInStore) CreateViewCode(ctx context.Context, c *signin.ViewCode) (err error) {
	ctx, end := s.db.op(ctx, "SignInStore.CreateViewCode")
	defer func() { end(err) }()
	_, err = s.db.ExecContext(ctx, `INSERT INTO signin_code (code_hash, session_id, host, path, nonce_hash, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)`, c.Hash, c.SessionID, c.Host, c.Path, c.NonceHash, toNanos(c.ExpiresAt))
	if err != nil {
		return fmt.Errorf("sqlite: create view code: %w", err)
	}
	return nil
}

// ConsumeViewCode implements signin.Store.
func (s *SignInStore) ConsumeViewCode(ctx context.Context, hash string, at time.Time) (_ *signin.ViewCode, err error) {
	ctx, end := s.db.op(ctx, "SignInStore.ConsumeViewCode")
	defer func() { end(err) }()
	var c *signin.ViewCode
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
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
			v       signin.ViewCode
			expires int64
			used    sql.NullInt64
		)
		err = tx.QueryRowContext(ctx, `SELECT session_id, host, path, nonce_hash, expires_at, used_at
			FROM signin_code WHERE code_hash = ?`, hash).Scan(&v.SessionID, &v.Host, &v.Path, &v.NonceHash, &expires, &used)
		if errors.Is(err, sql.ErrNoRows) {
			return signin.ErrNotFound
		}
		if err != nil {
			return err
		}
		v.Hash = hash
		v.ExpiresAt = fromNanos(expires)
		v.UsedAt = ptrTime(used)
		c = &v
		if n == 0 {
			return signin.ErrCodeUsed
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("sqlite: consume view code: %w", err)
	}
	return c, nil
}

// CreateViewSession implements signin.Store.
func (s *SignInStore) CreateViewSession(ctx context.Context, v *signin.ViewSession) (err error) {
	ctx, end := s.db.op(ctx, "SignInStore.CreateViewSession")
	defer func() { end(err) }()
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

// DeleteViewSession implements signin.Store.
func (s *SignInStore) DeleteViewSession(ctx context.Context, hash string) (err error) {
	ctx, end := s.db.op(ctx, "SignInStore.DeleteViewSession")
	defer func() { end(err) }()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM view_session WHERE token_hash = ?`, hash); err != nil {
		return fmt.Errorf("sqlite: delete view session: %w", err)
	}
	return nil
}

// GetViewSession implements signin.Store.
func (s *SignInStore) GetViewSession(ctx context.Context, hash string) (_ *signin.ViewSession, err error) {
	ctx, end := s.db.op(ctx, "SignInStore.GetViewSession")
	defer func() { end(err) }()
	var (
		v                signin.ViewSession
		created, expires int64
	)
	err = s.db.QueryRowContext(ctx, `SELECT v.id, v.session_id, v.host, v.created_at, v.expires_at, u.handle, s.key_prefix,
		u.disabled_at IS NOT NULL
		FROM view_session v
		JOIN signin_session s ON s.id = v.session_id
		JOIN user u ON u.id = s.user_id
		WHERE v.token_hash = ?`, hash).Scan(&v.ID, &v.SessionID, &v.Host, &created, &expires, &v.UserHandle, &v.KeyPrefix, &v.UserDisabled)
	if errors.Is(err, sql.ErrNoRows) {
		err = signin.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get view session: %w", err)
	}
	v.Hash = hash
	v.CreatedAt = fromNanos(created)
	v.ExpiresAt = fromNanos(expires)
	return &v, nil
}

// DeleteExpired implements signin.Store.
func (s *SignInStore) DeleteExpired(ctx context.Context, now time.Time) (_ int, err error) {
	ctx, end := s.db.op(ctx, "SignInStore.DeleteExpired")
	defer func() { end(err) }()
	var total int64
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
		// Codes and view sessions before their sessions, so a row that
		// expired is counted rather than removed by the cascade.
		for _, table := range []string{"signin_request", "signin_code", "view_session", "signin_session"} {
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
		return 0, fmt.Errorf("sqlite: delete expired sign-in rows: %w", err)
	}
	return int(total), nil
}
