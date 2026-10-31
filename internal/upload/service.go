package upload

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/PeterBooker/kazari/internal/instrument"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PeterBooker/kazari/internal/artifact"
)

const (
	DefaultMaxChunkBytes   = 4 << 20
	DefaultMaxTotalBytes   = 50 << 20
	DefaultMaxFiles        = artifact.DefaultMaxFiles
	DefaultTTL             = time.Hour
	DefaultMaxOpenPerOwner = 5
	// MaxArchiveAttempts bounds what a leaked archive URL costs: after this
	// many bodies that miss the digest the upload is gone, not open until
	// its TTL for anyone to keep pouring bytes into.
	MaxArchiveAttempts = 3
)

// base64url without padding encodes idBytes random bytes as idLen characters
// and tokenBytes as tokenLen.
const (
	idBytes    = 16
	idLen      = 22
	tokenBytes = 32
	tokenLen   = 43
)

// Options configures a Service. Zero fields take the Default constants.
type Options struct {
	Dir             string // staging root; default os.TempDir()/kazari-uploads
	MaxChunkBytes   int64  // after any transport decoding
	MaxTotalBytes   int64  // all files of one upload
	MaxFiles        int
	TTL             time.Duration
	MaxOpenPerOwner int
	Now             func() time.Time
	Telemetry       instrument.Providers
}

// Service stages chunked uploads on disk and records them in a Store.
type Service struct {
	store  Store
	opts   Options
	locks  keyedMutex
	tracer trace.Tracer
	chunks metric.Int64Counter
}

// New returns a Service. The staging directory is created on first use.
func New(store Store, opts Options) *Service {
	if opts.Dir == "" {
		opts.Dir = filepath.Join(os.TempDir(), "kazari-uploads")
	}
	if opts.MaxChunkBytes <= 0 {
		opts.MaxChunkBytes = DefaultMaxChunkBytes
	}
	if opts.MaxTotalBytes <= 0 {
		opts.MaxTotalBytes = DefaultMaxTotalBytes
	}
	if opts.MaxFiles <= 0 {
		opts.MaxFiles = DefaultMaxFiles
	}
	if opts.TTL <= 0 {
		opts.TTL = DefaultTTL
	}
	if opts.MaxOpenPerOwner <= 0 {
		opts.MaxOpenPerOwner = DefaultMaxOpenPerOwner
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	meter := opts.Telemetry.Meter(scope)
	s := &Service{store: store, opts: opts, tracer: opts.Telemetry.Tracer(scope),
		chunks: instrument.Handle(meter.Int64Counter("kazari.upload.chunks",
			metric.WithUnit("{chunk}"), metric.WithDescription("Chunks appended to uploads.")))}
	s.observeOpen(meter)
	return s
}

// MaxChunkBytes is the largest chunk AppendChunk accepts.
func (s *Service) MaxChunkBytes() int64 { return s.opts.MaxChunkBytes }

// MaxFiles is the most files one upload may stage.
func (s *Service) MaxFiles() int { return s.opts.MaxFiles }

// OffsetMismatchError unwraps to artifact.ErrOffsetMismatch, so
// artifact.CodeOf reports its code. Current is the staged size, where the
// client should resume.
type OffsetMismatchError struct {
	Path    string
	Current int64
}

func (e *OffsetMismatchError) Error() string {
	return fmt.Sprintf("%s: %q has %d bytes staged", artifact.ErrOffsetMismatch.Code, e.Path, e.Current)
}

func (e *OffsetMismatchError) Is(target error) bool { return target == artifact.ErrOffsetMismatch }

func (e *OffsetMismatchError) Unwrap() error { return artifact.ErrOffsetMismatch }

// Begin starts an upload for owner. MaxOpenPerOwner holds under concurrent
// Begins: the store checks it in the insert's transaction.
func (s *Service) Begin(ctx context.Context, owner string) (u *Upload, err error) {
	ctx, span := s.span(ctx, "upload.begin", "")
	defer func() { endBegin(span, u, err) }()
	return s.begin(ctx, owner, nil)
}

// BeginArchive starts an upload that also accepts one archive of exactly
// sum (hex SHA-256) and size bytes at a URL carrying the returned token,
// without a credential. The token is returned once; the store keeps its
// hash. The digest is required, not optional, because the URL will pass
// through a model's context and logs: bound to the bytes, it lets whoever
// holds it stage nothing the owner does not already have.
func (s *Service) BeginArchive(ctx context.Context, owner, sum string, size int64) (u *Upload, token string, err error) {
	ctx, span := s.span(ctx, "upload.begin_archive", "")
	defer func() { endBegin(span, u, err) }()
	sum = strings.ToLower(sum)
	if raw, err := hex.DecodeString(sum); err != nil || len(raw) != sha256.Size {
		return nil, "", artifact.Errorf(artifact.ErrInvalidArgument, "archive_sha256 must be the hex SHA-256 of the archive")
	}
	if size <= 0 {
		return nil, "", artifact.Errorf(artifact.ErrInvalidArgument, "archive_bytes must be the size of the archive")
	}
	if size > s.opts.MaxTotalBytes {
		return nil, "", artifact.Errorf(artifact.ErrTooLarge, "archive larger than %d bytes", s.opts.MaxTotalBytes)
	}
	token = randomText(tokenBytes)
	u, err = s.begin(ctx, owner, &Archive{TokenHash: hashToken(token), SHA256: sum, Bytes: size})
	if err != nil {
		return nil, "", err
	}
	return u, token, nil
}

func (s *Service) begin(ctx context.Context, owner string, a *Archive) (*Upload, error) {
	now := s.opts.Now()
	u := &Upload{
		ID:        randomText(idBytes),
		Owner:     owner,
		CreatedAt: now,
		ExpiresAt: now.Add(s.opts.TTL),
		Archive:   a,
	}
	// Row first, so the orphan sweep never sees a directory whose row is
	// still being written.
	if err := s.store.Create(ctx, u, s.opts.MaxOpenPerOwner); err != nil {
		if errors.Is(err, artifact.ErrTooManyUploads) {
			return nil, err
		}
		return nil, fmt.Errorf("upload: begin: %w", err)
	}
	if err := os.MkdirAll(s.dir(u.ID), 0o700); err != nil {
		return nil, errors.Join(fmt.Errorf("upload: begin: %w", err), s.store.Delete(ctx, u.ID))
	}
	return u, nil
}

// Get returns an open upload of owner, or artifact.ErrUploadNotFound if it
// is unknown, expired or foreign.
func (s *Service) Get(ctx context.Context, owner, id string) (*Upload, error) {
	return s.load(ctx, owner, id, false)
}

// AppendChunk writes r to the staged file path at offset. An offset equal to
// the staged size appends; a chunk ending exactly there is a retry,
// acknowledged without writing; any other offset is an *OffsetMismatchError.
// The first chunk of a path sets its content type, but every chunk's must
// pass the publish's check, so nothing invalid is staged.
func (s *Service) AppendChunk(ctx context.Context, owner, id, path string, offset int64, r io.Reader, contentType string) (_ StagedFile, err error) {
	ctx, span := s.span(ctx, "upload.append_chunk", id)
	defer func() { s.chunk(ctx, err); endSpan(span, err) }()
	if offset < 0 {
		return StagedFile{}, artifact.Errorf(artifact.ErrInvalidArgument, "negative offset %d", offset)
	}
	clean, err := artifact.ValidatePath(path)
	if err != nil {
		return StagedFile{}, err
	}
	if err := artifact.ValidateContentType(contentType); err != nil {
		return StagedFile{}, err
	}
	chunk, err := io.ReadAll(io.LimitReader(r, s.opts.MaxChunkBytes+1))
	if err != nil {
		return StagedFile{}, fmt.Errorf("upload: read chunk: %w", err)
	}
	if int64(len(chunk)) > s.opts.MaxChunkBytes {
		return StagedFile{}, artifact.Errorf(artifact.ErrTooLarge,
			"chunk larger than %d bytes", s.opts.MaxChunkBytes)
	}
	size := int64(len(chunk))

	unlock := s.locks.lock(id)
	defer unlock()
	u, err := s.load(ctx, owner, id, false)
	if err != nil {
		return StagedFile{}, err
	}

	i := slices.IndexFunc(u.Files, func(f StagedFile) bool { return f.Path == clean })
	var f StagedFile
	if i >= 0 {
		f = u.Files[i]
	} else {
		if len(u.Files) >= s.opts.MaxFiles {
			return StagedFile{}, artifact.Errorf(artifact.ErrTooManyFiles,
				"more than %d files in one upload", s.opts.MaxFiles)
		}
		next := 0
		for _, g := range u.Files {
			next = max(next, g.StagingIndex+1)
		}
		f = StagedFile{Path: clean, StagingIndex: next, ContentType: contentType}
	}

	switch {
	case offset == f.Bytes:
		// Append below.
	case offset < f.Bytes && offset+size == f.Bytes:
		return f, nil // retry of a chunk already written
	default:
		return StagedFile{}, &OffsetMismatchError{Path: clean, Current: f.Bytes}
	}
	if u.Bytes+size > s.opts.MaxTotalBytes {
		return StagedFile{}, artifact.Errorf(artifact.ErrTooLarge,
			"upload larger than %d bytes", s.opts.MaxTotalBytes)
	}

	if err := s.write(id, f, chunk); err != nil {
		return StagedFile{}, fmt.Errorf("upload: append %s %q: %w", id, clean, err)
	}
	f.Bytes += size
	if err := s.store.UpsertFile(ctx, id, f, u.Bytes+size); err != nil {
		return StagedFile{}, fmt.Errorf("upload: append %s %q: %w", id, clean, err)
	}
	return f, nil
}

// write truncates to the recorded size first, so bytes left by a write whose
// row update failed are never published.
func (s *Service) write(id string, f StagedFile, chunk []byte) error {
	if err := os.MkdirAll(s.dir(id), 0o700); err != nil {
		return err
	}
	fh, err := os.OpenFile(s.stagingPath(id, f.StagingIndex), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if err := fh.Truncate(f.Bytes); err != nil {
		fh.Close()
		return err
	}
	if _, err := fh.Write(chunk); err != nil {
		fh.Close()
		return err
	}
	return fh.Close()
}

// Files opens the staged files as FileInputs sorted by path; each Content
// reads its file up to the size recorded for it, so a chunk appended while
// the publish runs changes nothing. An empty upload is
// artifact.ErrInvalidArgument. Call cleanup after the publish succeeds or
// fails: it closes the files and removes the upload.
func (s *Service) Files(ctx context.Context, owner, id string) (_ []artifact.FileInput, _ func() error, err error) {
	ctx, span := s.span(ctx, "upload.files", id)
	defer func() { endSpan(span, err) }()
	unlock := s.locks.lock(id)
	defer unlock()
	u, err := s.load(ctx, owner, id, false)
	if err != nil {
		return nil, nil, err
	}
	if len(u.Files) == 0 {
		return nil, nil, artifact.Errorf(artifact.ErrInvalidArgument, "upload %s has no files", id)
	}
	staged := slices.Clone(u.Files)
	slices.SortFunc(staged, func(a, b StagedFile) int { return strings.Compare(a.Path, b.Path) })

	var opened []*os.File
	closeAll := func() error {
		var errs []error
		for _, fh := range opened {
			if err := fh.Close(); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
	inputs := make([]artifact.FileInput, 0, len(staged))
	for _, f := range staged {
		fh, err := s.openStaged(id, f)
		if err != nil {
			return nil, nil, errors.Join(fmt.Errorf("upload: files %s: %w", id, err), closeAll())
		}
		opened = append(opened, fh)
		inputs = append(inputs, artifact.FileInput{
			Path: f.Path, Content: io.NewSectionReader(fh, 0, f.Bytes), ContentType: f.ContentType,
		})
	}

	var (
		once       sync.Once
		cleanupErr error
	)
	cleanup := func() error {
		once.Do(func() {
			cleanupErr = errors.Join(closeAll(), s.remove(context.WithoutCancel(ctx), id))
		})
		return cleanupErr
	}
	return inputs, cleanup, nil
}

// openStaged trims the file to its recorded size.
func (s *Service) openStaged(id string, f StagedFile) (*os.File, error) {
	p := s.stagingPath(id, f.StagingIndex)
	fh, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	st, err := fh.Stat()
	if err == nil && st.Size() < f.Bytes {
		err = fmt.Errorf("staging file for %q has %d bytes, want %d", f.Path, st.Size(), f.Bytes)
	}
	if err == nil && st.Size() > f.Bytes {
		err = fh.Truncate(f.Bytes)
	}
	if err != nil {
		fh.Close()
		return nil, err
	}
	return fh, nil
}

// Abort removes an upload of owner, expired or not.
func (s *Service) Abort(ctx context.Context, owner, id string) (err error) {
	ctx, span := s.span(ctx, "upload.abort", id)
	defer func() { endSpan(span, err) }()
	unlock := s.locks.lock(id)
	defer unlock()
	if _, err := s.load(ctx, owner, id, true); err != nil {
		return err
	}
	return s.remove(ctx, id)
}

// Archive returns the upload that token opens, so the caller can size the
// body before reading any of it. An unknown, expired, spent or foreign
// token is artifact.ErrUploadNotFound, like every other reason.
func (s *Service) Archive(ctx context.Context, id, token string) (*Upload, error) {
	return s.loadArchive(ctx, id, token)
}

// RejectArchive counts a body that missed the digest and returns how many
// more may be sent; at zero the upload is removed.
func (s *Service) RejectArchive(ctx context.Context, id, token string) (_ int, err error) {
	ctx, span := s.span(ctx, "upload.reject_archive", id)
	defer func() { endSpan(span, err) }()
	unlock := s.locks.lock(id)
	defer unlock()
	if _, err := s.loadArchive(ctx, id, token); err != nil {
		return 0, err
	}
	n, err := s.store.CountArchiveAttempt(ctx, id)
	if err != nil {
		return 0, fmt.Errorf("upload: reject archive %s: %w", id, err)
	}
	left := max(MaxArchiveAttempts-n, 0)
	if left == 0 {
		return 0, s.remove(ctx, id)
	}
	return left, nil
}

// ReceiveArchive stages files as the archive of the upload token opens and
// spends the token. Once the token is accepted, any failure removes the
// upload: the digest already matched, so the same bytes would fail the
// same way, and the owner has to begin again with a corrected archive.
func (s *Service) ReceiveArchive(ctx context.Context, id, token string, files []artifact.FileInput) (_ *Upload, err error) {
	ctx, span := s.span(ctx, "upload.receive_archive", id)
	defer func() { endSpan(span, err) }()
	unlock := s.locks.lock(id)
	defer unlock()
	u, err := s.loadArchive(ctx, id, token)
	if err != nil {
		return nil, err
	}
	staged, err := s.stage(ctx, u, files)
	if err == nil {
		if err = s.store.ReceiveArchive(ctx, id, staged, u.Bytes, s.opts.Now()); err != nil {
			err = fmt.Errorf("upload: receive archive %s: %w", id, err)
		}
	}
	if err != nil {
		return nil, errors.Join(err, s.remove(context.WithoutCancel(ctx), id))
	}
	return s.load(ctx, u.Owner, id, false)
}

// stage writes each file to a fresh staging file, adding its size to u.
func (s *Service) stage(ctx context.Context, u *Upload, files []artifact.FileInput) ([]StagedFile, error) {
	if len(files) == 0 {
		return nil, artifact.Errorf(artifact.ErrInvalidArgument, "the archive has no files")
	}
	if err := os.MkdirAll(s.dir(u.ID), 0o700); err != nil {
		return nil, fmt.Errorf("upload: stage %s: %w", u.ID, err)
	}
	taken := make(map[string]bool, len(u.Files)+len(files))
	next := 0
	for _, f := range u.Files {
		taken[f.Path] = true
		next = max(next, f.StagingIndex+1)
	}
	left := s.opts.MaxTotalBytes - u.Bytes
	staged := make([]StagedFile, 0, len(files))
	for _, in := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		clean, err := artifact.ValidatePath(in.Path)
		if err != nil {
			return nil, err
		}
		if taken[clean] {
			return nil, artifact.Errorf(artifact.ErrInvalidArgument, "the archive names %q twice", clean)
		}
		if len(u.Files)+len(staged) >= s.opts.MaxFiles {
			return nil, artifact.Errorf(artifact.ErrTooManyFiles, "more than %d files in one upload", s.opts.MaxFiles)
		}
		n, err := s.writeNew(u.ID, next, in.Content, left)
		if err != nil {
			return nil, err
		}
		taken[clean] = true
		staged = append(staged, StagedFile{Path: clean, StagingIndex: next, Bytes: n})
		next++
		left -= n
		u.Bytes += n
	}
	return staged, nil
}

// writeNew copies r to a new staging file and refuses more than left bytes.
// A reader's own error passes through when it carries a code, so a corrupt
// archive entry is reported as the client's fault, not the server's.
func (s *Service) writeNew(id string, index int, r io.Reader, left int64) (int64, error) {
	fh, err := os.OpenFile(s.stagingPath(id, index), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, fmt.Errorf("upload: stage %s: %w", id, err)
	}
	n, err := io.Copy(fh, io.LimitReader(r, left+1))
	if cerr := fh.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil && artifact.CodeOf(err) != "":
		return 0, err
	case err != nil:
		return 0, fmt.Errorf("upload: stage %s: %w", id, err)
	case n > left:
		return 0, artifact.Errorf(artifact.ErrTooLarge, "upload larger than %d bytes", s.opts.MaxTotalBytes)
	}
	return n, nil
}

// ExpireStale removes expired uploads and staging directories with no row
// (left by a crash), returns how many and continues past errors.
func (s *Service) ExpireStale(ctx context.Context) (n int, err error) {
	ctx, span := s.span(ctx, "upload.expire_stale", "")
	defer func() { span.SetAttributes(attribute.Int("kazari.upload.expired_count", n)); endSpan(span, err) }()
	expired, err := s.store.ListExpired(ctx, s.opts.Now())
	if err != nil {
		return 0, fmt.Errorf("upload: expire: %w", err)
	}
	var (
		removed int
		errs    []error
	)
	for _, u := range expired {
		unlock := s.locks.lock(u.ID)
		err := s.remove(ctx, u.ID)
		unlock()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		removed++
	}

	entries, err := os.ReadDir(s.opts.Dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, fmt.Errorf("upload: expire: %w", err))
	}
	for _, e := range entries {
		id := e.Name()
		if !validID(id) {
			continue // not ours; never delete what we did not create
		}
		n, err := s.removeOrphan(ctx, id)
		if err != nil {
			errs = append(errs, err)
		}
		removed += n
	}
	return removed, errors.Join(errs...)
}

func (s *Service) removeOrphan(ctx context.Context, id string) (int, error) {
	unlock := s.locks.lock(id)
	defer unlock()
	_, err := s.store.Get(ctx, id)
	switch {
	case err == nil:
		return 0, nil
	case !errors.Is(err, artifact.ErrUploadNotFound):
		return 0, fmt.Errorf("upload: expire %s: %w", id, err)
	}
	if err := os.RemoveAll(s.dir(id)); err != nil {
		return 0, fmt.Errorf("upload: expire %s: %w", id, err)
	}
	return 1, nil
}

// load hides every reason the upload is unavailable to owner behind
// artifact.ErrUploadNotFound.
func (s *Service) load(ctx context.Context, owner, id string, allowExpired bool) (*Upload, error) {
	u, err := s.get(ctx, id)
	if err != nil {
		return nil, err
	}
	if u.Owner != owner || (!allowExpired && !s.opts.Now().Before(u.ExpiresAt)) {
		return nil, artifact.ErrUploadNotFound
	}
	return u, nil
}

// loadArchive is load for the holder of an archive token, who is nobody in
// particular: the token stands in for the owner. The id is not a secret, but
// the token is, so its comparison takes the same time whether or not it
// matches.
func (s *Service) loadArchive(ctx context.Context, id, token string) (*Upload, error) {
	if len(token) != tokenLen {
		return nil, artifact.ErrUploadNotFound
	}
	u, err := s.get(ctx, id)
	if err != nil {
		return nil, err
	}
	a := u.Archive
	if a == nil || !a.ReceivedAt.IsZero() || !s.opts.Now().Before(u.ExpiresAt) ||
		subtle.ConstantTimeCompare([]byte(hashToken(token)), []byte(a.TokenHash)) != 1 {
		return nil, artifact.ErrUploadNotFound
	}
	return u, nil
}

func (s *Service) get(ctx context.Context, id string) (*Upload, error) {
	if !validID(id) {
		return nil, artifact.ErrUploadNotFound
	}
	u, err := s.store.Get(ctx, id)
	if err != nil {
		if errors.Is(err, artifact.ErrUploadNotFound) {
			return nil, artifact.ErrUploadNotFound
		}
		return nil, fmt.Errorf("upload: get %s: %w", id, err)
	}
	return u, nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func randomText(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *Service) remove(ctx context.Context, id string) error {
	var errs []error
	if err := os.RemoveAll(s.dir(id)); err != nil {
		errs = append(errs, fmt.Errorf("upload: remove %s: %w", id, err))
	}
	if err := s.store.Delete(ctx, id); err != nil && !errors.Is(err, artifact.ErrUploadNotFound) {
		errs = append(errs, fmt.Errorf("upload: remove %s: %w", id, err))
	}
	return errors.Join(errs...)
}

func (s *Service) dir(id string) string { return filepath.Join(s.opts.Dir, id) }

func (s *Service) stagingPath(id string, index int) string {
	return filepath.Join(s.opts.Dir, id, strconv.Itoa(index))
}

// validID accepts only the shape Begin generates, so ids cannot name paths
// outside Dir and the orphan sweep leaves anything else there alone.
func validID(id string) bool {
	if len(id) != idLen {
		return false
	}
	for _, c := range id {
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

type keyedMutex struct {
	mu sync.Mutex
	m  map[string]*keyedEntry
}

type keyedEntry struct {
	mu   sync.Mutex
	refs int
}

func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	if k.m == nil {
		k.m = make(map[string]*keyedEntry)
	}
	e := k.m[key]
	if e == nil {
		e = &keyedEntry{}
		k.m[key] = e
	}
	e.refs++
	k.mu.Unlock()

	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		k.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(k.m, key)
		}
		k.mu.Unlock()
	}
}
