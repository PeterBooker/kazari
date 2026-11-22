//go:build linux || darwin

package cli

import (
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// withoutEcho runs read with the terminal's echo off, and as it was when
// r is not a terminal that allows that.
func withoutEcho(r io.Reader, read func() error) error {
	f, ok := r.(*os.File)
	if !ok {
		return read()
	}
	fd := int(f.Fd())
	old, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return read()
	}
	quiet := *old
	quiet.Lflag &^= unix.ECHO
	quiet.Lflag |= unix.ICANON | unix.ISIG
	quiet.Iflag |= unix.ICRNL
	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, &quiet); err != nil {
		return read()
	}
	defer func() { _ = unix.IoctlSetTermios(fd, ioctlSetTermios, old) }()
	return read()
}
