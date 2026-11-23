package artifact

import "github.com/PeterBooker/kazari/internal/errcode"

// Error is a domain error with a stable code.
type Error = errcode.Error

var (
	ErrNotFound           = errcode.New(errcode.NotFound, "artifact not found")
	ErrVersionNotFound    = errcode.New(errcode.VersionNotFound, "version not found")
	ErrFileNotFound       = errcode.New(errcode.FileNotFound, "file not found")
	ErrSlugTaken          = errcode.New(errcode.SlugTaken, "slug is already taken")
	ErrVersionConflict    = errcode.New(errcode.VersionConflict, "artifact has a newer version than expected")
	ErrPublishedNotLatest = errcode.New(errcode.PublishedNotLatest, "an earlier version is published")
	ErrTooLarge           = errcode.New(errcode.TooLarge, "content exceeds the size limit")
	ErrTooManyFiles       = errcode.New(errcode.TooManyFiles, "too many files")
	ErrQuotaExceeded      = errcode.New(errcode.QuotaExceeded, "owner quota exceeded")
	ErrInvalidSlug        = errcode.New(errcode.InvalidSlug, "invalid slug")
	ErrInvalidPath        = errcode.New(errcode.InvalidPath, "invalid path")
	ErrInvalidMetadata    = errcode.New(errcode.InvalidMetadata, "invalid metadata")
	ErrInvalidArgument    = errcode.New(errcode.InvalidArgument, "invalid argument")
	ErrForbidden          = errcode.New(errcode.Forbidden, "not allowed")
	ErrGone               = errcode.New(errcode.Gone, "artifact has been purged")
	ErrOffsetMismatch     = errcode.New(errcode.OffsetMismatch, "chunk offset does not match the staged size")
	ErrUploadNotFound     = errcode.New(errcode.UploadNotFound, "upload not found")
	ErrLinkNotFound       = errcode.New(errcode.LinkNotFound, "share link not found")
	ErrTooManyUploads     = errcode.New(errcode.TooManyUploads, "too many open uploads")
	ErrDigestMismatch     = errcode.New(errcode.DigestMismatch, "the archive does not match the digest the upload was begun with")
	ErrEditNotFound       = errcode.New(errcode.EditNotFound, "text to replace not found")
	ErrEditAmbiguous      = errcode.New(errcode.EditAmbiguous, "text to replace matches more than once")
	ErrAnchorMismatch     = errcode.New(errcode.AnchorMismatch, "anchor does not match the file")
	ErrFetchFailed        = errcode.New(errcode.FetchFailed, "could not fetch the URL")
)

// Errorf returns a copy of the sentinel with a formatted message.
func Errorf(sentinel *Error, format string, args ...any) error {
	return errcode.Errorf(sentinel, format, args...)
}

// CodeOf returns err's code, or "" if it holds no *Error.
func CodeOf(err error) string { return errcode.CodeOf(err) }
