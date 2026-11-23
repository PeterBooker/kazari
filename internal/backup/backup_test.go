package backup_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/backup"
	"github.com/PeterBooker/kazari/internal/blob"
	"github.com/PeterBooker/kazari/internal/sqlite"
)

var (
	start = time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	ana   = artifact.Actor{Handle: "ana", Admin: true}
)

// instance is a data directory with its services.
type instance struct {
	dir  string
	db   *sqlite.DB
	arts *artifact.Service
}

// open starts the services over dir as the server lays it out.
func open(t *testing.T, dir string) *instance {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(dir, "kazari.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	blobs, err := blob.NewFS(filepath.Join(dir, "blobs"), blob.Options{})
	if err != nil {
		t.Fatal(err)
	}
	arts := artifact.New(sqlite.NewArtifactStore(db), blobs, nil, artifact.Options{Now: func() time.Time { return start }})
	return &instance{dir: dir, db: db, arts: arts}
}

func newInstance(t *testing.T) *instance {
	t.Helper()
	in := open(t, t.TempDir())
	if err := os.MkdirAll(filepath.Join(in.dir, "tmp"), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := in.db.ExecContext(context.Background(),
		`INSERT INTO user (handle, role, quota_bytes, created_at) VALUES ('ana', 'admin', 0, ?)`, start.UnixNano()); err != nil {
		t.Fatal(err)
	}
	return in
}

func (in *instance) service(opts backup.Options) *backup.Service {
	opts.DBPath = filepath.Join(in.dir, "kazari.db")
	opts.BlobDir = filepath.Join(in.dir, "blobs")
	opts.CertDir = filepath.Join(in.dir, "certmagic")
	opts.TempDir = filepath.Join(in.dir, "tmp")
	if opts.Now == nil {
		opts.Now = func() time.Time { return start }
	}
	return backup.New(in.db, opts)
}

func (in *instance) publish(t testing.TB, slug string, files map[string]string) {
	t.Helper()
	if err := in.tryPublish(slug, files); err != nil {
		t.Fatalf("publish %s: %v", slug, err)
	}
}

func (in *instance) tryPublish(slug string, files map[string]string) error {
	var inputs []artifact.FileInput
	for p, body := range files {
		inputs = append(inputs, artifact.FileInput{Path: p, Content: strings.NewReader(body)})
	}
	_, _, err := in.arts.Create(context.Background(), ana, artifact.CreateRequest{Slug: slug, Files: inputs})
	return err
}

func (in *instance) read(t *testing.T, slug, p string) string {
	t.Helper()
	_, rc, err := in.arts.OpenFile(context.Background(), ana, slug, p, 0)
	if err != nil {
		t.Fatalf("open %s/%s: %v", slug, p, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// hookWriter calls hook once, at the first write: the database copy is
// taken by then and no blob has been read.
type hookWriter struct {
	w    io.Writer
	hook func()
	once sync.Once
}

func (h *hookWriter) Write(p []byte) (int, error) {
	h.once.Do(h.hook)
	return h.w.Write(p)
}

func names(t *testing.T, archive []byte) []string {
	t.Helper()
	var out []string
	tr := tar.NewReader(bytes.NewReader(archive))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, hdr.Name)
	}
}

func TestBackupDuringPublish(t *testing.T) {
	in := newInstance(t)
	pages := map[string]map[string]string{}
	for i := range 5 {
		slug := fmt.Sprintf("page-%d", i)
		pages[slug] = map[string]string{
			"index.html":     fmt.Sprintf("<h1>page %d</h1>", i),
			"assets/app.css": fmt.Sprintf("body { --n: %d }", i),
			"shared.txt":     "the same in every page",
		}
		in.publish(t, slug, pages[slug])
	}
	// A blob left half written by a crash is not part of a backup.
	if err := os.WriteFile(filepath.Join(in.dir, "blobs", ".put-123"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := in.tryPublish(fmt.Sprintf("concurrent-%d", i), map[string]string{"index.html": fmt.Sprintf("concurrent %d", i)}); err != nil {
				t.Errorf("concurrent publish: %v", err)
				return
			}
		}
	})

	svc := in.service(backup.Options{})
	var buf bytes.Buffer
	sum, err := svc.Write(context.Background(), &hookWriter{w: &buf, hook: func() {
		in.publish(t, "late", map[string]string{"index.html": "published after the copy"})
	}}, backup.WriteOptions{})
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if got := names(t, buf.Bytes()); len(got) != sum.Files || got[0] != "kazari.db" ||
		slices.ContainsFunc(got, func(n string) bool { return strings.Contains(n, ".put-") }) || !slices.IsSorted(got[1:]) {
		t.Errorf("archive of %d files = %v", sum.Files, got)
	}
	if left, _ := filepath.Glob(filepath.Join(in.dir, "tmp", "*")); len(left) != 0 {
		t.Errorf("left in tmp: %v", left)
	}

	dir := filepath.Join(t.TempDir(), "restored")
	if err := backup.Extract(bytes.NewReader(buf.Bytes()), dir); err != nil {
		t.Fatal(err)
	}
	restored := open(t, dir)
	for slug, files := range pages {
		for p, want := range files {
			if got := restored.read(t, slug, p); got != want {
				t.Errorf("%s/%s = %q, want %q", slug, p, got, want)
			}
		}
	}
	// Whatever the copy lists must be readable in full: a page published
	// while the backup ran is either absent or complete.
	page, err := restored.arts.List(context.Background(), ana, artifact.ListQuery{All: true, Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) < len(pages) {
		t.Errorf("restored %d artifacts, want at least %d", len(page.Items), len(pages))
	}
	for _, a := range page.Items {
		if a.Slug == "late" {
			t.Error("the copy holds an artifact published after it was taken")
		}
		if got := restored.read(t, a.Slug, "index.html"); got == "" {
			t.Errorf("%s/index.html is empty", a.Slug)
		}
	}
	if fi, err := os.Stat(filepath.Join(dir, "kazari.db")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("restored database: %v, %v", fi, err)
	}
}

func TestOneBackupAtATime(t *testing.T) {
	in := newInstance(t)
	in.publish(t, "page", map[string]string{"index.html": "hello"})
	svc := in.service(backup.Options{})

	var second error
	held := true
	_, err := svc.Write(context.Background(), &hookWriter{w: io.Discard, hook: func() {
		_, second = svc.Write(context.Background(), io.Discard, backup.WriteOptions{})
		_, held = svc.HoldGC()
	}}, backup.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(second, backup.ErrRunning) {
		t.Errorf("second backup while one runs = %v, want ErrRunning", second)
	}
	if held {
		t.Error("the garbage collector got its hold while a backup ran")
	}
	if _, err := svc.Write(context.Background(), io.Discard, backup.WriteOptions{}); err != nil {
		t.Errorf("backup after the first finished: %v", err)
	}
}

func TestBackupWaitsForGC(t *testing.T) {
	in := newInstance(t)
	in.publish(t, "page", map[string]string{"index.html": "hello"})
	svc := in.service(backup.Options{})

	release, ok := svc.HoldGC()
	if !ok {
		t.Fatal("HoldGC on an idle service refused")
	}
	if _, again := svc.HoldGC(); again {
		t.Error("two collections held at once")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var buf bytes.Buffer
	if _, err := svc.Write(ctx, &buf, backup.WriteOptions{}); !errors.Is(err, context.Canceled) || buf.Len() != 0 {
		t.Errorf("backup during a collection = %v with %d bytes written, want it to wait", err, buf.Len())
	}

	done := make(chan error, 1)
	go func() {
		_, err := svc.Write(context.Background(), &buf, backup.WriteOptions{})
		done <- err
	}()
	release()
	if err := <-done; err != nil || buf.Len() == 0 {
		t.Errorf("backup after the collection = %v with %d bytes", err, buf.Len())
	}
	if release, ok := svc.HoldGC(); !ok {
		t.Error("HoldGC refused after the backup finished")
	} else {
		release()
	}
}

func TestCertificatesOnlyWhenAsked(t *testing.T) {
	in := newInstance(t)
	in.publish(t, "page", map[string]string{"index.html": "hello"})
	key := filepath.Join(in.dir, "certmagic", "certificates", "acme", "wildcard_.art.example.com", "wildcard_.art.example.com.key")
	if err := os.MkdirAll(filepath.Dir(key), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte("PRIVATE KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"uploads", "tmp"} {
		p := filepath.Join(in.dir, dir, "leftover")
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	svc := in.service(backup.Options{})
	archive := func(o backup.WriteOptions) (string, []string) {
		t.Helper()
		var buf bytes.Buffer
		if _, err := svc.Write(context.Background(), &buf, o); err != nil {
			t.Fatal(err)
		}
		return buf.String(), names(t, buf.Bytes())
	}

	body, got := archive(backup.WriteOptions{})
	if strings.Contains(body, "PRIVATE KEY") {
		t.Error("the download holds the certificate's private key")
	}
	for _, n := range got {
		if n != "kazari.db" && !strings.HasPrefix(n, "blobs/") {
			t.Errorf("unexpected entry %q", n)
		}
	}

	body, got = archive(backup.WriteOptions{Certs: true})
	want := "certmagic/certificates/acme/wildcard_.art.example.com/wildcard_.art.example.com.key"
	if !slices.Contains(got, want) || !strings.Contains(body, "PRIVATE KEY") {
		t.Errorf("with certificates: %v", got)
	}
	if slices.ContainsFunc(got, func(n string) bool { return strings.HasPrefix(n, "uploads/") || strings.HasPrefix(n, "tmp/") }) {
		t.Errorf("uploads or tmp in the archive: %v", got)
	}
}

func TestNoSpace(t *testing.T) {
	in := newInstance(t)
	in.publish(t, "page", map[string]string{"index.html": "hello"})
	free := uint64(1 << 40)
	svc := in.service(backup.Options{Free: func(path string) (uint64, error) {
		if path != filepath.Join(in.dir, "tmp") {
			t.Errorf("free space asked for %s", path)
		}
		return free, nil
	}})

	est, err := svc.Estimate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if est.DatabaseBytes == 0 || est.BlobBytes != int64(len("hello")) || est.FreeBytes != free || est.Size() != est.DatabaseBytes+est.BlobBytes {
		t.Errorf("Estimate = %+v", est)
	}

	free = uint64(est.DatabaseBytes) - 1
	var buf bytes.Buffer
	if _, err := svc.Write(context.Background(), &buf, backup.WriteOptions{}); !errors.Is(err, backup.ErrNoSpace) || buf.Len() != 0 {
		t.Errorf("backup without room = %v with %d bytes written, want ErrNoSpace", err, buf.Len())
	}
	free = uint64(est.DatabaseBytes)
	if _, err := svc.Write(context.Background(), &buf, backup.WriteOptions{}); err != nil {
		t.Errorf("backup with exactly enough room: %v", err)
	}
}

func TestRemoveStale(t *testing.T) {
	dir := t.TempDir()
	old, fresh, other := filepath.Join(dir, "backup-0011.db"), filepath.Join(dir, "backup-0022.db"), filepath.Join(dir, "fetch-1")
	for _, p := range []string{old, fresh, other} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(old, start, start); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(fresh, start.Add(2*time.Hour), start.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	n, err := backup.RemoveStale(dir, start.Add(time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("RemoveStale = %d, %v; want 1", n, err)
	}
	for p, want := range map[string]bool{old: false, fresh: true, other: true} {
		if _, err := os.Stat(p); (err == nil) != want {
			t.Errorf("%s exists = %v, want %v", filepath.Base(p), err == nil, want)
		}
	}
	if n, err := backup.RemoveStale(filepath.Join(dir, "missing"), start); n != 0 || err != nil {
		t.Errorf("RemoveStale of a missing directory = %d, %v", n, err)
	}
}

func TestFileName(t *testing.T) {
	at := time.Date(2026, 11, 21, 13, 4, 5, 0, time.FixedZone("CET", 3600))
	tests := []struct {
		name, instance, want string
	}{
		{"a host name", "art.example.com", "kazari-art.example.com-20261121-120405.tar"},
		{"upper case", "Art.Test", "kazari-art.test-20261121-120405.tar"},
		{"quotes and slashes", `a"b/../c`, "kazari-ab..c-20261121-120405.tar"},
		{"no name", "", "kazari-20261121-120405.tar"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := backup.FileName(tt.instance, at); got != tt.want {
				t.Errorf("FileName(%q) = %q, want %q", tt.instance, got, tt.want)
			}
		})
	}
}
