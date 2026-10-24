// Package sqlite opens the database, applies the embedded migrations and
// implements the Store interfaces of artifact, user, upload, audit and oauth
// with hand-written SQL. Each store type wraps the same *sql.DB.
//
// Conventions shared by every store in this package:
//   - timestamps are INTEGER unix nanoseconds in UTC (see time.go);
//   - users are referenced by id internally and by handle externally;
//   - every public method is one unit of work and owns its transaction.
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"

	_ "github.com/ncruces/go-sqlite3/driver"
)

// DB is an open, migrated database.
type DB struct {
	*sql.DB
}

// Open opens (creating if needed) the database at path with WAL mode, a
// busy timeout and foreign keys on, and applies pending migrations.
func Open(ctx context.Context, path string) (*DB, error) {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Set("_txlock", "immediate")
	dsn := "file:" + path + "?" + q.Encode()
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: open %s: %w", path, err)
	}
	if err := Migrate(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return &DB{DB: db}, nil
}

// inTx runs fn in a transaction, committing on nil and rolling back on
// error or panic.
func inTx(ctx context.Context, db *sql.DB, fn func(tx *sql.Tx) error) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback()
			return
		}
		err = tx.Commit()
	}()
	return fn(tx)
}
