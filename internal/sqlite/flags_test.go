package sqlite_test

import (
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/sqlite"
)

func TestFlags(t *testing.T) {
	db := openArtifactTestDB(t)
	ctx := t.Context()
	for _, h := range []string{"alice", "bob"} {
		if _, err := db.ExecContext(ctx, `INSERT INTO user (handle, role, created_at) VALUES (?, 'admin', 0)`, h); err != nil {
			t.Fatalf("create user %s: %v", h, err)
		}
	}
	s := sqlite.NewFlagStore(db)
	flagged := func(handle, name string) bool {
		t.Helper()
		on, err := s.Flagged(ctx, handle, name)
		if err != nil {
			t.Fatalf("Flagged(%q, %q): %v", handle, name, err)
		}
		return on
	}
	if flagged("alice", "start-dismissed") {
		t.Error("Flagged before any was set = true")
	}
	for range 2 {
		if err := s.SetFlag(ctx, "alice", "start-dismissed", time.Unix(1, 0)); err != nil {
			t.Fatalf("SetFlag: %v", err)
		}
	}
	if !flagged("alice", "start-dismissed") {
		t.Error("Flagged(alice) after SetFlag = false")
	}
	if flagged("bob", "start-dismissed") || flagged("alice", "other") {
		t.Error("a flag of one person or name shows for another")
	}
	if err := s.SetFlag(ctx, "nobody", "start-dismissed", time.Unix(1, 0)); err != nil {
		t.Errorf("SetFlag for an unknown handle: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM user WHERE handle = 'alice'`); err != nil {
		t.Fatal(err)
	}
	if flagged("alice", "start-dismissed") {
		t.Error("a flag outlived its person")
	}
}
