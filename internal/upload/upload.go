// Package upload stages large bundles that arrive in chunks, so that an MCP
// client with no shell can publish a multi-megabyte artifact over several
// tool calls. The staged files are handed to artifact.Service as ordinary
// FileInputs; the artifact code never knows the bytes arrived in pieces.
package upload

import (
	"context"
	"time"
)

// Upload is a chunked upload in progress.
type Upload struct {
	ID        string // random, URL-safe
	Owner     string // handle
	Bytes     int64  // staged so far, all files
	CreatedAt time.Time
	ExpiresAt time.Time
	Files     []StagedFile // sorted by Path
}

// StagedFile is one file of an upload.
type StagedFile struct {
	Path         string
	StagingIndex int // name of the staging file under uploads/<id>/
	Bytes        int64
	ContentType  string
}

// Store is the persistence contract for upload records. Owners are handles;
// the store resolves them to user ids.
type Store interface {
	// Create inserts u (without files). When maxOpen > 0 and the owner
	// already has that many uploads, expired or not, nothing is inserted
	// and artifact.ErrTooManyUploads is returned; the count and the insert
	// are atomic.
	Create(ctx context.Context, u *Upload, maxOpen int) error
	// Get returns the upload with its files, or artifact.ErrUploadNotFound.
	Get(ctx context.Context, id string) (*Upload, error)
	// UpsertFile inserts or updates one file row and sets the upload's
	// total bytes.
	UpsertFile(ctx context.Context, id string, f StagedFile, totalBytes int64) error
	// Delete removes the upload and its file rows.
	Delete(ctx context.Context, id string) error
	// ListExpired returns uploads with expires_at <= now.
	ListExpired(ctx context.Context, now time.Time) ([]Upload, error)
}
