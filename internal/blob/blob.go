// Package blob stores content-addressed byte streams. The interface is
// deliberately dumb: size limits and validation belong to the caller, so an
// S3 implementation needs no limit logic.
package blob

import (
	"context"
	"errors"
	"io"
)

// ErrNotFound is returned by Open and Stat for an unknown hash.
var ErrNotFound = errors.New("blob: not found")

// Store is a content-addressed blob store keyed by hex SHA-256.
type Store interface {
	// Put streams r into the store and returns its hex SHA-256 and size.
	// Putting the same content twice is a no-op that returns the same hash;
	// a partially written blob is never visible.
	Put(ctx context.Context, r io.Reader) (hash string, size int64, err error)
	// Open returns a reader positioned at the start of the blob.
	Open(ctx context.Context, hash string) (io.ReadSeekCloser, error)
	// Stat returns the blob's size or ErrNotFound.
	Stat(ctx context.Context, hash string) (size int64, err error)
	// Delete removes the blob; deleting an unknown hash is not an error.
	Delete(ctx context.Context, hash string) error
}
