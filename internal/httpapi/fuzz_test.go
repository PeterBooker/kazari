package httpapi_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/httpapi"
)

type archived struct {
	name string
	typ  byte // of a tar entry; a zip entry whose name ends in "/" is a directory
	body string
}

var archivedSeeds = [][]archived{
	{{"index.html", tar.TypeReg, "<h1>hi</h1>"}, {"./a/b.css", tar.TypeReg, "b{}"}, {"assets/", tar.TypeDir, ""}},
	{{"link", tar.TypeSymlink, ""}, {"index.html", tar.TypeReg, strings.Repeat("hi ", 400)}},
	{{"../escape", tar.TypeReg, "x"}, {"/abs", tar.TypeReg, "y"}, {"././x", tar.TypeReg, ""}, {strings.Repeat("n", 1100), tar.TypeReg, ""}},
	{},
}

func zipOf(tb testing.TB, entries []archived) []byte {
	tb.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		w, err := zw.Create(e.name)
		if err != nil {
			tb.Fatal(err)
		}
		if _, err := io.WriteString(w, e.body); err != nil {
			tb.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		tb.Fatal(err)
	}
	return buf.Bytes()
}

func tarOf(tb testing.TB, entries []archived) []byte {
	tb.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o644, Size: int64(len(e.body)), Linkname: "target"}
		if err := tw.WriteHeader(hdr); err != nil {
			tb.Fatal(err)
		}
		if _, err := io.WriteString(tw, e.body); err != nil {
			tb.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		tb.Fatal(err)
	}
	return buf.Bytes()
}

func gzipOf(tb testing.TB, data []byte) []byte {
	tb.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		tb.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		tb.Fatal(err)
	}
	return buf.Bytes()
}

// pathError reports a name the readers should not have passed on.
func pathError(p string) string {
	switch {
	case p == "":
		return "an empty name"
	case strings.HasPrefix(p, "./"):
		return "a name that starts with ./"
	case len(p) > artifact.PathMaxBytes:
		return "a name longer than a path can be"
	}
	return ""
}

func FuzzReadZip(f *testing.F) {
	for _, entries := range archivedSeeds {
		f.Add(zipOf(f, entries))
	}
	whole := zipOf(f, archivedSeeds[0])
	f.Add(whole[:len(whole)-10])
	f.Add([]byte("PK\x03\x04 not a zip"))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		const maxFiles, maxRead = 20, 1 << 20
		files, err := httpapi.ReadZip(bytes.NewReader(data), int64(len(data)), maxFiles)
		if err != nil {
			if artifact.CodeOf(err) == "" || files != nil {
				t.Errorf("ReadZip(%q) = %d files, %v, want none and an error with a code", data, len(files), err)
			}
			return
		}
		if len(files) > maxFiles {
			t.Errorf("ReadZip(%q) = %d files, want at most %d", data, len(files), maxFiles)
		}
		for _, file := range files {
			if why := pathError(file.Path); why != "" {
				t.Errorf("ReadZip(%q) has the file %q, %s", data, file.Path, why)
			}
			// A deflated entry may hold a thousand times its size.
			if _, err := io.Copy(io.Discard, io.LimitReader(file.Content, maxRead)); err != nil && artifact.CodeOf(err) == "" {
				t.Errorf("ReadZip(%q): reading %q = %v, want an error with a code", data, file.Path, err)
			}
		}
	})
}

func FuzzReadBundle(f *testing.F) {
	for _, entries := range archivedSeeds {
		f.Add(tarOf(f, entries), false)
	}
	whole := tarOf(f, archivedSeeds[0])
	f.Add(whole[:len(whole)/3], false)
	f.Add(gzipOf(f, whole), true)
	f.Add(gzipOf(f, whole)[:40], true)
	f.Add([]byte("\x1f\x8b not gzip"), true)
	f.Add([]byte{}, true)

	f.Fuzz(func(t *testing.T, data []byte, compressed bool) {
		const limit = 64 << 10
		// Most changes to compressed bytes fail the checksum, so the tar
		// reader is reached by compressing here what the fuzzer changed.
		if !compressed {
			data = gzipOf(t, data)
		}
		dir := t.TempDir()
		files, closeBundle, err := httpapi.ReadBundle(bytes.NewReader(data), dir, limit)
		if err != nil {
			if artifact.CodeOf(err) == "" || files != nil {
				t.Errorf("ReadBundle(%q) = %d files, %v, want none and an error with a code", data, len(files), err)
			}
		} else {
			var total int64
			for _, file := range files {
				if why := pathError(file.Path); why != "" {
					t.Errorf("ReadBundle(%q) has the file %q, %s", data, file.Path, why)
				}
				n, err := io.Copy(io.Discard, file.Content)
				if err != nil {
					t.Errorf("ReadBundle(%q): reading %q = %v", data, file.Path, err)
				}
				total += n
			}
			if total > limit {
				t.Errorf("ReadBundle(%q) holds %d bytes, want at most %d", data, total, limit)
			}
			if err := closeBundle(); err != nil {
				t.Errorf("ReadBundle(%q): closing = %v", data, err)
			}
		}
		if left, err := os.ReadDir(dir); err != nil || len(left) > 0 {
			t.Errorf("ReadBundle(%q) left %v in its directory (%v), want nothing", data, left, err)
		}
	})
}
