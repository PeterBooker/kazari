// Package blob stores content-addressed byte streams. Limits and validation
// belong to the caller, so an S3 implementation needs none.
package blob

import (
	"context"
	"errors"
	"io"
)

// ErrNotFound is returned by Open and Stat for an unknown hash.
var ErrNotFound = errors.New("blob: not found")

// Store is a blob store keyed by hex SHA-256.
type Store interface {
	// Put is idempotent for the same content, and a partial blob is never
	// visible.
	Put(ctx context.Context, r io.Reader) (hash string, size int64, err error)
	Open(ctx context.Context, hash string) (io.ReadSeekCloser, error)
	Stat(ctx context.Context, hash string) (size int64, err error)
	// Delete of an unknown hash is not an error.
	Delete(ctx context.Context, hash string) error
}
