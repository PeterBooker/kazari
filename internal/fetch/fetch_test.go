package fetch_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/fetch"
)

// local allows loopback, which httptest servers listen on.
func local(t *testing.T) (*fetch.Client, string) {
	t.Helper()
	dir := t.TempDir()
	return fetch.New(fetch.Options{Transport: http.DefaultTransport, Dir: dir}), dir
}

func TestGetFollowsRedirectsAndReportsType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/old":
			http.Redirect(w, r, "/new", http.StatusFound)
		case "/new":
			if ua := r.Header.Get("User-Agent"); !strings.HasPrefix(ua, "Kazari") {
				t.Errorf("User-Agent = %q", ua)
			}
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
			io.WriteString(w, "body{}")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, _ := local(t)
	data, res, err := c.Get(context.Background(), srv.URL+"/old", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "body{}" || res.ContentType != "text/css; charset=utf-8" || res.URL.Path != "/new" {
		t.Errorf("got %q %+v", data, res)
	}
	_, _, err = c.Get(context.Background(), srv.URL+"/missing", 1<<20)
	if !errors.Is(err, artifact.ErrFetchFailed) || !strings.Contains(err.Error(), "404") {
		t.Errorf("missing: %v", err)
	}
}

func TestLimitsAndDownload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, strings.Repeat("x", 100))
	}))
	defer srv.Close()
	c, dir := local(t)
	ctx := context.Background()
	if _, _, err := c.Get(ctx, srv.URL, 99); !errors.Is(err, artifact.ErrFetchFailed) || !strings.Contains(err.Error(), "larger than 99") {
		t.Errorf("over limit: %v", err)
	}
	if _, err := c.Download(ctx, srv.URL, 99); !errors.Is(err, artifact.ErrFetchFailed) {
		t.Errorf("download over limit: %v", err)
	}
	f, err := c.Download(ctx, srv.URL, 100)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(f)
	if len(data) != 100 || f.Size != 100 || filepath.Dir(f.Name()) != dir {
		t.Errorf("download = %d bytes, size %d, at %s", len(data), f.Size, f.Name())
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.Name()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("file kept after Close: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("%d files left in %s", len(entries), dir)
	}
}

func TestRefusesNonPublicAndOtherSchemes(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	c := fetch.New(fetch.Options{Dir: t.TempDir()})
	_, _, err := c.Get(context.Background(), srv.URL, 1<<20)
	if !errors.Is(err, artifact.ErrFetchFailed) || !strings.Contains(err.Error(), "non-public address") {
		t.Errorf("loopback: %v", err)
	}
	for _, u := range []string{"ftp://example.com/x", "file:///etc/passwd", "example.com", ""} {
		if _, _, err := c.Get(context.Background(), u, 1<<20); !errors.Is(err, artifact.ErrInvalidArgument) {
			t.Errorf("%q: %v", u, err)
		}
	}
}

func TestRemoveStale(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "kazari-fetch-old")
	os.WriteFile(old, nil, 0o600)
	past := time.Now().Add(-2 * time.Hour)
	os.Chtimes(old, past, past)
	os.WriteFile(filepath.Join(dir, "kazari-fetch-new"), nil, 0o600)
	os.WriteFile(filepath.Join(dir, "other"), nil, 0o600)
	n, err := fetch.RemoveStale(dir, time.Now().Add(-time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("removed %d, %v", n, err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Errorf("%d entries left", len(entries))
	}
}
