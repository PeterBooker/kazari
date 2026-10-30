// Package upload stages bundles that arrive in chunks, or as one archive sent
// to a URL without a credential, so an MCP client with no shell can publish a
// large artifact over several tool calls, and one with a shell but no key can
// send it in one request. Staged files reach artifact.Service as ordinary
// FileInputs.
package upload

import (
	"context"
	"time"
)

// Upload is an upload in progress.
type Upload struct {
	ID        string
	Owner     string // handle
	Bytes     int64  // staged so far, all files
	CreatedAt time.Time
	ExpiresAt time.Time
	Files     []StagedFile // sorted by Path
	Archive   *Archive     // nil unless the upload accepts an archive
}

// Archive is the one archive an upload accepts without a credential. The
// token travels in the URL and only its hash is kept; the body must have
// exactly SHA256 and Bytes, so a leaked URL stages nothing but bytes the
// owner already has.
type Archive struct {
	TokenHash  string // hex SHA-256 of the token
	SHA256     string // hex, lower case
	Bytes      int64
	Attempts   int       // bodies that missed the digest
	ReceivedAt time.Time // zero until the archive arrived
}

// StagedFile is one file of an upload.
type StagedFile struct {
	Path         string
	StagingIndex int // staging file name under <Dir>/<id>/
	Bytes        int64
	ContentType  string
}

// Store persists upload records. Owners are handles; the store resolves them
// to user ids.
type Store interface {
	// Create inserts u without files, with its Archive when set. With
	// maxOpen > 0 it returns artifact.ErrTooManyUploads when the owner
	// already has maxOpen uploads, expired or not; the count and the insert
	// are atomic.
	Create(ctx context.Context, u *Upload, maxOpen int) error
	// Get returns the upload with its files, or artifact.ErrUploadNotFound.
	Get(ctx context.Context, id string) (*Upload, error)
	// UpsertFile also sets the upload's total bytes.
	UpsertFile(ctx context.Context, id string, f StagedFile, totalBytes int64) error
	// ReceiveArchive inserts files, sets the total bytes and marks the
	// archive received at the given time, all or nothing. An upload whose
	// archive was already received is artifact.ErrUploadNotFound.
	ReceiveArchive(ctx context.Context, id string, files []StagedFile, totalBytes int64, at time.Time) error
	// CountArchiveAttempt adds one missed digest and returns the new count.
	CountArchiveAttempt(ctx context.Context, id string) (int, error)
	Delete(ctx context.Context, id string) error
	// ListExpired returns uploads with expires_at <= now.
	ListExpired(ctx context.Context, now time.Time) ([]Upload, error)
}
