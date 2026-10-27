package artifact

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strconv"
	"sync"
	"time"

	"github.com/PeterBooker/kazari/internal/audit"
	"github.com/PeterBooker/kazari/internal/blob"
)

const (
	// WriteShared lets any authenticated actor change any artifact.
	WriteShared = "shared"
	// WriteOwner limits changes to the owner and admins.
	WriteOwner = "owner"
)

const (
	DefaultMaxVersionBytes = 50 << 20
	DefaultMaxFileBytes    = 25 << 20
	DefaultGrace           = 14 * 24 * time.Hour
	DefaultRetention       = 30 * 24 * time.Hour
	DefaultTombstone       = 90 * 24 * time.Hour
	DefaultBlobGrace       = time.Hour
	DefaultShareLinkTTL    = 7 * 24 * time.Hour
	DefaultMaxShareLinkTTL = 90 * 24 * time.Hour
)

const gcBatch = 1000

// Options configures a Service. Zero values get the Default* values.
type Options struct {
	MaxVersionBytes  int64
	MaxFileBytes     int64
	MaxFiles         int
	MaxMetadataPairs int
	ReservedSlugs    []string
	DefaultGrace     time.Duration // MarkStale grace when the caller gives none
	Retention        time.Duration // soft-deleted -> purged
	Tombstone        time.Duration // purged -> slug freed
	BlobGrace        time.Duration // how long an unreferenced blob survives GC
	WritePolicy      string        // WriteShared (default) or WriteOwner
	// DefaultVisibility defaults to Public.
	DefaultVisibility Visibility
	// MaxShareLinkTTL caps a share link's lifetime; links created without one
	// live DefaultShareLinkTTL, or this when shorter.
	MaxShareLinkTTL time.Duration
	// Pages takes the snapshots Import publishes; nil disables Import.
	Pages  PageSource
	Now    func() time.Time
	Logger *slog.Logger // audit failures; default slog.Default()
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
	if o.DefaultVisibility == "" {
		o.DefaultVisibility = Public
	}
	if o.MaxShareLinkTTL <= 0 {
		o.MaxShareLinkTTL = DefaultMaxShareLinkTTL
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return o
}

// Service implements the artifact operations. It is safe for concurrent
// use; the store resolves races.
type Service struct {
	store Store
	blobs blob.Store
	rec   audit.Recorder
	opts  Options

	// blobMu orders publishes against the garbage collector: a Put of stored
	// content writes nothing, so the blob looks unreferenced until its
	// version commits. Publishes hold the read side over that window,
	// CollectGarbage the write side over each batch.
	blobMu sync.RWMutex
}

// New returns a Service; a nil rec discards audit events.
func New(store Store, blobs blob.Store, rec audit.Recorder, opts Options) *Service {
	if rec == nil {
		rec = audit.Nop{}
	}
	return &Service{store: store, blobs: blobs, rec: rec, opts: opts.withDefaults()}
}

func (s *Service) now() time.Time { return s.opts.Now().UTC() }

func (s *Service) canWrite(actor Actor, a *Artifact) bool {
	if s.opts.WritePolicy == WriteShared {
		return true
	}
	return actor.Admin || actor.Handle == a.OwnerHandle
}

// record logs a failure rather than returning it: the change it describes
// has already happened.
func (s *Service) record(ctx context.Context, actor Actor, action, slug string, details map[string]string) {
	if actor.Credential != "" {
		details = maps.Clone(details)
		if details == nil {
			details = map[string]string{}
		}
		details["credential"] = actor.Credential
	}
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
	if err := validateTexts(req.Name, req.Description, req.Label); err != nil {
		return nil, nil, err
	}
	if req.Visibility == "" {
		req.Visibility = s.opts.DefaultVisibility
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
	// Fail before streaming when the slug is taken; the store's unique
	// constraint is the real guard.
	if _, err := s.store.Get(ctx, req.Slug); err == nil {
		return nil, nil, Errorf(ErrSlugTaken, "slug %q is already taken", req.Slug)
	} else if CodeOf(err) != ErrNotFound.Code {
		return nil, nil, err
	}

	// Checked early to spare a doomed upload; the store re-checks.
	quota, err := s.quotaLeft(ctx, actor)
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
	v := &Version{Label: req.Label, CreatedBy: actor.Handle, CreatedAt: now}
	err = s.holdBlobs(func() error {
		files, size, err := s.buildFiles(ctx, nil, req.Files, quota)
		if err != nil {
			return err
		}
		v.Files, v.SizeBytes = files, size
		return s.store.Create(ctx, a, v, actor.QuotaBytes)
	})
	if err != nil {
		return nil, nil, err
	}
	s.record(ctx, actor, audit.ArtifactCreate, a.Slug, versionDetails(v))
	got, err := s.store.Get(ctx, a.Slug)
	if err != nil {
		return nil, nil, err
	}
	return got, v, nil
}

// holdBlobs runs fn under blobMu's read side. Every content reader is local
// (memory, a spilled bundle or a staged upload), so the hold is bounded by
// disk speed, not by a client.
func (s *Service) holdBlobs(fn func() error) error {
	s.blobMu.RLock()
	defer s.blobMu.RUnlock()
	return fn()
}

func validateTexts(name, description, label string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if err := ValidateDescription(description); err != nil {
		return err
	}
	return ValidateLabel(label)
}

func versionDetails(v *Version) map[string]string {
	return map[string]string{
		"version": strconv.Itoa(v.Number),
		"size":    strconv.FormatInt(v.SizeBytes, 10),
	}
}

// quotaLeft is nil when the actor has no quota.
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

// Update changes fields and, unless req.Files is empty, adds a version of
// the latest files plus the changes; otherwise the version is nil.
func (s *Service) Update(ctx context.Context, actor Actor, req UpdateRequest) (*Artifact, *Version, error) {
	if err := ValidateLabel(req.Label); err != nil {
		return nil, nil, err
	}
	now := s.now()
	// The store calls apply on the row it writes, inside its transaction, so
	// a concurrent transfer or delete is neither lost nor ignored.
	var restoring bool
	apply := func(a *Artifact) error {
		if a.PurgedAt != nil {
			return ErrGone
		}
		restoring = a.DeletedAt != nil
		if restoring && !req.Restore {
			return ErrNotFound
		}
		if !s.canWrite(actor, a) {
			return ErrForbidden
		}
		if err := s.applyFields(a, req); err != nil {
			return err
		}
		a.DeletedAt = nil
		a.UpdatedAt = now
		return nil
	}

	if len(req.Files) == 0 {
		err := s.store.Update(ctx, req.Slug, actor.Handle, func(a *Artifact) error {
			if err := apply(a); err != nil {
				return err
			}
			if req.ExpectedVersion != 0 && req.ExpectedVersion != a.LatestVersion {
				return Errorf(ErrVersionConflict, "latest version is %d, expected %d", a.LatestVersion, req.ExpectedVersion)
			}
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
		s.recordUpdate(ctx, actor, req.Slug, nil, restoring)
		got, err := s.store.Get(ctx, req.Slug)
		if err != nil {
			return nil, nil, err
		}
		return got, nil, nil
	}

	// Check a first read so a doomed request uploads nothing; the store
	// repeats the checks in its transaction.
	a, err := s.store.Get(ctx, req.Slug)
	if err != nil {
		return nil, nil, err
	}
	if err := apply(a); err != nil {
		return nil, nil, err
	}
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
	// The actor carries only its own quota; another user's version is
	// charged to the owner.
	var (
		quota      *int64
		quotaBytes int64
	)
	if actor.Handle == a.OwnerHandle {
		if quota, err = s.quotaLeft(ctx, actor); err != nil {
			return nil, nil, err
		}
		quotaBytes = actor.QuotaBytes
	}
	v := &Version{Label: req.Label, CreatedBy: actor.Handle, CreatedAt: now}
	err = s.holdBlobs(func() error {
		files, size, err := s.buildFiles(ctx, base, req.Files, quota)
		if err != nil {
			return err
		}
		v.Files, v.SizeBytes = files, size
		return s.store.AddVersion(ctx, req.Slug, v, req.ExpectedVersion, quotaBytes, func(a *Artifact) error {
			if err := apply(a); err != nil {
				return err
			}
			if !req.KeepStale {
				a.StaleAt, a.ExpiresAt = nil, nil
			}
			return nil
		})
	})
	if err != nil {
		return nil, nil, err
	}
	s.recordUpdate(ctx, actor, req.Slug, v, restoring)
	got, err := s.store.Get(ctx, req.Slug)
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

func (s *Service) applyFields(a *Artifact, req UpdateRequest) error {
	if req.Name != nil {
		if err := ValidateName(*req.Name); err != nil {
			return err
		}
		a.Name = *req.Name
	}
	if req.Description != nil {
		if err := ValidateDescription(*req.Description); err != nil {
			return err
		}
		a.Description = *req.Description
	}
	if req.Visibility != nil {
		// ParseVisibility reads "" as public, the create default; here it
		// would quietly open a members page to everyone.
		if *req.Visibility == "" {
			return Errorf(ErrInvalidArgument, "visibility must be %q, %q or %q", Public, Unlisted, Members)
		}
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

func (s *Service) checkWritable(actor Actor, a *Artifact, allowDeleted bool) error {
	if a.PurgedAt != nil {
		return ErrGone
	}
	if a.DeletedAt != nil && !allowDeleted {
		return ErrNotFound
	}
	if !s.canWrite(actor, a) {
		return ErrForbidden
	}
	return nil
}

// errUnchanged from a change func skips the write and the audit event.
var errUnchanged = errors.New("artifact: unchanged")

// modify runs checkWritable and change on the current row inside the
// store's transaction, then records action with change's details.
func (s *Service) modify(ctx context.Context, actor Actor, slug string, allowDeleted bool, action string,
	change func(a *Artifact) (map[string]string, error)) (*Artifact, error) {
	var details map[string]string
	err := s.store.Update(ctx, slug, actor.Handle, func(a *Artifact) error {
		if err := s.checkWritable(actor, a, allowDeleted); err != nil {
			return err
		}
		d, err := change(a)
		if err != nil {
			return err
		}
		details = d
		a.UpdatedAt = s.now()
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.record(ctx, actor, action, slug, details)
	got, err := s.store.Get(ctx, slug)
	if err != nil {
		return nil, fmt.Errorf("artifact: reload %s: %w", slug, err)
	}
	return got, nil
}
