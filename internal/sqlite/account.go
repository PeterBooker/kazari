package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/PeterBooker/kazari/internal/user"
)

// The built-in accounts half of user.Store: passwords and the links that
// set them.

// PasswordHash implements user.Store.
func (s *UserStore) PasswordHash(ctx context.Context, handle string) (_ string, err error) {
	ctx, end := s.db.op(ctx, "UserStore.PasswordHash")
	defer func() { end(err) }()
	var hash string
	err = s.db.QueryRowContext(ctx, `SELECT p.hash FROM user_password p JOIN user u ON u.id = p.user_id
		WHERE u.handle = ?`, handle).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("sqlite: password of %q: %w", handle, err)
	}
	return hash, nil
}

// SetPassword implements user.Store.
func (s *UserStore) SetPassword(ctx context.Context, handle, hash string, at time.Time) (err error) {
	ctx, end := s.db.op(ctx, "UserStore.SetPassword")
	defer func() { end(err) }()
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
		uid, err := userLookupID(ctx, tx, handle)
		if err != nil {
			return err
		}
		return userSetPassword(ctx, tx, uid, hash, at)
	})
	if err != nil {
		return fmt.Errorf("sqlite: set password of %q: %w", handle, err)
	}
	return nil
}

func userSetPassword(ctx context.Context, tx *sql.Tx, uid int64, hash string, at time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO user_password (user_id, hash, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (user_id) DO UPDATE SET hash = excluded.hash, updated_at = excluded.updated_at`,
		uid, hash, toNanos(at))
	return err
}

// TouchPassword implements user.Store.
func (s *UserStore) TouchPassword(ctx context.Context, handle string, at time.Time) (err error) {
	ctx, end := s.db.op(ctx, "UserStore.TouchPassword")
	defer func() { end(err) }()
	_, err = s.db.ExecContext(ctx, `UPDATE user_password SET last_login_at = ?
		WHERE user_id = (SELECT id FROM user WHERE handle = ?)`, toNanos(at), handle)
	if err != nil {
		return fmt.Errorf("sqlite: touch password of %q: %w", handle, err)
	}
	return nil
}

// CreateLink implements user.Store.
func (s *UserStore) CreateLink(ctx context.Context, l *user.Link) (err error) {
	ctx, end := s.db.op(ctx, "UserStore.CreateLink")
	defer func() { end(err) }()
	err = inTx(ctx, s.db, func(tx *sql.Tx) error { return userInsertLink(ctx, tx, l) })
	if err != nil {
		return fmt.Errorf("sqlite: create %s link: %w", l.Purpose, err)
	}
	return nil
}

// CreateInvited implements user.Store.
func (s *UserStore) CreateInvited(ctx context.Context, u *user.User, l *user.Link) (err error) {
	ctx, end := s.db.op(ctx, "UserStore.CreateInvited")
	defer func() { end(err) }()
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
		if err := userInsert(ctx, tx, u); err != nil {
			return err
		}
		return userInsertLink(ctx, tx, l)
	})
	if err != nil {
		return fmt.Errorf("sqlite: invite: %w", err)
	}
	return nil
}

func userInsertLink(ctx context.Context, tx *sql.Tx, l *user.Link) error {
	var uid sql.NullInt64
	if l.Purpose == user.PurposeSetup {
		if err := userNoneExist(ctx, tx); err != nil {
			return err
		}
	} else {
		id, err := userLookupID(ctx, tx, l.UserHandle)
		if err != nil {
			return err
		}
		uid = sql.NullInt64{Int64: id, Valid: true}
	}
	// IS, not =: a setup link's user is NULL.
	if _, err := tx.ExecContext(ctx, `UPDATE invite SET revoked_at = ?
		WHERE user_id IS ? AND purpose = ? AND used_at IS NULL AND revoked_at IS NULL`,
		toNanos(l.CreatedAt), uid, string(l.Purpose)); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO invite
		(user_id, purpose, token_hash, revoke_all, created_by, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		uid, string(l.Purpose), l.Hash, l.RevokeAll, l.CreatedBy, toNanos(l.CreatedAt), toNanos(l.ExpiresAt))
	if err != nil {
		return err
	}
	l.ID, err = res.LastInsertId()
	return err
}

// userNoneExist returns user.ErrSetupDone once the user table has a row.
func userNoneExist(ctx context.Context, tx *sql.Tx) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM user)`).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return user.ErrSetupDone
	}
	return nil
}

const linkSelect = `SELECT v.id, coalesce(u.handle, ''), v.purpose, v.token_hash, v.revoke_all, v.created_by,
	v.created_at, v.expires_at, v.used_at, v.revoked_at
	FROM invite v LEFT JOIN user u ON u.id = v.user_id`

func scanLink(row rowScanner, l *user.Link, extra ...any) error {
	var (
		purpose          string
		created, expires int64
		used, revoked    sql.NullInt64
	)
	dest := []any{&l.ID, &l.UserHandle, &purpose, &l.Hash, &l.RevokeAll, &l.CreatedBy, &created, &expires, &used, &revoked}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return err
	}
	l.Purpose = user.Purpose(purpose)
	l.CreatedAt = fromNanos(created)
	l.ExpiresAt = fromNanos(expires)
	l.UsedAt = ptrTime(used)
	l.RevokedAt = ptrTime(revoked)
	return nil
}

// linkQuerier is a *sql.DB or a *sql.Tx.
type linkQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// userGetLink returns the link with its user, nil for a setup link.
func userGetLink(ctx context.Context, q linkQuerier, where string, arg any) (*user.Link, *user.User, error) {
	var l user.Link
	if err := scanLink(q.QueryRowContext(ctx, linkSelect+" WHERE "+where, arg), &l); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = user.ErrLinkNotFound
		}
		return nil, nil, err
	}
	if l.UserHandle == "" {
		return &l, nil, nil
	}
	var u user.User
	row := q.QueryRowContext(ctx, `SELECT `+userColumns+` FROM user WHERE handle = ?`, l.UserHandle)
	if err := userScan(row, &u); err != nil {
		return nil, nil, err
	}
	return &l, &u, nil
}

// GetLink implements user.Store.
func (s *UserStore) GetLink(ctx context.Context, hash string) (_ *user.Link, _ *user.User, err error) {
	ctx, end := s.db.op(ctx, "UserStore.GetLink")
	defer func() { end(err) }()
	l, u, err := userGetLink(ctx, s.db, "v.token_hash = ?", hash)
	if err != nil {
		return nil, nil, fmt.Errorf("sqlite: get link: %w", err)
	}
	return l, u, nil
}

// ListLinks implements user.Store.
func (s *UserStore) ListLinks(ctx context.Context, now time.Time) (_ []user.Link, err error) {
	ctx, end := s.db.op(ctx, "UserStore.ListLinks")
	defer func() { end(err) }()
	rows, err := s.db.QueryContext(ctx, linkSelect+`
		WHERE v.used_at IS NULL AND v.revoked_at IS NULL AND v.expires_at > ?
		  AND u.disabled_at IS NULL
		  AND (v.purpose <> 'setup' OR NOT EXISTS (SELECT 1 FROM user))
		ORDER BY v.created_at DESC, v.id DESC`, toNanos(now))
	links, err := collect(rows, err, func(row rowScanner) (l user.Link, err error) {
		err = scanLink(row, &l)
		return l, err
	})
	if err != nil {
		return nil, fmt.Errorf("sqlite: list links: %w", err)
	}
	return links, nil
}

// RevokeUserLinks implements user.Store.
func (s *UserStore) RevokeUserLinks(ctx context.Context, handle string, at time.Time) (err error) {
	ctx, end := s.db.op(ctx, "UserStore.RevokeUserLinks")
	defer func() { end(err) }()
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
		uid, err := userLookupID(ctx, tx, handle)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE invite SET revoked_at = ?
			WHERE user_id = ? AND used_at IS NULL AND revoked_at IS NULL`, toNanos(at), uid)
		return err
	})
	if err != nil {
		return fmt.Errorf("sqlite: revoke links: %w", err)
	}
	return nil
}

// RevokeLink implements user.Store.
func (s *UserStore) RevokeLink(ctx context.Context, id int64, at time.Time) (_ *user.Link, err error) {
	ctx, end := s.db.op(ctx, "UserStore.RevokeLink")
	defer func() { end(err) }()
	var l *user.Link
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE invite SET revoked_at = ?1
			WHERE id = ?2 AND used_at IS NULL AND revoked_at IS NULL AND expires_at > ?1`, toNanos(at), id)
		if err == nil {
			err = userOneRow(res, user.ErrLinkNotFound)
		}
		if err != nil {
			return err
		}
		l, _, err = userGetLink(ctx, tx, "v.id = ?", id)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("sqlite: revoke link %d: %w", id, err)
	}
	return l, nil
}

// UseLink implements user.Store.
func (s *UserStore) UseLink(ctx context.Context, hash string, at time.Time, passwordHash string, first *user.User) (_ *user.Link, _ *user.User, err error) {
	ctx, end := s.db.op(ctx, "UserStore.UseLink")
	defer func() { end(err) }()
	var (
		l *user.Link
		u *user.User
	)
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
		// The update is the claim: of two requests with one token, the
		// second changes no row.
		res, err := tx.ExecContext(ctx, `UPDATE invite SET used_at = ?1
			WHERE token_hash = ?2 AND used_at IS NULL AND revoked_at IS NULL AND expires_at > ?1`, toNanos(at), hash)
		if err == nil {
			err = userOneRow(res, user.ErrLinkNotFound)
		}
		if err != nil {
			return err
		}
		if l, u, err = userGetLink(ctx, tx, "v.token_hash = ?", hash); err != nil {
			return err
		}
		if l.Purpose == user.PurposeSetup {
			if first == nil {
				return errors.New("a setup link needs the first user")
			}
			if err := userNoneExist(ctx, tx); err != nil {
				return err
			}
			if err := userInsert(ctx, tx, first); err != nil {
				return err
			}
			u = first
		}
		if u.DisabledAt != nil {
			return user.ErrLinkNotFound
		}
		if _, err := tx.ExecContext(ctx, `UPDATE invite SET revoked_at = ?
			WHERE user_id = ? AND id <> ? AND used_at IS NULL AND revoked_at IS NULL`, toNanos(at), u.ID, l.ID); err != nil {
			return err
		}
		return userSetPassword(ctx, tx, u.ID, passwordHash, at)
	})
	if err != nil {
		return nil, nil, fmt.Errorf("sqlite: use link: %w", err)
	}
	return l, u, nil
}

const accountSelect = `SELECT u.handle, p.user_id IS NOT NULL, p.last_login_at,
		(SELECT MAX(NULLIF(i.last_login_at, 0)) FROM user_identity i WHERE i.user_id = u.id),
		(SELECT COUNT(*) FROM user_identity i WHERE i.user_id = u.id),
		(SELECT MAX(k.last_used_at) FROM api_key k WHERE k.user_id = u.id),
		(SELECT COUNT(*) FROM api_key k WHERE k.user_id = u.id AND k.revoked_at IS NULL
			AND (k.expires_at IS NULL OR k.expires_at > ?)),
		EXISTS (SELECT 1 FROM invite v WHERE v.user_id = u.id AND v.purpose = 'invite' AND v.used_at IS NULL),
		(SELECT COUNT(*) FROM recovery_code c WHERE c.user_id = u.id AND c.used_at IS NULL),
		(SELECT COUNT(*) FROM passkey k WHERE k.user_id = u.id),
		(SELECT MAX(k.last_used_at) FROM passkey k WHERE k.user_id = u.id)
	FROM user u LEFT JOIN user_password p ON p.user_id = u.id`

func scanAccount(row rowScanner, a *user.Account) error {
	var (
		password, identity, key, passkey sql.NullInt64
		invited                          bool
	)
	if err := row.Scan(&a.Handle, &a.HasPassword, &password, &identity, &a.Identities, &key, &a.LiveKeys, &invited, &a.RecoveryCodes, &a.Passkeys, &passkey); err != nil {
		return err
	}
	a.Invited = invited && !a.HasPassword
	a.PasswordLoginAt, a.IdentityLoginAt, a.KeyUsedAt = ptrTime(password), ptrTime(identity), ptrTime(key)
	a.PasskeyLoginAt = ptrTime(passkey)
	return nil
}

// ListAccounts implements user.Store.
func (s *UserStore) ListAccounts(ctx context.Context, now time.Time) (_ []user.Account, err error) {
	ctx, end := s.db.op(ctx, "UserStore.ListAccounts")
	defer func() { end(err) }()
	rows, err := s.db.QueryContext(ctx, accountSelect+` ORDER BY u.handle`, toNanos(now))
	out, err := collect(rows, err, fill(scanAccount))
	if err != nil {
		return nil, fmt.Errorf("sqlite: list accounts: %w", err)
	}
	return out, nil
}

// GetAccount implements user.Store.
func (s *UserStore) GetAccount(ctx context.Context, handle string, now time.Time) (_ user.Account, err error) {
	ctx, end := s.db.op(ctx, "UserStore.GetAccount")
	defer func() { end(err) }()
	var a user.Account
	err = scanAccount(s.db.QueryRowContext(ctx, accountSelect+` WHERE u.handle = ?`, toNanos(now), handle), &a)
	if errors.Is(err, sql.ErrNoRows) {
		err = user.ErrNotFound
	}
	if err != nil {
		return user.Account{}, fmt.Errorf("sqlite: get account: %w", err)
	}
	return a, nil
}
