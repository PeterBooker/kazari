package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/sqlite"
)

func TestVacuumInto(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := sqlite.Open(ctx, filepath.Join(dir, "kazari.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	insert := func(handle string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `INSERT INTO user (handle, role, quota_bytes, created_at) VALUES (?, 'member', 0, ?)`,
			handle, time.Now().UnixNano()); err != nil {
			t.Fatal(err)
		}
	}
	insert("ana")

	path := filepath.Join(dir, "copy.db")
	if err := db.VacuumInto(ctx, path); err != nil {
		t.Fatal(err)
	}
	insert("ben")

	// Opening the copy migrates nothing: it carries schema_migration too.
	copied, err := sqlite.Open(ctx, path, sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer copied.Close()
	var handles int
	if err := copied.QueryRowContext(ctx, `SELECT COUNT(*) FROM user`).Scan(&handles); err != nil {
		t.Fatal(err)
	}
	if handles != 1 {
		t.Errorf("users in the copy = %d, want the 1 that existed when it was taken", handles)
	}
	var handle string
	if err := copied.QueryRowContext(ctx, `SELECT handle FROM user`).Scan(&handle); err != nil || handle != "ana" {
		t.Errorf("handle in the copy = %q, %v", handle, err)
	}

	if err := db.VacuumInto(ctx, path); err == nil {
		t.Error("VacuumInto over an existing file = nil error")
	}
}
