package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/PeterBooker/kazari/internal/audit"
)

func auditTestStore(t *testing.T) *AuditStore {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "kazari.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return NewAuditStore(db)
}

func TestAuditStoreRecordList(t *testing.T) {
	ctx := context.Background()
	s := auditTestStore(t)
	t0 := time.Date(2026, 10, 24, 12, 0, 0, 5, time.UTC)
	events := []audit.Event{
		{At: t0, Actor: "alice", Action: audit.ArtifactCreate, Slug: "demo", Details: map[string]string{"version": "1"}},
		{At: t0.Add(time.Minute), Actor: "bob", Action: audit.ArtifactUpdate, Slug: "demo"},
		{At: t0.Add(2 * time.Minute), Actor: "alice", Action: audit.ArtifactCreate, Slug: "other"},
		{At: t0.Add(3 * time.Minute), Actor: "scheduler", Action: audit.ArtifactStale, Slug: "demo",
			Details: map[string]string{"reason": `quoted "value"`, "b": "2"}},
	}
	for _, e := range events {
		if err := s.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name string
		q    audit.Query
		want []int // indexes into events, newest first
	}{
		{"all", audit.Query{}, []int{3, 2, 1, 0}},
		{"slug", audit.Query{Slug: "demo"}, []int{3, 1, 0}},
		{"actor", audit.Query{Actor: "alice"}, []int{2, 0}},
		{"since inclusive", audit.Query{Since: t0.Add(time.Minute)}, []int{3, 2, 1}},
		{"combined", audit.Query{Since: t0.Add(time.Minute), Slug: "demo", Actor: "bob"}, []int{1}},
		{"limit", audit.Query{Limit: 2}, []int{3, 2}},
		{"no match", audit.Query{Actor: "nobody"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.List(ctx, tt.q)
			if err != nil {
				t.Fatal(err)
			}
			var want []audit.Event
			for _, i := range tt.want {
				want = append(want, events[i])
			}
			if diff := cmp.Diff(want, got, cmpopts.IgnoreFields(audit.Event{}, "ID")); diff != "" {
				t.Errorf("List (-want +got):\n%s", diff)
			}
			for _, e := range got {
				if e.ID == 0 {
					t.Errorf("event %s has no ID", e.Action)
				}
			}
		})
	}
}

func TestAuditStoreRecordStampsTime(t *testing.T) {
	ctx := context.Background()
	s := auditTestStore(t)
	before := time.Now()
	if err := s.Record(ctx, audit.Event{Actor: "admin", Action: audit.UserCreate}); err != nil {
		t.Fatal(err)
	}
	got, err := s.List(ctx, audit.Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].At.Before(before) || got[0].Details != nil {
		t.Fatalf("List = %+v, want one event stamped after %v with nil details", got, before)
	}
}

func TestAuditStoreListLimits(t *testing.T) {
	ctx := context.Background()
	s := auditTestStore(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Direct inserts: 1001 Record calls are slow under -race.
	err := inTx(ctx, s.db, func(tx *sql.Tx) error {
		for i := range 1001 {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO audit_event (at, actor, action) VALUES (?, 'a', 'x')`,
				toNanos(t0.Add(time.Duration(i)*time.Second))); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		limit int
		want  int
	}{
		{0, 100},
		{-1, 100},
		{5, 5},
		{1000, 1000},
		{5000, 1000},
	}
	for _, tt := range tests {
		got, err := s.List(ctx, audit.Query{Limit: tt.limit})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != tt.want {
			t.Errorf("Limit %d: got %d events, want %d", tt.limit, len(got), tt.want)
		}
	}
}
