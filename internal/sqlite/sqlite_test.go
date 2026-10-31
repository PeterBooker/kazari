package sqlite

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func TestOpenMigratesIdempotently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kazari.db")
	ctx := context.Background()
	db, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migration`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Fatalf("no migrations recorded")
	}
	var fk int
	if err := db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Fatalf("foreign_keys = %d, want 1", fk)
	}
	var mode string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
	db.Close()

	db, err = Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var m int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migration`).Scan(&m); err != nil {
		t.Fatal(err)
	}
	if m != n {
		t.Fatalf("migrations after reopen = %d, want %d", m, n)
	}
}

// The 0004 migration rebuilds artifact with foreign keys off: the rows
// that reference it (versions, files, actor rows) must survive, and the
// pool must get its foreign keys back.
func TestMigrationRebuildKeepsReferences(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kazari.db")
	raw, err := sql.Open("sqlite3", "file:"+path+"?_pragma=foreign_keys(1)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	sub, err := fs.Sub(migrationFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	older := fstest.MapFS{}
	for _, name := range []string{"0001_init.sql", "0002_oauth.sql", "0003_oauth_key.sql"} {
		b, err := fs.ReadFile(sub, name)
		if err != nil {
			t.Fatal(err)
		}
		older[name] = &fstest.MapFile{Data: b}
	}
	if err := migrate(ctx, raw, older); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO user (id, handle, role, created_at) VALUES (1, 'alice', 'member', 1)`,
		`INSERT INTO artifact (id, slug, owner_id, visibility, created_at, updated_at) VALUES (1, 'demo', 1, 'unlisted', 1, 1)`,
		`INSERT INTO version (id, artifact_id, number, created_at) VALUES (1, 1, 1, 1)`,
		`INSERT INTO blob (hash, size, created_at) VALUES ('h', 1, 1)`,
		`INSERT INTO version_file (version_id, path, blob_hash, size) VALUES (1, 'index.html', 'h', 1)`,
		`UPDATE artifact SET published_version_id = 1 WHERE id = 1`,
		`INSERT INTO artifact_actor (artifact_id, user_id, created, last_at) VALUES (1, 1, 1, 1)`,
	} {
		if _, err := raw.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := migrate(ctx, raw, sub); err != nil {
		t.Fatal(err)
	}

	var versions, files, actors int
	var vis string
	var published sql.NullInt64
	err = raw.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM version), (SELECT count(*) FROM version_file), (SELECT count(*) FROM artifact_actor),
		visibility, published_version_id FROM artifact WHERE slug = 'demo'`).Scan(&versions, &files, &actors, &vis, &published)
	if err != nil {
		t.Fatal(err)
	}
	if versions != 1 || files != 1 || actors != 1 || vis != "unlisted" || published.Int64 != 1 {
		t.Fatalf("after rebuild: versions %d, files %d, actors %d, visibility %q, published %v",
			versions, files, actors, vis, published)
	}
	if _, err := raw.ExecContext(ctx, `UPDATE artifact SET visibility = 'members' WHERE id = 1`); err != nil {
		t.Fatalf("members refused: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `UPDATE artifact SET visibility = 'secret' WHERE id = 1`); err == nil {
		t.Fatal("an unknown visibility was stored")
	}
	// Every pooled connection, the migration's among them, has foreign
	// keys on again, so deleting the artifact still cascades.
	var conns []*sql.Conn
	for range 3 {
		c, err := raw.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		var fk int
		if err := c.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
			t.Fatal(err)
		}
		if fk != 1 {
			t.Fatalf("foreign_keys = %d after the migration", fk)
		}
	}
	for _, c := range conns {
		c.Close()
	}
	if _, err := raw.ExecContext(ctx, `DELETE FROM artifact WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRowContext(ctx, `SELECT count(*) FROM version`).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 0 {
		t.Fatalf("versions = %d after deleting the artifact, want the cascade", versions)
	}
	raw.Close()
}
