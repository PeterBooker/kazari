package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"agalma/internal/artifact"
	"agalma/internal/artifact/storetest"
	"agalma/internal/sqlite"
)

// openArtifactTestDB opens a migrated database in a temp dir.
func openArtifactTestDB(t *testing.T) *sqlite.DB {
	t.Helper()
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "agalma.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestArtifactStoreConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) (artifact.Store, func(string)) {
		db := openArtifactTestDB(t)
		mkUser := func(handle string) {
			t.Helper()
			_, err := db.ExecContext(context.Background(),
				`INSERT INTO user (handle, role, quota_bytes, created_at) VALUES (?, 'member', 0, ?)`,
				handle, time.Now().UnixNano())
			if err != nil {
				t.Fatalf("create user %s: %v", handle, err)
			}
		}
		return sqlite.NewArtifactStore(db), mkUser
	})
}
