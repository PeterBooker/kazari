package blob_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/blob"
	"github.com/PeterBooker/kazari/internal/blob/blobtest"
)

func TestFSConformance(t *testing.T) {
	blobtest.Run(t, func(t *testing.T) blob.Store {
		fs, err := blob.NewFS(filepath.Join(t.TempDir(), "blobs"))
		if err != nil {
			t.Fatal(err)
		}
		return fs
	})
}

func TestFSLayout(t *testing.T) {
	dir := t.TempDir()
	fs, err := blob.NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	hash, _, err := fs.Put(context.Background(), strings.NewReader("layout"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, hash[:2], hash)); err != nil {
		t.Fatalf("blob not at dir/<hash[0:2]>/<hash>: %v", err)
	}
	if _, _, err := fs.Put(context.Background(), strings.NewReader("layout")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".put-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestFSRemoveStaleTemp(t *testing.T) {
	dir := t.TempDir()
	fs, err := blob.NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	hash, _, err := fs.Put(ctx, strings.NewReader("kept"))
	if err != nil {
		t.Fatal(err)
	}
	cutoff := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	for name, mtime := range map[string]time.Time{
		".put-old":    cutoff.Add(-time.Minute),
		".put-recent": cutoff.Add(time.Minute),
		"README":      cutoff.Add(-time.Hour),
	} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	n, err := fs.RemoveStaleTemp(cutoff)
	if err != nil || n != 1 {
		t.Fatalf("RemoveStaleTemp = %d, %v; want 1, nil", n, err)
	}
	for name, want := range map[string]bool{".put-old": false, ".put-recent": true, "README": true} {
		_, err := os.Stat(filepath.Join(dir, name))
		if got := err == nil; got != want {
			t.Errorf("%s present = %v, want %v", name, got, want)
		}
	}
	if _, err := fs.Stat(ctx, hash); err != nil {
		t.Errorf("blob after sweep: %v", err)
	}
}

func TestFSRejectsMalformedHash(t *testing.T) {
	fs, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, h := range []string{
		"",
		"abc",
		"../../../../etc/passwd",
		strings.Repeat("A", 64),
		strings.Repeat("g", 64),
		strings.Repeat("a", 63) + "/",
		strings.Repeat("a", 65),
	} {
		if _, err := fs.Open(ctx, h); !errors.Is(err, blob.ErrInvalidHash) {
			t.Errorf("Open(%q): err = %v, want ErrInvalidHash", h, err)
		}
		if _, err := fs.Stat(ctx, h); !errors.Is(err, blob.ErrInvalidHash) {
			t.Errorf("Stat(%q): err = %v, want ErrInvalidHash", h, err)
		}
		if err := fs.Delete(ctx, h); !errors.Is(err, blob.ErrInvalidHash) {
			t.Errorf("Delete(%q): err = %v, want ErrInvalidHash", h, err)
		}
	}
}

func TestFSPutCancelled(t *testing.T) {
	dir := t.TempDir()
	fs, err := blob.NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := fs.Put(ctx, strings.NewReader("never")); !errors.Is(err, context.Canceled) {
		t.Fatalf("Put with cancelled context: err = %v, want context.Canceled", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("cancelled Put left %d entries", len(entries))
	}
}
