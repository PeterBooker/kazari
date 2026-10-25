package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrInvalidHash is returned for a hash that is not 64 lower-case hex
// characters, so no caller-supplied string is ever used as a path.
var ErrInvalidHash = errors.New("blob: invalid hash")

// FS is a Store on the local filesystem, at dir/<hash[0:2]>/<hash>.
type FS struct {
	dir string
}

var _ Store = (*FS)(nil)

// NewFS returns an FS rooted at dir, creating it if needed.
func NewFS(dir string) (*FS, error) {
	if dir == "" {
		return nil, errors.New("blob: empty directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("blob: create %s: %w", dir, err)
	}
	return &FS{dir: dir}, nil
}

// Put renames a temp file into place, so readers never see a partial blob.
func (f *FS) Put(ctx context.Context, r io.Reader) (string, int64, error) {
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
	size, err := io.Copy(io.MultiWriter(tmp, h), ctxReader{ctx: ctx, r: r})
	if err != nil {
		return "", 0, fmt.Errorf("blob: put: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return "", 0, fmt.Errorf("blob: put: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", 0, fmt.Errorf("blob: put: %w", err)
	}

	hash := hex.EncodeToString(h.Sum(nil))
	dst := f.path(hash)
	if _, err := os.Stat(dst); err == nil {
		// Already stored; the deferred cleanup removes tmp.
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
func (f *FS) Open(_ context.Context, hash string) (io.ReadSeekCloser, error) {
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
	return file, nil
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
func (f *FS) Delete(_ context.Context, hash string) error {
	if !ValidHash(hash) {
		return fmt.Errorf("%w: %q", ErrInvalidHash, hash)
	}
	err := os.Remove(f.path(hash))
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

func (f *FS) path(hash string) string {
	return filepath.Join(f.dir, hash[:2], hash)
}

// ValidHash reports whether s is 64 lower-case hex characters.
func ValidHash(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
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
