//go:build !linux && !darwin

package cli

import "io"

func withoutEcho(_ io.Reader, read func() error) error { return read() }
