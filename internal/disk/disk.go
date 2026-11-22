// Package disk reports how full a filesystem is and how much a directory
// tree holds.
package disk

import (
	"errors"
	"io/fs"
	"path/filepath"
)

// ErrUnsupported is returned by Usage on a platform Kazari's server is not
// built for.
var ErrUnsupported = errors.New("disk: usage is not supported on this platform")

// TreeSize sums the regular files under root. A file that disappears while
// the tree is walked is left out, and a missing root holds nothing.
func TreeSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}
