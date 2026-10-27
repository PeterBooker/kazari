// Package audit records who did what to which artifact.
package audit

import (
	"context"
	"time"
)

const (
	ArtifactCreate     = "artifact.create"
	ArtifactImport     = "artifact.import"
	ArtifactUpdate     = "artifact.update"
	ArtifactPublish    = "artifact.publish"
	ArtifactStale      = "artifact.stale"
	ArtifactUnstale    = "artifact.unstale"
	ArtifactVisibility = "artifact.visibility"
	ArtifactDelete     = "artifact.delete"
	ArtifactRestore    = "artifact.restore"
	ArtifactPurge      = "artifact.purge"
	ArtifactFree       = "artifact.free" // tombstone removed
	ArtifactTransfer   = "artifact.transfer"
	ShareLinkCreate    = "share_link.create"
	ShareLinkRevoke    = "share_link.revoke"
	UserCreate         = "user.create"
	UserUpdate         = "user.update"
	KeyIssue           = "key.issue"
	KeyRevoke          = "key.revoke"
)

// Event is one audit row.
type Event struct {
	ID      int64
	At      time.Time
	Actor   string // handle, or "scheduler" / "admin"
	Action  string
	Slug    string
	Details map[string]string
}

// Recorder appends events. Rows are written after the store call they
// describe, outside its transaction, so a crash in between loses one row.
type Recorder interface {
	Record(ctx context.Context, e Event) error
}

// Query filters a listing for the admin API.
type Query struct {
	Since time.Time
	Slug  string
	Actor string
	Limit int // 0 = 100, capped at 1000
}

// Reader lists events, newest first.
type Reader interface {
	List(ctx context.Context, q Query) ([]Event, error)
}

// Nop discards events.
type Nop struct{}

func (Nop) Record(context.Context, Event) error { return nil }

// Memory keeps events in a slice, for tests.
type Memory struct{ Events []Event }

func (m *Memory) Record(_ context.Context, e Event) error {
	m.Events = append(m.Events, e)
	return nil
}
