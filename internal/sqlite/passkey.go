package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ncruces/go-sqlite3"

	"github.com/PeterBooker/kazari/internal/user"
)

// PasskeyUserID implements user.Store.
func (s *UserStore) PasskeyUserID(ctx context.Context, handle string) (_ []byte, err error) {
	ctx, end := s.db.op(ctx, "UserStore.PasskeyUserID")
	defer func() { end(err) }()
	var (
		uid int64
		id  []byte
	)
	err = s.db.QueryRowContext(ctx, `SELECT u.id, p.webauthn_id FROM user u
		LEFT JOIN passkey_user p ON p.user_id = u.id WHERE u.handle = ?`, handle).Scan(&uid, &id)
	if errors.Is(err, sql.ErrNoRows) {
		err = user.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: passkey user handle of %q: %w", handle, err)
	}
	return id, nil
}

// CreatePasskey implements user.Store.
func (s *UserStore) CreatePasskey(ctx context.Context, p *user.Passkey, webauthnID []byte, max int) (err error) {
	ctx, end := s.db.op(ctx, "UserStore.CreatePasskey")
	defer func() { end(err) }()
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
		var (
			uid      int64
			disabled sql.NullInt64
			stored   []byte
			count    int
		)
		err := tx.QueryRowContext(ctx, `SELECT u.id, u.disabled_at, p.webauthn_id,
			(SELECT COUNT(*) FROM passkey k WHERE k.user_id = u.id)
			FROM user u LEFT JOIN passkey_user p ON p.user_id = u.id WHERE u.handle = ?`, p.UserHandle).
			Scan(&uid, &disabled, &stored, &count)
		if errors.Is(err, sql.ErrNoRows) {
			return user.ErrNotFound
		}
		if err != nil {
			return err
		}
		switch {
		case disabled.Valid:
			return user.ErrDisabled
		case stored != nil && !bytes.Equal(stored, webauthnID):
			return user.ErrPasskeyStale
		case max > 0 && count >= max:
			return user.ErrTooManyPasskeys
		}
		if stored == nil {
			if _, err := tx.ExecContext(ctx, `INSERT INTO passkey_user (user_id, webauthn_id) VALUES (?, ?)`, uid, webauthnID); err != nil {
				// Another person holds the same random handle: never in practice.
				return err
			}
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO passkey (user_id, credential_id, name, record, created_at)
			VALUES (?, ?, ?, ?, ?)`, uid, p.CredentialID, p.Name, p.Record, toNanos(p.CreatedAt))
		if errors.Is(err, sqlite3.CONSTRAINT_UNIQUE) {
			return user.ErrPasskeyExists
		}
		if err != nil {
			return err
		}
		p.ID, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return fmt.Errorf("sqlite: create passkey of %q: %w", p.UserHandle, err)
	}
	return nil
}

const passkeySelect = `SELECT k.id, u.handle, k.name, k.credential_id, k.record, k.created_at, k.last_used_at
	FROM passkey k JOIN user u ON u.id = k.user_id`

func scanPasskey(row userRowScanner, p *user.Passkey, extra ...any) error {
	var (
		created int64
		used    sql.NullInt64
	)
	dest := append([]any{&p.ID, &p.UserHandle, &p.Name, &p.CredentialID, &p.Record, &created, &used}, extra...)
	if err := row.Scan(dest...); err != nil {
		return err
	}
	p.CreatedAt, p.LastUsedAt = fromNanos(created), ptrTime(used)
	return nil
}

// ListPasskeys implements user.Store.
func (s *UserStore) ListPasskeys(ctx context.Context, handle string) (_ []user.Passkey, err error) {
	ctx, end := s.db.op(ctx, "UserStore.ListPasskeys")
	defer func() { end(err) }()
	out := []user.Passkey{}
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := userLookupID(ctx, tx, handle); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, passkeySelect+` WHERE u.handle = ? ORDER BY k.id`, handle)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p user.Passkey
			if err := scanPasskey(rows, &p); err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("sqlite: list passkeys of %q: %w", handle, err)
	}
	return out, nil
}

// GetPasskey implements user.Store.
func (s *UserStore) GetPasskey(ctx context.Context, credentialID []byte) (_ *user.Passkey, _ *user.User, _ []byte, err error) {
	ctx, end := s.db.op(ctx, "UserStore.GetPasskey")
	defer func() { end(err) }()
	var (
		p      user.Passkey
		userID []byte
	)
	err = scanPasskey(s.db.QueryRowContext(ctx, `SELECT k.id, u.handle, k.name, k.credential_id, k.record, k.created_at,
		k.last_used_at, w.webauthn_id
		FROM passkey k JOIN user u ON u.id = k.user_id JOIN passkey_user w ON w.user_id = k.user_id
		WHERE k.credential_id = ?`, credentialID), &p, &userID)
	if errors.Is(err, sql.ErrNoRows) {
		err = user.ErrPasskeyNotFound
	}
	if err != nil {
		return nil, nil, nil, fmt.Errorf("sqlite: get passkey: %w", err)
	}
	u, err := s.GetUser(ctx, p.UserHandle)
	if err != nil {
		return nil, nil, nil, err
	}
	return &p, u, userID, nil
}

// TouchPasskey implements user.Store.
func (s *UserStore) TouchPasskey(ctx context.Context, id int64, record []byte, at time.Time) (err error) {
	ctx, end := s.db.op(ctx, "UserStore.TouchPasskey")
	defer func() { end(err) }()
	res, err := s.db.ExecContext(ctx, `UPDATE passkey SET record = ?, last_used_at = ? WHERE id = ?`, record, toNanos(at), id)
	if err == nil {
		err = userOneRow(res, user.ErrPasskeyNotFound)
	}
	if err != nil {
		return fmt.Errorf("sqlite: touch passkey %d: %w", id, err)
	}
	return nil
}

// DeletePasskey implements user.Store.
func (s *UserStore) DeletePasskey(ctx context.Context, handle string, id int64) (_ *user.Passkey, err error) {
	ctx, end := s.db.op(ctx, "UserStore.DeletePasskey")
	defer func() { end(err) }()
	var p user.Passkey
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
		err := scanPasskey(tx.QueryRowContext(ctx, passkeySelect+` WHERE k.id = ? AND u.handle = ?`, id, handle), &p)
		if errors.Is(err, sql.ErrNoRows) {
			return user.ErrPasskeyNotFound
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM passkey WHERE id = ?`, id)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("sqlite: delete passkey %d of %q: %w", id, handle, err)
	}
	return &p, nil
}

// DeletePasskeys implements user.Store.
func (s *UserStore) DeletePasskeys(ctx context.Context, handle string) (err error) {
	ctx, end := s.db.op(ctx, "UserStore.DeletePasskeys")
	defer func() { end(err) }()
	_, err = s.db.ExecContext(ctx, `DELETE FROM passkey WHERE user_id = (SELECT id FROM user WHERE handle = ?)`, handle)
	if err != nil {
		return fmt.Errorf("sqlite: delete passkeys of %q: %w", handle, err)
	}
	return nil
}
