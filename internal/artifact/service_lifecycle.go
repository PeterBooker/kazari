package artifact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"kazari/internal/audit"
)

// List limits applied by Service.List.
const (
	DefaultListLimit = 50
	MaxListLimit     = 200
)

// Get returns an artifact and its versions (newest first, without files).
// Unpublished and soft-deleted artifacts are returned, so their state can
// be shown, to the actors checkReadable allows; tombstones are ErrGone.
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

// checkReadable hides an unpublished or soft-deleted artifact from everyone
// but admins, its owner and the users who created or updated it, the rule
// the listing applies. A live artifact, listed or not, is served to anyone
// and so is readable by any actor.
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

// Publish points the served version at an existing version, for rollback
// or roll-forward.
func (s *Service) Publish(ctx context.Context, actor Actor, slug string, version int) (*Artifact, error) {
	return s.modify(ctx, actor, slug, false, audit.ArtifactPublish, func(a *Artifact) (map[string]string, error) {
		// Versions are numbered 1..LatestVersion and only removed all at
		// once by a purge, so the range check is exact.
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

// SetVisibility sets "public" or "unlisted" (which also re-publishes an
// unpublished artifact) or "unpublished" (which stops serving it and keeps
// the stored visibility for when it comes back).
func (s *Service) SetVisibility(ctx context.Context, actor Actor, slug string, v string) (*Artifact, error) {
	switch v {
	case string(Public), string(Unlisted), Unpublished:
	default:
		return nil, Errorf(ErrInvalidArgument, "visibility must be %q, %q or %q", Public, Unlisted, Unpublished)
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

// Transfer gives the artifact to newOwner. Only admins and the current
// owner may transfer, whatever the write policy. Soft-deleted artifacts can
// be transferred so an admin can hand content over before the purge. An
// unknown handle is ErrInvalidArgument (the store resolves it).
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

// Delete soft-deletes the artifact (restorable until the retention period
// ends) or, with hard, purges it now. A hard delete leaves the blobs for
// CollectGarbage.
func (s *Service) Delete(ctx context.Context, actor Actor, slug string, hard bool) error {
	if hard {
		a, err := s.store.Get(ctx, slug)
		if err != nil {
			return err
		}
		if err := s.checkWritable(actor, a, true); err != nil {
			return err
		}
		if err := s.store.Purge(ctx, slug, s.now()); err != nil {
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

// OpenFile opens one file of a version; version 0 means the published one.
// It works on unpublished and soft-deleted artifacts for the actors
// checkReadable allows, so an owner can recover content before the purge.
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

// Sweep advances the lifecycle: stale artifacts past expiry are
// soft-deleted, soft-deleted artifacts past retention are purged, and
// tombstones past the tombstone period are removed. It carries on past
// individual failures and returns the first error.
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
			// The row may have changed since the listing: unmarked, given
			// a later expiry, or deleted.
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

	due, err := s.store.ListRetentionDue(ctx, now.Add(-s.opts.Retention))
	if err != nil {
		fail(err)
	}
	for _, a := range due {
		if err := s.store.Purge(ctx, a.Slug, now); err != nil {
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

// CollectGarbage deletes blobs that no version references and that were
// last recorded before the BlobGrace window. The file goes first and the row
// second, so a crash leaves a row that the next run retries. Each batch
// holds blobMu exclusively from the query to the last delete, so no publish
// can reuse a blob the batch selected. It returns the number of blobs
// deleted.
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

// collectBatch deletes up to gcBatch blobs and reports whether another
// batch may follow. It stops at a failing batch, so a persistent error
// does not loop forever, and returns the first error.
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
	// A short batch is the last.
	return n, len(hashes) == gcBatch, firstErr
}

// Usage returns the total size of every version owned by the handle, for
// quota reporting.
func (s *Service) Usage(ctx context.Context, ownerHandle string) (int64, error) {
	return s.store.Usage(ctx, ownerHandle)
}
