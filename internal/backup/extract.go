package backup

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// ErrUnsafeEntry is returned by Extract for an archive entry that is not a
// plain file or directory inside the target.
var ErrUnsafeEntry = errors.New("backup: unsafe archive entry")

// ErrNotEmpty is returned by Extract when the target directory holds
// anything.
var ErrNotEmpty = errors.New("backup: the directory is not empty")

// Extract unpacks an archive from Write into dir, which must be empty or
// absent, with every file readable by its owner alone: the database holds
// every credential hash. An entry that is not a regular file or a directory,
// or whose name leads outside dir, fails with ErrUnsafeEntry; what was
// unpacked before it stays.
func Extract(r io.Reader, dir string) error {
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return fmt.Errorf("backup: extract: %w", err)
	case len(entries) > 0:
		return fmt.Errorf("%w: %s", ErrNotEmpty, dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("backup: extract: %w", err)
	}
	// os.Root refuses names that leave dir, through a symlink too, though
	// Extract never creates one.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("backup: extract: %w", err)
	}
	defer root.Close()

	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("backup: extract: %w", err)
		}
		name := filepath.FromSlash(hdr.Name)
		if !filepath.IsLocal(name) {
			return fmt.Errorf("%w: %q leads outside the directory", ErrUnsafeEntry, hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o700); err != nil {
				return fmt.Errorf("backup: extract %s: %w", hdr.Name, err)
			}
		case tar.TypeReg:
			if err := extractFile(root, name, tr); err != nil {
				return fmt.Errorf("backup: extract %s: %w", hdr.Name, err)
			}
		default:
			return fmt.Errorf("%w: %q is neither a file nor a directory", ErrUnsafeEntry, hdr.Name)
		}
	}
}

func extractFile(root *os.Root, name string, r io.Reader) error {
	if parent := filepath.Dir(name); parent != "." {
		if err := root.MkdirAll(parent, 0o700); err != nil {
			return err
		}
	}
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
