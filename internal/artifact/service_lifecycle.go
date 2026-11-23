package artifact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/PeterBooker/kazari/internal/audit"
	"github.com/PeterBooker/kazari/internal/blob"
)

const (
	DefaultListLimit = 50
	MaxListLimit     = 200
)

// Get returns an artifact and its versions (newest first, without files).
// Unpublished and soft-deleted artifacts are returned to the actors
// checkReadable allows; tombstones are ErrGone.
func (s *Service) Get(ctx context.Context, actor Actor, slug string) (_ *Artifact, _ []Version, err error) {
	ctx, span := s.span(ctx, "artifact.get", slug)
	defer func() { endSpan(span, err) }()
	if err := checkSlug(actor, slug); err != nil {
		return nil, nil, err
	}
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

// checkReadable is canRead with the involvement looked up: an unpublished
// or soft-deleted artifact is hidden from all but admins, the owner and
// those who created or updated it, as the listing does. Any actor may read
// a live artifact: every visibility is served to anyone signed in.
func (s *Service) checkReadable(ctx context.Context, actor Actor, a *Artifact) error {
	involved, err := s.involved(ctx, actor, a.Slug)
	if err != nil {
		return err
	}
	if !canRead(actor, a, involved) {
		return ErrNotFound
	}
	return nil
}

// List returns one page of artifacts visible to actor. Only admins may set
// q.All.
func (s *Service) List(ctx context.Context, actor Actor, q ListQuery) (_ ListPage, err error) {
	ctx, span := s.span(ctx, "artifact.list", "")
	defer func() { endSpan(span, err) }()
	q.Viewer = actor.Handle
	q.All = q.All && actor.Admin
	q.Slugs = actor.Slugs
	q.Search = strings.TrimSpace(q.Search)
	if err := q.Validate(); err != nil {
		return ListPage{}, err
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

// Publish serves an existing version: a rollback, a roll-forward, or a
// staged version going live.
func (s *Service) Publish(ctx context.Context, actor Actor, slug string, version int) (_ *Artifact, err error) {
	ctx, span := s.span(ctx, "artifact.publish", slug)
	defer func() {
		s.published(ctx, publishRollback, nil, err)
		span.SetAttributes(versionKey.Int(version))
		endSpan(span, err)
	}()
	var outOfRange bool
	a, err := s.modify(ctx, actor, slug, false, audit.ArtifactPublish, func(a *Artifact) (map[string]string, error) {
		if version < 1 || version > a.LatestVersion {
			outOfRange = true
			return nil, Errorf(ErrVersionNotFound, "version %d does not exist", version)
		}
		a.PublishedVersion = version
		return map[string]string{"version": strconv.Itoa(version)}, nil
	})
	// The store found no version for a number below the latest.
	if errors.Is(err, ErrVersionNotFound) && !outOfRange {
		return nil, Errorf(ErrVersionNotFound, "version %d of %s was deleted", version, slug)
	}
	return a, err
}

// DeleteVersion deletes version n, which must be neither the published
// version nor the latest. Its host answers 410 from then on, and its blobs
// go at the next garbage collection unless another version holds them.
func (s *Service) DeleteVersion(ctx context.Context, actor Actor, slug string, n int) (_ *Artifact, err error) {
	ctx, span := s.span(ctx, "artifact.delete_version", slug)
	defer func() { span.SetAttributes(versionKey.Int(n)); endSpan(span, err) }()
	a, _, err := s.deleteVersions(ctx, actor, slug, nil, false, func(a *Artifact, vs []Version) ([]int, error) {
		if !slices.ContainsFunc(vs, func(v Version) bool { return v.Number == n }) {
			if n >= 1 && n < a.LatestVersion {
				return nil, Errorf(ErrVersionNotFound, "version %d of %s was already deleted", n, slug)
			}
			return nil, Errorf(ErrVersionNotFound, "%s has no version %d; its latest is %d", slug, n, a.LatestVersion)
		}
		switch n {
		case a.PublishedVersion:
			return nil, Errorf(ErrInvalidArgument, "version %d of %s is published; publish another version before deleting it", n, slug)
		case a.LatestVersion:
			return nil, Errorf(ErrInvalidArgument, "version %d of %s is the latest, which is kept so that its number "+
				"never names other content", n, slug)
		}
		return []int{n}, nil
	})
	return a, err
}

// PruneVersions deletes every version but the keep newest, the published
// version and the staged versions newer than it, which wait for review, and
// returns the numbers deleted, oldest first. With dryRun it deletes nothing
// and returns the numbers it would delete.
func (s *Service) PruneVersions(ctx context.Context, actor Actor, slug string, keep int, dryRun bool) (_ *Artifact, _ []int, err error) {
	ctx, span := s.span(ctx, "artifact.prune_versions", slug)
	defer func() { endSpan(span, err) }()
	if keep < 1 || keep > MaxKeepVersions {
		return nil, nil, Errorf(ErrInvalidArgument, "keep must be a whole number from 1 to %d", MaxKeepVersions)
	}
	return s.deleteVersions(ctx, actor, slug, nil, dryRun, func(a *Artifact, vs []Version) ([]int, error) {
		return prunable(a, vs, keep), nil
	})
}

// prunable is what a prune that keeps keep versions deletes, oldest first;
// vs is newest first.
func prunable(a *Artifact, vs []Version, keep int) []int {
	var out []int
	for i := len(vs) - 1; i >= keep; i-- {
		v := vs[i]
		if v.Number == a.PublishedVersion || v.Number == a.LatestVersion || (v.Staged && v.Number > a.PublishedVersion) {
			continue
		}
		out = append(out, v.Number)
	}
	return out
}

// keepVersions is the artifact's MetaKeepVersions, if it sets a valid one.
func keepVersions(a *Artifact) (int, bool) {
	keep, err := ParseKeepVersions(a.Metadata[MetaKeepVersions])
	return keep, err == nil
}

// errDryRun from a DeleteVersions func rolls the deletion back.
var errDryRun = errors.New("artifact: dry run")

// deleteVersions deletes the versions choose returns, after the checks of a
// change and the rule MarkStale has: deleting for good takes the delete
// scope on an artifact the actor does not own.
func (s *Service) deleteVersions(ctx context.Context, actor Actor, slug string, details map[string]string, dryRun bool,
	choose func(a *Artifact, vs []Version) ([]int, error)) (*Artifact, []int, error) {
	if err := checkSlug(actor, slug); err != nil {
		return nil, nil, err
	}
	involved, err := s.involved(ctx, actor, slug)
	if err != nil {
		return nil, nil, err
	}
	var planned []int
	deleted, err := s.store.DeleteVersions(ctx, slug, func(a *Artifact, vs []Version) ([]int, error) {
		if err := s.checkWritable(actor, a, involved, false); err != nil {
			return nil, err
		}
		if !mayDelete(actor, a) {
			return nil, Errorf(ErrForbidden, "deleting versions of %q needs the artifacts:delete scope or ownership", slug)
		}
		ns, err := choose(a, vs)
		if err != nil || !dryRun {
			return ns, err
		}
		planned = ns
		return nil, errDryRun
	})
	if errors.Is(err, errDryRun) {
		deleted, err = planned, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if !dryRun && len(deleted) > 0 {
		d := maps.Clone(details)
		if d == nil {
			d = map[string]string{}
		}
		parts := make([]string, len(deleted))
		for i, n := range deleted {
			parts[i] = strconv.Itoa(n)
		}
		d["versions"] = strings.Join(parts, ",")
		s.record(ctx, actor, audit.ArtifactVersionDelete, slug, d)
	}
	a, err := s.store.Get(ctx, slug)
	if err != nil {
		return nil, nil, err
	}
	return a, deleted, nil
}

// MarkStale starts the grace period after which Sweep soft-deletes the
// artifact. grace <= 0 uses Options.DefaultGrace. The page keeps serving.
// An expiry ends in a purge, so on an artifact the actor does not own it
// takes the delete scope as well as the write policy; owners may retire
// their own artifacts without it, as CI keys do. Being involved is not
// owning: any update makes an actor involved.
func (s *Service) MarkStale(ctx context.Context, actor Actor, slug string, grace time.Duration) (_ *Artifact, err error) {
	ctx, span := s.span(ctx, "artifact.mark_stale", slug)
	defer func() { endSpan(span, err) }()
	if grace <= 0 {
		grace = s.opts.DefaultGrace
	}
	now := s.now()
	expires := now.Add(grace)
	return s.modify(ctx, actor, slug, false, audit.ArtifactStale, func(a *Artifact) (map[string]string, error) {
		if !mayDelete(actor, a) {
			return nil, Errorf(ErrForbidden, "marking %q stale needs the artifacts:delete scope or ownership", slug)
		}
		a.StaleAt, a.ExpiresAt = &now, &expires
		return map[string]string{"expires_at": expires.Format(time.RFC3339)}, nil
	})
}

// UnmarkStale cancels a pending expiry.
func (s *Service) UnmarkStale(ctx context.Context, actor Actor, slug string) (_ *Artifact, err error) {
	ctx, span := s.span(ctx, "artifact.unmark_stale", slug)
	defer func() { endSpan(span, err) }()
	return s.modify(ctx, actor, slug, false, audit.ArtifactUnstale, func(a *Artifact) (map[string]string, error) {
		a.StaleAt, a.ExpiresAt = nil, nil
		return nil, nil
	})
}

// SetVisibility sets "public", "unlisted" or "members", which also
// re-publishes, or "unpublished", which stops serving and keeps the stored
// visibility.
func (s *Service) SetVisibility(ctx context.Context, actor Actor, slug string, v string) (_ *Artifact, err error) {
	ctx, span := s.span(ctx, "artifact.set_visibility", slug)
	defer func() { endSpan(span, err) }()
	if err := checkSlug(actor, slug); err != nil {
		return nil, err
	}
	if err := checkVisibility(v); err != nil {
		return nil, err
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

func checkVisibility(v string) error {
	switch v {
	case string(Public), string(Unlisted), string(Members), Unpublished:
		return nil
	}
	return Errorf(ErrInvalidArgument, "visibility must be %q, %q, %q or %q%s", Public, Unlisted, Members, Unpublished, VisibilityHint(v))
}

// Transfer gives the artifact to newOwner. Only admins may, whatever the
// write policy: the versions move onto the new owner's quota, and nothing
// asks that owner. Soft-deleted artifacts can be transferred too, so content
// can be handed over before the purge. An unknown handle is
// ErrInvalidArgument.
func (s *Service) Transfer(ctx context.Context, actor Actor, slug, newOwner string) (_ *Artifact, err error) {
	ctx, span := s.span(ctx, "artifact.transfer", slug)
	defer func() { endSpan(span, err) }()
	if err := checkSlug(actor, slug); err != nil {
		return nil, err
	}
	if newOwner == "" {
		return nil, Errorf(ErrInvalidArgument, "a new owner is required")
	}
	if !actor.Admin {
		return nil, Errorf(ErrForbidden, "only an admin may transfer %q", slug)
	}
	return s.modify(ctx, actor, slug, true, audit.ArtifactTransfer, func(a *Artifact) (map[string]string, error) {
		from := a.OwnerHandle
		a.OwnerHandle = newOwner
		return map[string]string{"from": from, "to": newOwner}, nil
	})
}

// TransferAll gives every artifact from owns, soft-deleted ones included,
// to newOwner in one step, for handing a person's work over before they
// leave. Like Transfer it is for admins alone, and a credential restricted
// to certain slugs cannot reach every artifact, so it is refused. It returns
// the slugs moved, in order. An unknown handle is ErrInvalidArgument.
func (s *Service) TransferAll(ctx context.Context, actor Actor, from, newOwner string) (_ []string, err error) {
	ctx, span := s.span(ctx, "artifact.transfer_all", "")
	defer func() { endSpan(span, err) }()
	if from == "" || newOwner == "" {
		return nil, Errorf(ErrInvalidArgument, "the current owner and a new owner are required")
	}
	if from == newOwner {
		return nil, Errorf(ErrInvalidArgument, "%q already owns these artifacts", from)
	}
	if !actor.Admin {
		return nil, Errorf(ErrForbidden, "only an admin may transfer the artifacts of %q", from)
	}
	if len(actor.Slugs) > 0 {
		return nil, Errorf(ErrForbidden, "this credential is restricted to slugs matching %s, so it cannot transfer every artifact of %q",
			strings.Join(actor.Slugs, ", "), from)
	}
	by := actor.Handle
	if actor.Operator {
		by = ""
	}
	moved, err := s.store.TransferOwned(ctx, from, newOwner, by, s.now())
	if err != nil {
		return nil, err
	}
	details := map[string]string{"from": from, "to": newOwner}
	for _, slug := range moved {
		s.record(ctx, actor, audit.ArtifactTransfer, slug, details)
	}
	return moved, nil
}

// Delete soft-deletes the artifact, restorable until retention ends, or
// with hard purges it now, leaving the blobs for CollectGarbage.
func (s *Service) Delete(ctx context.Context, actor Actor, slug string, hard bool) (err error) {
	ctx, span := s.span(ctx, "artifact.delete", slug)
	defer func() { endSpan(span, err) }()
	if err := checkSlug(actor, slug); err != nil {
		return err
	}
	if hard {
		involved, err := s.involved(ctx, actor, slug)
		if err != nil {
			return err
		}
		err = s.store.Purge(ctx, slug, s.now(), func(a *Artifact) error {
			return s.checkWritable(actor, a, involved, true)
		})
		if err != nil {
			return err
		}
		s.record(ctx, actor, audit.ArtifactPurge, slug, nil)
		return nil
	}
	_, err = s.modify(ctx, actor, slug, true, audit.ArtifactDelete, func(a *Artifact) (map[string]string, error) {
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
func (s *Service) OpenFile(ctx context.Context, actor Actor, slug, p string, version int) (_ File, _ io.ReadSeekCloser, err error) {
	ctx, span := s.span(ctx, "artifact.open_file", slug)
	defer func() { endSpan(span, err) }()
	a, err := s.readableArtifact(ctx, actor, slug)
	if err != nil {
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
	return File{}, nil, Errorf(ErrFileNotFound, "no file %q in version %d", clean, v.Number)
}

// FileOpener opens the content of one of a version's files.
type FileOpener func(ctx context.Context, f File) (io.ReadCloser, error)

// OpenVersion returns a version with its Files, 0 meaning the published one,
// and an opener for their content, to the actors OpenFile serves.
func (s *Service) OpenVersion(ctx context.Context, actor Actor, slug string, version int) (_ *Version, _ FileOpener, err error) {
	ctx, span := s.span(ctx, "artifact.open_version", slug)
	defer func() { endSpan(span, err) }()
	a, err := s.readableArtifact(ctx, actor, slug)
	if err != nil {
		return nil, nil, err
	}
	if version == 0 {
		version = a.PublishedVersion
	}
	v, err := s.store.GetVersion(ctx, slug, version)
	if err != nil {
		return nil, nil, err
	}
	open := func(ctx context.Context, f File) (io.ReadCloser, error) {
		rc, err := s.blobs.Open(ctx, f.Hash)
		if err != nil {
			return nil, fmt.Errorf("artifact: open %s/%s: %w", slug, f.Path, err)
		}
		return rc, nil
	}
	return v, open, nil
}

func (s *Service) readableArtifact(ctx context.Context, actor Actor, slug string) (*Artifact, error) {
	if err := checkSlug(actor, slug); err != nil {
		return nil, err
	}
	a, err := s.store.Get(ctx, slug)
	if err != nil {
		return nil, err
	}
	if a.PurgedAt != nil {
		return nil, ErrGone
	}
	if err := s.checkReadable(ctx, actor, a); err != nil {
		return nil, err
	}
	return a, nil
}

// WorkerControlled reports whether the person who uploaded the version's file
// at path could still change the artifact, the test a service worker must
// pass to stay in a browser. It looks at the upload and not the latest
// version, which carries unchanged files on: a script stays its author's
// until someone replaces its content.
func (s *Service) WorkerControlled(ctx context.Context, versionID int64, path string) (bool, error) {
	o, err := s.store.FileOrigin(ctx, versionID, path)
	if err != nil {
		return false, err
	}
	if o.Disabled {
		return false, nil
	}
	return s.canWrite(Actor{Handle: o.Author, Admin: o.Admin}, &Artifact{OwnerHandle: o.Owner}), nil
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
	files, err := s.store.Files(ctx, versionID)
	if err != nil {
		return nil, err
	}
	m := make(map[string]File, len(files))
	for _, f := range files {
		m[f.Path] = f
	}
	return m, nil
}

// ListFiles sets Files on vs, which Get returns newest first, from the first
// version numbered below before (the newest when 0) until the next version
// would take the page past MaxListedFiles; a page always lists one version.
// It returns the before of the next page, 0 on the last. Unbounded, a listing
// would cost versions times files in one read.
func (s *Service) ListFiles(ctx context.Context, vs []Version, before int) (int, error) {
	if before < 0 {
		return 0, Errorf(ErrInvalidArgument, "files_before must not be negative")
	}
	listed, last := 0, 0
	for i := range vs {
		if before != 0 && vs[i].Number >= before {
			continue
		}
		files, err := s.store.Files(ctx, vs[i].ID)
		if err != nil {
			return 0, err
		}
		if last != 0 && listed+len(files) > MaxListedFiles {
			return last, nil
		}
		vs[i].Files = files
		listed += len(files)
		last = vs[i].Number
	}
	return 0, nil
}

// Sweep soft-deletes expired artifacts, purges those past retention and
// frees old tombstones, carrying on past failures to return the first.
func (s *Service) Sweep(ctx context.Context) (report SweepReport, err error) {
	ctx, span := s.span(ctx, "artifact.sweep", "")
	defer func() { sweepAttrs(span, report); endSpan(span, err) }()
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

	kept, err := s.store.ListKeepVersions(ctx)
	if err != nil {
		fail(err)
	}
	for _, a := range kept {
		// Fewer versions than the number is nothing to prune.
		if keep, ok := keepVersions(&a); !ok || a.VersionCount <= keep {
			continue
		}
		_, n, err := s.deleteVersions(ctx, System, a.Slug, map[string]string{"reason": "keep_versions"}, false,
			func(cur *Artifact, vs []Version) ([]int, error) {
				// The row may have changed since the listing.
				keep, ok := keepVersions(cur)
				if !ok {
					return nil, nil
				}
				return prunable(cur, vs, keep), nil
			})
		// Deleted, purged or freed since the listing: nothing left to prune.
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrGone) {
			continue
		}
		if err != nil {
			fail(fmt.Errorf("artifact: sweep prune %s: %w", a.Slug, err))
			continue
		}
		rep.VersionsDeleted += len(n)
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
func (s *Service) CollectGarbage(ctx context.Context) (n int, err error) {
	ctx, span := s.span(ctx, "artifact.collect_garbage", "")
	defer func() { span.SetAttributes(attribute.Int("kazari.gc.blobs_deleted_count", n)); endSpan(span, err) }()
	before := s.now().Add(-s.opts.BlobGrace)
	for {
		deleted, more, err := s.collectBatch(ctx, before)
		n += deleted
		if err != nil || !more {
			return n, err
		}
	}
}

// collectBatch deletes the files of one batch, then their rows in one
// transaction. A failing batch ends the run, so a persistent error cannot
// loop forever.
func (s *Service) collectBatch(ctx context.Context, before time.Time) (int, bool, error) {
	hashes, deleted, err := s.deleteUnreferenced(ctx, before)
	if len(deleted) == 0 {
		return 0, len(hashes) == gcBatch, err
	}
	// After the lock: a publish since then found each file gone and wrote
	// it again, and DeleteBlobs keeps the row it recorded or references.
	n, rerr := s.index.DeleteBlobs(ctx, deleted, before)
	if rerr != nil {
		rerr = fmt.Errorf("artifact: gc blob rows: %w", rerr)
	}
	return n, len(hashes) == gcBatch, errors.Join(err, rerr)
}

// deleteUnreferenced holds blobMu from the query to the last file deleted:
// a publish's Put of content still stored writes nothing, so a file deleted
// after it would leave its version without one. It returns the hashes
// selected and those whose files are gone.
func (s *Service) deleteUnreferenced(ctx context.Context, before time.Time) (hashes, deleted []string, err error) {
	s.blobMu.Lock()
	defer s.blobMu.Unlock()
	hashes, err = s.index.UnreferencedBlobs(ctx, before, gcBatch)
	if err != nil {
		return nil, nil, err
	}
	deleted = make([]string, 0, len(hashes))
	for _, h := range hashes {
		if derr := s.blobs.Delete(ctx, h); derr != nil {
			if err == nil {
				err = fmt.Errorf("artifact: gc blob %s: %w", h, derr)
			}
			continue
		}
		deleted = append(deleted, h)
	}
	return hashes, deleted, err
}

// SweepOrphans removes the stored blob files that no row records and that
// were last written before the OrphanAge window: what a publish interrupted
// by a crash before it recorded them leaves, or a restore that brought back
// files the database does not know. CollectGarbage works from the rows and
// never sees them. A sweep runs at most once per OrphanSweepInterval; a
// call before then does nothing. It carries on past failures to return the
// first.
func (s *Service) SweepOrphans(ctx context.Context) (rep OrphanReport, err error) {
	now := s.now()
	s.orphanMu.Lock()
	due := s.orphansSwept.IsZero() || !now.Before(s.orphansSwept.Add(OrphanSweepInterval))
	if due {
		s.orphansSwept = now
	}
	s.orphanMu.Unlock()
	if !due {
		return OrphanReport{}, nil
	}

	ctx, span := s.span(ctx, "artifact.sweep_orphans", "")
	defer func() {
		s.m.orphans.Add(ctx, int64(rep.Removed))
		s.m.orphanBytes.Add(ctx, rep.Bytes)
		orphanAttrs(span, rep)
		endSpan(span, err)
	}()
	rep.Ran = true
	before := now.Add(-s.opts.OrphanAge)
	var firstErr error
	fail := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	old := make([]blob.Info, 0, gcBatch)
	for info, err := range s.blobs.List(ctx) {
		if err != nil {
			fail(fmt.Errorf("artifact: orphan sweep: %w", err))
			break
		}
		if !info.ModTime.Before(before) {
			continue
		}
		old = append(old, info)
		if len(old) == gcBatch {
			if err := s.removeOrphans(ctx, old, &rep); err != nil {
				fail(err)
			}
			old = old[:0]
		}
	}
	if err := s.removeOrphans(ctx, old, &rep); err != nil {
		fail(err)
	}
	return rep, firstErr
}

// removeOrphans deletes the files of old that no row records. The lookup
// before the lock spares publishes a wait for a batch that is all
// recorded. The one under it sees every row a publish recorded since the
// walk, and the lock waits out the publishes in progress and keeps new
// ones from a Put until the files are gone: a Put of content still stored
// writes nothing, so a file deleted after it would leave its version
// without one.
func (s *Service) removeOrphans(ctx context.Context, old []blob.Info, rep *OrphanReport) error {
	if len(old) == 0 {
		return nil
	}
	unrecorded, err := s.unrecorded(ctx, old)
	if err != nil || len(unrecorded) == 0 {
		return err
	}
	s.blobMu.Lock()
	defer s.blobMu.Unlock()
	unrecorded, err = s.unrecorded(ctx, unrecorded)
	if err != nil {
		return err
	}
	var firstErr error
	for _, b := range unrecorded {
		if err := s.blobs.Delete(ctx, b.Hash); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("artifact: orphan sweep %s: %w", b.Hash, err)
			}
			continue
		}
		rep.Removed++
		rep.Bytes += b.Size
	}
	return firstErr
}

// unrecorded returns the blobs of bs that have no row, in one query.
func (s *Service) unrecorded(ctx context.Context, bs []blob.Info) ([]blob.Info, error) {
	hashes := make([]string, len(bs))
	for i, b := range bs {
		hashes[i] = b.Hash
	}
	known, err := s.index.KnownBlobs(ctx, hashes)
	if err != nil {
		return nil, fmt.Errorf("artifact: orphan sweep rows: %w", err)
	}
	return slices.DeleteFunc(slices.Clone(bs), func(b blob.Info) bool { return known[b.Hash] }), nil
}

// Usage sums every version of the handle's artifacts, for quota reporting.
func (s *Service) Usage(ctx context.Context, ownerHandle string) (int64, error) {
	return s.store.Usage(ctx, ownerHandle)
}

// OwnerUsage returns what every owner holds: their artifacts, soft-deleted
// ones included, and the bytes charged to their quota. A user who owns
// nothing is absent.
func (s *Service) OwnerUsage(ctx context.Context) ([]OwnerUsage, error) {
	return s.store.OwnerUsage(ctx)
}

// Census counts every artifact, tombstones included, by Status and
// visibility.
func (s *Service) Census(ctx context.Context) ([]StatusCount, error) {
	return s.store.CountArtifacts(ctx)
}

// Storage returns the versions with their logical size, which quotas count,
// and the blobs with what they occupy once each is stored once.
func (s *Service) Storage(ctx context.Context) (versions, blobs Totals, err error) {
	if versions, err = s.store.VersionTotals(ctx); err != nil {
		return Totals{}, Totals{}, err
	}
	if blobs, err = s.index.BlobTotals(ctx); err != nil {
		return Totals{}, Totals{}, err
	}
	return versions, blobs, nil
}
