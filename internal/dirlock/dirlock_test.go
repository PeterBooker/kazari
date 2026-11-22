package dirlock_test

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/PeterBooker/kazari/internal/dirlock"
)

func TestAcquire(t *testing.T) {
	dir := t.TempDir()
	first, err := dirlock.Acquire(dir)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, dirlock.FileName))
	if err != nil || strings.TrimSpace(string(b)) != strconv.Itoa(os.Getpid()) {
		t.Errorf("lock file = %q, %v; want this process's id", b, err)
	}

	_, err = dirlock.Acquire(dir)
	var locked *dirlock.LockedError
	if !errors.As(err, &locked) || locked.PID != os.Getpid() || locked.Dir != dir {
		t.Fatalf("second Acquire = %v, want a LockedError naming pid %d", err, os.Getpid())
	}
	if want := "another kazari (pid " + strconv.Itoa(os.Getpid()) + ") is using " + dir; err.Error() != want {
		t.Errorf("second Acquire = %q, want %q", err, want)
	}

	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := first.Release(); err != nil {
		t.Errorf("second Release: %v", err)
	}
	again, err := dirlock.Acquire(dir)
	if err != nil {
		t.Fatalf("Acquire after Release: %v", err)
	}
	if err := again.Release(); err != nil {
		t.Error(err)
	}
}

func TestAcquireMissingDir(t *testing.T) {
	if _, err := dirlock.Acquire(filepath.Join(t.TempDir(), "none")); err == nil {
		t.Error("Acquire of a missing directory = nil, want an error")
	}
}
