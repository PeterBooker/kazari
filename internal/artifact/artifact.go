// Package artifact holds the artifact domain: the types, their validation
// rules, the Store contract a persistence layer must satisfy, and the
// Service that implements create, update, lifecycle and garbage collection.
//
// Nothing in this package knows about HTTP, MCP or SQLite.
package artifact

import (
	"io"
	"time"
)

// Visibility controls whether an artifact appears in listings. Both values
// are served; gating is a Phase 4 feature.
type Visibility string

const (
	Public   Visibility = "public"
	Unlisted Visibility = "unlisted"
)

// Unpublished is accepted by Service.SetVisibility in addition to the two
// Visibility values. It is a lifecycle state (unpublished_at set), not a
// stored visibility.
const Unpublished = "unpublished"

// Metadata keys interpreted by the server. Every other key is opaque.
const (
	MetaSPA       = "agalma.spa"       // "true" serves index.html for any extension-less miss
	MetaIndexable = "agalma.indexable" // "true" drops the X-Robots-Tag: noindex header
	MetaCSP       = "agalma.csp"       // replaces the instance Content-Security-Policy
)

// Artifact is the record behind a slug.
type Artifact struct {
	Slug        string
	Name        string
	Description string
	OwnerHandle string
	Visibility  Visibility
	Metadata    map[string]string

	// PublishedVersion is the number of the version being served, 0 when
	// nothing is published (a purged artifact).
	PublishedVersion int
	// LatestVersion is the highest version number that exists.
	LatestVersion int
	// SizeBytes is the sum of every version's size.
	SizeBytes int64

	StaleAt       *time.Time
	ExpiresAt     *time.Time
	UnpublishedAt *time.Time
	DeletedAt     *time.Time
	PurgedAt      *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Version is one immutable snapshot of an artifact's files.
type Version struct {
	// ID is the store's row id. It is used on the serving path to load the
	// file list and is never exposed through an API.
	ID        int64
	Number    int
	Label     string
	CreatedBy string // handle
	CreatedAt time.Time
	SizeBytes int64
	Files     []File // sorted by Path; empty unless requested
}

// File is one entry of a version.
type File struct {
	Path        string // normalised, no leading slash
	Hash        string // hex sha256 of the content
	Size        int64
	ContentType string
}

// FileInput is a file being added to or removed from a version.
type FileInput struct {
	Path        string
	Content     io.Reader // nil with Delete=true removes the path on update
	ContentType string    // optional; derived from the extension, then sniffed
	Delete      bool
}

// Actor is who is calling the service.
type Actor struct {
	Handle     string
	Admin      bool
	QuotaBytes int64 // 0 means unlimited
	// Credential names what the actor authenticated with, for the audit
	// trail: "api_key:<prefix>", "oauth:<id>" or "admin_token". Empty for
	// System.
	Credential string
}

// System is the actor used by scheduler jobs.
var System = Actor{Handle: "scheduler", Admin: true}

// CreateRequest creates an artifact with its first version.
type CreateRequest struct {
	Slug        string
	Name        string
	Description string
	Visibility  Visibility // defaults to Public
	Metadata    map[string]string
	Label       string
	Files       []FileInput
}

// UpdateRequest changes fields and/or adds a version. Nil pointers leave a
// field alone. Files empty means a fields-only update with no new version.
type UpdateRequest struct {
	Slug            string
	Name            *string
	Description     *string
	Metadata        map[string]*string // nil value deletes the key
	Visibility      *Visibility
	Label           string
	ExpectedVersion int  // compared with LatestVersion; 0 = no check
	KeepStale       bool // a new version normally clears stale_at/expires_at
	Restore         bool // clear deleted_at on a soft-deleted artifact
	Files           []FileInput
}

// ListQuery filters a listing. The store applies visibility: Viewer sees
// public artifacts plus any it created or updated; All bypasses that.
type ListQuery struct {
	Viewer   string            // handle of the caller
	All      bool              // admin: every artifact regardless of visibility
	Mine     string            // "", "created", "updated", "any" (relative to Viewer)
	Owner    string            // filter by owner handle
	Metadata map[string]string // every pair must match
	Stale    string            // "", "include" (default), "exclude", "only"
	Deleted  string            // "", "exclude" (default), "include", "only"
	Cursor   string
	Limit    int // 0 = store default (50); capped at 200
}

// ListPage is one page of a listing.
type ListPage struct {
	Items      []Artifact
	NextCursor string // empty on the last page
}

// Published is what the serving path needs to answer a request: which
// version to serve and whether the artifact is in a state to be served.
type Published struct {
	Slug          string
	VersionID     int64 // 0 when nothing is published
	VersionNumber int
	Visibility    Visibility
	Metadata      map[string]string
	UnpublishedAt *time.Time
	DeletedAt     *time.Time
	PurgedAt      *time.Time

	// Derived from Metadata by Service; the store leaves them zero.
	SPA       bool
	Indexable bool
	CSP       string // empty = instance default
}

// DeriveFlags fills SPA, Indexable and CSP from Metadata.
func (p *Published) DeriveFlags() {
	p.SPA = p.Metadata[MetaSPA] == "true"
	p.Indexable = p.Metadata[MetaIndexable] == "true"
	p.CSP = p.Metadata[MetaCSP]
}

// Servable reports whether the artifact is in a state to be served.
func (p *Published) Servable() bool {
	return p.VersionID != 0 && p.UnpublishedAt == nil && p.DeletedAt == nil && p.PurgedAt == nil
}

// SweepReport counts what one Sweep did.
type SweepReport struct {
	SoftDeleted int // stale past expiry
	Purged      int // soft-deleted past retention
	Freed       int // tombstones removed
}
