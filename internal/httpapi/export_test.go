package httpapi

import (
	"io"

	"github.com/PeterBooker/kazari/internal/artifact"
)

// UserRoutes returns the pattern of every route of the user API behind the
// bearer check, so a test can tell when one is added.
func UserRoutes() []string {
	var out []string
	for _, rt := range (&userAPI{}).routes() {
		out = append(out, rt.pattern)
	}
	return out
}

// ReadZip is the zip reader of an archive upload.
func ReadZip(ra io.ReaderAt, size int64, maxFiles int) ([]artifact.FileInput, error) {
	return readZip(ra, size, maxFiles)
}

// ReadBundle is the reader of a gzip-compressed tar, spilled into dir until
// the returned function is called.
func ReadBundle(r io.Reader, dir string, limit int64) ([]artifact.FileInput, func() error, error) {
	b, err := readBundle(r, dir, limit)
	if err != nil {
		return nil, nil, err
	}
	return b.files, b.Close, nil
}
