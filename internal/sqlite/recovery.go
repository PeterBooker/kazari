package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/PeterBooker/kazari/internal/user"
)

// ReplaceRecoveryCodes implements user.Store.
func (s *UserStore) ReplaceRecoveryCodes(ctx context.Context, handle string, hashes []string, at time.Time) (err error) {
	ctx, end := s.db.op(ctx, "UserStore.ReplaceRecoveryCodes")
	defer func() { end(err) }()
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
		uid, err := userLookupID(ctx, tx, handle)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM recovery_code WHERE user_id = ?`, uid); err != nil {
			return err
		}
		for _, h := range hashes {
			if _, err := tx.ExecContext(ctx, `INSERT INTO recovery_code (user_id, code_hash, created_at) VALUES (?, ?, ?)`,
				uid, h, toNanos(at)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("sqlite: recovery codes of %q: %w", handle, err)
	}
	return nil
}

// HasRecoveryCode implements user.Store.
func (s *UserStore) HasRecoveryCode(ctx context.Context, handle, hash string) (_ bool, err error) {
	ctx, end := s.db.op(ctx, "UserStore.HasRecoveryCode")
	defer func() { end(err) }()
	var ok bool
	err = s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM recovery_code c JOIN user u ON u.id = c.user_id
		WHERE u.handle = ? AND c.code_hash = ? AND c.used_at IS NULL)`, handle, hash).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("sqlite: recovery code of %q: %w", handle, err)
	}
	return ok, nil
}

// UseRecoveryCode implements user.Store.
func (s *UserStore) UseRecoveryCode(ctx context.Context, handle, hash string, at time.Time, passwordHash string) (_ int, err error) {
	ctx, end := s.db.op(ctx, "UserStore.UseRecoveryCode")
	defer func() { end(err) }()
	var left int
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
		// The update is the claim: of two requests with one code, the
		// second changes no row.
		res, err := tx.ExecContext(ctx, `UPDATE recovery_code SET used_at = ?
			WHERE code_hash = ? AND used_at IS NULL
			AND user_id = (SELECT id FROM user WHERE handle = ? AND disabled_at IS NULL)`, toNanos(at), hash, handle)
		if err == nil {
			err = userOneRow(res, user.ErrBadRecoveryCode)
		}
		if err != nil {
			return err
		}
		uid, err := userLookupID(ctx, tx, handle)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE invite SET revoked_at = ?
			WHERE user_id = ? AND used_at IS NULL AND revoked_at IS NULL`, toNanos(at), uid); err != nil {
			return err
		}
		if err := userSetPassword(ctx, tx, uid, passwordHash, at); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM recovery_code WHERE user_id = ? AND used_at IS NULL`, uid).Scan(&left)
	})
	if err != nil {
		return 0, fmt.Errorf("sqlite: use recovery code of %q: %w", handle, err)
	}
	return left, nil
}

// DeleteRecoveryCodes implements user.Store.
func (s *UserStore) DeleteRecoveryCodes(ctx context.Context, handle string) (err error) {
	ctx, end := s.db.op(ctx, "UserStore.DeleteRecoveryCodes")
	defer func() { end(err) }()
	_, err = s.db.ExecContext(ctx, `DELETE FROM recovery_code WHERE user_id = (SELECT id FROM user WHERE handle = ?)`, handle)
	if err != nil {
		return fmt.Errorf("sqlite: delete recovery codes of %q: %w", handle, err)
	}
	return nil
}
