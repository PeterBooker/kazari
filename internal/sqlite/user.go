package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ncruces/go-sqlite3"

	"github.com/PeterBooker/kazari/internal/user"
)

// UserStore implements user.Store.
type UserStore struct {
	db *DB
}

var _ user.Store = (*UserStore)(nil)

// NewUserStore returns a user store backed by db.
func NewUserStore(db *DB) *UserStore {
	return &UserStore{db: db}
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

func (s *UserStore) CreateUser(ctx context.Context, u *user.User) (err error) {
	ctx, end := s.db.op(ctx, "UserStore.CreateUser")
	defer func() { end(err) }()
	if err := userInsert(ctx, s.db, u); err != nil {
		return fmt.Errorf("sqlite: create user %q: %w", u.Handle, err)
	}
	return nil
}

// userExecer is a *sql.DB or a *sql.Tx.
type userExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func userInsert(ctx context.Context, q userExecer, u *user.User) error {
	res, err := q.ExecContext(ctx, `INSERT INTO user
		(handle, email, display_name, role, quota_bytes, disabled_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		u.Handle, userNullEmail(u.Email), u.DisplayName, string(u.Role), u.QuotaBytes,
		nullNanos(u.DisabledAt), toNanos(u.CreatedAt))
	if err != nil {
		return userUniqueErr(err)
	}
	u.ID, err = res.LastInsertId()
	return err
}

func (s *UserStore) GetUser(ctx context.Context, handle string) (_ *user.User, err error) {
	ctx, end := s.db.op(ctx, "UserStore.GetUser")
	defer func() { end(err) }()
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

func (s *UserStore) GetUserByEmail(ctx context.Context, email string) (_ *user.User, err error) {
	ctx, end := s.db.op(ctx, "UserStore.GetUserByEmail")
	defer func() { end(err) }()
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

func (s *UserStore) ListUsers(ctx context.Context) (_ []user.User, err error) {
	ctx, end := s.db.op(ctx, "UserStore.ListUsers")
	defer func() { end(err) }()
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

func (s *UserStore) UpdateUser(ctx context.Context, handle string, fn func(u *user.User) error) (_ *user.User, err error) {
	ctx, end := s.db.op(ctx, "UserStore.UpdateUser")
	defer func() { end(err) }()
	var u user.User
	var fnErr error
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, `SELECT `+userColumns+` FROM user WHERE handle = ?`, handle)
		if err := userScan(row, &u); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return user.ErrNotFound
			}
			return err
		}
		wasAdmin := u.Role == user.RoleAdmin && u.DisabledAt == nil
		if fnErr = fn(&u); fnErr != nil {
			return fnErr
		}
		if wasAdmin && (u.Role != user.RoleAdmin || u.DisabledAt != nil) {
			var others int
			err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM user
				WHERE role = 'admin' AND disabled_at IS NULL AND id <> ?`, u.ID).Scan(&others)
			if err != nil {
				return err
			}
			if others == 0 {
				return user.ErrLastAdmin
			}
		}
		_, err := tx.ExecContext(ctx, `UPDATE user
			SET email = ?, display_name = ?, role = ?, quota_bytes = ?, disabled_at = ?
			WHERE id = ?`,
			userNullEmail(u.Email), u.DisplayName, string(u.Role), u.QuotaBytes, nullNanos(u.DisabledAt), u.ID)
		return userUniqueErr(err)
	})
	if fnErr != nil {
		return nil, fnErr
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: update user %q: %w", handle, err)
	}
	return &u, nil
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

// userInsertKey resolves k.IssuedBy among the keys of the same user, and
// counts their live keys against maxLive inside the caller's transaction, so
// concurrent requests cannot pass the limit together.
func userInsertKey(ctx context.Context, tx *sql.Tx, k *user.APIKey, maxLive int) error {
	var (
		uid      int64
		disabled sql.NullInt64
	)
	err := tx.QueryRowContext(ctx, `SELECT id, disabled_at FROM user WHERE handle = ?`, k.UserHandle).Scan(&uid, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return user.ErrNotFound
	}
	if err != nil {
		return err
	}
	if disabled.Valid {
		return user.ErrDisabled
	}
	if maxLive > 0 {
		var live int
		err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_key
			WHERE user_id = ? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)`,
			uid, toNanos(k.CreatedAt)).Scan(&live)
		if err != nil {
			return err
		}
		if live >= maxLive {
			return fmt.Errorf("%w: %d is the limit; revoke one first", user.ErrTooManyKeys, maxLive)
		}
	}
	var issuer sql.NullInt64
	if k.IssuedBy != "" {
		// Live as of now, read here: a key revoked between the caller's
		// check and this insert must not leave a key that outlives it.
		err := tx.QueryRowContext(ctx, `SELECT id FROM api_key WHERE prefix = ?1 AND user_id = ?2
			AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?3)`,
			k.IssuedBy, uid, toNanos(k.CreatedAt)).Scan(&issuer.Int64)
		if errors.Is(err, sql.ErrNoRows) {
			return user.ErrKeyNotFound
		}
		if err != nil {
			return err
		}
		issuer.Valid = true
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO api_key
		(user_id, prefix, hash, label, scopes, slugs, issued_by, last_used_at, expires_at, revoked_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		uid, k.Prefix, k.Hash, k.Label, userJoinScopes(k.Scopes), userJoinScopes(k.Slugs), issuer,
		nullNanos(k.LastUsedAt), nullNanos(k.ExpiresAt), nullNanos(k.RevokedAt),
		toNanos(k.CreatedAt))
	if err != nil {
		return err
	}
	k.ID, err = res.LastInsertId()
	return err
}

func (s *UserStore) CreateKey(ctx context.Context, k *user.APIKey, maxLive int) (err error) {
	ctx, end := s.db.op(ctx, "UserStore.CreateKey")
	defer func() { end(err) }()
	err = inTx(ctx, s.db, func(tx *sql.Tx) error { return userInsertKey(ctx, tx, k, maxLive) })
	if err != nil {
		return fmt.Errorf("sqlite: create key for %q: %w", k.UserHandle, err)
	}
	return nil
}

func (s *UserStore) RotateKey(ctx context.Context, prefix string, k *user.APIKey, expires time.Time, maxLive int) (err error) {
	ctx, end := s.db.op(ctx, "UserStore.RotateKey")
	defer func() { end(err) }()
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE api_key
			SET expires_at = CASE WHEN expires_at IS NULL OR expires_at > ?1 THEN ?1 ELSE expires_at END
			WHERE prefix = ?2 AND user_id = (SELECT id FROM user WHERE handle = ?3)
			  AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?4)`,
			toNanos(expires), prefix, k.UserHandle, toNanos(k.CreatedAt))
		if err == nil {
			err = userOneRow(res, user.ErrKeyNotFound)
		}
		if err != nil {
			return err
		}
		// The key being replaced is still live for its overlap and is not
		// a key more.
		if maxLive > 0 && expires.After(k.CreatedAt) {
			maxLive++
		}
		return userInsertKey(ctx, tx, k, maxLive)
	})
	if err != nil {
		return fmt.Errorf("sqlite: rotate key %q of %q: %w", prefix, k.UserHandle, err)
	}
	return nil
}

const userKeyColumns = `api_key.id, user.handle, api_key.prefix, api_key.hash, api_key.label,
	api_key.scopes, api_key.slugs,
	coalesce((SELECT i.prefix FROM api_key i WHERE i.id = api_key.issued_by), ''),
	api_key.last_used_at, api_key.expires_at, api_key.revoked_at, api_key.created_at`

func userScanKey(s userRowScanner, k *user.APIKey, extra ...any) error {
	var (
		scopes, slugs            string
		lastUsed, expires, revok sql.NullInt64
		created                  int64
	)
	dest := []any{&k.ID, &k.UserHandle, &k.Prefix, &k.Hash, &k.Label,
		&scopes, &slugs, &k.IssuedBy, &lastUsed, &expires, &revok, &created}
	if err := s.Scan(append(dest, extra...)...); err != nil {
		return err
	}
	k.Scopes = userSplitScopes(scopes)
	if slugs != "" {
		k.Slugs = userSplitScopes(slugs)
	}
	k.LastUsedAt = ptrTime(lastUsed)
	k.ExpiresAt = ptrTime(expires)
	k.RevokedAt = ptrTime(revok)
	k.CreatedAt = fromNanos(created)
	return nil
}

func (s *UserStore) GetKey(ctx context.Context, prefix string) (_ *user.APIKey, _ *user.User, err error) {
	ctx, end := s.db.op(ctx, "UserStore.GetKey")
	defer func() { end(err) }()
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
	err = userScanKey(row, &k, &u.ID, &email, &u.DisplayName, &role, &u.QuotaBytes, &disabled, &created)
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

func (s *UserStore) ListKeys(ctx context.Context, handle string) (_ []user.APIKey, err error) {
	ctx, end := s.db.op(ctx, "UserStore.ListKeys")
	defer func() { end(err) }()
	var keys []user.APIKey
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
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

// RevokeKey walks issued_by down from the key: a key issued with a revoked
// key is revoked with it, however many keys lie between.
func (s *UserStore) RevokeKey(ctx context.Context, handle, prefix string, at time.Time) (_ []string, err error) {
	ctx, end := s.db.op(ctx, "UserStore.RevokeKey")
	defer func() { end(err) }()
	var issued []string
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
		var id int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM api_key
			WHERE prefix = ? AND user_id = (SELECT id FROM user WHERE handle = ?)`, prefix, handle).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return user.ErrKeyNotFound
		}
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `WITH RECURSIVE tree(id) AS (
				SELECT ?1
				UNION
				SELECT k.id FROM api_key k JOIN tree ON k.issued_by = tree.id
			)
			UPDATE api_key SET revoked_at = ?2
			WHERE id IN (SELECT id FROM tree) AND id <> ?1 AND revoked_at IS NULL
			RETURNING prefix`, id, toNanos(at))
		if err != nil {
			return err
		}
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				rows.Close()
				return err
			}
			issued = append(issued, p)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE api_key SET revoked_at = coalesce(revoked_at, ?) WHERE id = ?`, toNanos(at), id)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("sqlite: revoke key %q of %q: %w", prefix, handle, err)
	}
	slices.Sort(issued)
	return issued, nil
}

func (s *UserStore) RevokeAllKeys(ctx context.Context, handle string, at time.Time) (err error) {
	ctx, end := s.db.op(ctx, "UserStore.RevokeAllKeys")
	defer func() { end(err) }()
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
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

func (s *UserStore) TouchKey(ctx context.Context, prefix string, at time.Time) (err error) {
	ctx, end := s.db.op(ctx, "UserStore.TouchKey")
	defer func() { end(err) }()
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

func (s *UserStore) GetIdentity(ctx context.Context, issuer, subject string) (_ *user.User, err error) {
	ctx, end := s.db.op(ctx, "UserStore.GetIdentity")
	defer func() { end(err) }()
	var u user.User
	row := s.db.QueryRowContext(ctx, `SELECT `+userColumns+`
		FROM user_identity JOIN user ON user.id = user_identity.user_id
		WHERE user_identity.issuer = ? AND user_identity.subject = ?`, issuer, subject)
	if err := userScan(row, &u); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = user.ErrNotFound
		}
		return nil, fmt.Errorf("sqlite: get identity %q %q: %w", issuer, subject, err)
	}
	return &u, nil
}

func (s *UserStore) LinkIdentity(ctx context.Context, handle string, id user.Identity) (err error) {
	ctx, end := s.db.op(ctx, "UserStore.LinkIdentity")
	defer func() { end(err) }()
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
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
			uid, id.Issuer, id.Subject, id.EmailAtLogin, identityLogin(id.LastLoginAt))
		return err
	})
	if err != nil {
		return fmt.Errorf("sqlite: link identity %q %q to %q: %w", id.Issuer, id.Subject, handle, err)
	}
	return nil
}

// identityLogin stores a sign-in time, with 0 for the zero time of a link
// made by hand: the column takes no NULL.
func identityLogin(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return toNanos(t)
}

func (s *UserStore) UnlinkIdentity(ctx context.Context, handle, issuer, subject string) (err error) {
	ctx, end := s.db.op(ctx, "UserStore.UnlinkIdentity")
	defer func() { end(err) }()
	res, err := s.db.ExecContext(ctx, `DELETE FROM user_identity
		WHERE issuer = ? AND subject = ? AND user_id = (SELECT id FROM user WHERE handle = ?)`,
		issuer, subject, handle)
	if err == nil {
		err = userOneRow(res, user.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("sqlite: unlink identity %q %q from %q: %w", issuer, subject, handle, err)
	}
	return nil
}

func (s *UserStore) ListIdentities(ctx context.Context, handle string) (_ []user.Identity, err error) {
	ctx, end := s.db.op(ctx, "UserStore.ListIdentities")
	defer func() { end(err) }()
	var ids []user.Identity
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
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
			if last != 0 {
				id.LastLoginAt = fromNanos(last)
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("sqlite: list identities of %q: %w", handle, err)
	}
	return ids, nil
}

func (s *UserStore) CountUsers(ctx context.Context) (_ []user.Count, err error) {
	ctx, end := s.db.op(ctx, "UserStore.CountUsers")
	defer func() { end(err) }()
	rows, err := s.db.QueryContext(ctx, `SELECT role, disabled_at IS NOT NULL, COUNT(*) FROM user GROUP BY 1, 2`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: count users: %w", err)
	}
	defer rows.Close()
	var out []user.Count
	for rows.Next() {
		var c user.Count
		if err := rows.Scan(&c.Role, &c.Disabled, &c.Count); err != nil {
			return nil, fmt.Errorf("sqlite: count users: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: count users: %w", err)
	}
	return out, nil
}
