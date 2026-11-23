// Package errcode holds the codes that tell a client what went wrong, and the
// error that carries one. A package declares its expected failures with it;
// internal/present gives each code its HTTP status, and internal/api names
// the codes on the wire.
package errcode

import (
	"errors"
	"fmt"
)

const (
	NotFound           = "not_found"
	VersionNotFound    = "version_not_found"
	FileNotFound       = "file_not_found"
	UploadNotFound     = "upload_not_found"
	SlugTaken          = "slug_taken"
	VersionConflict    = "version_conflict"
	PublishedNotLatest = "published_not_latest"
	TooLarge           = "too_large"
	TooManyFiles       = "too_many_files"
	TooManyUploads     = "too_many_uploads"
	QuotaExceeded      = "quota_exceeded"
	InvalidSlug        = "invalid_slug"
	InvalidPath        = "invalid_path"
	InvalidMetadata    = "invalid_metadata"
	InvalidArgument    = "invalid_argument"
	Forbidden          = "forbidden"
	Unauthorized       = "unauthorized"
	Gone               = "gone"
	OffsetMismatch     = "upload_offset_mismatch"
	DigestMismatch     = "upload_digest_mismatch"
	RateLimited        = "rate_limited"
	HandleTaken        = "handle_taken"
	EmailTaken         = "email_taken"
	Internal           = "internal"
	MethodNotAllowed   = "method_not_allowed"
	EditNotFound       = "edit_not_found"
	EditAmbiguous      = "edit_ambiguous"
	AnchorMismatch     = "anchor_mismatch"
	LinkNotFound       = "share_link_not_found"
	FetchFailed        = "fetch_failed"
	JobRunning         = "job_running"
	LastAdmin          = "last_admin"
	TooManyKeys        = "too_many_keys"
	SetupDone          = "setup_done"
	BackupRunning      = "backup_running"
	NoSpace            = "no_space"
	Busy               = "busy"
)

// Codes is every code above, the bounded set metrics count errors by.
var Codes = []string{
	NotFound, VersionNotFound, FileNotFound, UploadNotFound, SlugTaken,
	VersionConflict, PublishedNotLatest, TooLarge, TooManyFiles, TooManyUploads, QuotaExceeded,
	InvalidSlug, InvalidPath, InvalidMetadata, InvalidArgument, Forbidden,
	Unauthorized, Gone, OffsetMismatch, DigestMismatch, RateLimited, HandleTaken,
	EmailTaken, Internal, MethodNotAllowed, EditNotFound, EditAmbiguous,
	AnchorMismatch, LinkNotFound, FetchFailed, JobRunning, LastAdmin,
	TooManyKeys, SetupDone, BackupRunning, NoSpace, Busy,
}

// Error is an expected failure with a code a client can act on. It matches
// the sentinel it was made from and every copy Errorf makes of it, never
// another sentinel with the same code: a missing user and a missing artifact
// are both not_found to a client, and neither is the other to the code that
// handles them.
type Error struct {
	Code string
	// Message is what a client is shown.
	Message string
	// text is what the error reads as, when that is not "code: message".
	text string
	// context shows a client the whole error that wraps this one, which
	// names the input that was refused.
	context bool
	// of is the sentinel this error is a copy of.
	of *Error
}

// New returns a sentinel that reads as "code: message" and shows a client
// message.
func New(code, message string) *Error {
	return &Error{Code: code, Message: message}
}

// Text returns a sentinel that reads as text and shows a client the text of
// the whole error that wraps it.
func Text(code, text string) *Error {
	return &Error{Code: code, Message: text, text: text, context: true}
}

// Shown returns a sentinel that reads as text and shows a client message,
// whatever wraps it.
func Shown(code, text, message string) *Error {
	return &Error{Code: code, Message: message, text: text}
}

// Errorf returns a copy of sentinel that reads as "code: message" and shows
// a client the formatted message.
func Errorf(sentinel *Error, format string, args ...any) error {
	return &Error{Code: sentinel.Code, Message: fmt.Sprintf(format, args...), of: sentinel.root()}
}

func (e *Error) Error() string {
	switch {
	case e.text != "":
		return e.text
	case e.Message == "":
		return e.Code
	}
	return e.Code + ": " + e.Message
}

func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && e.root() == t.root()
}

func (e *Error) root() *Error {
	if e.of != nil {
		return e.of
	}
	return e
}

// Public returns the code of the *Error err holds and the message a client
// is shown for err, or false when err holds none.
func Public(err error) (code, message string, ok bool) {
	e, ok := errors.AsType[*Error](err)
	switch {
	case !ok:
		return "", "", false
	case e.context:
		return e.Code, err.Error(), true
	}
	return e.Code, e.Message, true
}

// CodeOf returns the code of the *Error err holds, or "" when it holds none.
func CodeOf(err error) string {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Code
	}
	return ""
}
