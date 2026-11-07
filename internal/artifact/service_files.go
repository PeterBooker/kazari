package artifact

import (
	"bufio"
	"context"
	"errors"
	"io"
	"maps"
	"mime"
	"net/http"
	"path"
	"slices"
	"strings"
)

// sniffLen is how much of a file http.DetectContentType looks at.
const sniffLen = 512

// buildFiles applies inputs to base (nil on create), streaming additions
// into the blob store, and returns the files sorted by path with their
// total size. A nil quotaLeft is unlimited. The hash of every blob stored
// is added to put, on failure too.
func (s *Service) buildFiles(ctx context.Context, base map[string]File, inputs []FileInput, quotaLeft *int64, put *[]string) ([]File, int64, error) {
	type addition struct {
		path string
		in   FileInput
	}
	result := maps.Clone(base)
	if result == nil {
		result = map[string]File{}
	}
	seen := make(map[string]bool, len(inputs))
	var adds []addition
	for _, in := range inputs {
		p, err := ValidatePath(in.Path)
		if err != nil {
			return nil, 0, err
		}
		if seen[p] {
			return nil, 0, Errorf(ErrInvalidArgument, "path %q appears more than once", p)
		}
		seen[p] = true
		delete(result, p) // removed, or replaced by the addition
		if in.Delete {
			continue
		}
		if in.Content == nil {
			return nil, 0, Errorf(ErrInvalidArgument, "file %q has no content", p)
		}
		if err := ValidateContentType(in.ContentType); err != nil {
			return nil, 0, err
		}
		adds = append(adds, addition{p, in})
	}

	count := len(result) + len(adds)
	if count == 0 {
		return nil, 0, Errorf(ErrInvalidArgument, "a version needs at least one file")
	}
	if count > s.opts.MaxFiles {
		return nil, 0, Errorf(ErrTooManyFiles, "%d files exceeds the limit of %d", count, s.opts.MaxFiles)
	}

	var carried int64
	for _, f := range result {
		carried += f.Size
	}
	versionLeft := s.opts.MaxVersionBytes - carried
	if versionLeft < 0 {
		return nil, 0, Errorf(ErrTooLarge, "version exceeds %d bytes", s.opts.MaxVersionBytes)
	}
	if quotaLeft != nil {
		*quotaLeft -= carried + int64(count)*FileChargeBytes
		if *quotaLeft < 0 {
			return nil, 0, Errorf(ErrQuotaExceeded, "the new version would exceed the owner's quota")
		}
	}

	for _, ad := range adds {
		f, err := s.putFile(ctx, ad.path, ad.in, &versionLeft, quotaLeft, put)
		if err != nil {
			return nil, 0, err
		}
		result[f.Path] = f
	}

	files := slices.Collect(maps.Values(result))
	slices.SortFunc(files, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
	var total int64
	for _, f := range files {
		total += f.Size
	}
	return files, total, nil
}

// putFile records the blob row so the garbage collector's grace window
// starts now.
func (s *Service) putFile(ctx context.Context, p string, in FileInput, versionLeft, quotaLeft *int64, put *[]string) (File, error) {
	fileLeft := s.opts.MaxFileBytes
	var r io.Reader = &limitReader{r: in.Content, left: &fileLeft,
		err: Errorf(ErrTooLarge, "file %q exceeds %d bytes", p, s.opts.MaxFileBytes)}
	r = &limitReader{r: r, left: versionLeft,
		err: Errorf(ErrTooLarge, "version exceeds %d bytes", s.opts.MaxVersionBytes)}
	if quotaLeft != nil {
		r = &limitReader{r: r, left: quotaLeft,
			err: Errorf(ErrQuotaExceeded, "the new version would exceed the owner's quota")}
	}

	ct := in.ContentType
	if ct == "" {
		ct = mime.TypeByExtension(path.Ext(p))
	}
	if ct == "" {
		br := bufio.NewReaderSize(r, sniffLen)
		head, err := br.Peek(sniffLen)
		if err != nil && !errors.Is(err, io.EOF) {
			return File{}, domainErr(err)
		}
		ct = http.DetectContentType(head)
		r = br
	}

	hash, size, err := s.blobs.Put(ctx, r)
	if err != nil {
		return File{}, domainErr(err)
	}
	*put = append(*put, hash)
	if err := s.store.RecordBlob(ctx, hash, size, s.now()); err != nil {
		return File{}, err
	}
	return File{Path: p, Hash: hash, Size: size, ContentType: ct}, nil
}

// dropBlobs removes what a failed publish stored, so a failure takes no disk
// that a quota does not count. The write side of blobMu waits out publishes
// in progress, any of which may have stored the same content, and a blob a
// version references stays. A blob that cannot be dropped is left to
// CollectGarbage.
func (s *Service) dropBlobs(ctx context.Context, hashes []string) {
	s.blobMu.Lock()
	defer s.blobMu.Unlock()
	for _, h := range hashes {
		if err := s.dropBlob(ctx, h); err != nil {
			s.opts.Logger.ErrorContext(ctx, "dropping a failed publish's blob", "err", err)
		}
	}
}

// dropBlob deletes in CollectGarbage's order: the file before the row.
func (s *Service) dropBlob(ctx context.Context, hash string) error {
	referenced, err := s.store.BlobReferenced(ctx, hash)
	if err != nil || referenced {
		return err
	}
	if err := s.blobs.Delete(ctx, hash); err != nil {
		return err
	}
	return s.store.DeleteBlob(ctx, hash)
}

// domainErr unwraps an *Error the blob store wrapped, so callers see the
// domain error itself.
func domainErr(err error) error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return err
}

// limitReader fails with err once more than *left bytes are read; left may
// be shared to enforce a total across files.
type limitReader struct {
	r    io.Reader
	left *int64
	err  error
}

func (l *limitReader) Read(p []byte) (int, error) {
	if *l.left < 0 {
		return 0, l.err
	}
	// Read at most one byte past the limit: enough to detect overflow.
	if int64(len(p)) > *l.left+1 {
		p = p[:*l.left+1]
	}
	n, err := l.r.Read(p)
	*l.left -= int64(n)
	if *l.left < 0 {
		return 0, l.err
	}
	return n, err
}
