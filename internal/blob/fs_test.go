package blob_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agalma/internal/blob"
	"agalma/internal/blob/blobtest"
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
	// No temp files are left behind, including after an idempotent Put.
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
