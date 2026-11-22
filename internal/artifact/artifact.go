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
	MetaCSP       = "kazari.csp"       // adds sources to the instance Content-Security-Policy
	// MetaKeepVersions, a whole number from 1 to MaxKeepVersions, has Sweep
	// prune the artifact's versions as PruneVersions does.
	MetaKeepVersions = "kazari.keep_versions"
)

// MaxKeepVersions bounds MetaKeepVersions and a prune's keep.
const MaxKeepVersions = 10000

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
	// VersionCount is how many versions there are, fewer than LatestVersion
	// once some were deleted.
	VersionCount int
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
	// Staged is a version added without being published that has not been
	// published since. Its host is served only to signed-in members.
	Staged bool
	Files  []File // sorted by Path; empty unless requested

	// Unchanged, Warnings and Edited describe the publish that returned the
	// version and are never stored. Unchanged means the files matched the
	// latest version, which is returned instead of a new one. Edited is how
	// the edits of each FileInput with Edits applied.
	Unchanged bool
	Warnings  []string
	Edited    []FileEdits
}

// FileEdits reports the edits an update applied to one file.
type FileEdits struct {
	Path     string
	Outcomes []EditOutcome
}

// File is one entry of a version.
type File struct {
	Path        string // normalised, no leading slash
	Hash        string // hex sha256
	Size        int64
	ContentType string
	// UploadedBy is the handle of whoever first put this content at this
	// path: kept while later versions carry the content unchanged, the
	// version's author for new content. The store fills it in when empty.
	UploadedBy string
}

// FileInput is a file being added to or removed from a version. On update,
// Edits instead of Content changes the base version's text file at Path, as
// Service.Edit does, so one version can hold edits to several files.
type FileInput struct {
	Path        string
	Content     io.Reader // nil with Delete=true removes the path on update
	ContentType string    // optional; derived from the extension, then sniffed
	Delete      bool
	Edits       []Edit
}

// Actor is who is calling the service.
type Actor struct {
	Handle     string
	Admin      bool
	QuotaBytes int64 // 0 means unlimited
	// Credential is "api_key:<prefix>", "oauth:<id>" or "admin_token", for
	// the audit trail; empty for System.
	Credential string
	// CanDelete is whether the credential holds artifacts:delete, the scope
	// MarkStale needs on an artifact the actor does not own.
	CanDelete bool
	// Slugs restricts the credential to slugs matching one of these
	// patterns (see MatchSlug); empty means no restriction. It narrows what
	// the actor may otherwise do and never widens it.
	Slugs []string
	// Operator is the admin token or socket: Handle is only a name in the
	// audit trail, whatever a user row may hold.
	Operator bool
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
	ExpectedVersion int // compared with LatestVersion; 0 = no check
	// BaseVersion is the version a new one is built on; 0 is the latest,
	// refused while an earlier version is published.
	BaseVersion int
	KeepStale   bool // a new version or a restore normally clears stale_at/expires_at
	Restore     bool
	// Stage adds the version without publishing it; it needs Files.
	Stage bool
	Files []FileInput
}

// ListQuery filters a listing. Viewer sees public artifacts plus any it
// created or updated; All sees every artifact. Slugs, when not empty, keeps
// only the artifacts whose slug matches one of the patterns, whatever else
// is set.
type ListQuery struct {
	Viewer   string            // handle
	All      bool              // admin only
	Slugs    []string          // an actor's restriction, see MatchSlug
	Mine     string            // "", "created", "updated", "any" (relative to Viewer)
	Owner    string            // handle
	Metadata map[string]string // every pair must match
	Stale    string            // "", "include" (default), "exclude", "only"
	Deleted  string            // "", "exclude" (default), "include", "only"
	// Published keeps only the artifacts that are not unpublished, whatever
	// their visibility.
	Published bool
	// Search keeps the artifacts whose slug or name contains it, ignoring
	// ASCII case; at most MaxSearchLen bytes.
	Search string
	// Visibility is "", public, unlisted or members (that visibility, and
	// not unpublished), or unpublished.
	Visibility string
	Cursor     string
	Limit      int // 0 = store default (50); capped at 200
}

// MaxSearchLen bounds ListQuery.Search.
const MaxSearchLen = 100

// Validate reports ErrInvalidArgument for a filter value outside its set.
// Metadata keys and the cursor are checked where they are used.
func (q ListQuery) Validate() error {
	switch q.Mine {
	case "", "created", "updated", "any":
	default:
		return Errorf(ErrInvalidArgument, "mine must be created, updated or any")
	}
	switch q.Stale {
	case "", "include", "exclude", "only":
	default:
		return Errorf(ErrInvalidArgument, "stale must be include, exclude or only")
	}
	switch q.Deleted {
	case "", "exclude", "include", "only":
	default:
		return Errorf(ErrInvalidArgument, "deleted must be exclude, include or only")
	}
	switch Visibility(q.Visibility) {
	case "", Public, Unlisted, Members, Unpublished:
	default:
		return Errorf(ErrInvalidArgument, "visibility must be public, unlisted, members or unpublished")
	}
	if len(q.Search) > MaxSearchLen {
		return Errorf(ErrInvalidArgument, "the search text is longer than %d bytes", MaxSearchLen)
	}
	return nil
}

// ListPage is one page of a listing.
type ListPage struct {
	Items      []Artifact
	NextCursor string // empty on the last page
}

// Published is what the serving path needs to answer a request.
type Published struct {
	ArtifactID    int64
	Slug          string
	VersionID     int64 // 0 when nothing is published
	VersionNumber int
	// Staged is set when the version is staged (Version.Staged).
	Staged bool
	// LatestVersion is the artifact's latest version number, and
	// VersionDeleted is set by ResolveVersion for a number below it whose
	// version was deleted.
	LatestVersion  int
	VersionDeleted bool
	Visibility     Visibility
	Metadata       map[string]string
	UnpublishedAt  *time.Time
	DeletedAt      *time.Time
	PurgedAt       *time.Time

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

// Gated reports whether serving needs a signed-in viewer: a members
// artifact, or a staged version, which a share link does not open either.
func (p *Published) Gated() bool { return p.Visibility.Gated() || p.Staged }

// Servable reports whether the artifact is in a state to be served.
func (p *Published) Servable() bool {
	return p.VersionID != 0 && p.UnpublishedAt == nil && p.DeletedAt == nil && p.PurgedAt == nil
}

// FileOrigin says who first put a file's content at its path in an artifact.
// A handle with no user (the admin token, the scheduler) counts as an admin.
type FileOrigin struct {
	Author   string
	Admin    bool
	Disabled bool
	Owner    string // the artifact's owner now
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
	// HasPassword is set for a link that asks for a password before it
	// opens its artifact.
	HasPassword bool
}

// Active reports whether the link opens its artifact at now.
func (l *ShareLink) Active(now time.Time) bool {
	return l.RevokedAt == nil && now.Before(l.ExpiresAt)
}

// RevokedShareLink names a link that a bulk revoke ended.
type RevokedShareLink struct {
	Slug   string
	Prefix string
}

// SweepReport counts what one Sweep did.
type SweepReport struct {
	SoftDeleted     int // stale past expiry
	Purged          int // soft-deleted past retention
	Freed           int // tombstones removed
	VersionsDeleted int // past an artifact's MetaKeepVersions
}
