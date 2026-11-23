package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/artifact/storetest"
	"github.com/PeterBooker/kazari/internal/sqlite"
)

func openArtifactTestDB(t *testing.T) *sqlite.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kazari.db")
	sqlite.EmptyDB.Copy(t, path)
	db, err := sqlite.Open(context.Background(), path, sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func openArtifactStores(t *testing.T) storetest.Stores {
	t.Helper()
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
	st := sqlite.NewArtifactStore(db)
	return storetest.Stores{Artifacts: st, Blobs: st, Links: st, User: mkUser}
}

func TestArtifactStoreConformance(t *testing.T) { storetest.Run(t, openArtifactStores) }

func TestBlobIndexConformance(t *testing.T) { storetest.RunBlobIndex(t, openArtifactStores) }

func TestLinkStoreConformance(t *testing.T) { storetest.RunLinks(t, openArtifactStores) }
