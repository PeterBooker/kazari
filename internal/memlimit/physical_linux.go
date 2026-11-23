package memlimit

import "golang.org/x/sys/unix"

func physical() (int64, error) {
	var si unix.Sysinfo_t
	if err := unix.Sysinfo(&si); err != nil {
		return 0, err
	}
	return int64(si.Totalram) * int64(si.Unit), nil
}
