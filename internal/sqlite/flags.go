package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// FlagStore keeps what each person has put away, by name.
type FlagStore struct {
	db *DB
}

// NewFlagStore returns a flag store backed by db.
func NewFlagStore(db *DB) *FlagStore {
	return &FlagStore{db: db}
}

// Flagged reports whether the person with handle set the flag name. An
// unknown handle has set none.
func (s *FlagStore) Flagged(ctx context.Context, handle, name string) (_ bool, err error) {
	ctx, end := s.db.op(ctx, "FlagStore.Flagged")
	defer func() { end(err) }()
	var one int
	err = s.db.QueryRowContext(ctx,
		`SELECT 1 FROM user_flag f JOIN user u ON u.id = f.user_id WHERE u.handle = ? AND f.name = ?`, handle, name).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("sqlite: flagged: %w", err)
	}
	return true, nil
}

// SetFlag sets the flag name for the person with handle; setting it again
// keeps the first time. An unknown handle sets nothing.
func (s *FlagStore) SetFlag(ctx context.Context, handle, name string, at time.Time) (err error) {
	ctx, end := s.db.op(ctx, "FlagStore.SetFlag")
	defer func() { end(err) }()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO user_flag (user_id, name, set_at) SELECT id, ?, ? FROM user WHERE handle = ?
		 ON CONFLICT (user_id, name) DO NOTHING`, name, toNanos(at), handle)
	if err != nil {
		return fmt.Errorf("sqlite: set flag: %w", err)
	}
	return nil
}
