//go:build !linux && !darwin

package cli

import (
	"io"
	"os"
)

func withoutEcho(_ io.Reader, read func() error) error { return read() }

// IsTerminal reports whether stream is a character device.
func IsTerminal(stream any) bool {
	f, ok := stream.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
