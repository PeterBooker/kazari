package httpapi

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/artifact"
)

type tarEntry struct {
	name string
	typ  byte
	body string
}

func gzipTar(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o644, Size: int64(len(e.body))}
		if e.typ != tar.TypeReg {
			hdr.Size = 0
			hdr.Linkname = "target"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func many(n int, typ byte) []tarEntry {
	out := make([]tarEntry, n)
	for i := range out {
		out[i] = tarEntry{name: fmt.Sprintf("f%d", i), typ: typ}
	}
	return out
}

// Header blocks count against the cap, so many empty or skipped entries
// cannot decode without bound.
func TestReadBundleEntryCap(t *testing.T) {
	const limit = 64 << 10
	tests := []struct {
		name    string
		entries []tarEntry
		files   int
		tooBig  bool
	}{
		{"small bundle", []tarEntry{{"index.html", tar.TypeReg, "hi"}, {"./a/b.css", tar.TypeReg, "x"}}, 2, false},
		{"skipped links still parse", []tarEntry{{"l", tar.TypeSymlink, ""}, {"index.html", tar.TypeReg, "hi"}}, 1, false},
		{"many empty files", many(1000, tar.TypeReg), 0, true},
		{"many skipped symlinks", many(1000, tar.TypeSymlink), 0, true},
		{"many hard links", many(1000, tar.TypeLink), 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := gzipTar(t, tt.entries)
			b, err := readBundle(bytes.NewReader(raw), t.TempDir(), limit)
			if tt.tooBig {
				if !errors.Is(err, artifact.ErrTooLarge) {
					t.Fatalf("readBundle err = %v, want too_large", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			if len(b.files) != tt.files {
				t.Errorf("files = %d, want %d", len(b.files), tt.files)
			}
		})
	}
}

// gzipStream appends trailer zero bytes after the archive.
func gzipStream(t *testing.T, write func(tw *tar.Writer) error, trailer int) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	if err := write(tw); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(make([]byte, trailer)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func regular(tw *tar.Writer, name string, body []byte, format tar.Format) error {
	hdr := &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)), Format: format}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err := tw.Write(body)
	return err
}

// The cap counts every byte tar.Reader reads inside Next and anything after
// the archive, not just what the bundle code sees.
func TestReadBundleCapsWholeStream(t *testing.T) {
	const limit = 64 << 10
	longName := strings.Repeat("a", 2*limit)
	tests := []struct {
		name    string
		write   func(tw *tar.Writer) error
		trailer int
		want    error // nil: accepted
		files   int
	}{
		{"fits", func(tw *tar.Writer) error {
			return regular(tw, "index.html", make([]byte, 60<<10), tar.FormatUSTAR)
		}, 0, nil, 1},
		{"file body over the cap", func(tw *tar.Writer) error {
			return regular(tw, "index.html", make([]byte, limit), tar.FormatUSTAR)
		}, 0, artifact.ErrTooLarge, 0},
		{"PAX long name", func(tw *tar.Writer) error {
			return regular(tw, longName, []byte("x"), tar.FormatPAX)
		}, 0, artifact.ErrTooLarge, 0},
		{"GNU long name", func(tw *tar.Writer) error {
			return regular(tw, longName, []byte("x"), tar.FormatGNU)
		}, 0, artifact.ErrTooLarge, 0},
		{"data of a skipped entry", func(tw *tar.Writer) error {
			if err := tw.WriteHeader(&tar.Header{Name: "x", Typeflag: tar.TypeCont, Mode: 0o644, Size: 1 << 20}); err != nil {
				return err
			}
			if _, err := tw.Write(make([]byte, 1<<20)); err != nil {
				return err
			}
			return regular(tw, "index.html", []byte("hi"), tar.FormatUSTAR)
		}, 0, artifact.ErrTooLarge, 0},
		{"data after the archive", func(tw *tar.Writer) error {
			return regular(tw, "index.html", []byte("hi"), tar.FormatUSTAR)
		}, 1 << 20, artifact.ErrTooLarge, 0},
		{"name at the path limit", func(tw *tar.Writer) error {
			return regular(tw, strings.Repeat("n", artifact.PathMaxBytes), []byte("x"), tar.FormatPAX)
		}, 0, nil, 1},
		{"name over the path limit", func(tw *tar.Writer) error {
			return regular(tw, strings.Repeat("n", artifact.PathMaxBytes+1), []byte("x"), tar.FormatPAX)
		}, 0, artifact.ErrInvalidPath, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := gzipStream(t, tt.write, tt.trailer)
			b, err := readBundle(bytes.NewReader(raw), t.TempDir(), limit)
			if tt.want != nil {
				if !errors.Is(err, tt.want) {
					t.Fatalf("readBundle err = %v, want %v", err, tt.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			if len(b.files) != tt.files {
				t.Errorf("files = %d, want %d", len(b.files), tt.files)
			}
		})
	}
}

func TestRemoveStaleSpills(t *testing.T) {
	dir := t.TempDir()
	cutoff := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	for name, mtime := range map[string]time.Time{
		spillPrefix + "old":  cutoff.Add(-time.Minute),
		spillPrefix + "new":  cutoff.Add(time.Minute),
		"someone-elses-file": cutoff.Add(-time.Hour),
	} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, spillPrefix+"dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	n, err := RemoveStaleSpills(dir, cutoff)
	if err != nil || n != 1 {
		t.Fatalf("RemoveStaleSpills = %d, %v; want 1, nil", n, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	want := []string{spillPrefix + "dir", spillPrefix + "new", "someone-elses-file"}
	if !slices.Equal(left, want) {
		t.Fatalf("left %v, want %v", left, want)
	}
	if n, err := RemoveStaleSpills(filepath.Join(dir, "missing"), cutoff); err != nil || n != 0 {
		t.Fatalf("missing directory: %d, %v", n, err)
	}
}

// A header may declare more than the body carries: archive/tar fills a sparse
// entry's holes itself, and a truncated stream stops early. Either way the
// declared size is refused before anything is copied.
func TestReadBundleChargesDeclaredSize(t *testing.T) {
	const limit = 64 << 10
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{Name: "index.html", Typeflag: tar.TypeReg, Mode: 0o644, Size: limit + 1}); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := readBundle(bytes.NewReader(buf.Bytes()), t.TempDir(), limit)
	if !errors.Is(err, artifact.ErrTooLarge) {
		t.Fatalf("readBundle err = %v, want too_large", err)
	}
}

type zipFile struct {
	name string
	body string
	mode os.FileMode // 0 means a regular file
}

func zipArchive(t *testing.T, files []zipFile) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range files {
		hdr := &zip.FileHeader{Name: f.name, Method: zip.Deflate}
		if f.mode != 0 {
			hdr.SetMode(f.mode)
		}
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func readAllZip(t *testing.T, raw []byte, maxFiles int) (map[string]string, error) {
	t.Helper()
	files, err := readZip(bytes.NewReader(raw), int64(len(raw)), maxFiles)
	if err != nil {
		return nil, err
	}
	got := map[string]string{}
	for _, f := range files {
		b, err := io.ReadAll(f.Content)
		if err != nil {
			return nil, err
		}
		got[f.Path] = string(b)
	}
	return got, nil
}

func TestReadZip(t *testing.T) {
	raw := zipArchive(t, []zipFile{
		{name: "./index.html", body: "<h1>hi</h1>"},
		{name: "assets/", mode: os.ModeDir | 0o755},
		{name: "assets/app.js", body: "js"},
		{name: "link", body: "index.html", mode: os.ModeSymlink | 0o777},
		{name: "../escape", body: "kept for the path check"},
	})
	got, err := readAllZip(t, raw, 100)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"index.html": "<h1>hi</h1>", "assets/app.js": "js", "../escape": "kept for the path check"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("readZip (-want +got):\n%s", diff)
	}

	if _, err := readAllZip(t, raw, 2); !errors.Is(err, artifact.ErrTooManyFiles) {
		t.Errorf("maxFiles: err = %v, want too_many_files", err)
	}
	long := zipArchive(t, []zipFile{{name: strings.Repeat("n", artifact.PathMaxBytes+1), body: "x"}})
	if _, err := readAllZip(t, long, 100); !errors.Is(err, artifact.ErrInvalidPath) {
		t.Errorf("long name: err = %v, want invalid_path", err)
	}
	if _, err := readAllZip(t, []byte("PK not really a zip"), 100); !errors.Is(err, artifact.ErrInvalidArgument) {
		t.Errorf("not a zip: err = %v, want invalid_argument", err)
	}
}

// A corrupt entry surfaces when it is read, as the sender's fault.
func TestReadZipCorruptEntry(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "a.txt", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	const body = "stored without compression"
	if _, err := w.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()
	raw[bytes.Index(raw, []byte(body))] ^= 0xff
	_, err = readAllZip(t, raw, 100)
	if !errors.Is(err, artifact.ErrInvalidArgument) || !strings.Contains(err.Error(), "a.txt") {
		t.Fatalf("err = %v, want invalid_argument naming the entry", err)
	}
}
