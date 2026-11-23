package cli_test

import (
	"archive/tar"
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/api"
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

// publishBundle runs kazari publish src against an API that keeps the bundle.
func publishBundle(t *testing.T, src string) (result, bundleRequest) {
	t.Helper()
	var got bundleRequest
	_, srv := newFakeAPI(t, map[string]http.HandlerFunc{
		"POST /v1/artifacts": func(w http.ResponseWriter, r *http.Request) {
			got = decodeBundleRequest(t, r)
			reply(w, 201, api.PublishResult{URL: "https://demo.art.test", Version: 1})
		},
	})
	r := runCLI(t, nil, "--url", srv.URL, "--token", "t", "publish", src, "--slug", "demo")
	return r, got
}

func TestBundle(t *testing.T) {
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
	r, got := publishBundle(t, dir)
	if r.code != 0 {
		t.Fatalf("publish: %+v", r)
	}
	epoch := time.Unix(0, 0).UTC()
	want := []tarEntry{
		{".well-known/security.txt", "contact", 0o644, epoch, 0, tar.TypeReg},
		{"a.css", "a", 0o644, epoch, 0, tar.TypeReg},
		{"a/b/c.js", "c", 0o644, epoch, 0, tar.TypeReg},
		{"b/z.txt", "z", 0o644, epoch, 0, tar.TypeReg},
		{"index.html", "<h1>hi</h1>", 0o644, epoch, 0, tar.TypeReg},
	}
	if diff := cmp.Diff(want, listTarGz(t, got.raw)); diff != "" {
		t.Errorf("entries (-want +got):\n%s", diff)
	}
}

func TestBundleReproducible(t *testing.T) {
	files := map[string]string{"index.html": "x", "a/b.txt": "y", "big.bin": strings.Repeat("ab", 1<<18)}
	_, first := publishBundle(t, writeTree(t, files))
	dir := writeTree(t, files)
	old := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(dir, "index.html"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "a", "b.txt"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, second := publishBundle(t, dir)
	if len(first.raw) == 0 || !bytes.Equal(first.raw, second.raw) {
		t.Errorf("same tree produced different archives: %d and %d bytes", len(first.raw), len(second.raw))
	}
}

func TestBundleErrors(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"missing", filepath.Join(t.TempDir(), "nope"), "no such directory"},
		{"not a directory or a file", "/dev/null", "is neither a directory nor a file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := runCLI(t, nil, "--url", "http://127.0.0.1:1", "--token", "t", "publish", tt.src, "--slug", "demo")
			if r.code != exitUsage || !strings.Contains(r.stderr, tt.want) {
				t.Errorf("publish %s: %+v, want exit %d and %q", tt.src, r, exitUsage, tt.want)
			}
		})
	}
}

// A server that refuses the bundle before reading it must not leave the
// publish waiting on the archive it streams.
func TestBundleRefusedEarly(t *testing.T) {
	dir := writeTree(t, map[string]string{"big.bin": strings.Repeat("\x01", 4<<20)})
	_, srv := newFakeAPI(t, map[string]http.HandlerFunc{
		"POST /v1/artifacts": replyWith(http.StatusRequestEntityTooLarge,
			api.ErrorResponse{Error: api.Error{Code: api.CodeTooLarge, Message: "bundle over 1 MiB"}}),
	})
	r := runCLI(t, nil, "--url", srv.URL, "--token", "t", "publish", dir, "--slug", "demo")
	if r.code != exitError || !strings.Contains(r.stderr, "too_large") {
		t.Errorf("publish refused early: %+v", r)
	}
}
