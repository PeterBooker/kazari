//go:build !linux && !darwin

package cli

import (
	"io"
	"os"
)

func withoutEcho(_ io.Reader, read func() error) error { return read() }

func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
