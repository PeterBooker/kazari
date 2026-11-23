package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/PeterBooker/kazari/internal/errcode"
	"github.com/PeterBooker/kazari/internal/instrument"
)

const scope = "github.com/PeterBooker/kazari/internal/blob"

// Attribute keys. A blob's hash is never recorded: even a prefix of the hash
// of a members-only file would confirm a guess of its content.
const (
	sizeKey     = attribute.Key("kazari.blob.size")
	existingKey = attribute.Key("kazari.blob.existing")
)

// Options configure an FS. The zero value is valid.
type Options struct {
	Telemetry instrument.Providers
}

// ErrInvalidHash is returned for a hash that is not 64 lower-case hex
// characters, so no caller-supplied string is ever used as a path.
var ErrInvalidHash = errors.New("blob: invalid hash")

// FS is a Store on the local filesystem, at dir/<hash[0:2]>/<hash>.
type FS struct {
	dir    string
	tracer trace.Tracer
	writes metric.Int64Counter
	// stored and existing are the counter's two attribute sets, built once.
	stored, existing metric.AddOption
}

var _ Store = (*FS)(nil)

// NewFS returns an FS rooted at dir, creating it if needed.
func NewFS(dir string, o Options) (*FS, error) {
	if dir == "" {
		return nil, errors.New("blob: empty directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("blob: create %s: %w", dir, err)
	}
	return &FS{
		dir:    dir,
		tracer: o.Telemetry.Tracer(scope),
		writes: instrument.Handle(o.Telemetry.Meter(scope).Int64Counter("kazari.blob.writes",
			metric.WithDescription("Blob writes, by whether the content was already stored: the deduplication ratio."),
			metric.WithUnit("{write}"))),
		stored:   metric.WithAttributes(existingKey.Bool(false)),
		existing: metric.WithAttributes(existingKey.Bool(true)),
	}, nil
}

// Put renames a temp file into place, so readers never see a partial blob.
func (f *FS) Put(ctx context.Context, r io.Reader) (hash string, size int64, err error) {
	ctx, span := instrument.StartChild(ctx, f.tracer, "blob.put")
	existed := false
	defer func() {
		if err == nil {
			attrs := f.stored
			if existed {
				attrs = f.existing
			}
			f.writes.Add(ctx, 1, attrs)
			span.SetAttributes(sizeKey.Int64(size), existingKey.Bool(existed))
		}
		instrument.End(span, err, "")
	}()
	tmp, err := os.CreateTemp(f.dir, tempPrefix+"*")
	if err != nil {
		return "", 0, fmt.Errorf("blob: put: %w", err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	h := sha256.New()
	size, err = io.Copy(io.MultiWriter(tmp, h), ctxReader{ctx: ctx, r: r})
	if err != nil {
		return "", 0, fmt.Errorf("blob: put: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return "", 0, fmt.Errorf("blob: put: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", 0, fmt.Errorf("blob: put: %w", err)
	}

	hash = hex.EncodeToString(h.Sum(nil))
	dst := f.path(hash)
	if _, err := os.Stat(dst); err == nil {
		// Already stored; the deferred cleanup removes tmp.
		existed = true
		return hash, size, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", 0, fmt.Errorf("blob: put: %w", err)
	}
	// A concurrent Put of the same content may win; replacing an identical
	// file is harmless.
	if err := os.Rename(tmpName, dst); err != nil {
		return "", 0, fmt.Errorf("blob: put: %w", err)
	}
	committed = true
	return hash, size, nil
}

// Open returns the blob as an *os.File.
func (f *FS) Open(ctx context.Context, hash string) (_ io.ReadSeekCloser, err error) {
	_, span := instrument.StartChild(ctx, f.tracer, "blob.open")
	defer func() { instrument.End(span, err, errorCode(err)) }()
	if !ValidHash(hash) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidHash, hash)
	}
	file, err := os.Open(f.path(hash))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("blob: open: %w", err)
	}
	if span.IsRecording() {
		if fi, err := file.Stat(); err == nil {
			span.SetAttributes(sizeKey.Int64(fi.Size()))
		}
	}
	return file, nil
}

// errorCode keeps a missing blob, which a caller may expect, out of the
// internal errors.
func errorCode(err error) string {
	if errors.Is(err, ErrNotFound) {
		return errcode.NotFound
	}
	return ""
}

// Stat returns the size of the blob.
func (f *FS) Stat(_ context.Context, hash string) (int64, error) {
	if !ValidHash(hash) {
		return 0, fmt.Errorf("%w: %q", ErrInvalidHash, hash)
	}
	fi, err := os.Stat(f.path(hash))
	if errors.Is(err, os.ErrNotExist) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("blob: stat: %w", err)
	}
	return fi.Size(), nil
}

// Delete ignores a missing blob.
func (f *FS) Delete(ctx context.Context, hash string) (err error) {
	_, span := instrument.StartChild(ctx, f.tracer, "blob.delete")
	defer func() { instrument.End(span, err, "") }()
	if !ValidHash(hash) {
		return fmt.Errorf("%w: %q", ErrInvalidHash, hash)
	}
	err = os.Remove(f.path(hash))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("blob: delete: %w", err)
	}
	return nil
}

const tempPrefix = ".put-"

// RemoveStaleTemp removes Put's temp files older than before, which only a
// crash leaves behind, and returns how many. It continues past errors.
func (f *FS) RemoveStaleTemp(before time.Time) (int, error) {
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		return 0, fmt.Errorf("blob: remove stale temp: %w", err)
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
			err = os.Remove(filepath.Join(f.dir, e.Name()))
		}
		if errors.Is(err, os.ErrNotExist) {
			continue // committed or removed by its Put meanwhile
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("blob: remove stale temp: %w", err))
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

// List yields every stored blob, and nothing a Put has not finished. It
// walks dir/<hash[0:2]>/ in order, skipping temp files and any name that is
// not a blob where Open would find it. An error ends the sequence.
func (f *FS) List(ctx context.Context) iter.Seq2[Info, error] {
	return func(yield func(Info, error) bool) {
		prefixes, err := os.ReadDir(f.dir)
		if err != nil {
			yield(Info{}, fmt.Errorf("blob: list: %w", err))
			return
		}
		for _, p := range prefixes {
			if !p.IsDir() || len(p.Name()) != 2 || !lowerHex(p.Name()) {
				continue
			}
			if err := ctx.Err(); err != nil {
				yield(Info{}, err)
				return
			}
			entries, err := os.ReadDir(filepath.Join(f.dir, p.Name()))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				yield(Info{}, fmt.Errorf("blob: list: %w", err))
				return
			}
			for _, e := range entries {
				if !e.Type().IsRegular() || !ValidHash(e.Name()) || e.Name()[:2] != p.Name() {
					continue
				}
				fi, err := e.Info()
				if errors.Is(err, os.ErrNotExist) {
					continue // deleted since the directory was read
				}
				if err != nil {
					yield(Info{}, fmt.Errorf("blob: list: %w", err))
					return
				}
				if !yield(Info{Hash: e.Name(), Size: fi.Size(), ModTime: fi.ModTime()}, nil) {
					return
				}
			}
		}
	}
}

func (f *FS) path(hash string) string {
	return filepath.Join(f.dir, hash[:2], hash)
}

// ValidHash reports whether s is 64 lower-case hex characters.
func ValidHash(s string) bool {
	return len(s) == sha256.Size*2 && lowerHex(s)
}

func lowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
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
