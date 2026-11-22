package backup_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/PeterBooker/kazari/internal/backup"
)

// tarOf builds an archive from headers; a regular file's content is its
// name.
func tarOf(t *testing.T, hdrs ...tar.Header) *bytes.Reader {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range hdrs {
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(h.Name))
		}
		h.Mode = 0o777
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(h.Name)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(buf.Bytes())
}

func TestExtract(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	err := backup.Extract(tarOf(t,
		tar.Header{Typeflag: tar.TypeReg, Name: "kazari.db"},
		tar.Header{Typeflag: tar.TypeDir, Name: "blobs/"},
		tar.Header{Typeflag: tar.TypeReg, Name: "blobs/ab/abcdef"},
	), dir)
	if err != nil {
		t.Fatal(err)
	}
	for name, mode := range map[string]os.FileMode{
		"kazari.db": 0o600, "blobs": os.ModeDir | 0o700, "blobs/ab": os.ModeDir | 0o700, "blobs/ab/abcdef": 0o600,
	} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode() != mode {
			t.Errorf("%s has mode %v, want %v", name, fi.Mode(), mode)
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "blobs", "ab", "abcdef")); err != nil || string(b) != "blobs/ab/abcdef" {
		t.Errorf("blob = %q, %v", b, err)
	}

	if err := backup.Extract(tarOf(t, tar.Header{Typeflag: tar.TypeReg, Name: "kazari.db"}), dir); !errors.Is(err, backup.ErrNotEmpty) {
		t.Errorf("Extract into a directory with files = %v, want ErrNotEmpty", err)
	}
	empty := t.TempDir()
	if err := backup.Extract(tarOf(t, tar.Header{Typeflag: tar.TypeReg, Name: "kazari.db"}), empty); err != nil {
		t.Errorf("Extract into an empty directory: %v", err)
	}
}

func TestExtractRefusesUnsafeEntries(t *testing.T) {
	tests := map[string][]tar.Header{
		"parent":            {{Typeflag: tar.TypeReg, Name: "../escaped"}},
		"parent in between": {{Typeflag: tar.TypeReg, Name: "blobs/../../escaped"}},
		"absolute":          {{Typeflag: tar.TypeReg, Name: "/escaped"}},
		"absolute dir":      {{Typeflag: tar.TypeDir, Name: "/escaped/"}},
		"empty name":        {{Typeflag: tar.TypeReg, Name: ""}},
		"symlink":           {{Typeflag: tar.TypeSymlink, Name: "blobs", Linkname: "../outside"}},
		"hard link":         {{Typeflag: tar.TypeLink, Name: "kazari.db", Linkname: "/etc/passwd"}},
		"device":            {{Typeflag: tar.TypeChar, Name: "null"}},
		"fifo":              {{Typeflag: tar.TypeFifo, Name: "pipe"}},
		"through a symlink": {
			{Typeflag: tar.TypeSymlink, Name: "blobs", Linkname: "../outside"},
			{Typeflag: tar.TypeReg, Name: "blobs/escaped"},
		},
	}
	for name, hdrs := range tests {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			dir := filepath.Join(parent, "data")
			if err := os.Mkdir(filepath.Join(parent, "outside"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := backup.Extract(tarOf(t, hdrs...), dir); !errors.Is(err, backup.ErrUnsafeEntry) {
				t.Errorf("Extract = %v, want ErrUnsafeEntry", err)
			}
			for _, p := range []string{filepath.Join(parent, "escaped"), filepath.Join(parent, "outside", "escaped"), "/escaped"} {
				if _, err := os.Lstat(p); err == nil {
					t.Errorf("%s was written", p)
				}
			}
			if left, _ := os.ReadDir(dir); len(left) != 0 {
				t.Errorf("left in the directory: %v", left)
			}
		})
	}
}

func TestExtractKeepsOutOfExistingFiles(t *testing.T) {
	err := backup.Extract(tarOf(t,
		tar.Header{Typeflag: tar.TypeReg, Name: "kazari.db"},
		tar.Header{Typeflag: tar.TypeReg, Name: "kazari.db"},
	), filepath.Join(t.TempDir(), "data"))
	if err == nil {
		t.Error("Extract wrote the same file twice")
	}
}
