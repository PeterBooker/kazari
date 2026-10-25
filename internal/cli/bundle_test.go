package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

type tarEntry struct {
	Name    string
	Content string
	Mode    int64
	ModTime time.Time
	Uid     int
	Type    byte
}

func listTarGz(t *testing.T, b []byte) []tarEntry {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	var out []tarEntry
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		c, _ := io.ReadAll(tr)
		out = append(out, tarEntry{h.Name, string(c), h.Mode, h.ModTime.UTC(), h.Uid, h.Typeflag})
	}
}

func TestTarGz(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"index.html":               "<h1>hi</h1>",
		"b/z.txt":                  "z",
		"a/b/c.js":                 "c",
		"a.css":                    "a",
		".DS_Store":                "junk",
		".git/config":              "secret",
		"sub/.hidden":              "h",
		".well-known/security.txt": "contact",
	})
	if err := os.Symlink(filepath.Join(dir, "index.html"), filepath.Join(dir, "link.html")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := TarGz(dir, &buf); err != nil {
		t.Fatal(err)
	}
	epoch := time.Unix(0, 0).UTC()
	want := []tarEntry{
		{".well-known/security.txt", "contact", 0o644, epoch, 0, tar.TypeReg},
		{"a.css", "a", 0o644, epoch, 0, tar.TypeReg},
		{"a/b/c.js", "c", 0o644, epoch, 0, tar.TypeReg},
		{"b/z.txt", "z", 0o644, epoch, 0, tar.TypeReg},
		{"index.html", "<h1>hi</h1>", 0o644, epoch, 0, tar.TypeReg},
	}
	if diff := cmp.Diff(want, listTarGz(t, buf.Bytes())); diff != "" {
		t.Errorf("entries (-want +got):\n%s", diff)
	}
}

func TestTarGzReproducible(t *testing.T) {
	files := map[string]string{"index.html": "x", "a/b.txt": "y"}
	var first, second bytes.Buffer
	if err := TarGz(writeTree(t, files), &first); err != nil {
		t.Fatal(err)
	}
	dir := writeTree(t, files)
	old := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(dir, "index.html"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "a", "b.txt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := TarGz(dir, &second); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Error("same tree produced different archives")
	}
}

func TestTarGzErrors(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		dir  string
	}{
		{"missing", filepath.Join(t.TempDir(), "nope")},
		{"not a directory", file},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := TarGz(tt.dir, io.Discard); err == nil {
				t.Error("no error")
			}
			b, err := io.ReadAll(TarGzReader(tt.dir))
			if err == nil {
				t.Errorf("reader: no error, read %d bytes", len(b))
			}
		})
	}
}

func TestTarGzReaderMatchesTarGz(t *testing.T) {
	dir := writeTree(t, map[string]string{"index.html": "x", "big.bin": string(bytes.Repeat([]byte("ab"), 1<<18))})
	var want bytes.Buffer
	if err := TarGz(dir, &want); err != nil {
		t.Fatal(err)
	}
	rc := TarGzReader(dir)
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want.Bytes(), got) {
		t.Error("TarGzReader differs from TarGz")
	}
}

func TestTarGzReaderEarlyClose(t *testing.T) {
	dir := writeTree(t, map[string]string{"big.bin": string(bytes.Repeat([]byte{1}, 1<<20))})
	rc := TarGzReader(dir)
	if _, err := rc.Read(make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	if err := rc.Close(); err != nil { // must unblock the writer goroutine
		t.Fatal(err)
	}
}
