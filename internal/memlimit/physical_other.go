//go:build !linux && !darwin

package memlimit

import "errors"

func physical() (int64, error) {
	return 0, errors.New("memlimit: physical memory is not known on this system")
}
