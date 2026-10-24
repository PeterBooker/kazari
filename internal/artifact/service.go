package artifact

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"strconv"
	"time"

	"agalma/internal/audit"
	"agalma/internal/blob"
)

// Write policies for Options.WritePolicy.
const (
	// WriteShared lets any authenticated actor change any artifact.
	WriteShared = "shared"
	// WriteOwner limits changes to the owner and admins.
	WriteOwner = "owner"
)

// Defaults applied by New to zero-valued Options fields.
const (
	DefaultMaxVersionBytes = 50 << 20
	DefaultMaxFileBytes    = 25 << 20
	DefaultGrace           = 14 * 24 * time.Hour
	DefaultRetention       = 30 * 24 * time.Hour
	DefaultTombstone       = 90 * 24 * time.Hour
	DefaultBlobGrace       = time.Hour
)

// gcBatch is how many blobs CollectGarbage asks for at a time.
const gcBatch = 1000

// Options configures a Service. Zero values get the defaults above.
type Options struct {
	MaxVersionBytes  int64 // total size of one version
	MaxFileBytes     int64 // size of one file
	MaxFiles         int   // files in one version
	MaxMetadataPairs int
	ReservedSlugs    []string
	DefaultGrace     time.Duration // MarkStale grace when the caller gives none
	Retention        time.Duration // soft-deleted -> purged
	Tombstone        time.Duration // purged -> slug freed
	BlobGrace        time.Duration // how long an unreferenced blob survives GC
	WritePolicy      string        // WriteShared (default) or WriteOwner
	Now              func() time.Time
	Logger           *slog.Logger // audit failures are logged here; default slog.Default()
}

func (o Options) withDefaults() Options {
	if o.MaxVersionBytes <= 0 {
		o.MaxVersionBytes = DefaultMaxVersionBytes
	}
	if o.MaxFileBytes <= 0 {
		o.MaxFileBytes = DefaultMaxFileBytes
	}
	if o.MaxFiles <= 0 {
		o.MaxFiles = DefaultMaxFiles
	}
	if o.MaxMetadataPairs <= 0 {
		o.MaxMetadataPairs = DefaultMaxPairs
	}
	if o.DefaultGrace <= 0 {
		o.DefaultGrace = DefaultGrace
	}
	if o.Retention <= 0 {
		o.Retention = DefaultRetention
	}
	if o.Tombstone <= 0 {
		o.Tombstone = DefaultTombstone
	}
	if o.BlobGrace <= 0 {
		o.BlobGrace = DefaultBlobGrace
	}
	if o.WritePolicy == "" {
		o.WritePolicy = WriteShared
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return o
}

// Service implements the artifact operations on top of a Store and a blob
// store. It is safe for concurrent use; the store resolves races.
type Service struct {
	store Store
	blobs blob.Store
	rec   audit.Recorder
	opts  Options
}

// New returns a Service. rec may be nil, which discards audit events.
func New(store Store, blobs blob.Store, rec audit.Recorder, opts Options) *Service {
	if rec == nil {
		rec = audit.Nop{}
	}
	return &Service{store: store, blobs: blobs, rec: rec, opts: opts.withDefaults()}
}

func (s *Service) now() time.Time { return s.opts.Now().UTC() }

// canWrite is the single write-policy check for every mutation.
func (s *Service) canWrite(actor Actor, a *Artifact) bool {
	if s.opts.WritePolicy == WriteShared {
		return true
	}
	return actor.Admin || actor.Handle == a.OwnerHandle
}

// record writes an audit event. The store change it describes has already
// happened, so a failure is logged rather than returned.
func (s *Service) record(ctx context.Context, actor Actor, action, slug string, details map[string]string) {
	e := audit.Event{At: s.now(), Actor: actor.Handle, Action: action, Slug: slug, Details: details}
	if err := s.rec.Record(ctx, e); err != nil {
		s.opts.Logger.ErrorContext(ctx, "audit record failed", "action", action, "slug", slug, "err", err)
	}
}

// Create creates an artifact with its first version, owned by actor.
func (s *Service) Create(ctx context.Context, actor Actor, req CreateRequest) (*Artifact, *Version, error) {
	if actor.Handle == "" {
		return nil, nil, Errorf(ErrForbidden, "an authenticated actor is required")
	}
	if err := ValidateSlug(req.Slug, s.opts.ReservedSlugs); err != nil {
		return nil, nil, err
	}
	vis, err := ParseVisibility(string(req.Visibility))
	if err != nil {
		return nil, nil, err
	}
	if err := ValidateMetadata(req.Metadata, s.opts.MaxMetadataPairs); err != nil {
		return nil, nil, err
	}
	if len(req.Files) == 0 {
		return nil, nil, Errorf(ErrInvalidArgument, "at least one file is required")
	}
	for _, in := range req.Files {
		if in.Delete {
			return nil, nil, Errorf(ErrInvalidArgument, "cannot delete %q from a new artifact", in.Path)
		}
	}
	// Fail before streaming anything when the slug is obviously taken; the
	// store's unique constraint is the real guard.
	if _, err := s.store.Get(ctx, req.Slug); err == nil {
		return nil, nil, Errorf(ErrSlugTaken, "slug %q is already taken", req.Slug)
	} else if CodeOf(err) != ErrNotFound.Code {
		return nil, nil, err
	}

	quota, err := s.quotaLeft(ctx, actor)
	if err != nil {
		return nil, nil, err
	}
	files, size, err := s.buildFiles(ctx, nil, req.Files, quota)
	if err != nil {
		return nil, nil, err
	}

	now := s.now()
	a := &Artifact{
		Slug:        req.Slug,
		Name:        req.Name,
		Description: req.Description,
		OwnerHandle: actor.Handle,
		Visibility:  vis,
		Metadata:    maps.Clone(req.Metadata),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if a.Metadata == nil {
		a.Metadata = map[string]string{}
	}
	v := &Version{
		Label:     req.Label,
		CreatedBy: actor.Handle,
		CreatedAt: now,
		SizeBytes: size,
		Files:     files,
	}
	if err := s.store.Create(ctx, a, v); err != nil {
		return nil, nil, err
	}
	s.record(ctx, actor, audit.ArtifactCreate, a.Slug, versionDetails(v))
	got, err := s.store.Get(ctx, a.Slug)
	if err != nil {
		return nil, nil, err
	}
	return got, v, nil
}

func versionDetails(v *Version) map[string]string {
	return map[string]string{
		"version": strconv.Itoa(v.Number),
		"size":    strconv.FormatInt(v.SizeBytes, 10),
	}
}

// quotaLeft returns how many more bytes the actor may store, or nil when
// the actor has no quota.
func (s *Service) quotaLeft(ctx context.Context, actor Actor) (*int64, error) {
	if actor.QuotaBytes <= 0 {
		return nil, nil
	}
	used, err := s.store.Usage(ctx, actor.Handle)
	if err != nil {
		return nil, err
	}
	left := actor.QuotaBytes - used
	return &left, nil
}

// Update changes an artifact's fields and, when req.Files is not empty,
// adds a version built from the latest version's files plus the changes.
// The returned version is nil for a fields-only update.
func (s *Service) Update(ctx context.Context, actor Actor, req UpdateRequest) (*Artifact, *Version, error) {
	a, err := s.store.Get(ctx, req.Slug)
	if err != nil {
		return nil, nil, err
	}
	if a.PurgedAt != nil {
		return nil, nil, ErrGone
	}
	restoring := a.DeletedAt != nil
	if restoring && !req.Restore {
		return nil, nil, ErrNotFound
	}
	if !s.canWrite(actor, a) {
		return nil, nil, ErrForbidden
	}
	if err := s.applyFields(a, req); err != nil {
		return nil, nil, err
	}
	if restoring {
		a.DeletedAt = nil
	}
	now := s.now()
	a.UpdatedAt = now

	if len(req.Files) == 0 {
		if req.ExpectedVersion != 0 && req.ExpectedVersion != a.LatestVersion {
			return nil, nil, Errorf(ErrVersionConflict, "latest version is %d, expected %d", a.LatestVersion, req.ExpectedVersion)
		}
		if err := s.store.Update(ctx, a, actor.Handle); err != nil {
			return nil, nil, err
		}
		s.recordUpdate(ctx, actor, a.Slug, nil, restoring)
		got, err := s.store.Get(ctx, a.Slug)
		if err != nil {
			return nil, nil, err
		}
		return got, nil, nil
	}

	// Cheap early check so a stale client does not upload for nothing; the
	// store repeats it inside its transaction.
	if req.ExpectedVersion != 0 && req.ExpectedVersion != a.LatestVersion {
		return nil, nil, Errorf(ErrVersionConflict, "latest version is %d, expected %d", a.LatestVersion, req.ExpectedVersion)
	}
	base := map[string]File{}
	if a.LatestVersion > 0 {
		latest, err := s.store.GetVersion(ctx, a.Slug, a.LatestVersion)
		if err != nil {
			return nil, nil, err
		}
		for _, f := range latest.Files {
			base[f.Path] = f
		}
	}
	// The quota is the acting user's; it applies when they add to their
	// own artifact. Under the shared policy another user's version is
	// charged to the owner, whose quota the actor does not carry.
	var quota *int64
	if actor.Handle == a.OwnerHandle {
		if quota, err = s.quotaLeft(ctx, actor); err != nil {
			return nil, nil, err
		}
	}
	files, size, err := s.buildFiles(ctx, base, req.Files, quota)
	if err != nil {
		return nil, nil, err
	}
	v := &Version{
		Label:     req.Label,
		CreatedBy: actor.Handle,
		CreatedAt: now,
		SizeBytes: size,
		Files:     files,
	}
	if !req.KeepStale {
		a.StaleAt, a.ExpiresAt = nil, nil
	}
	if err := s.store.AddVersion(ctx, a, v, req.ExpectedVersion); err != nil {
		return nil, nil, err
	}
	s.recordUpdate(ctx, actor, a.Slug, v, restoring)
	got, err := s.store.Get(ctx, a.Slug)
	if err != nil {
		return nil, nil, err
	}
	return got, v, nil
}

func (s *Service) recordUpdate(ctx context.Context, actor Actor, slug string, v *Version, restored bool) {
	if restored {
		s.record(ctx, actor, audit.ArtifactRestore, slug, nil)
	}
	var details map[string]string
	if v != nil {
		details = versionDetails(v)
	}
	s.record(ctx, actor, audit.ArtifactUpdate, slug, details)
}

// applyFields copies the requested field changes onto a and validates them.
func (s *Service) applyFields(a *Artifact, req UpdateRequest) error {
	if req.Name != nil {
		a.Name = *req.Name
	}
	if req.Description != nil {
		a.Description = *req.Description
	}
	if req.Visibility != nil {
		vis, err := ParseVisibility(string(*req.Visibility))
		if err != nil {
			return err
		}
		a.Visibility = vis
	}
	if len(req.Metadata) > 0 {
		m := maps.Clone(a.Metadata)
		if m == nil {
			m = map[string]string{}
		}
		for k, v := range req.Metadata {
			if v == nil {
				delete(m, k)
				continue
			}
			m[k] = *v
		}
		if err := ValidateMetadata(m, s.opts.MaxMetadataPairs); err != nil {
			return err
		}
		a.Metadata = m
	}
	return nil
}

// loadWritable fetches an artifact for a lifecycle mutation: tombstones are
// gone, soft-deleted artifacts are not found unless allowDeleted, and the
// write policy applies.
func (s *Service) loadWritable(ctx context.Context, actor Actor, slug string, allowDeleted bool) (*Artifact, error) {
	a, err := s.store.Get(ctx, slug)
	if err != nil {
		return nil, err
	}
	if a.PurgedAt != nil {
		return nil, ErrGone
	}
	if a.DeletedAt != nil && !allowDeleted {
		return nil, ErrNotFound
	}
	if !s.canWrite(actor, a) {
		return nil, ErrForbidden
	}
	return a, nil
}

// save writes a's fields, records the event and returns the stored state.
func (s *Service) save(ctx context.Context, actor Actor, a *Artifact, action string, details map[string]string) (*Artifact, error) {
	a.UpdatedAt = s.now()
	if err := s.store.Update(ctx, a, actor.Handle); err != nil {
		return nil, err
	}
	s.record(ctx, actor, action, a.Slug, details)
	got, err := s.store.Get(ctx, a.Slug)
	if err != nil {
		return nil, fmt.Errorf("artifact: reload %s: %w", a.Slug, err)
	}
	return got, nil
}
