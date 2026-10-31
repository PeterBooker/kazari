package httpapi

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strings"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/upload"
)

// receiveArchive is the one route without a credential: the token in the URL
// stands for one. The body is read only once the token has opened an upload,
// is capped at the size that upload declared, and is kept only when it hashes
// to the declared digest, so whoever holds a leaked URL can stage nothing but
// bytes the owner already has.
func (h *userAPI) receiveArchive(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	if h.d.Uploads == nil {
		return artifact.ErrUploadNotFound
	}
	ctx := r.Context()
	id, token := r.PathValue("id"), r.URL.Query().Get("token")
	u, err := h.d.Uploads.Archive(ctx, id, token)
	if err != nil {
		return err
	}
	want := u.Archive

	spill, err := os.CreateTemp(h.o.TempDir, spillPrefix+"*")
	if err != nil {
		return fmt.Errorf("httpapi: spill archive: %w", err)
	}
	defer func() {
		if err := errors.Join(spill.Close(), os.Remove(spill.Name())); err != nil {
			h.log.ErrorContext(ctx, "httpapi: remove archive spill", "err", err)
		}
	}()
	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(spill, sum), http.MaxBytesReader(w, r.Body, want.Bytes))
	var mbe *http.MaxBytesError
	switch {
	case errors.As(err, &mbe):
		return h.rejectArchive(ctx, id, token, fmt.Sprintf("the body is longer than the %d bytes declared", want.Bytes))
	case err != nil:
		return bodyErr("reading the archive", err)
	}
	got := hex.EncodeToString(sum.Sum(nil))
	if n != want.Bytes || subtle.ConstantTimeCompare([]byte(got), []byte(want.SHA256)) != 1 {
		return h.rejectArchive(ctx, id, token,
			fmt.Sprintf("the body has %d bytes and sha256 %s, not what the upload was begun with", n, got))
	}

	files, closeFiles, err := h.readArchive(ctx, spill, n)
	if err != nil {
		return h.dropArchive(ctx, u, err)
	}
	defer closeFiles()
	staged, err := h.d.Uploads.ReceiveArchive(ctx, id, token, files)
	switch {
	case errors.Is(err, artifact.ErrUploadNotFound):
		return err
	case err != nil:
		return removedArchive(err)
	}
	writeJSON(w, http.StatusOK, api.ArchiveResult{UploadID: staged.ID, Files: len(staged.Files), Bytes: staged.Bytes})
	return nil
}

func (h *userAPI) rejectArchive(ctx context.Context, id, token, why string) error {
	left, err := h.d.Uploads.RejectArchive(ctx, id, token)
	if err != nil {
		return err
	}
	if left == 0 {
		return artifact.Errorf(artifact.ErrDigestMismatch, "%s; the upload is removed, begin a new one", why)
	}
	return artifact.Errorf(artifact.ErrDigestMismatch, "%s; %d more attempts", why, left)
}

// dropArchive removes the upload when the body matched but is not an archive
// Kazari can read: the same bytes would fail again, so holding the slot open
// until the TTL would only block the owner's next attempt.
func (h *userAPI) dropArchive(ctx context.Context, u *upload.Upload, err error) error {
	if aerr := h.d.Uploads.Abort(ctx, u.Owner, u.ID); aerr != nil && !errors.Is(aerr, artifact.ErrUploadNotFound) {
		h.log.ErrorContext(ctx, "httpapi: drop archive upload", "upload", u.ID, "err", aerr)
	}
	return removedArchive(err)
}

// removedArchive tells the sender that the upload is gone with the error that
// ended it, so the next step is a new begin_upload rather than a retry.
func removedArchive(err error) error {
	var ae *artifact.Error
	if errors.As(err, &ae) {
		return &artifact.Error{Code: ae.Code, Message: ae.Message + "; the upload is removed, begin a new one"}
	}
	return err
}

// readArchive tells a zip from a gzip-compressed tar by its first bytes, not
// by a Content-Type the sender may not have set.
func (h *userAPI) readArchive(ctx context.Context, spill *os.File, size int64) ([]artifact.FileInput, func(), error) {
	var head [2]byte
	if _, err := spill.ReadAt(head[:], 0); err != nil {
		return nil, nil, invalid("the archive is too short to be a zip or a gzip-compressed tar")
	}
	switch head {
	case [2]byte{'P', 'K'}:
		files, err := readZip(spill, size, h.d.Uploads.MaxFiles())
		return files, func() {}, err
	case [2]byte{0x1f, 0x8b}:
		b, err := readBundle(io.NewSectionReader(spill, 0, size), h.o.TempDir, h.o.MaxBodyBytes)
		if err != nil {
			return nil, nil, err
		}
		return b.files, func() { h.closeBundle(ctx, b) }, nil
	}
	return nil, nil, invalid("the archive is neither a zip nor a gzip-compressed tar")
}

// readZip lists the regular files of a zip. Entries inflate only when read,
// so nothing is decompressed before the upload service has accepted the
// path and has room for the bytes, and the listing stops at maxFiles so a
// central directory of a million names is not held in memory. Names are
// validated by the service like any other path; the reader's own name check
// is not relied on.
func readZip(ra io.ReaderAt, size int64, maxFiles int) ([]artifact.FileInput, error) {
	zr, err := zip.NewReader(ra, size)
	if err != nil && !errors.Is(err, zip.ErrInsecurePath) {
		return nil, invalid("the archive is not a valid zip: %v", err)
	}
	var files []artifact.FileInput
	for _, f := range zr.File {
		if !f.Mode().IsRegular() {
			continue
		}
		name := f.Name
		for strings.HasPrefix(name, "./") {
			name = name[2:]
		}
		if name == "" {
			continue
		}
		if len(name) > artifact.PathMaxBytes {
			return nil, artifact.Errorf(artifact.ErrInvalidPath, "archive entry name longer than %d bytes", artifact.PathMaxBytes)
		}
		if len(files) >= maxFiles {
			return nil, artifact.Errorf(artifact.ErrTooManyFiles, "more than %d files in one upload", maxFiles)
		}
		files = append(files, artifact.FileInput{Path: name, Content: &zipEntry{f: f}})
	}
	return files, nil
}

// zipEntry opens its entry on the first Read and closes it at the end.
type zipEntry struct {
	f  *zip.File
	rc io.ReadCloser
}

func (e *zipEntry) Read(p []byte) (int, error) {
	if e.rc == nil {
		rc, err := e.f.Open()
		if err != nil {
			return 0, zipErr(e.f.Name, err)
		}
		e.rc = rc
	}
	n, err := e.rc.Read(p)
	switch {
	case errors.Is(err, io.EOF):
		if cerr := e.rc.Close(); cerr != nil {
			return n, zipErr(e.f.Name, cerr)
		}
		e.rc = io.NopCloser(strings.NewReader(""))
	case err != nil:
		return n, zipErr(e.f.Name, err)
	}
	return n, err
}

// zipErr reports a failure to read the spill as the server's fault and
// anything else as a bad archive.
func zipErr(name string, err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("httpapi: read archive spill: %w", err)
	}
	return invalid("the zip entry %q cannot be read: %v", name, err)
}
