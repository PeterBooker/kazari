// Package audit records who did what to which artifact.
package audit

import (
	"context"
	"errors"
	"fmt"
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
	KeyRotate          = "key.rotate"
	// A link that sets a password: Details["purpose"] is invite, reset or
	// setup.
	InviteCreate    = "invite.create"
	InviteUse       = "invite.use"
	InviteRevoke    = "invite.revoke"
	PasswordSet     = "password.set"
	SessionRevoke   = "session.revoke" // a person's browser sessions ended
	DashboardSignIn = "dashboard.signin"
	AuditExport     = "audit.export"
	BackupCreate    = "backup.create"
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

// Query filters a listing. Every set field must match.
type Query struct {
	Since time.Time // inclusive
	Until time.Time // exclusive
	Slug  string
	Actor string
	// Actions keeps the events whose action is one of these. An entry
	// ending in "." is a prefix: "artifact." matches every artifact action.
	Actions []string
	// Cursor is the NextCursor of the page before; it continues that
	// listing when the other fields are the same.
	Cursor string
	Limit  int // 0 = 100, capped at 1000
}

// MaxActions bounds Query.Actions.
const MaxActions = 32

// ErrInvalidQuery is a Query no listing can answer: a cursor that is not
// one, an empty action or more than MaxActions of them.
var ErrInvalidQuery = errors.New("audit: invalid query")

// Validate reports ErrInvalidQuery for the Actions of q. The cursor is
// checked where it is decoded.
func (q Query) Validate() error {
	if len(q.Actions) > MaxActions {
		return fmt.Errorf("%w: at most %d actions", ErrInvalidQuery, MaxActions)
	}
	for _, a := range q.Actions {
		if a == "" || a == "." {
			return fmt.Errorf("%w: an action is empty", ErrInvalidQuery)
		}
	}
	return nil
}

// Page is one page of a listing.
type Page struct {
	Events     []Event
	NextCursor string // empty on the last page
}

// Reader lists events, newest first. ErrInvalidQuery.
type Reader interface {
	List(ctx context.Context, q Query) (Page, error)
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
