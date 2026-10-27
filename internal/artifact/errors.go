package artifact

import (
	"errors"
	"fmt"
)

// Error is a domain error with a stable code.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

// Is matches any *Error with the same code, so an Errorf copy still matches
// its sentinel.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

var (
	ErrNotFound        = &Error{Code: "not_found", Message: "artifact not found"}
	ErrVersionNotFound = &Error{Code: "version_not_found", Message: "version not found"}
	ErrFileNotFound    = &Error{Code: "file_not_found", Message: "file not found"}
	ErrSlugTaken       = &Error{Code: "slug_taken", Message: "slug is already taken"}
	ErrVersionConflict = &Error{Code: "version_conflict", Message: "artifact has a newer version than expected"}
	ErrTooLarge        = &Error{Code: "too_large", Message: "content exceeds the size limit"}
	ErrTooManyFiles    = &Error{Code: "too_many_files", Message: "too many files"}
	ErrQuotaExceeded   = &Error{Code: "quota_exceeded", Message: "owner quota exceeded"}
	ErrInvalidSlug     = &Error{Code: "invalid_slug", Message: "invalid slug"}
	ErrInvalidPath     = &Error{Code: "invalid_path", Message: "invalid path"}
	ErrInvalidMetadata = &Error{Code: "invalid_metadata", Message: "invalid metadata"}
	ErrInvalidArgument = &Error{Code: "invalid_argument", Message: "invalid argument"}
	ErrForbidden       = &Error{Code: "forbidden", Message: "not allowed"}
	ErrGone            = &Error{Code: "gone", Message: "artifact has been purged"}
	ErrOffsetMismatch  = &Error{Code: "upload_offset_mismatch", Message: "chunk offset does not match the staged size"}
	ErrUploadNotFound  = &Error{Code: "upload_not_found", Message: "upload not found"}
	ErrLinkNotFound    = &Error{Code: "share_link_not_found", Message: "share link not found"}
	ErrTooManyUploads  = &Error{Code: "too_many_uploads", Message: "too many open uploads"}
	ErrEditNotFound    = &Error{Code: "edit_not_found", Message: "text to replace not found"}
	ErrEditAmbiguous   = &Error{Code: "edit_ambiguous", Message: "text to replace matches more than once"}
	ErrAnchorMismatch  = &Error{Code: "anchor_mismatch", Message: "anchor does not match the file"}
	ErrFetchFailed     = &Error{Code: "fetch_failed", Message: "could not fetch the URL"}
)

// Errorf returns a copy of the sentinel with a formatted message.
func Errorf(sentinel *Error, format string, args ...any) error {
	return &Error{Code: sentinel.Code, Message: fmt.Sprintf(format, args...)}
}

// CodeOf returns err's code, or "" if it is not an *Error.
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}
