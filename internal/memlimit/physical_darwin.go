package memlimit

import (
	"math"

	"golang.org/x/sys/unix"
)

func physical() (int64, error) {
	n, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0, err
	}
	return int64(min(n, math.MaxInt64)), nil
}
