//go:build !linux && !darwin

package disk

// Usage returns ErrUnsupported.
func Usage(string) (free, total uint64, err error) {
	return 0, 0, ErrUnsupported
}
