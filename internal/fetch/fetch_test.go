package fetch_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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
	if !errors.Is(err, artifact.ErrFetchFailed) || !strings.HasSuffix(err.Error(), ": not a public address") {
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

func TestRefusesCredentialsAndHidesAddresses(t *testing.T) {
	c := fetch.New(fetch.Options{Dir: t.TempDir()})
	if _, _, err := c.Get(context.Background(), "https://alice:hunter2@example.com/", 1<<20); !errors.Is(err, artifact.ErrInvalidArgument) || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("credentials: %v", err)
	}
	_, _, err := c.Get(context.Background(), "http://127.0.0.1:1/", 1<<20)
	if !errors.Is(err, artifact.ErrFetchFailed) || !strings.HasSuffix(err.Error(), ": not a public address") {
		t.Errorf("loopback: %v", err)
	}
	if _, _, err := c.Get(context.Background(), "http://no-such-host.invalid/", 1<<20); !errors.Is(err, artifact.ErrFetchFailed) ||
		!strings.HasSuffix(err.Error(), "could not resolve the host") {
		t.Errorf("unknown host: %v", err)
	}
}

func TestRedirectLimitAndStatusText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/hop/"):
			n, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/hop/"))
			if n == 0 {
				io.WriteString(w, "done")
				return
			}
			http.Redirect(w, r, "/hop/"+strconv.Itoa(n-1), http.StatusFound)
		case r.URL.Path == "/teapot":
			w.WriteHeader(http.StatusTeapot)
			io.WriteString(w, "ignore this status line")
		}
	}))
	defer srv.Close()
	c, _ := local(t)
	if data, _, err := c.Get(context.Background(), srv.URL+"/hop/5", 1<<20); err != nil || string(data) != "done" {
		t.Errorf("five redirects: %q %v", data, err)
	}
	if _, _, err := c.Get(context.Background(), srv.URL+"/hop/6", 1<<20); !errors.Is(err, artifact.ErrFetchFailed) {
		t.Errorf("six redirects: %v", err)
	}
	if _, _, err := c.Get(context.Background(), srv.URL+"/teapot", 1<<20); err == nil || !strings.HasSuffix(err.Error(), "answered 418 I'm a teapot") {
		t.Errorf("status: %v", err)
	}
}

func TestDownloadsBudgetAndTotal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, strings.Repeat("x", 100))
	}))
	defer srv.Close()
	c, dir := local(t)
	d := c.Downloads(context.Background(), 1000, 150)
	in, err := d.Add("a.txt", srv.URL, "")
	if err != nil || in.Path != "a.txt" || in.ContentType != "text/plain" {
		t.Fatalf("first: %+v %v", in, err)
	}
	if _, err := d.Add("b.txt", srv.URL, ""); !errors.Is(err, artifact.ErrFetchFailed) || !strings.Contains(err.Error(), "larger than 50") {
		t.Errorf("over the total: %v", err)
	}
	d.Close()
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("%d files left after Close", len(entries))
	}
	var none *fetch.Client
	if _, err := none.Downloads(context.Background(), 1, 1).Add("a", srv.URL, ""); !errors.Is(err, artifact.ErrForbidden) {
		t.Errorf("nil client: %v", err)
	}
	none.Downloads(context.Background(), 1, 1).Close()

	f, err := c.Download(context.Background(), srv.URL, 1000)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Replace([]byte("new")); err != nil {
		t.Fatal(err)
	}
	if data, _ := io.ReadAll(f); string(data) != "new" || f.Size != 3 {
		t.Errorf("after Replace: %q size %d", data, f.Size)
	}
}
