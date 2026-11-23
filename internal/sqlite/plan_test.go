package sqlite_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/PeterBooker/kazari/internal/sqlite"
)

// The garbage collector's query reads the blobs old enough to collect, in
// the index's order, not the whole table.
func TestUnreferencedBlobsPlan(t *testing.T) {
	ctx := t.Context()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "kazari.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	rows, err := db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+sqlite.UnreferencedBlobsQuery, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(plan, "\n")
	if !strings.Contains(all, "SEARCH b USING COVERING INDEX blob_created (created_at<?)") ||
		strings.Contains(all, "SCAN b") || strings.Contains(all, "TEMP B-TREE") {
		t.Errorf("plan of UnreferencedBlobs =\n%s\nwant a search of blob_created that gives the order", all)
	}
}
