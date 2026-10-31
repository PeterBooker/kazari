package artifact

import (
	"context"
	"time"
)

// Store is the persistence contract, checked by storetest. Each method owns
// its transaction. Users are named by handle, and the caller passes the time
// so the store never reads the clock.
type Store interface {
	// Create inserts a with its first version (setting v.ID, v.Number = 1),
	// the blob rows and the owner's actor row. ErrSlugTaken also covers
	// tombstones. ErrQuotaExceeded, with nothing written, when quota > 0 and
	// the owner's usage plus v.SizeBytes would exceed it.
	Create(ctx context.Context, a *Artifact, v *Version, quota int64) error

	// Get returns tombstones too, or ErrNotFound.
	Get(ctx context.Context, slug string) (*Artifact, error)

	// GetVersion returns the version with its Files, or ErrVersionNotFound.
	GetVersion(ctx context.Context, slug string, number int) (*Version, error)

	// ListVersions returns newest first, without Files.
	ListVersions(ctx context.Context, slug string) ([]Version, error)

	// AddVersion calls fn on the current row and, when it returns nil, inserts
	// v (setting v.ID and v.Number = latest+1), publishes it, writes what fn
	// may change (name, description, visibility, metadata, the stale, expires,
	// unpublished and deleted times, updated_at), records the blobs and marks
	// v.CreatedBy an updater, in one transaction. Nothing is written on an
	// error from fn (returned as is), on ErrVersionConflict when expected != 0
	// is not the latest number, or on a quota failure as for Create.
	AddVersion(ctx context.Context, slug string, v *Version, expected int, quota int64, fn func(a *Artifact) error) error

	// Update calls fn on the current row and, when it returns nil, writes its
	// mutable fields (including OwnerHandle and PublishedVersion) and marks
	// `by` an updater unless empty, in one transaction, so fields fn leaves
	// alone keep concurrent changes. An error from fn writes nothing.
	Update(ctx context.Context, slug, by string, fn func(a *Artifact) error) error

	// Involved reports whether the handle created or updated the artifact
	// (the actor rows behind ListQuery.Mine); false for an unknown slug or
	// handle.
	Involved(ctx context.Context, slug, handle string) (bool, error)

	// List never lists tombstones.
	List(ctx context.Context, q ListQuery) (ListPage, error)

	// ResolvePublished includes the lifecycle times so the caller can choose
	// between 404 and 410. ErrNotFound only when no row exists at all.
	ResolvePublished(ctx context.Context, slug string) (*Published, error)

	// ResolveVersion is ResolvePublished for one version number;
	// ErrVersionNotFound when the artifact exists but the version does not.
	ResolveVersion(ctx context.Context, slug string, number int) (*Published, error)

	Files(ctx context.Context, versionID int64) (map[string]File, error)

	// Usage sums every version of the handle's artifacts.
	Usage(ctx context.Context, ownerHandle string) (int64, error)

	// The sweep's queries, oldest first; each excludes later states.
	ListExpired(ctx context.Context, now time.Time) ([]Artifact, error)          // expires_at <= now, not deleted
	ListRetentionDue(ctx context.Context, before time.Time) ([]Artifact, error)  // deleted_at <= before, not purged
	ListTombstonesDue(ctx context.Context, before time.Time) ([]Artifact, error) // purged_at <= before

	// Purge calls fn, when not nil, on the current row and, when it returns
	// nil, deletes every version and file row, clears the published pointer
	// and sets purged_at, in one transaction; the row stays as a tombstone.
	// An error from fn writes nothing and is returned as is.
	Purge(ctx context.Context, slug string, at time.Time, fn func(a *Artifact) error) error

	// DeleteTombstone removes the row, freeing the slug.
	DeleteTombstone(ctx context.Context, slug string) error

	// RecordBlob upserts, refreshing created_at so a re-put blob survives the
	// garbage collector's grace window.
	RecordBlob(ctx context.Context, hash string, size int64, at time.Time) error

	// UnreferencedBlobs returns up to limit hashes that no file references
	// and whose created_at is before the given time.
	UnreferencedBlobs(ctx context.Context, before time.Time, limit int) ([]string, error)

	DeleteBlob(ctx context.Context, hash string) error

	// CreateShareLink adds l to the artifact; hash is the hex SHA-256 of
	// the link's token. ErrNotFound for an unknown slug. A link belongs to
	// the artifact row, not the slug: it goes when the tombstone does and
	// never opens a later artifact with the same slug.
	CreateShareLink(ctx context.Context, slug string, l *ShareLink, hash string) error

	// ListShareLinks returns the artifact's links, revoked and expired
	// ones included, newest first.
	ListShareLinks(ctx context.Context, slug string) ([]ShareLink, error)

	// RevokeShareLink sets the revoke time of the artifact's link with
	// prefix, keeping an earlier one. ErrLinkNotFound when the artifact
	// has no such link.
	RevokeShareLink(ctx context.Context, slug, prefix string, at time.Time) error

	// ShareLink returns the artifact's link whose token hashes to hash, in
	// any state. ErrLinkNotFound otherwise, including for another
	// artifact's link.
	ShareLink(ctx context.Context, slug, hash string) (*ShareLink, error)

	// UseShareLink counts one opening of the artifact's link with prefix
	// and sets its last use to at. ErrLinkNotFound.
	UseShareLink(ctx context.Context, slug, prefix string, at time.Time) error

	// CountArtifacts counts every artifact, tombstones included, by Status
	// and visibility.
	CountArtifacts(ctx context.Context) ([]StatusCount, error)

	// VersionTotals counts every version and sums their logical sizes.
	VersionTotals(ctx context.Context) (Totals, error)

	// BlobTotals counts the stored blobs and sums their sizes.
	BlobTotals(ctx context.Context) (Totals, error)
}

// Statuses of an artifact, one each, in the order the lifecycle checks them:
// a soft-deleted artifact that was stale counts as deleted.
const (
	StatusPurged      = "purged"
	StatusDeleted     = "deleted"
	StatusUnpublished = "unpublished"
	StatusStale       = "stale"
	StatusPublished   = "published"
)

// Statuses are every Status value.
var Statuses = []string{StatusPurged, StatusDeleted, StatusUnpublished, StatusStale, StatusPublished}

// StatusCount is how many artifacts have a status and visibility.
type StatusCount struct {
	Status     string
	Visibility Visibility
	Count      int64
}

// Totals is a count and a size in bytes.
type Totals struct {
	Count int64
	Bytes int64
}
