package backup_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/backup"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func readTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		out[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRestore(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src, map[string]string{"kazari.db": "db", "blobs/ab/abcd": "blob", "certmagic/acme/key": "key"})
	data := filepath.Join(t.TempDir(), "data")

	sum, err := backup.Restore(src, data, backup.RestoreOptions{})
	if err != nil {
		t.Fatalf("Restore into a new directory: %v", err)
	}
	if sum != (backup.RestoreSummary{Files: 3, Bytes: 9}) {
		t.Errorf("Restore summary = %+v, want 3 files, 9 bytes", sum)
	}
	want := map[string]string{"kazari.db": "db", "blobs/ab/abcd": "blob", "certmagic/acme/key": "key"}
	if diff := cmp.Diff(want, readTree(t, data)); diff != "" {
		t.Errorf("restored tree (-want +got):\n%s", diff)
	}
	if fi, err := os.Stat(filepath.Join(data, "kazari.db")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("kazari.db mode = %v, %v; want 0600", fi.Mode().Perm(), err)
	}

	if _, err := backup.Restore(src, data, backup.RestoreOptions{}); !errors.Is(err, backup.ErrNotEmpty) {
		t.Errorf("Restore into a used directory = %v, want ErrNotEmpty", err)
	}
}

func TestRestoreReplace(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src, map[string]string{"kazari.db": "new", "blobs/cd/cdef": "new blob"})
	data := t.TempDir()
	writeTree(t, data, map[string]string{
		"kazari.db": "old", "kazari.db-wal": "wal", "blobs/ab/abcd": "old blob", "uploads/u1/f": "staged",
		"certmagic/acme/key": "current key", "kazari.lock": "123\n",
	})

	if _, err := backup.Restore(src, data, backup.RestoreOptions{Replace: true}); err != nil {
		t.Fatalf("Restore with Replace: %v", err)
	}
	want := map[string]string{
		"kazari.db": "new", "blobs/cd/cdef": "new blob",
		// A backup without certificates leaves the current ones.
		"certmagic/acme/key": "current key", "kazari.lock": "123\n",
	}
	if diff := cmp.Diff(want, readTree(t, data)); diff != "" {
		t.Errorf("restored tree (-want +got):\n%s", diff)
	}
}

func TestRestoreIgnoresLockAndSocket(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src, map[string]string{"kazari.db": "db"})
	data := t.TempDir()
	writeTree(t, data, map[string]string{"kazari.lock": "", "admin.sock": ""})
	if _, err := backup.Restore(src, data, backup.RestoreOptions{}); err != nil {
		t.Errorf("Restore beside a lock file and a stale socket: %v", err)
	}
}

func TestRestoreNotBackup(t *testing.T) {
	src := t.TempDir()
	writeTree(t, src, map[string]string{"notes.txt": "x"})
	if _, err := backup.Restore(src, t.TempDir(), backup.RestoreOptions{}); !errors.Is(err, backup.ErrNotBackup) {
		t.Errorf("Restore of a directory without kazari.db = %v, want ErrNotBackup", err)
	}
}
