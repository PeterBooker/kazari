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

func newFS(t *testing.T) *blob.FS {
	t.Helper()
	fs, err := blob.NewFS(filepath.Join(t.TempDir(), "blobs"), blob.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

func TestFSConformance(t *testing.T) {
	blobtest.Run(t, func(t *testing.T) blob.Store { t.Helper(); return newFS(t) })
	blobtest.RunList(t, func(t *testing.T) blobtest.Lister { t.Helper(); return newFS(t) })
}

func TestFSLayout(t *testing.T) {
	dir := t.TempDir()
	fs, err := blob.NewFS(dir, blob.Options{})
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
	fs, err := blob.NewFS(dir, blob.Options{})
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
	fs, err := blob.NewFS(t.TempDir(), blob.Options{})
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

func TestFSListSkipsWhatIsNotABlob(t *testing.T) {
	dir := t.TempDir()
	fs, err := blob.NewFS(dir, blob.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	hash, _, err := fs.Put(ctx, strings.NewReader("listed"))
	if err != nil {
		t.Fatal(err)
	}
	mtime := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(dir, hash[:2], hash), mtime, mtime); err != nil {
		t.Fatal(err)
	}
	other := "ff"
	if hash[:2] == other {
		other = "00"
	}
	for _, name := range []string{
		".put-123",
		"README",
		hash,
		filepath.Join(hash[:2], ".put-456"),
		filepath.Join(hash[:2], "notes.txt"),
		filepath.Join(hash[:2], strings.ToUpper(hash)),
		filepath.Join(hash[:2], other+hash[2:]),
		filepath.Join("lost", hash),
		filepath.Join(strings.ToUpper(other), other+hash[2:]),
	} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("not a blob"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, hash[:2], hash[:2]+strings.Repeat("0", 62)), 0o755); err != nil {
		t.Fatal(err)
	}

	var got []blob.Info
	for info, err := range fs.List(ctx) {
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		got = append(got, info)
	}
	if len(got) != 1 || got[0].Hash != hash || got[0].Size != int64(len("listed")) || !got[0].ModTime.Equal(mtime) {
		t.Errorf("List = %+v, want only %s of size %d modified at %v", got, hash, len("listed"), mtime)
	}
}

func TestFSListCancelled(t *testing.T) {
	fs, err := blob.NewFS(t.TempDir(), blob.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fs.Put(context.Background(), strings.NewReader("listed")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var errs []error
	for _, err := range fs.List(ctx) {
		errs = append(errs, err)
	}
	if len(errs) != 1 || !errors.Is(errs[0], context.Canceled) {
		t.Errorf("List with a cancelled context yielded %v, want one context.Canceled", errs)
	}
}

func TestFSPutCancelled(t *testing.T) {
	dir := t.TempDir()
	fs, err := blob.NewFS(dir, blob.Options{})
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
