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
	"slices"
	"strings"

	"agalma/internal/api"
	"agalma/internal/artifact"
)

// manifestLimit caps the manifest part of a multipart publish.
const manifestLimit = 64 << 10

// inlineFiles converts the files[] of a JSON body. Delete entries are only
// allowed on update.
func inlineFiles(in []api.FileInput, allowDelete bool) ([]artifact.FileInput, error) {
	out := make([]artifact.FileInput, 0, len(in))
	for _, f := range in {
		if f.Delete {
			if !allowDelete {
				return nil, invalid("cannot delete %q when creating an artifact", f.Path)
			}
			out = append(out, artifact.FileInput{Path: f.Path, Delete: true})
			continue
		}
		var content io.Reader
		switch f.Encoding {
		case "", "utf8":
			content = strings.NewReader(f.Content)
		case "base64":
			b, err := base64.StdEncoding.DecodeString(f.Content)
			if err != nil {
				return nil, invalid("file %q: invalid base64: %v", f.Path, err)
			}
			content = bytes.NewReader(b)
		default:
			return nil, invalid("file %q: encoding must be utf8 or base64, not %q", f.Path, f.Encoding)
		}
		out = append(out, artifact.FileInput{Path: f.Path, Content: content, ContentType: f.ContentType})
	}
	return out, nil
}

// mergeFiles returns primary plus every entry of secondary whose path
// primary does not already name; primary wins a clash.
func mergeFiles(primary, secondary []artifact.FileInput) []artifact.FileInput {
	if len(secondary) == 0 {
		return primary
	}
	taken := make(map[string]bool, len(primary))
	for _, f := range primary {
		taken[normPath(f.Path)] = true
	}
	out := slices.Clone(primary)
	for _, f := range secondary {
		if !taken[normPath(f.Path)] {
			out = append(out, f)
		}
	}
	return out
}

// normPath compares paths the way the service will store them; an invalid
// path is left as it is for the service to reject.
func normPath(p string) string {
	if clean, err := artifact.ValidatePath(p); err == nil {
		return clean
	}
	return p
}

// withUpload merges the staged files of upload id under inline. The
// returned done func removes the staging directory; the caller must call
// it once the publish has succeeded or failed.
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
	return mergeFiles(inline, staged), done, nil
}

// readManifest reads the first part of a multipart publish, which must be
// "manifest", as strict JSON into v.
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

// nextBundle reads the part after the manifest, which must be "bundle",
// into a spilled bundle. It returns nil when the body has no further part.
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

// bundle is the regular files of a tar.gz bundle.
//
// The artifact service validates every path of a version before it reads
// any content, and a tar stream only reveals the next path after the
// previous file's bytes, so the entries are spilled to one temporary file
// (never memory) and handed over as sections of it.
type bundle struct {
	f     *os.File
	files []artifact.FileInput
}

// readBundle decodes gzip -> tar from r, keeping regular files only
// (directories, links and anything else are skipped) and stripping "./"
// prefixes. limit caps the uncompressed total, so a gzip bomb stops at the
// same size as the request body.
func readBundle(r io.Reader, dir string, limit int64) (*bundle, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, bodyErr("the bundle is not gzip", err)
	}
	defer zr.Close()
	f, err := os.CreateTemp(dir, "agalma-bundle-*")
	if err != nil {
		return nil, fmt.Errorf("httpapi: spill bundle: %w", err)
	}
	b := &bundle{f: f}
	if err := b.fill(tar.NewReader(zr), limit); err != nil {
		return nil, errors.Join(err, b.Close())
	}
	// Drain to the end of the gzip stream so its checksum is verified.
	if _, err := io.Copy(io.Discard, io.LimitReader(zr, limit)); err != nil {
		return nil, errors.Join(bodyErr("the bundle is corrupt", err), b.Close())
	}
	return b, nil
}

func (b *bundle) fill(tr *tar.Reader, limit int64) error {
	left := limit
	var off int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return bodyErr("the bundle is not a valid tar archive", err)
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
		n, err := io.Copy(b.f, io.LimitReader(tr, left+1))
		if err != nil {
			var pe *fs.PathError
			if errors.As(err, &pe) {
				return fmt.Errorf("httpapi: spill bundle: %w", err)
			}
			return bodyErr("the bundle is not a valid tar archive", err)
		}
		left -= n
		if left < 0 {
			return artifact.Errorf(artifact.ErrTooLarge, "the bundle exceeds %d bytes uncompressed", limit)
		}
		b.files = append(b.files, artifact.FileInput{Path: name, Content: io.NewSectionReader(b.f, off, n)})
		off += n
	}
}

// Close removes the spill file.
func (b *bundle) Close() error {
	return errors.Join(b.f.Close(), os.Remove(b.f.Name()))
}

// closeBundle closes b, logging a failure; b may be nil.
func (h *userAPI) closeBundle(ctx context.Context, b *bundle) {
	if b == nil {
		return
	}
	if err := b.Close(); err != nil {
		h.log.ErrorContext(ctx, "httpapi: remove bundle spill", "err", err)
	}
}
