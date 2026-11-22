//go:build linux || darwin

package disk

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// Usage returns the bytes an unprivileged process may still write to the
// filesystem holding path, and that filesystem's size.
func Usage(path string) (free, total uint64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, fmt.Errorf("disk: usage of %s: %w", path, err)
	}
	bsize := uint64(st.Bsize)
	return st.Bavail * bsize, st.Blocks * bsize, nil
}
