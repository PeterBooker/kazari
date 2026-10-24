package cli

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// bundleFiles returns the slash-separated paths, relative to dir and sorted,
// of the regular files TarGz includes. Symlinks and other non-regular files
// are skipped, as is anything whose name starts with a dot (.git, .DS_Store),
// except .well-known, which websites need.
func bundleFiles(dir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == dir {
			if !d.IsDir() {
				return fmt.Errorf("%s is not a directory", dir)
			}
			return nil
		}
		if hidden(d.Name()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // directories are implicit; symlinks, devices, sockets skipped
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", dir, err)
	}
	slices.Sort(files)
	return files, nil
}

func hidden(name string) bool {
	return strings.HasPrefix(name, ".") && name != ".well-known"
}

// TarGz writes the regular files under dir to w as a gzip-compressed tar.
// Entries are relative, slash-separated and sorted, with zero ownership, mode
// 0644 and the Unix epoch as mtime, so the same tree always yields the same
// bytes. Dotfiles and dot-directories (except .well-known) and symlinks are
// skipped.
func TarGz(dir string, w io.Writer) error {
	files, err := bundleFiles(dir)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(w)
	tw := tar.NewWriter(zw)
	for _, name := range files {
		if err := addFile(tw, dir, name); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("close tar: %w", err)
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("close gzip: %w", err)
	}
	return nil
}

func addFile(tw *tar.Writer, dir, name string) error {
	f, err := os.Open(filepath.Join(dir, filepath.FromSlash(name)))
	if err != nil {
		return fmt.Errorf("open %s: %w", name, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", name, err)
	}
	hdr := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Size:     st.Size(),
		Mode:     0o644,
		ModTime:  time.Unix(0, 0),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("tar header %s: %w", name, err)
	}
	n, err := io.Copy(tw, f)
	if err != nil {
		return fmt.Errorf("tar %s: %w", name, err)
	}
	if n != st.Size() {
		return fmt.Errorf("tar %s: %w", name, errors.New("file changed while reading"))
	}
	return nil
}

// TarGzReader streams TarGz(dir) through a pipe, so a large directory is
// never held in memory. Closing the reader early stops the writer; a walk or
// read error surfaces from Read.
func TarGzReader(dir string) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(TarGz(dir, pw))
	}()
	return pr
}
