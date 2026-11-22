package backup_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/PeterBooker/kazari/internal/backup"
)

func archiveOf(tb testing.TB, hdrs ...tar.Header) []byte {
	tb.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range hdrs {
		body := []byte(nil)
		if h.Typeflag == tar.TypeReg {
			body = []byte("content of " + h.Name)
		}
		h.Size, h.Mode = int64(len(body)), 0o777
		if err := tw.WriteHeader(&h); err != nil {
			tb.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			tb.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		tb.Fatal(err)
	}
	return buf.Bytes()
}

// maxFuzzedBytes is the most an archive may claim to hold: a sparse entry
// declares any size in a few bytes, and Extract writes all of it.
const maxFuzzedBytes = 1 << 20

func FuzzExtract(f *testing.F) {
	whole := archiveOf(f,
		tar.Header{Typeflag: tar.TypeReg, Name: "kazari.db"},
		tar.Header{Typeflag: tar.TypeDir, Name: "blobs/"},
		tar.Header{Typeflag: tar.TypeReg, Name: "blobs/ab/abcdef"})
	f.Add(whole)
	f.Add(whole[:len(whole)/2])
	f.Add(archiveOf(f))
	f.Add(archiveOf(f, tar.Header{Typeflag: tar.TypeReg, Name: "../escape"}))
	f.Add(archiveOf(f, tar.Header{Typeflag: tar.TypeReg, Name: "/etc/passwd"}))
	f.Add(archiveOf(f, tar.Header{Typeflag: tar.TypeReg, Name: "a/../../escape"}))
	f.Add(archiveOf(f, tar.Header{Typeflag: tar.TypeReg, Name: "a/../b"}))
	f.Add(archiveOf(f, tar.Header{Typeflag: tar.TypeSymlink, Name: "link", Linkname: "/etc"}, tar.Header{Typeflag: tar.TypeReg, Name: "link/x"}))
	f.Add(archiveOf(f, tar.Header{Typeflag: tar.TypeLink, Name: "hard", Linkname: "kazari.db"}))
	f.Add(archiveOf(f, tar.Header{Typeflag: tar.TypeFifo, Name: "fifo"}))
	f.Add(archiveOf(f, tar.Header{Typeflag: tar.TypeReg, Name: "twice"}, tar.Header{Typeflag: tar.TypeReg, Name: "twice"}))
	f.Add(archiveOf(f, tar.Header{Typeflag: tar.TypeReg, Name: "x"}, tar.Header{Typeflag: tar.TypeDir, Name: "x/"}))
	f.Add([]byte("not an archive"))

	f.Fuzz(func(t *testing.T, data []byte) {
		type file struct {
			name string
			body []byte
		}
		var files []file
		safe, complete := true, false
		var claimed int64
		for tr := tar.NewReader(bytes.NewReader(data)); ; {
			hdr, err := tr.Next()
			if err != nil {
				complete = errors.Is(err, io.EOF)
				break
			}
			if claimed += hdr.Size; hdr.Size < 0 || claimed > maxFuzzedBytes {
				return
			}
			body, err := io.ReadAll(tr)
			if err != nil {
				break
			}
			switch {
			case !filepath.IsLocal(filepath.FromSlash(hdr.Name)):
				safe = false
			case hdr.Typeflag == tar.TypeReg:
				files = append(files, file{hdr.Name, body})
			case hdr.Typeflag != tar.TypeDir:
				safe = false
			}
		}

		parent := t.TempDir()
		dir := filepath.Join(parent, "data")
		err := backup.Extract(bytes.NewReader(data), dir)

		beside, rerr := os.ReadDir(parent)
		if rerr != nil || len(beside) != 1 || beside[0].Name() != "data" {
			t.Errorf("Extract left %v beside the directory (%v), want only the directory", beside, rerr)
		}
		werr := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			if mode := info.Mode(); !(mode.IsDir() || mode.IsRegular()) || mode.Perm()&0o077 != 0 {
				t.Errorf("Extract made %s with mode %v, want a file or directory of its owner alone", p, mode)
			}
			return nil
		})
		if werr != nil {
			t.Errorf("walking what Extract made: %v", werr)
		}
		if err != nil {
			return
		}
		if !safe || !complete {
			t.Errorf("Extract = nil, want an error (every entry safe: %v, archive read to its end: %v)", safe, complete)
		}
		for _, f := range files {
			got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f.name)))
			if err != nil || !bytes.Equal(got, f.body) {
				t.Errorf("Extract wrote %q (%v) to %s, want %q", got, err, f.name, f.body)
			}
		}
	})
}
