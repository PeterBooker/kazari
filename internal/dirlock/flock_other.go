//go:build !unix

package dirlock

import "os"

// tryLock always succeeds where flock does not exist: Kazari's releases are
// for Linux and macOS.
func tryLock(*os.File) (bool, error) { return true, nil }
