//go:build unix

package backup

import (
	"os"
	"syscall"
)

// owner is the user and group restored files get; apply does nothing
// unless Restore runs as root, the only user that can give files away.
type owner struct {
	uid, gid int
	set      bool
}

func ownerOf(dir string) (owner, error) {
	if os.Geteuid() != 0 {
		return owner{}, nil
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return owner{}, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return owner{}, nil
	}
	return owner{uid: int(st.Uid), gid: int(st.Gid), set: true}, nil
}

func (o owner) apply(path string) error {
	if !o.set {
		return nil
	}
	return os.Lchown(path, o.uid, o.gid)
}
