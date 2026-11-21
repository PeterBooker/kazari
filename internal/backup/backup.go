// Package backup writes an instance's data as one tar stream, laid out as
// the data directory is: a consistent copy of the database first, then every
// blob, so each blob the copy refers to is in the archive. Unpacked into an
// empty data directory (Extract), the archive is what a stopped server
// starts from.
package backup

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/audit"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/disk"
	"github.com/PeterBooker/kazari/internal/instrument"
)

const scope = "github.com/PeterBooker/kazari/internal/backup"

// Attribute keys of the backup.write span.
const (
	filesKey = attribute.Key("kazari.backup.file_count")
	sizeKey  = attribute.Key("kazari.backup.size")
	certsKey = attribute.Key("kazari.backup.certificates")
)

var (
	// ErrRunning is returned by Write while another backup is being written.
	ErrRunning = errors.New("backup: a backup is already running")
	// ErrNoSpace is returned by Write when the temporary directory cannot
	// hold the copy of the database.
	ErrNoSpace = errors.New("backup: not enough free space for the database copy")
)

// Database makes the consistent copy. *sqlite.DB implements it.
type Database interface {
	// VacuumInto writes a copy of the database to path, which does not exist.
	VacuumInto(ctx context.Context, path string) error
}

// Options configures a Service. The three paths lie directly in the data
// directory, and their base names are the names in the archive.
type Options struct {
	DBPath  string // the database file, e.g. <data>/kazari.db
	BlobDir string // e.g. <data>/blobs
	CertDir string // e.g. <data>/certmagic; "" when there is none
	// TempDir receives the database copy, which is removed once written.
	TempDir string
	// Free reports the bytes that can still be written under path. The
	// default asks the filesystem.
	Free func(path string) (uint64, error)
	// Now stamps the database copy in the archive. Default: time.Now.
	Now    func() time.Time
	Logger *slog.Logger // nil means slog.Default()
	// Audit records each archive written, by the caller in the context: an
	// archive holds every page and every credential's hash. Nil records
	// nothing.
	Audit     audit.Recorder
	Telemetry instrument.Providers
}

// WriteOptions choose what one archive holds beyond the database and the
// blobs.
type WriteOptions struct {
	// Certs adds the certificate directory, private key included. Leave it
	// off for an archive that leaves the host.
	Certs bool
}

// Summary describes a written archive.
type Summary struct {
	Files int   // entries written
	Bytes int64 // their content, without the tar framing
}

// Estimate is what a backup would hold and what room its temporary copy
// has.
type Estimate struct {
	DatabaseBytes int64  // the database file and its write-ahead log
	BlobBytes     int64  // every file under the blob directory
	FreeBytes     uint64 // free in the temporary directory
}

// Size is the content of the archive, give or take the tar framing and what
// compacting the database saves.
func (e Estimate) Size() int64 { return e.DatabaseBytes + e.BlobBytes }

// Service writes backups, one at a time, and keeps the garbage collector
// from deleting blobs while it does.
//
// The collector and a backup exclude each other through one slot. Write
// takes it for its whole run, waiting for a collection in progress. The
// collector asks for it with HoldGC and skips its run when a backup has it:
// a blob deleted between the database copy and its place in the archive
// would leave the copy referring to nothing.
type Service struct {
	db   Database
	opts Options
	log  *slog.Logger

	running atomic.Bool
	slot    chan struct{}

	tracer   trace.Tracer
	duration metric.Float64Histogram
	size     metric.Int64Histogram
	// lastSuccess is when the newest archive was finished, in unix
	// nanoseconds; 0 until one is.
	lastSuccess atomic.Int64
}

// New returns a Service that copies db.
func New(db Database, opts Options) *Service {
	if opts.Free == nil {
		opts.Free = func(path string) (uint64, error) {
			free, _, err := disk.Usage(path)
			return free, err
		}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	s := &Service{db: db, opts: opts, log: log, slot: make(chan struct{}, 1), tracer: opts.Telemetry.Tracer(scope)}
	s.instruments(opts.Telemetry.Meter(scope))
	return s
}

func (s *Service) instruments(m metric.Meter) {
	s.duration = instrument.Handle(m.Float64Histogram("kazari.backup.duration",
		metric.WithUnit("s"), metric.WithDescription("Duration of backups, refused and failed ones included."),
		metric.WithExplicitBucketBoundaries(0.04, 0.16, 0.64, 2.56, 10.24, 40.96, 163.84, 655.36)))
	s.size = instrument.Handle(m.Int64Histogram("kazari.backup.size",
		metric.WithUnit("By"), metric.WithDescription("Content of each backup archive written."),
		metric.WithExplicitBucketBoundaries(1<<20, 4<<20, 16<<20, 64<<20, 256<<20, 1<<30, 4<<30, 16<<30, 64<<30)))
	// An alert on its age says the timer that takes backups has stopped; a
	// server that has written none since it started reports nothing.
	last := instrument.Handle(m.Float64ObservableGauge("kazari.backup.last_success.timestamp",
		metric.WithUnit("s"), metric.WithDescription("When the last backup was written, in seconds since the Unix epoch.")))
	_ = instrument.Handle(m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		if at := s.lastSuccess.Load(); at != 0 {
			o.ObserveFloat64(last, float64(at)/1e9)
		}
		return nil
	}, last))
}

// errorCode names the refusals a caller can act on; anything else is
// internal.
func errorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrRunning):
		return api.CodeBackupRunning
	case errors.Is(err, ErrNoSpace):
		return api.CodeNoSpace
	}
	return instrument.CodeInternal
}

// observe ends a backup's span and records how it went.
func (s *Service) observe(ctx context.Context, span trace.Span, began time.Time, sum Summary, o WriteOptions, err error) {
	code := errorCode(err)
	if span.IsRecording() {
		span.SetAttributes(filesKey.Int(sum.Files), sizeKey.Int64(sum.Bytes), certsKey.Bool(o.Certs && s.opts.CertDir != ""))
	}
	instrument.End(span, err, code)
	var attrs []attribute.KeyValue
	if code != "" {
		attrs = append(attrs, semconv.ErrorTypeKey.String(code))
	}
	s.duration.Record(ctx, time.Since(began).Seconds(), metric.WithAttributes(attrs...))
	if err == nil {
		s.size.Record(ctx, sum.Bytes)
		s.lastSuccess.Store(s.opts.Now().UnixNano())
	}
}

// HoldGC is for the garbage collector. ok is false while a backup is being
// written, and the collector must then not run. Otherwise the collector
// calls release when it is done, and a backup that starts meanwhile waits
// for that.
func (s *Service) HoldGC() (release func(), ok bool) {
	select {
	case s.slot <- struct{}{}:
		return func() { <-s.slot }, true
	default:
		return nil, false
	}
}

// Estimate measures what a backup would hold.
func (s *Service) Estimate(ctx context.Context) (Estimate, error) {
	var e Estimate
	var err error
	if e.DatabaseBytes, err = s.databaseBytes(); err != nil {
		return Estimate{}, err
	}
	if err := ctx.Err(); err != nil {
		return Estimate{}, err
	}
	if e.BlobBytes, err = disk.TreeSize(s.opts.BlobDir); err != nil {
		return Estimate{}, fmt.Errorf("backup: measure blobs: %w", err)
	}
	if e.FreeBytes, err = s.opts.Free(s.opts.TempDir); err != nil {
		return Estimate{}, fmt.Errorf("backup: free space in %s: %w", s.opts.TempDir, err)
	}
	return e, nil
}

// databaseBytes bounds the size of the copy from above: the database file
// and the write-ahead log, whose pages the copy folds in.
func (s *Service) databaseBytes() (int64, error) {
	var total int64
	for _, p := range []string{s.opts.DBPath, s.opts.DBPath + "-wal"} {
		fi, err := os.Stat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("backup: measure database: %w", err)
		}
		total += fi.Size()
	}
	return total, nil
}

// Write streams a backup to w: the database copy, the blobs and, when asked,
// the certificates, uncompressed, since most large blobs already are.
// ErrRunning when a backup is being written, ErrNoSpace when the temporary
// directory cannot hold the database copy, both before anything is written
// to w. After a later error w holds a truncated archive.
func (s *Service) Write(ctx context.Context, w io.Writer, o WriteOptions) (sum Summary, err error) {
	ctx, span := instrument.StartChild(ctx, s.tracer, "backup.write")
	began := time.Now()
	defer func() { s.observe(ctx, span, began, sum, o, err) }()
	if !s.running.CompareAndSwap(false, true) {
		return Summary{}, ErrRunning
	}
	defer s.running.Store(false)
	select {
	case s.slot <- struct{}{}:
		defer func() { <-s.slot }()
	case <-ctx.Done():
		return Summary{}, fmt.Errorf("backup: waiting for the garbage collector: %w", ctx.Err())
	}

	need, err := s.databaseBytes()
	if err != nil {
		return Summary{}, err
	}
	free, err := s.opts.Free(s.opts.TempDir)
	if err != nil {
		return Summary{}, fmt.Errorf("backup: free space in %s: %w", s.opts.TempDir, err)
	}
	if free < uint64(need) {
		return Summary{}, fmt.Errorf("%w: %s has %d bytes free and the copy needs up to %d", ErrNoSpace, s.opts.TempDir, free, need)
	}

	copyPath, err := tempName(s.opts.TempDir)
	if err != nil {
		return Summary{}, err
	}
	defer func() {
		if rerr := os.Remove(copyPath); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			s.log.WarnContext(ctx, "backup: remove database copy", "path", copyPath, "err", rerr)
		}
	}()
	if err := s.db.VacuumInto(ctx, copyPath); err != nil {
		return Summary{}, fmt.Errorf("backup: copy database: %w", err)
	}

	a := &archive{ctx: ctx, tw: tar.NewWriter(w)}
	if err := a.file(copyPath, filepath.Base(s.opts.DBPath), s.opts.Now()); err != nil {
		return a.sum, err
	}
	// Dot files in the blob directory are blobs still being written.
	if err := a.tree(s.opts.BlobDir, true); err != nil {
		return a.sum, err
	}
	if o.Certs && s.opts.CertDir != "" {
		if err := a.tree(s.opts.CertDir, false); err != nil {
			return a.sum, err
		}
	}
	if err := a.tw.Close(); err != nil {
		return a.sum, fmt.Errorf("backup: finish archive: %w", err)
	}
	s.record(ctx, a.sum, o)
	return a.sum, nil
}

func (s *Service) record(ctx context.Context, sum Summary, o WriteOptions) {
	if s.opts.Audit == nil {
		return
	}
	actor := "admin"
	if p, ok := auth.FromContext(ctx); ok && p.Handle != "" {
		actor = p.Handle
	}
	err := s.opts.Audit.Record(ctx, audit.Event{At: s.opts.Now(), Actor: actor, Action: audit.BackupCreate, Details: map[string]string{
		"files": strconv.Itoa(sum.Files), "bytes": strconv.FormatInt(sum.Bytes, 10), "certificates": strconv.FormatBool(o.Certs && s.opts.CertDir != ""),
	}})
	if err != nil {
		s.log.WarnContext(ctx, "backup: audit", "err", err)
	}
}

const tempPrefix = "backup-"

func tempName(dir string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("backup: name database copy: %w", err)
	}
	return filepath.Join(dir, tempPrefix+hex.EncodeToString(b[:])+".db"), nil
}

// RemoveStale removes database copies in dir older than before, which a
// crash left behind, and returns how many.
func RemoveStale(dir string, before time.Time) (int, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("backup: remove stale copies: %w", err)
	}
	var (
		n    int
		errs []error
	)
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasPrefix(e.Name(), tempPrefix) {
			continue
		}
		info, err := e.Info()
		if err == nil && !info.ModTime().Before(before) {
			continue
		}
		if err == nil {
			err = os.Remove(filepath.Join(dir, e.Name()))
		}
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("backup: remove stale copy: %w", err))
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

// archive is a tar stream being written.
type archive struct {
	ctx context.Context
	tw  *tar.Writer
	sum Summary
}

// tree adds every regular file under root, named from root's base name, in
// lexical order. A missing root adds nothing.
func (a *archive) tree(root string, skipDotFiles bool) error {
	base := filepath.Base(root)
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("backup: read %s: %w", base, err)
		}
		if !d.Type().IsRegular() || skipDotFiles && strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return fmt.Errorf("backup: read %s: %w", base, err)
		}
		return a.file(p, path.Join(base, filepath.ToSlash(rel)), time.Time{})
	})
}

// file adds the file at p as name. A zero mtime keeps the file's own. A
// file that is gone by now was never referenced: only an abandoned write
// removes one while the collector is held off.
func (a *archive) file(p, name string, mtime time.Time) error {
	if err := a.ctx.Err(); err != nil {
		return err
	}
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	if mtime.IsZero() {
		mtime = fi.ModTime()
	}
	hdr := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Size:     fi.Size(),
		Mode:     0o600,
		// Whole seconds: a finer time costs an extended header, 1 KiB, for
		// every blob.
		ModTime: mtime.UTC().Truncate(time.Second),
	}
	if err := a.tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("backup: write %s: %w", name, err)
	}
	// Exactly the size the header promised, whatever happens to the file.
	n, err := io.CopyN(a.tw, ctxReader{a.ctx, f}, fi.Size())
	a.sum.Bytes += n
	if err != nil {
		return fmt.Errorf("backup: write %s: %w", name, err)
	}
	a.sum.Files++
	return nil
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
