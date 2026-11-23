package sqlite_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/sqlite"
)

// BenchmarkUnreferencedBlobs is the garbage collector's query over 100,000
// blobs, of which the oldest 1,000 are old enough to collect.
func BenchmarkUnreferencedBlobs(b *testing.B) {
	ctx := b.Context()
	db, err := sqlite.Open(ctx, filepath.Join(b.TempDir(), "kazari.db"), sqlite.Options{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { db.Close() })
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if _, err := db.ExecContext(ctx, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 100000)
		INSERT INTO blob (hash, size, created_at) SELECT printf('%064x', i), 1, ? + i * 1000000000 FROM n`, start.UnixNano()); err != nil {
		b.Fatal(err)
	}
	store := sqlite.NewArtifactStore(db)
	before := start.Add(1001 * time.Second)
	for b.Loop() {
		got, err := store.UnreferencedBlobs(ctx, before, 1000)
		if err != nil || len(got) != 1000 {
			b.Fatalf("UnreferencedBlobs = %d hashes, %v; want 1000", len(got), err)
		}
	}
}
