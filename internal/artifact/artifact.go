// Package artifact is the artifact domain: types, validation, the Store
// contract and the Service. It knows nothing of HTTP, MCP or SQLite.
package artifact

import (
	"io"
	"time"
)

// Visibility controls who may open an artifact and who sees it listed.
// Unlisted is left out of other people's listings; Members is served only to
// people signed in to the instance and share-link holders, and listed like
// Public.
type Visibility string

const (
	Public   Visibility = "public"
	Unlisted Visibility = "unlisted"
	Members  Visibility = "members"
)

// Gated reports whether serving needs a signed-in viewer or a share link.
func (v Visibility) Gated() bool { return v == Members }

// Unpublished is a Service.SetVisibility value that sets unpublished_at; it
// is never a stored Visibility.
const Unpublished = "unpublished"

// Metadata keys the server interprets; all others are opaque.
const (
	MetaSPA       = "kazari.spa"       // "true" serves index.html for any extension-less miss
	MetaIndexable = "kazari.indexable" // "true" drops the X-Robots-Tag: noindex header
	MetaCSP       = "kazari.csp"       // replaces the instance Content-Security-Policy
)

// Artifact is the record behind a slug.
type Artifact struct {
	Slug        string
	Name        string
	Description string
	OwnerHandle string
	Visibility  Visibility
	Metadata    map[string]string

	// PublishedVersion is 0 when nothing is published (a purged artifact).
	PublishedVersion int
	LatestVersion    int
	// SizeBytes is the sum over every version.
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
	// ID is the store's row id, used by the serving path and never exposed
	// through an API.
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
	Hash        string // hex sha256
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
	// Credential is "api_key:<prefix>", "oauth:<id>" or "admin_token", for
	// the audit trail; empty for System.
	Credential string
}

// System is the actor used by scheduler jobs.
var System = Actor{Handle: "scheduler", Admin: true}

// CreateRequest creates an artifact with its first version.
type CreateRequest struct {
	Slug        string
	Name        string
	Description string
	Visibility  Visibility // defaults to Options.DefaultVisibility
	Metadata    map[string]string
	Label       string
	Files       []FileInput
}

// UpdateRequest changes fields and, unless Files is empty, adds a version.
// Nil pointers leave a field alone.
type UpdateRequest struct {
	Slug            string
	Name            *string
	Description     *string
	Metadata        map[string]*string // nil value deletes the key
	Visibility      *Visibility
	Label           string
	ExpectedVersion int  // compared with LatestVersion; 0 = no check
	KeepStale       bool // a new version normally clears stale_at/expires_at
	Restore         bool
	Files           []FileInput
}

// ListQuery filters a listing. Viewer sees public artifacts plus any it
// created or updated; All sees every artifact.
type ListQuery struct {
	Viewer   string            // handle
	All      bool              // admin only
	Mine     string            // "", "created", "updated", "any" (relative to Viewer)
	Owner    string            // handle
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

// Published is what the serving path needs to answer a request.
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

// ShareLink opens one members-only artifact, on its latest and version
// hosts, without signing in. Its token, kzs_<prefix>_<secret>, is shown
// once when the link is created; the store keeps only the token's hash, and
// the prefix names the link afterwards.
type ShareLink struct {
	Prefix     string
	Label      string
	CreatedBy  string // handle
	CreatedAt  time.Time
	ExpiresAt  time.Time
	RevokedAt  *time.Time
	LastUsedAt *time.Time
	Uses       int // times the link was opened
}

// Active reports whether the link opens its artifact at now.
func (l *ShareLink) Active(now time.Time) bool {
	return l.RevokedAt == nil && now.Before(l.ExpiresAt)
}

// SweepReport counts what one Sweep did.
type SweepReport struct {
	SoftDeleted int // stale past expiry
	Purged      int // soft-deleted past retention
	Freed       int // tombstones removed
}
