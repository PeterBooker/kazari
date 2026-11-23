package httpapi

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"os"
	"path/filepath"

	"strings"
	"time"

	"github.com/PeterBooker/kazari/internal/artifact"
)

const manifestLimit = 64 << 10

const spillPrefix = "kazari-bundle-"

func readManifest(mr *multipart.Reader, v any) error {
	p, err := mr.NextPart()
	if errors.Is(err, io.EOF) {
		return invalid("the multipart body has no manifest part")
	}
	if err != nil {
		return bodyErr("malformed multipart body", err)
	}
	defer p.Close()
	if p.FormName() != "manifest" {
		return invalid("the first part must be \"manifest\", not %q", p.FormName())
	}
	raw, err := io.ReadAll(io.LimitReader(p, manifestLimit+1))
	if err != nil {
		return bodyErr("reading the manifest", err)
	}
	if len(raw) > manifestLimit {
		return artifact.Errorf(artifact.ErrTooLarge, "the manifest exceeds %s", artifact.FormatBytes(manifestLimit))
	}
	return decodeStrict(bytes.NewReader(raw), v, false)
}

// nextBundle returns nil when the body has no further part.
func (h *userAPI) nextBundle(mr *multipart.Reader) (*bundle, error) {
	p, err := mr.NextPart()
	if errors.Is(err, io.EOF) {
		return nil, nil
	}
	if err != nil {
		return nil, bodyErr("malformed multipart body", err)
	}
	defer p.Close()
	if p.FormName() != "bundle" {
		return nil, invalid("the second part must be \"bundle\", not %q", p.FormName())
	}
	return h.readAnyBundle(p)
}

// readAnyBundle tells a gzip-compressed tar, a plain tar and a zip apart by
// their first bytes, not by a Content-Type the sender may not have set.
func (h *userAPI) readAnyBundle(r io.Reader) (*bundle, error) {
	br := bufio.NewReaderSize(r, 512)
	head, _ := br.Peek(tarMagicEnd)
	switch {
	case bytes.HasPrefix(head, []byte{0x1f, 0x8b}):
		return readBundle(br, h.o.TempDir, h.o.MaxBodyBytes)
	case bytes.HasPrefix(head, []byte("PK")):
		return h.readZipBundle(br)
	case len(head) == tarMagicEnd && string(head[tarMagicStart:]) == "ustar":
		return readTarBundle(br, h.o.TempDir, h.o.MaxBodyBytes)
	}
	return nil, invalid("the bundle is neither a gzip-compressed tar, a tar nor a zip")
}

// The ustar magic sits at offset 257 of the first header; GNU tar writes
// "ustar " there too.
const (
	tarMagicStart = 257
	tarMagicEnd   = tarMagicStart + len("ustar")
)

// readZipBundle spills the zip whole, since its directory is at the end.
// The request body is already capped, so the spill is too.
func (h *userAPI) readZipBundle(r io.Reader) (*bundle, error) {
	f, err := os.CreateTemp(h.o.TempDir, spillPrefix+"*")
	if err != nil {
		return nil, fmt.Errorf("httpapi: spill bundle: %w", err)
	}
	b := &bundle{f: f}
	n, err := io.Copy(f, r)
	if err != nil {
		if artifact.CodeOf(err) == "" {
			err = bodyErr("reading the bundle", err)
		}
		return nil, errors.Join(err, b.Close())
	}
	if b.files, err = readZip(f, n, h.d.Uploads.MaxFiles()); err != nil {
		return nil, errors.Join(err, b.Close())
	}
	return b, nil
}

// stripTopDir drops a directory every path starts with, when that directory
// holds an index.html and the root does not: what "tar czf site.tar.gz site"
// makes. A bundle of only assets/... is left alone, since an update may mean
// to add files there.
func stripTopDir(files []artifact.FileInput) []artifact.FileInput {
	if len(files) == 0 {
		return files
	}
	dir, _, ok := strings.Cut(files[0].Path, "/")
	if !ok || dir == "" {
		return files
	}
	prefix := dir + "/"
	hasIndex := false
	for _, f := range files {
		if !strings.HasPrefix(f.Path, prefix) {
			return files
		}
		hasIndex = hasIndex || f.Path == prefix+"index.html"
	}
	if !hasIndex {
		return files
	}
	out := make([]artifact.FileInput, len(files))
	for i, f := range files {
		f.Path = strings.TrimPrefix(f.Path, prefix)
		out[i] = f
	}
	return out
}

// bundle spills its entries to one temporary file, never memory: the service
// validates every path before it reads any content, and a tar stream reveals
// the next path only after the previous file's bytes.
type bundle struct {
	f     *os.File
	files []artifact.FileInput
}

// readBundle keeps regular files only. limit caps every decompressed byte tar
// reads, headers, PAX records and skipped entries included, so a gzip bomb
// stops at the size of the request body.
func readBundle(r io.Reader, dir string, limit int64) (*bundle, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, bodyErr("the bundle is not gzip", err)
	}
	defer zr.Close()
	return readTarBundle(zr, dir, limit)
}

// readTarBundle is readBundle for a tar that is not compressed, or already
// decompressed.
func readTarBundle(r io.Reader, dir string, limit int64) (*bundle, error) {
	f, err := os.CreateTemp(dir, spillPrefix+"*")
	if err != nil {
		return nil, fmt.Errorf("httpapi: spill bundle: %w", err)
	}
	b := &bundle{f: f}
	capped := &capReader{r: r, left: limit,
		err: artifact.Errorf(artifact.ErrTooLarge, "the bundle is over %s uncompressed", artifact.FormatBytes(limit))}
	if err := b.fill(tar.NewReader(capped), limit, capped.err); err != nil {
		return nil, errors.Join(err, b.Close())
	}
	// Drain so a gzip checksum is verified.
	if _, err := io.Copy(io.Discard, capped); err != nil {
		if artifact.CodeOf(err) == "" {
			err = bodyErr("the bundle is corrupt", err)
		}
		return nil, errors.Join(err, b.Close())
	}
	return b, nil
}

// fill charges each entry's declared size against limit before copying it:
// archive/tar materialises a sparse entry's holes from zeros that never pass
// the capping reader, so the read-side cap alone does not bound the spill.
func (b *bundle) fill(tr *tar.Reader, limit int64, tooLarge error) error {
	var off int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return tarErr(err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		name := hdr.Name
		for strings.HasPrefix(name, "./") {
			name = name[2:]
		}
		if name == "" {
			continue
		}
		// Names are held until the service validates paths, so refuse an
		// overlong one now.
		if len(name) > artifact.PathMaxBytes {
			return artifact.Errorf(artifact.ErrInvalidPath, "bundle entry name longer than %d bytes", artifact.PathMaxBytes)
		}
		if hdr.Size > limit-off {
			return tooLarge
		}
		n, err := io.Copy(b.f, tr)
		if err != nil {
			return tarErr(err)
		}
		b.files = append(b.files, artifact.FileInput{Path: name, Content: io.NewSectionReader(b.f, off, n)})
		off += n
	}
}

// tarErr reports a *fs.PathError as a failed spill write, not a bad archive.
func tarErr(err error) error {
	var pe *fs.PathError
	switch {
	case artifact.CodeOf(err) != "":
		return err
	case errors.As(err, &pe):
		return fmt.Errorf("httpapi: spill bundle: %w", err)
	}
	return bodyErr("the bundle is not a valid tar archive", err)
}

type capReader struct {
	r    io.Reader
	left int64
	err  error
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left < 0 {
		return 0, c.err
	}
	// One byte past the cap is enough to detect overflow.
	if int64(len(p)) > c.left+1 {
		p = p[:c.left+1]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	if c.left < 0 {
		return 0, c.err
	}
	return n, err
}

// Close removes the spill file.
func (b *bundle) Close() error {
	return errors.Join(b.f.Close(), os.Remove(b.f.Name()))
}

// RemoveStaleSpills removes the spill files in dir (UserOptions.TempDir) last
// written before the given time, which a crash left behind. It returns how
// many it removed and carries on past errors.
func RemoveStaleSpills(dir string, before time.Time) (int, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("httpapi: remove stale spills: %w", err)
	}
	var (
		n    int
		errs []error
	)
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasPrefix(e.Name(), spillPrefix) {
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
			continue // removed by its request meanwhile
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("httpapi: remove stale spill: %w", err))
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

func (h *userAPI) closeBundle(ctx context.Context, b *bundle) {
	if b == nil {
		return
	}
	if err := b.Close(); err != nil {
		h.log.ErrorContext(ctx, "httpapi: remove bundle spill", "err", err)
	}
}
