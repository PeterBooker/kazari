package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/api"
)

// tarGz writes entries as an archive; a zero Type is a regular file.
func tarGz(t testing.TB, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		typ := e.Type
		if typ == 0 {
			typ = tar.TypeReg
		}
		hdr := &tar.Header{Typeflag: typ, Name: e.Name, Mode: 0o644}
		if typ == tar.TypeReg {
			hdr.Size = int64(len(e.Content))
		} else {
			hdr.Linkname = "../escape"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.Content)); err != nil {
			t.Fatal(err)
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

// readTree returns every regular file under dir by its slash path.
func readTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		files[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestRunPull(t *testing.T) {
	archive := tarGz(t,
		tarEntry{Name: ".nojekyll"},
		tarEntry{Name: "css/site.css", Content: "body{}"},
		tarEntry{Name: "index.html", Content: "<h1>three</h1>"},
	)
	var queries []string
	f, srv := newFakeAPI(t, map[string]http.HandlerFunc{
		"GET /v1/artifacts/hello/bundle": func(w http.ResponseWriter, r *http.Request) {
			queries = append(queries, r.URL.RawQuery)
			w.Header().Set(api.VersionHeader, "3")
			w.Header().Set("Content-Type", "application/gzip")
			w.Write(archive)
		},
	})
	base := []string{"--url", srv.URL, "--token", "secret-key"}
	want := map[string]string{".nojekyll": "", "css/site.css": "body{}", "index.html": "<h1>three</h1>"}
	work := t.TempDir()

	out := filepath.Join(work, "out")
	r := runCLI(t, nil, append(base, "pull", "hello", out)...)
	if wantOut := "pulled version 3 of hello to " + out + ": 3 files, 20 B\n"; r.code != 0 || r.stdout != wantOut {
		t.Errorf("pull hello out: %+v, want stdout %q", r, wantOut)
	}
	if !strings.Contains(r.stderr, ".nojekyll") || !strings.Contains(r.stderr, "!") {
		t.Errorf("pull hello out: stderr %q, want a warning that deploy leaves out .nojekyll", r.stderr)
	}
	if diff := cmp.Diff(want, readTree(t, out)); diff != "" {
		t.Errorf("pulled files (-want +got):\n%s", diff)
	}

	t.Run("not empty", func(t *testing.T) {
		before := len(f.seen)
		r := runCLI(t, nil, append(base, "pull", "hello", out)...)
		if r.code != exitUsage || !strings.Contains(r.stderr, "--force") {
			t.Errorf("pull into a pulled directory: %+v, want exit %d naming --force", r, exitUsage)
		}
		if len(f.seen) != before {
			t.Errorf("pull into a pulled directory sent %v, want no request", f.seen[before:])
		}
	})

	t.Run("force", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(out, "index.html"), []byte("changed"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(out, "notes.txt"), []byte("mine"), 0o644); err != nil {
			t.Fatal(err)
		}
		r := runCLI(t, nil, append(base, "pull", "hello", out, "--force", "--version", "2")...)
		if r.code != 0 {
			t.Fatalf("pull --force: %+v", r)
		}
		forced := map[string]string{"notes.txt": "mine"}
		maps.Copy(forced, want)
		if diff := cmp.Diff(forced, readTree(t, out)); diff != "" {
			t.Errorf("files after pull --force (-want +got):\n%s", diff)
		}
		if got := queries[len(queries)-1]; got != "version=2" {
			t.Errorf("pull --version 2 sent query %q, want version=2", got)
		}
	})

	t.Run("default directory", func(t *testing.T) {
		t.Chdir(t.TempDir())
		r := runCLI(t, nil, append(base, "pull", "hello", "--json")...)
		var got struct {
			Version     int      `json:"version"`
			Dir         string   `json:"dir"`
			Files       int      `json:"files"`
			NotDeployed []string `json:"not_deployed"`
		}
		if err := json.Unmarshal([]byte(r.stdout), &got); r.code != 0 || err != nil {
			t.Fatalf("pull hello --json: %+v, %v", r, err)
		}
		if got.Version != 3 || got.Dir != "hello" || got.Files != 3 || !cmp.Equal(got.NotDeployed, []string{".nojekyll"}) {
			t.Errorf("pull hello --json = %+v, want version 3 in hello, 3 files, .nojekyll not deployed", got)
		}
		if _, err := os.Stat(filepath.Join("hello", "index.html")); err != nil {
			t.Errorf("pull hello wrote no ./hello/index.html: %v", err)
		}
	})

	t.Run("stdout", func(t *testing.T) {
		r := runCLI(t, nil, append(base, "pull", "hello", "-")...)
		if r.code != 0 || r.stdout != string(archive) {
			t.Errorf("pull hello -: exit %d, %d bytes on stdout, want the %d bytes of the archive", r.code, len(r.stdout), len(archive))
		}
	})

	t.Run("a file", func(t *testing.T) {
		file := filepath.Join(work, "file")
		if err := os.WriteFile(file, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if r := runCLI(t, nil, append(base, "pull", "hello", file, "--force")...); r.code != exitUsage {
			t.Errorf("pull into a file: %+v, want exit %d", r, exitUsage)
		}
	})
}

func TestRunPullWithoutVersionHeader(t *testing.T) {
	_, srv := newFakeAPI(t, map[string]http.HandlerFunc{
		"GET /v1/artifacts/hello/bundle": func(w http.ResponseWriter, r *http.Request) {
			w.Write(tarGz(t, tarEntry{Name: "index.html", Content: "x"}))
		},
	})
	r := runCLI(t, nil, "--url", srv.URL, "--token", "secret-key", "pull", "hello", filepath.Join(t.TempDir(), "out"))
	if r.code == 0 || !strings.Contains(r.stderr, api.VersionHeader) {
		t.Errorf("pull from a server that names no version: %+v, want a failure naming %s", r, api.VersionHeader)
	}
}

func TestExtractBundleRefuses(t *testing.T) {
	long := tarGz(t, tarEntry{Name: "a", Content: strings.Repeat("x", 4096)})
	tests := []struct {
		name    string
		archive []byte
		unsafe  bool
	}{
		{"parent", tarGz(t, tarEntry{Name: "../escape", Content: "x"}), true},
		{"absolute", tarGz(t, tarEntry{Name: "/escape", Content: "x"}), true},
		{"symlink", tarGz(t, tarEntry{Name: "link", Type: tar.TypeSymlink}), true},
		{"twice", tarGz(t, tarEntry{Name: "a", Content: "1"}, tarEntry{Name: "a", Content: "2"}), true},
		{"cut short", long[:len(long)-4], false},
		{"not gzip", []byte("plain text"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parent := t.TempDir()
			dir := filepath.Join(parent, "out")
			_, _, err := extractBundle(bytes.NewReader(tc.archive), dir)
			if err == nil {
				t.Fatalf("extractBundle(%s) = nil error, want one", tc.name)
			}
			if got := errors.Is(err, errUnsafeEntry); got != tc.unsafe {
				t.Errorf("extractBundle(%s) = %v; unsafe %t, want %t", tc.name, err, got, tc.unsafe)
			}
			if _, err := os.Lstat(filepath.Join(parent, "escape")); err == nil {
				t.Errorf("extractBundle(%s) wrote outside the directory", tc.name)
			}
		})
	}
}

func TestExtractBundleThroughSymlink(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "out")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(parent, filepath.Join(dir, "up")); err != nil {
		t.Fatal(err)
	}
	_, _, err := extractBundle(bytes.NewReader(tarGz(t, tarEntry{Name: "up/escape", Content: "x"})), dir)
	if err == nil {
		t.Error("extractBundle through a symlink out of the directory = nil error, want one")
	}
	if _, err := os.Lstat(filepath.Join(parent, "escape")); err == nil {
		t.Error("extractBundle followed a symlink out of the directory")
	}
}
