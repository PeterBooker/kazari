// Package dirlock gives one process at a time a directory: the server holds
// its data directory for as long as it runs, and an offline restore holds it
// while it writes.
package dirlock

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// FileName is the lock file in the directory. It stays after release: a
// lock is the open file's, not the file's existence.
const FileName = "kazari.lock"

// LockedError is returned by Acquire while another process holds the
// directory.
type LockedError struct {
	Dir string
	PID int // 0 when the holder did not record it
}

func (e *LockedError) Error() string {
	if e.PID > 0 {
		return fmt.Sprintf("another kazari (pid %d) is using %s", e.PID, e.Dir)
	}
	return "another kazari is using " + e.Dir
}

// Lock is a held directory.
type Lock struct {
	f *os.File
}

// Acquire takes the lock on dir, which must exist, without waiting, and
// records this process's id in it. It returns a *LockedError when another
// process holds it.
func Acquire(dir string) (*Lock, error) {
	path := filepath.Join(dir, FileName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	writable := err == nil
	if os.IsPermission(err) {
		// A lock file left by root, such as by a restore, still locks.
		f, err = os.Open(path)
	}
	if err != nil {
		return nil, fmt.Errorf("dirlock: %w", err)
	}
	held, err := tryLock(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("dirlock: lock %s: %w", path, err)
	}
	if !held {
		f.Close()
		return nil, &LockedError{Dir: dir, PID: readPID(path)}
	}
	if writable {
		if err := f.Truncate(0); err == nil {
			_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
		}
	}
	return &Lock{f: f}, nil
}

// Release gives the directory up. It is safe to call more than once.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := l.f.Close() // closing the last descriptor drops the lock
	l.f = nil
	return err
}

func readPID(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}
