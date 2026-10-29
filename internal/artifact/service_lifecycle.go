package artifact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/PeterBooker/kazari/internal/audit"
)

const (
	DefaultListLimit = 50
	MaxListLimit     = 200
)

// Get returns an artifact and its versions (newest first, without files).
// Unpublished and soft-deleted artifacts are returned to the actors
// checkReadable allows; tombstones are ErrGone.
func (s *Service) Get(ctx context.Context, actor Actor, slug string) (*Artifact, []Version, error) {
	a, err := s.store.Get(ctx, slug)
	if err != nil {
		return nil, nil, err
	}
	if a.PurgedAt != nil {
		return nil, nil, ErrGone
	}
	if err := s.checkReadable(ctx, actor, a); err != nil {
		return nil, nil, err
	}
	versions, err := s.store.ListVersions(ctx, slug)
	if err != nil {
		return nil, nil, err
	}
	return a, versions, nil
}

// checkReadable hides an unpublished or soft-deleted artifact from all but
// admins, the owner and those who created or updated it, as the listing
// does. Any actor may read a live artifact: every visibility is served to
// anyone signed in.
func (s *Service) checkReadable(ctx context.Context, actor Actor, a *Artifact) error {
	if a.UnpublishedAt == nil && a.DeletedAt == nil {
		return nil
	}
	if actor.Admin || (actor.Handle != "" && actor.Handle == a.OwnerHandle) {
		return nil
	}
	if actor.Handle != "" {
		ok, err := s.store.Involved(ctx, a.Slug, actor.Handle)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}
	return ErrNotFound
}

// List returns one page of artifacts visible to actor. Only admins may set
// q.All.
func (s *Service) List(ctx context.Context, actor Actor, q ListQuery) (ListPage, error) {
	q.Viewer = actor.Handle
	q.All = q.All && actor.Admin
	switch q.Mine {
	case "", "created", "updated", "any":
	default:
		return ListPage{}, Errorf(ErrInvalidArgument, "mine must be created, updated or any")
	}
	switch q.Stale {
	case "", "include", "exclude", "only":
	default:
		return ListPage{}, Errorf(ErrInvalidArgument, "stale must be include, exclude or only")
	}
	switch q.Deleted {
	case "", "exclude", "include", "only":
	default:
		return ListPage{}, Errorf(ErrInvalidArgument, "deleted must be exclude, include or only")
	}
	for k := range q.Metadata {
		if err := ValidateMetadataKey(k); err != nil {
			return ListPage{}, err
		}
	}
	if q.Limit <= 0 {
		q.Limit = DefaultListLimit
	}
	q.Limit = min(q.Limit, MaxListLimit)
	return s.store.List(ctx, q)
}

// Publish serves an existing version, for rollback or roll-forward.
func (s *Service) Publish(ctx context.Context, actor Actor, slug string, version int) (*Artifact, error) {
	return s.modify(ctx, actor, slug, false, audit.ArtifactPublish, func(a *Artifact) (map[string]string, error) {
		// Only a purge removes versions, all at once, so 1..LatestVersion
		// has no gaps.
		if version < 1 || version > a.LatestVersion {
			return nil, Errorf(ErrVersionNotFound, "version %d does not exist", version)
		}
		a.PublishedVersion = version
		return map[string]string{"version": strconv.Itoa(version)}, nil
	})
}

// MarkStale starts the grace period after which Sweep soft-deletes the
// artifact. grace <= 0 uses Options.DefaultGrace. The page keeps serving.
func (s *Service) MarkStale(ctx context.Context, actor Actor, slug string, grace time.Duration) (*Artifact, error) {
	if grace <= 0 {
		grace = s.opts.DefaultGrace
	}
	now := s.now()
	expires := now.Add(grace)
	return s.modify(ctx, actor, slug, false, audit.ArtifactStale, func(a *Artifact) (map[string]string, error) {
		a.StaleAt, a.ExpiresAt = &now, &expires
		return map[string]string{"expires_at": expires.Format(time.RFC3339)}, nil
	})
}

// UnmarkStale cancels a pending expiry.
func (s *Service) UnmarkStale(ctx context.Context, actor Actor, slug string) (*Artifact, error) {
	return s.modify(ctx, actor, slug, false, audit.ArtifactUnstale, func(a *Artifact) (map[string]string, error) {
		a.StaleAt, a.ExpiresAt = nil, nil
		return nil, nil
	})
}

// SetVisibility sets "public", "unlisted" or "members", which also
// re-publishes, or "unpublished", which stops serving and keeps the stored
// visibility.
func (s *Service) SetVisibility(ctx context.Context, actor Actor, slug string, v string) (*Artifact, error) {
	switch v {
	case string(Public), string(Unlisted), string(Members), Unpublished:
	default:
		return nil, Errorf(ErrInvalidArgument, "visibility must be %q, %q, %q or %q", Public, Unlisted, Members, Unpublished)
	}
	return s.modify(ctx, actor, slug, false, audit.ArtifactVisibility, func(a *Artifact) (map[string]string, error) {
		if v == Unpublished {
			if a.UnpublishedAt == nil {
				now := s.now()
				a.UnpublishedAt = &now
			}
		} else {
			a.Visibility = Visibility(v)
			a.UnpublishedAt = nil
		}
		return map[string]string{"visibility": v}, nil
	})
}

// Transfer gives the artifact to newOwner. Only admins and the owner may,
// whatever the write policy; soft-deleted artifacts too, so content can be
// handed over before the purge. An unknown handle is ErrInvalidArgument.
func (s *Service) Transfer(ctx context.Context, actor Actor, slug, newOwner string) (*Artifact, error) {
	if newOwner == "" {
		return nil, Errorf(ErrInvalidArgument, "a new owner is required")
	}
	return s.modify(ctx, actor, slug, true, audit.ArtifactTransfer, func(a *Artifact) (map[string]string, error) {
		if !actor.Admin && actor.Handle != a.OwnerHandle {
			return nil, Errorf(ErrForbidden, "only the owner or an admin may transfer %q", slug)
		}
		from := a.OwnerHandle
		a.OwnerHandle = newOwner
		return map[string]string{"from": from, "to": newOwner}, nil
	})
}

// Delete soft-deletes the artifact, restorable until retention ends, or
// with hard purges it now, leaving the blobs for CollectGarbage.
func (s *Service) Delete(ctx context.Context, actor Actor, slug string, hard bool) error {
	if hard {
		err := s.store.Purge(ctx, slug, s.now(), func(a *Artifact) error {
			return s.checkWritable(actor, a, true)
		})
		if err != nil {
			return err
		}
		s.record(ctx, actor, audit.ArtifactPurge, slug, nil)
		return nil
	}
	_, err := s.modify(ctx, actor, slug, true, audit.ArtifactDelete, func(a *Artifact) (map[string]string, error) {
		if a.DeletedAt != nil {
			return nil, errUnchanged
		}
		now := s.now()
		a.DeletedAt = &now
		return nil, nil
	})
	if errors.Is(err, errUnchanged) {
		return nil
	}
	return err
}

// OpenFile opens a file of a version, 0 meaning the published one. It works
// on unpublished and soft-deleted artifacts for the actors checkReadable
// allows, so content can be recovered before the purge.
func (s *Service) OpenFile(ctx context.Context, actor Actor, slug, p string, version int) (File, io.ReadSeekCloser, error) {
	a, err := s.store.Get(ctx, slug)
	if err != nil {
		return File{}, nil, err
	}
	if a.PurgedAt != nil {
		return File{}, nil, ErrGone
	}
	if err := s.checkReadable(ctx, actor, a); err != nil {
		return File{}, nil, err
	}
	clean, err := ValidatePath(p)
	if err != nil {
		return File{}, nil, err
	}
	if version == 0 {
		version = a.PublishedVersion
	}
	v, err := s.store.GetVersion(ctx, slug, version)
	if err != nil {
		return File{}, nil, err
	}
	for _, f := range v.Files {
		if f.Path != clean {
			continue
		}
		rc, err := s.blobs.Open(ctx, f.Hash)
		if err != nil {
			return File{}, nil, fmt.Errorf("artifact: open %s/%s: %w", slug, clean, err)
		}
		return f, rc, nil
	}
	return File{}, nil, Errorf(ErrFileNotFound, "no file %q in version %d", clean, version)
}

// ResolvePublished returns the serving record for the published version.
func (s *Service) ResolvePublished(ctx context.Context, slug string) (*Published, error) {
	p, err := s.store.ResolvePublished(ctx, slug)
	if err != nil {
		return nil, err
	}
	p.DeriveFlags()
	return p, nil
}

// ResolveVersion returns the serving record for a pinned version.
func (s *Service) ResolveVersion(ctx context.Context, slug string, n int) (*Published, error) {
	p, err := s.store.ResolveVersion(ctx, slug, n)
	if err != nil {
		return nil, err
	}
	p.DeriveFlags()
	return p, nil
}

// Files returns a version's file map keyed by path.
func (s *Service) Files(ctx context.Context, versionID int64) (map[string]File, error) {
	return s.store.Files(ctx, versionID)
}

// Sweep soft-deletes expired artifacts, purges those past retention and
// frees old tombstones, carrying on past failures to return the first.
func (s *Service) Sweep(ctx context.Context) (SweepReport, error) {
	var (
		rep      SweepReport
		firstErr error
	)
	fail := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	now := s.now()

	expired, err := s.store.ListExpired(ctx, now)
	if err != nil {
		fail(err)
	}
	for _, a := range expired {
		err := s.store.Update(ctx, a.Slug, "", func(cur *Artifact) error {
			// The row may have changed since the listing.
			if cur.ExpiresAt == nil || cur.ExpiresAt.After(now) || cur.DeletedAt != nil || cur.PurgedAt != nil {
				return errUnchanged
			}
			cur.DeletedAt = &now
			cur.UpdatedAt = now
			return nil
		})
		if errors.Is(err, errUnchanged) {
			continue
		}
		if err != nil {
			fail(fmt.Errorf("artifact: sweep expire %s: %w", a.Slug, err))
			continue
		}
		rep.SoftDeleted++
		s.record(ctx, System, audit.ArtifactDelete, a.Slug, map[string]string{"reason": "expired"})
	}

	retained := now.Add(-s.opts.Retention)
	due, err := s.store.ListRetentionDue(ctx, retained)
	if err != nil {
		fail(err)
	}
	for _, a := range due {
		err := s.store.Purge(ctx, a.Slug, now, func(cur *Artifact) error {
			// The row may have been restored or purged since the listing.
			if cur.DeletedAt == nil || cur.DeletedAt.After(retained) || cur.PurgedAt != nil {
				return errUnchanged
			}
			return nil
		})
		if errors.Is(err, errUnchanged) {
			continue
		}
		if err != nil {
			fail(fmt.Errorf("artifact: sweep purge %s: %w", a.Slug, err))
			continue
		}
		rep.Purged++
		s.record(ctx, System, audit.ArtifactPurge, a.Slug, map[string]string{"reason": "retention"})
	}

	tombs, err := s.store.ListTombstonesDue(ctx, now.Add(-s.opts.Tombstone))
	if err != nil {
		fail(err)
	}
	for _, a := range tombs {
		if err := s.store.DeleteTombstone(ctx, a.Slug); err != nil {
			fail(fmt.Errorf("artifact: sweep free %s: %w", a.Slug, err))
			continue
		}
		rep.Freed++
		s.record(ctx, System, audit.ArtifactFree, a.Slug, nil)
	}
	return rep, firstErr
}

// CollectGarbage deletes unreferenced blobs last recorded before the
// BlobGrace window and returns how many. The file goes before the row, so a
// crash leaves a row the next run retries.
func (s *Service) CollectGarbage(ctx context.Context) (int, error) {
	before := s.now().Add(-s.opts.BlobGrace)
	var n int
	for {
		deleted, more, err := s.collectBatch(ctx, before)
		n += deleted
		if err != nil || !more {
			return n, err
		}
	}
}

// collectBatch holds blobMu from the query to the last delete, so no
// publish can reuse a blob it selected. A failing batch ends the run, so a
// persistent error cannot loop forever.
func (s *Service) collectBatch(ctx context.Context, before time.Time) (int, bool, error) {
	s.blobMu.Lock()
	defer s.blobMu.Unlock()
	hashes, err := s.store.UnreferencedBlobs(ctx, before, gcBatch)
	if err != nil {
		return 0, false, err
	}
	var (
		n        int
		firstErr error
	)
	for _, h := range hashes {
		if err := s.blobs.Delete(ctx, h); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("artifact: gc blob %s: %w", h, err)
			}
			continue
		}
		if err := s.store.DeleteBlob(ctx, h); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("artifact: gc blob row %s: %w", h, err)
			}
			continue
		}
		n++
	}
	return n, len(hashes) == gcBatch, firstErr
}

// Usage sums every version of the handle's artifacts, for quota reporting.
func (s *Service) Usage(ctx context.Context, ownerHandle string) (int64, error) {
	return s.store.Usage(ctx, ownerHandle)
}
