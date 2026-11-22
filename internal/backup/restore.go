package backup

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
)

// ErrNotBackup is returned by Restore for a directory without the database
// a backup holds.
var ErrNotBackup = errors.New("backup: not a backup directory")

// restored are the entries of a data directory a backup replaces. The
// staged uploads go too: they belong to the database that is replaced.
var restored = []string{"kazari.db", "kazari.db-wal", "kazari.db-shm", "blobs", "certmagic", "uploads"}

// keep are the entries of a data directory that do not count against an
// empty target: the lock the restore holds and a socket a stopped server
// left behind.
var keep = []string{"kazari.lock", "admin.sock"}

// RestoreOptions tune Restore.
type RestoreOptions struct {
	// Replace removes what the data directory holds of an instance first;
	// without it a data directory with any of it fails with ErrNotEmpty.
	Replace bool
}

// RestoreSummary is what Restore copied.
type RestoreSummary struct {
	Files int
	Bytes int64
}

// Restore copies a backup directory, as Extract writes it, into dataDir,
// which is created when absent; the server must not be running on it, which
// the caller ensures. Files are readable by their owner alone, and when
// Restore runs as root they are given the data directory's owner, so the
// service user reads them. A backup without certmagic/ leaves the data
// directory's certificates in place.
func Restore(src, dataDir string, opts RestoreOptions) (RestoreSummary, error) {
	if fi, err := os.Stat(filepath.Join(src, "kazari.db")); err != nil || !fi.Mode().IsRegular() {
		return RestoreSummary{}, fmt.Errorf("%w: %s has no kazari.db", ErrNotBackup, src)
	}
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return RestoreSummary{}, fmt.Errorf("backup: restore: %w", err)
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return RestoreSummary{}, fmt.Errorf("backup: restore: %w", err)
	}
	var present []string
	for _, e := range entries {
		if !slices.Contains(keep, e.Name()) {
			present = append(present, e.Name())
		}
	}
	if len(present) > 0 && !opts.Replace {
		return RestoreSummary{}, fmt.Errorf("%w: %s", ErrNotEmpty, dataDir)
	}
	_, certsErr := os.Stat(filepath.Join(src, "certmagic"))
	for _, name := range restored {
		if name == "certmagic" && certsErr != nil {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dataDir, name)); err != nil {
			return RestoreSummary{}, fmt.Errorf("backup: restore: %w", err)
		}
	}
	owner, err := ownerOf(dataDir)
	if err != nil {
		return RestoreSummary{}, fmt.Errorf("backup: restore: %w", err)
	}
	var sum RestoreSummary
	err = filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil || rel == "." {
			return err
		}
		dst := filepath.Join(dataDir, rel)
		switch {
		case d.IsDir():
			if err := os.Mkdir(dst, 0o750); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
		case d.Type().IsRegular():
			n, err := copyFile(path, dst)
			if err != nil {
				return err
			}
			sum.Files++
			sum.Bytes += n
		default:
			return fmt.Errorf("%w: %s is neither a file nor a directory", ErrUnsafeEntry, path)
		}
		return owner.apply(dst)
	})
	if err != nil {
		return sum, fmt.Errorf("backup: restore: %w; the data directory is incomplete, run the restore again with --force", err)
	}
	// The lock file the caller holds may be new, and the server opens it.
	if err := owner.apply(filepath.Join(dataDir, "kazari.lock")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return sum, fmt.Errorf("backup: restore: %w", err)
	}
	return sum, nil
}

func copyFile(src, dst string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return n, err
}
