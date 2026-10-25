// Package upload stages bundles that arrive in chunks, so an MCP client with
// no shell can publish a large artifact over several tool calls. Staged files
// reach artifact.Service as ordinary FileInputs.
package upload

import (
	"context"
	"time"
)

// Upload is a chunked upload in progress.
type Upload struct {
	ID        string
	Owner     string // handle
	Bytes     int64  // staged so far, all files
	CreatedAt time.Time
	ExpiresAt time.Time
	Files     []StagedFile // sorted by Path
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
	// Create inserts u without files. With maxOpen > 0 it returns
	// artifact.ErrTooManyUploads when the owner already has maxOpen uploads,
	// expired or not; the count and the insert are atomic.
	Create(ctx context.Context, u *Upload, maxOpen int) error
	// Get returns the upload with its files, or artifact.ErrUploadNotFound.
	Get(ctx context.Context, id string) (*Upload, error)
	// UpsertFile also sets the upload's total bytes.
	UpsertFile(ctx context.Context, id string, f StagedFile, totalBytes int64) error
	Delete(ctx context.Context, id string) error
	// ListExpired returns uploads with expires_at <= now.
	ListExpired(ctx context.Context, now time.Time) ([]Upload, error)
}
