package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ncruces/go-sqlite3"

	"github.com/PeterBooker/kazari/internal/user"
)

// UserStore implements user.Store.
type UserStore struct {
	db *sql.DB
}

var _ user.Store = (*UserStore)(nil)

// NewUserStore returns a user store backed by db.
func NewUserStore(db *DB) *UserStore {
	return &UserStore{db: db.DB}
}

const userColumns = `user.id, user.handle, user.email, user.display_name, user.role,
	user.quota_bytes, user.disabled_at, user.created_at`

type userRowScanner interface {
	Scan(dest ...any) error
}

func userScan(s userRowScanner, u *user.User) error {
	var (
		email    sql.NullString
		role     string
		disabled sql.NullInt64
		created  int64
	)
	if err := s.Scan(&u.ID, &u.Handle, &email, &u.DisplayName, &role,
		&u.QuotaBytes, &disabled, &created); err != nil {
		return err
	}
	u.Email = email.String
	u.Role = user.Role(role)
	u.DisabledAt = ptrTime(disabled)
	u.CreatedAt = fromNanos(created)
	return nil
}

// An unknown email is NULL so UNIQUE allows any number of users without one.
func userNullEmail(email string) sql.NullString {
	return sql.NullString{String: email, Valid: email != ""}
}

func userUniqueErr(err error) error {
	if !errors.Is(err, sqlite3.CONSTRAINT_UNIQUE) {
		return err
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "user.handle"):
		return user.ErrHandleTaken
	case strings.Contains(msg, "user.email"):
		return user.ErrEmailTaken
	}
	return err
}

func userLookupID(ctx context.Context, tx *sql.Tx, handle string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM user WHERE handle = ?`, handle).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, user.ErrNotFound
	}
	return id, err
}

func (s *UserStore) CreateUser(ctx context.Context, u *user.User) error {
	res, err := s.db.ExecContext(ctx, `INSERT INTO user
		(handle, email, display_name, role, quota_bytes, disabled_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		u.Handle, userNullEmail(u.Email), u.DisplayName, string(u.Role), u.QuotaBytes,
		nullNanos(u.DisabledAt), toNanos(u.CreatedAt))
	if err != nil {
		return fmt.Errorf("sqlite: create user %q: %w", u.Handle, userUniqueErr(err))
	}
	if u.ID, err = res.LastInsertId(); err != nil {
		return fmt.Errorf("sqlite: create user %q: %w", u.Handle, err)
	}
	return nil
}

func (s *UserStore) GetUser(ctx context.Context, handle string) (*user.User, error) {
	var u user.User
	row := s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM user WHERE handle = ?`, handle)
	if err := userScan(row, &u); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = user.ErrNotFound
		}
		return nil, fmt.Errorf("sqlite: get user %q: %w", handle, err)
	}
	return &u, nil
}

func (s *UserStore) GetUserByEmail(ctx context.Context, email string) (*user.User, error) {
	var u user.User
	row := s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM user WHERE lower(email) = lower(?)`, email)
	if err := userScan(row, &u); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = user.ErrNotFound
		}
		return nil, fmt.Errorf("sqlite: get user by email: %w", err)
	}
	return &u, nil
}

func (s *UserStore) ListUsers(ctx context.Context) ([]user.User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userColumns+` FROM user ORDER BY handle`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list users: %w", err)
	}
	defer rows.Close()
	var users []user.User
	for rows.Next() {
		var u user.User
		if err := userScan(rows, &u); err != nil {
			return nil, fmt.Errorf("sqlite: list users: %w", err)
		}
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: list users: %w", err)
	}
	return users, nil
}

func (s *UserStore) UpdateUser(ctx context.Context, u *user.User) error {
	res, err := s.db.ExecContext(ctx, `UPDATE user
		SET email = ?, display_name = ?, role = ?, quota_bytes = ?, disabled_at = ?
		WHERE handle = ?`,
		userNullEmail(u.Email), u.DisplayName, string(u.Role), u.QuotaBytes,
		nullNanos(u.DisabledAt), u.Handle)
	if err != nil {
		return fmt.Errorf("sqlite: update user %q: %w", u.Handle, userUniqueErr(err))
	}
	if err := userOneRow(res, user.ErrNotFound); err != nil {
		return fmt.Errorf("sqlite: update user %q: %w", u.Handle, err)
	}
	return nil
}

func userOneRow(res sql.Result, notFound error) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return notFound
	}
	return nil
}

func userJoinScopes(scopes []string) string { return strings.Join(scopes, " ") }

func userSplitScopes(s string) []string { return strings.Fields(s) }

func (s *UserStore) CreateKey(ctx context.Context, k *user.APIKey) error {
	err := inTx(ctx, s.db, func(tx *sql.Tx) error {
		uid, err := userLookupID(ctx, tx, k.UserHandle)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO api_key
			(user_id, prefix, hash, label, scopes, last_used_at, expires_at, revoked_at, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			uid, k.Prefix, k.Hash, k.Label, userJoinScopes(k.Scopes),
			nullNanos(k.LastUsedAt), nullNanos(k.ExpiresAt), nullNanos(k.RevokedAt),
			toNanos(k.CreatedAt))
		if err != nil {
			return err
		}
		k.ID, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return fmt.Errorf("sqlite: create key for %q: %w", k.UserHandle, err)
	}
	return nil
}

const userKeyColumns = `api_key.id, user.handle, api_key.prefix, api_key.hash, api_key.label,
	api_key.scopes, api_key.last_used_at, api_key.expires_at, api_key.revoked_at, api_key.created_at`

func userScanKey(s userRowScanner, k *user.APIKey, extra ...any) error {
	var (
		scopes                   string
		lastUsed, expires, revok sql.NullInt64
		created                  int64
	)
	dest := []any{&k.ID, &k.UserHandle, &k.Prefix, &k.Hash, &k.Label,
		&scopes, &lastUsed, &expires, &revok, &created}
	if err := s.Scan(append(dest, extra...)...); err != nil {
		return err
	}
	k.Scopes = userSplitScopes(scopes)
	k.LastUsedAt = ptrTime(lastUsed)
	k.ExpiresAt = ptrTime(expires)
	k.RevokedAt = ptrTime(revok)
	k.CreatedAt = fromNanos(created)
	return nil
}

func (s *UserStore) GetKey(ctx context.Context, prefix string) (*user.APIKey, *user.User, error) {
	var (
		k        user.APIKey
		u        user.User
		email    sql.NullString
		role     string
		disabled sql.NullInt64
		created  int64
	)
	row := s.db.QueryRowContext(ctx, `SELECT `+userKeyColumns+`,
		user.id, user.email, user.display_name, user.role, user.quota_bytes,
		user.disabled_at, user.created_at
		FROM api_key JOIN user ON user.id = api_key.user_id
		WHERE api_key.prefix = ?`, prefix)
	err := userScanKey(row, &k, &u.ID, &email, &u.DisplayName, &role, &u.QuotaBytes, &disabled, &created)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = user.ErrKeyNotFound
		}
		return nil, nil, fmt.Errorf("sqlite: get key %q: %w", prefix, err)
	}
	u.Handle = k.UserHandle
	u.Email = email.String
	u.Role = user.Role(role)
	u.DisabledAt = ptrTime(disabled)
	u.CreatedAt = fromNanos(created)
	return &k, &u, nil
}

func (s *UserStore) ListKeys(ctx context.Context, handle string) ([]user.APIKey, error) {
	var keys []user.APIKey
	err := inTx(ctx, s.db, func(tx *sql.Tx) error {
		uid, err := userLookupID(ctx, tx, handle)
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT `+userKeyColumns+`
			FROM api_key JOIN user ON user.id = api_key.user_id
			WHERE api_key.user_id = ?
			ORDER BY api_key.created_at DESC, api_key.id DESC`, uid)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k user.APIKey
			if err := userScanKey(rows, &k); err != nil {
				return err
			}
			keys = append(keys, k)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("sqlite: list keys of %q: %w", handle, err)
	}
	return keys, nil
}

func (s *UserStore) RevokeKey(ctx context.Context, handle, prefix string, at time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE api_key
		SET revoked_at = coalesce(revoked_at, ?)
		WHERE prefix = ? AND user_id = (SELECT id FROM user WHERE handle = ?)`,
		toNanos(at), prefix, handle)
	if err == nil {
		err = userOneRow(res, user.ErrKeyNotFound)
	}
	if err != nil {
		return fmt.Errorf("sqlite: revoke key %q of %q: %w", prefix, handle, err)
	}
	return nil
}

func (s *UserStore) RevokeAllKeys(ctx context.Context, handle string, at time.Time) error {
	err := inTx(ctx, s.db, func(tx *sql.Tx) error {
		uid, err := userLookupID(ctx, tx, handle)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE api_key SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL`,
			toNanos(at), uid)
		return err
	})
	if err != nil {
		return fmt.Errorf("sqlite: revoke keys of %q: %w", handle, err)
	}
	return nil
}

func (s *UserStore) TouchKey(ctx context.Context, prefix string, at time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE api_key SET last_used_at = ? WHERE prefix = ?`, toNanos(at), prefix)
	if err == nil {
		err = userOneRow(res, user.ErrKeyNotFound)
	}
	if err != nil {
		return fmt.Errorf("sqlite: touch key %q: %w", prefix, err)
	}
	return nil
}

func (s *UserStore) GetIdentity(ctx context.Context, issuer, subject string) (*user.User, error) {
	var u user.User
	row := s.db.QueryRowContext(ctx, `SELECT `+userColumns+`
		FROM user_identity JOIN user ON user.id = user_identity.user_id
		WHERE user_identity.issuer = ? AND user_identity.subject = ?`, issuer, subject)
	if err := userScan(row, &u); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = user.ErrNotFound
		}
		return nil, fmt.Errorf("sqlite: get identity %s %s: %w", issuer, subject, err)
	}
	return &u, nil
}

func (s *UserStore) LinkIdentity(ctx context.Context, handle string, id user.Identity) error {
	err := inTx(ctx, s.db, func(tx *sql.Tx) error {
		uid, err := userLookupID(ctx, tx, handle)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO user_identity
			(user_id, issuer, subject, email_at_login, last_login_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (issuer, subject) DO UPDATE SET
				user_id = excluded.user_id,
				email_at_login = excluded.email_at_login,
				last_login_at = excluded.last_login_at`,
			uid, id.Issuer, id.Subject, id.EmailAtLogin, toNanos(id.LastLoginAt))
		return err
	})
	if err != nil {
		return fmt.Errorf("sqlite: link identity %s %s to %q: %w", id.Issuer, id.Subject, handle, err)
	}
	return nil
}

func (s *UserStore) UnlinkIdentity(ctx context.Context, handle, issuer, subject string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM user_identity
		WHERE issuer = ? AND subject = ? AND user_id = (SELECT id FROM user WHERE handle = ?)`,
		issuer, subject, handle)
	if err == nil {
		err = userOneRow(res, user.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("sqlite: unlink identity %s %s from %q: %w", issuer, subject, handle, err)
	}
	return nil
}

func (s *UserStore) ListIdentities(ctx context.Context, handle string) ([]user.Identity, error) {
	var ids []user.Identity
	err := inTx(ctx, s.db, func(tx *sql.Tx) error {
		uid, err := userLookupID(ctx, tx, handle)
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT issuer, subject, email_at_login, last_login_at
			FROM user_identity WHERE user_id = ? ORDER BY issuer, subject`, uid)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				id   user.Identity
				last int64
			)
			if err := rows.Scan(&id.Issuer, &id.Subject, &id.EmailAtLogin, &last); err != nil {
				return err
			}
			id.LastLoginAt = fromNanos(last)
			ids = append(ids, id)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("sqlite: list identities of %q: %w", handle, err)
	}
	return ids, nil
}
