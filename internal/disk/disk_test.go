package disk_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/PeterBooker/kazari/internal/disk"
)

func TestUsage(t *testing.T) {
	free, total, err := disk.Usage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if total == 0 || free > total {
		t.Errorf("Usage = %d free of %d", free, total)
	}
	if _, _, err := disk.Usage(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("Usage of a missing path = nil error")
	}
}

func TestTreeSize(t *testing.T) {
	root := t.TempDir()
	write := func(name string, n int) {
		t.Helper()
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, n), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("a", 3)
	write("sub/b", 5)
	write("sub/deep/c", 7)
	if err := os.Symlink(filepath.Join(root, "a"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if got, err := disk.TreeSize(root); err != nil || got != 15 {
		t.Errorf("TreeSize = %d, %v; want 15", got, err)
	}
	if got, err := disk.TreeSize(filepath.Join(root, "missing")); err != nil || got != 0 {
		t.Errorf("TreeSize of a missing root = %d, %v; want 0", got, err)
	}
}
