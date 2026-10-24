package artifact

import (
	"context"
	"time"
)

// Store is the persistence contract. Each method is one unit of work and
// owns its transaction. Implementations live in other packages (sqlite) and
// are checked with the storetest conformance suite.
//
// Handles are the external key for users; the store resolves them to ids.
// Time is passed in by the caller so the store never reads the clock.
type Store interface {
	// Create inserts the artifact, its first version with files, the blob
	// rows the files reference, and the actor row for a.OwnerHandle.
	// It sets v.ID and v.Number (1). ErrSlugTaken when the slug exists,
	// including as a tombstone.
	Create(ctx context.Context, a *Artifact, v *Version) error

	// Get returns the artifact (including tombstones) or ErrNotFound.
	Get(ctx context.Context, slug string) (*Artifact, error)

	// GetVersion returns one version with its Files, or ErrVersionNotFound.
	GetVersion(ctx context.Context, slug string, number int) (*Version, error)

	// ListVersions returns every version, newest first, without Files.
	ListVersions(ctx context.Context, slug string) ([]Version, error)

	// AddVersion inserts v (setting v.ID and v.Number = latest+1), points
	// the published version at it, writes a's mutable fields (name,
	// description, visibility, metadata, stale/expires/unpublished/deleted
	// timestamps, updated_at), records the blobs, and marks v.CreatedBy as
	// an updater in the actor table. When expected != 0 and the current
	// latest number differs, nothing is written and ErrVersionConflict is
	// returned.
	AddVersion(ctx context.Context, a *Artifact, v *Version, expected int) error

	// Update writes a's mutable fields, including OwnerHandle and
	// PublishedVersion (by number), and marks `by` as an updater when it is
	// not empty.
	Update(ctx context.Context, a *Artifact, by string) error

	// List applies q and returns one page. Tombstones are never listed.
	List(ctx context.Context, q ListQuery) (ListPage, error)

	// ResolvePublished returns the serving record for the published
	// version, with the lifecycle timestamps so the caller can decide
	// between 404 and 410. ErrNotFound only when no row exists at all.
	ResolvePublished(ctx context.Context, slug string) (*Published, error)

	// ResolveVersion is ResolvePublished for a specific version number.
	// ErrVersionNotFound when the artifact exists but the version does not.
	ResolveVersion(ctx context.Context, slug string, number int) (*Published, error)

	// Files returns the file map of a version keyed by path.
	Files(ctx context.Context, versionID int64) (map[string]File, error)

	// Usage returns the total size of every version owned by the handle.
	Usage(ctx context.Context, ownerHandle string) (int64, error)

	// Lifecycle queries for the sweep. Each returns artifacts in the state
	// named, oldest first, excluding later states.
	ListExpired(ctx context.Context, now time.Time) ([]Artifact, error)          // expires_at <= now, not deleted
	ListRetentionDue(ctx context.Context, before time.Time) ([]Artifact, error)  // deleted_at <= before, not purged
	ListTombstonesDue(ctx context.Context, before time.Time) ([]Artifact, error) // purged_at <= before

	// Purge deletes every version and file row of the artifact, clears the
	// published pointer, and sets purged_at. The row stays as a tombstone.
	Purge(ctx context.Context, slug string, at time.Time) error

	// DeleteTombstone removes the artifact row, freeing the slug.
	DeleteTombstone(ctx context.Context, slug string) error

	// RecordBlob upserts a blob row, refreshing created_at so a blob that
	// was just re-put survives the garbage collector's grace window.
	RecordBlob(ctx context.Context, hash string, size int64, at time.Time) error

	// UnreferencedBlobs returns up to limit hashes with no version_file
	// reference whose created_at is before the given time.
	UnreferencedBlobs(ctx context.Context, before time.Time, limit int) ([]string, error)

	// DeleteBlob removes the blob row.
	DeleteBlob(ctx context.Context, hash string) error
}
