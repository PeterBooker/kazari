package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// noForeignKeys, as a migration's first line, runs it with foreign keys off,
// as rebuilding a referenced table needs: SQLite ignores PRAGMA foreign_keys
// inside a transaction, and dropping a table while they are on deletes every
// row that references it.
const noForeignKeys = "-- kazari:foreign-keys-off"

// Migrate applies every migrations/NNNN_*.sql not yet in schema_migration,
// in numeric order, each in its own transaction.
func Migrate(ctx context.Context, db *sql.DB) error {
	sub, err := fs.Sub(migrationFS, "migrations")
	if err != nil {
		return err
	}
	return migrate(ctx, db, sub)
}

// migrate applies the NNNN_*.sql files of fsys; tests pass a subset to
// build an older schema.
func migrate(ctx context.Context, db *sql.DB, fsys fs.FS) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migration (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("sqlite: migrate: %w", err)
	}
	applied := map[int]bool{}
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migration`)
	if err != nil {
		return fmt.Errorf("sqlite: migrate: %w", err)
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		applied[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return err
	}
	type mig struct {
		version int
		name    string
	}
	var migs []mig
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		num, _, ok := strings.Cut(name, "_")
		if !ok {
			return fmt.Errorf("sqlite: migrate: bad migration name %q", name)
		}
		v, err := strconv.Atoi(num)
		if err != nil {
			return fmt.Errorf("sqlite: migrate: bad migration name %q", name)
		}
		migs = append(migs, mig{v, name})
	}
	sort.Slice(migs, func(i, j int) bool { return migs[i].version < migs[j].version })

	for _, m := range migs {
		if applied[m.version] {
			continue
		}
		body, err := fs.ReadFile(fsys, m.name)
		if err != nil {
			return err
		}
		if err := apply(ctx, db, m.version, m.name, string(body)); err != nil {
			return fmt.Errorf("sqlite: migrate %s: %w", m.name, err)
		}
	}
	return nil
}

// apply runs one migration and records it in the same transaction. A
// migration marked noForeignKeys runs on a connection of its own with
// foreign keys off, and commits only if PRAGMA foreign_key_check finds no
// broken reference afterwards.
func apply(ctx context.Context, db *sql.DB, version int, name, body string) error {
	run := func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, body); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migration (version, name, applied_at) VALUES (?, ?, ?)`,
			version, name, nowNanos())
		return err
	}
	if !strings.HasPrefix(body, noForeignKeys) {
		return inTx(ctx, db, run)
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return err
	}
	defer func() {
		// A connection must never go back to the pool with foreign keys
		// off. The pragma does nothing inside an open transaction, so read
		// it back, and discard the connection unless it reads 1.
		bg := context.WithoutCancel(ctx)
		var on int
		_, err := conn.ExecContext(bg, `PRAGMA foreign_keys = ON`)
		if err == nil {
			err = conn.QueryRowContext(bg, `PRAGMA foreign_keys`).Scan(&on)
		}
		if err != nil || on != 1 {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	return inTx(ctx, conn, func(tx *sql.Tx) error {
		if err := run(tx); err != nil {
			return err
		}
		return foreignKeyCheck(ctx, tx)
	})
}

// foreignKeyCheck fails when any row references a missing parent.
func foreignKeyCheck(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		var (
			table, parent string
			rowid         sql.NullInt64
			fkid          int
		)
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return err
		}
		return fmt.Errorf("foreign key check: a row of %s (rowid %d) references a missing row of %s", table, rowid.Int64, parent)
	}
	return rows.Err()
}
