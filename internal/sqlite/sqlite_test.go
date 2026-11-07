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

// A database from before admin and scheduler were reserved can hold users
// with those handles. The handles must go, and with them the actor rows that
// only the operator's own events explain.
func TestMigrationFreesReservedHandles(t *testing.T) {
	ctx := context.Background()
	raw, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "kazari.db")+"?_pragma=foreign_keys(1)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	sub, err := fs.Sub(migrationFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	older := fstest.MapFS{}
	for _, name := range []string{
		"0001_init.sql", "0002_oauth.sql", "0003_oauth_key.sql", "0004_gated.sql",
		"0005_oauth_client_grant.sql", "0006_oauth_code_family.sql", "0007_upload_archive.sql",
	} {
		b, err := fs.ReadFile(sub, name)
		if err != nil {
			t.Fatal(err)
		}
		older[name] = &fstest.MapFile{Data: b}
	}
	if err := migrate(ctx, raw, older); err != nil {
		t.Fatal(err)
	}
	const (
		byLocal = `{"credential":"local"}`
		byToken = `{"credential":"admin_token"}`
		byKey   = `{"credential":"api_key:kzk_dummy"}`
	)
	for _, q := range []string{
		`INSERT INTO user (id, handle, role, created_at) VALUES
			(1, 'admin', 'member', 1), (2, 'admin-2', 'member', 1), (3, 'scheduler', 'member', 1), (4, 'alice', 'member', 1)`,
		`INSERT INTO artifact (id, slug, owner_id, visibility, created_at, updated_at) VALUES
			(1, 'hidden', 4, 'public', 1, 1), (2, 'moved', 4, 'public', 1, 1), (3, 'edited', 4, 'public', 1, 1),
			(4, 'their-own', 1, 'public', 1, 1), (5, 'unknown', 4, 'public', 1, 1)`,
		`INSERT INTO artifact_actor (artifact_id, user_id, created, updated, last_at) VALUES
			(1, 1, 0, 1, 1), (1, 4, 1, 0, 1), (2, 1, 0, 1, 1), (3, 1, 0, 1, 1), (4, 1, 1, 1, 1), (5, 1, 0, 1, 1)`,
		`INSERT INTO audit_event (at, actor, action, slug, details) VALUES
			(1, 'admin', 'artifact.visibility', 'hidden', '` + byLocal + `'),
			(1, 'admin', 'artifact.transfer', 'moved', '` + byToken + `'),
			(1, 'admin', 'artifact.visibility', 'edited', '` + byLocal + `'),
			(1, 'admin', 'artifact.update', 'edited', '` + byKey + `'),
			(1, 'admin', 'artifact.visibility', 'their-own', '` + byLocal + `'),
			(1, 'admin', 'artifact.update', 'unknown', '{}')`,
		`INSERT INTO version (id, artifact_id, number, created_by, created_at) VALUES (1, 3, 1, 'admin', 1), (2, 1, 1, 'alice', 1)`,
		`INSERT INTO share_link (artifact_id, prefix, token_hash, created_by, created_at, expires_at) VALUES (3, 'aaaa', 'h', 'admin', 1, 2)`,
	} {
		if _, err := raw.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := migrate(ctx, raw, sub); err != nil {
		t.Fatal(err)
	}

	handles := map[int]string{}
	rows, err := raw.QueryContext(ctx, `SELECT id, handle FROM user`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int
		var h string
		if err := rows.Scan(&id, &h); err != nil {
			t.Fatal(err)
		}
		handles[id] = h
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := map[int]string{1: "admin-3", 2: "admin-2", 3: "scheduler-2", 4: "alice"}
	for id, h := range want {
		if handles[id] != h {
			t.Errorf("user %d = %q, want %q", id, handles[id], h)
		}
	}

	involved := func(slug string, id int) bool {
		t.Helper()
		var ok bool
		err := raw.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM artifact_actor aa
			JOIN artifact a ON a.id = aa.artifact_id WHERE a.slug = ? AND aa.user_id = ?)`, slug, id).Scan(&ok)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	for slug, keep := range map[string]bool{
		"hidden": false, "moved": false, // the operator's events alone
		"edited": true, "their-own": true, "unknown": true, // their own, or no proof otherwise
	} {
		if got := involved(slug, 1); got != keep {
			t.Errorf("user 1 involved in %s = %v, want %v", slug, got, keep)
		}
	}
	if !involved("hidden", 4) {
		t.Error("the owner's actor row went too")
	}

	var version, link, other string
	err = raw.QueryRowContext(ctx, `SELECT
		(SELECT created_by FROM version WHERE id = 1), (SELECT created_by FROM share_link),
		(SELECT created_by FROM version WHERE id = 2)`).Scan(&version, &link, &other)
	if err != nil {
		t.Fatal(err)
	}
	if version != "admin-3" || link != "admin-3" || other != "alice" {
		t.Errorf("created_by: version %q, share link %q, other version %q", version, link, other)
	}

	var history, renames int
	err = raw.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM audit_event WHERE actor = 'admin' AND action LIKE 'artifact.%'),
		(SELECT count(*) FROM audit_event WHERE action = 'user.update'
			AND json_extract(details, '$.handle') IN ('admin-3', 'scheduler-2')
			AND json_extract(details, '$.renamed_from') IN ('admin', 'scheduler'))`).Scan(&history, &renames)
	if err != nil {
		t.Fatal(err)
	}
	if history != 6 || renames != 2 {
		t.Errorf("audit: %d events left under admin, %d renames, want 6 and 2", history, renames)
	}
}
