package httpapi

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"os"
	"path/filepath"

	"strings"
	"time"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/artifact"
)

const manifestLimit = 64 << 10

const spillPrefix = "kazari-bundle-"

// inlineFiles returns the files of a JSON body, with those named by url
// downloaded; the func it returns removes the downloads.
func (h *userAPI) inlineFiles(ctx context.Context, in []api.FileInput, allowDelete bool) ([]artifact.FileInput, func(), error) {
	dl := h.d.Fetch.Downloads(ctx, h.o.MaxFileBytes, h.o.MaxTotalBytes)
	out := make([]artifact.FileInput, 0, len(in))
	for _, f := range in {
		var (
			fi  artifact.FileInput
			err error
		)
		switch {
		case f.Delete && !allowDelete:
			err = invalid("cannot delete %q when creating an artifact", f.Path)
		case f.Delete:
			fi = artifact.FileInput{Path: f.Path, Delete: true}
		case f.URL != "" && f.Content != "":
			err = invalid("file %q has both url and content", f.Path)
		case f.URL != "":
			fi, err = dl.Add(f.Path, f.URL, f.ContentType)
		default:
			fi, err = inlineFile(f)
		}
		if err != nil {
			dl.Close()
			return nil, nil, err
		}
		out = append(out, fi)
	}
	return out, dl.Close, nil
}

func inlineFile(f api.FileInput) (artifact.FileInput, error) {
	var content io.Reader
	switch f.Encoding {
	case "", "utf8":
		content = strings.NewReader(f.Content)
	case "base64":
		b, err := base64.StdEncoding.DecodeString(f.Content)
		if err != nil {
			return artifact.FileInput{}, invalid("file %q: invalid base64: %v", f.Path, err)
		}
		content = bytes.NewReader(b)
	default:
		return artifact.FileInput{}, invalid("file %q: encoding must be utf8 or base64, not %q", f.Path, f.Encoding)
	}
	return artifact.FileInput{Path: f.Path, Content: content, ContentType: f.ContentType}, nil
}

// withUpload merges upload id's staged files under inline. The caller must
// call done once the publish has succeeded or failed.
func (h *userAPI) withUpload(ctx context.Context, owner, id string, inline []artifact.FileInput) ([]artifact.FileInput, func(), error) {
	if id == "" {
		return inline, func() {}, nil
	}
	if h.d.Uploads == nil {
		return nil, nil, invalid("chunked uploads are not enabled")
	}
	staged, cleanup, err := h.d.Uploads.Files(ctx, owner, id)
	if err != nil {
		return nil, nil, err
	}
	done := func() {
		if err := cleanup(); err != nil {
			h.log.ErrorContext(ctx, "httpapi: upload cleanup failed", "upload", id, "err", err)
		}
	}
	return artifact.MergeFiles(inline, staged), done, nil
}

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
		return artifact.Errorf(artifact.ErrTooLarge, "the manifest exceeds %d bytes", manifestLimit)
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
	return readBundle(p, h.o.TempDir, h.o.MaxBodyBytes)
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
	f, err := os.CreateTemp(dir, spillPrefix+"*")
	if err != nil {
		return nil, fmt.Errorf("httpapi: spill bundle: %w", err)
	}
	b := &bundle{f: f}
	capped := &capReader{r: zr, left: limit,
		err: artifact.Errorf(artifact.ErrTooLarge, "the bundle exceeds %d bytes uncompressed", limit)}
	if err := b.fill(tar.NewReader(capped)); err != nil {
		return nil, errors.Join(err, b.Close())
	}
	// Drain so the gzip checksum is verified.
	if _, err := io.Copy(io.Discard, capped); err != nil {
		if artifact.CodeOf(err) == "" {
			err = bodyErr("the bundle is corrupt", err)
		}
		return nil, errors.Join(err, b.Close())
	}
	return b, nil
}

func (b *bundle) fill(tr *tar.Reader) error {
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
