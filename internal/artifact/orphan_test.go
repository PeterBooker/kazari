package artifact_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/blob"
	"github.com/PeterBooker/kazari/internal/sqlite"
)

// orphan stores content with no row, as a publish that crashed before
// recording it leaves it, last written age before the clock.
func (e *env) orphan(t *testing.T, content string, age time.Duration) string {
	t.Helper()
	hash, _, err := e.blobs.Put(e.ctx, strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	e.touch(t, hash, age)
	return hash
}

// touch sets the blob file's modification time to age before the clock.
func (e *env) touch(t *testing.T, hash string, age time.Duration) {
	t.Helper()
	if err := e.age(hash, age); err != nil {
		t.Fatal(err)
	}
}

func (e *env) age(hash string, age time.Duration) error {
	mtime := e.clock.Now().Add(-age)
	return os.Chtimes(filepath.Join(e.blobDir, hash[:2], hash), mtime, mtime)
}

func (e *env) stored(t *testing.T, hash string) bool {
	t.Helper()
	_, err := e.blobs.Stat(e.ctx, hash)
	if err != nil && !errors.Is(err, blob.ErrNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

const longAgo = artifact.DefaultOrphanAge + time.Hour

func TestSweepOrphansRemovesOldUnrecordedFiles(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	gone := e.orphan(t, "left by a crashed publish", longAgo)
	recent := e.orphan(t, "written an hour ago", time.Hour)

	rep, err := e.svc.SweepOrphans(e.ctx)
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if want := (artifact.OrphanReport{Ran: true, Removed: 1, Bytes: int64(len("left by a crashed publish"))}); rep != want {
		t.Errorf("SweepOrphans = %+v, want %+v", rep, want)
	}
	if e.stored(t, gone) {
		t.Error("the old file no row records is still stored")
	}
	if !e.stored(t, recent) {
		t.Error("the recent file no row records was removed")
	}
}

func TestSweepOrphansKeepsRecordedFiles(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice := e.user(t, "alice")
	const live, purged = "<h1>live</h1>", "<h1>purged</h1>"
	e.create(t, alice, "live", "index.html", live)
	e.create(t, alice, "purged", "index.html", purged)
	if err := e.svc.Delete(e.ctx, alice, "purged", true); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(longAgo)
	e.touch(t, sha(live), longAgo)
	e.touch(t, sha(purged), longAgo)

	rep, err := e.svc.SweepOrphans(e.ctx)
	if err != nil || rep.Removed != 0 {
		t.Fatalf("SweepOrphans = %+v, %v; want nothing removed: both files have rows", rep, err)
	}
	if !e.stored(t, sha(live)) || !e.stored(t, sha(purged)) {
		t.Fatal("the sweep removed a file with a row")
	}
	if n, err := e.svc.CollectGarbage(e.ctx); err != nil || n != 1 {
		t.Fatalf("CollectGarbage = %d, %v; want the purged page's blob", n, err)
	}
	if e.stored(t, sha(purged)) {
		t.Error("CollectGarbage left the unreferenced blob")
	}
	if body := e.read(t, alice, "live", "index.html", 0); body != live {
		t.Errorf("live index.html = %q, want %q", body, live)
	}
}

func TestSweepOrphansLeavesOtherFiles(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	hash := e.orphan(t, "a blob", longAgo)
	others := []string{
		".put-123456",
		"README",
		filepath.Join(hash[:2], ".put-789"),
		filepath.Join(hash[:2], "notes.txt"),
		filepath.Join(hash[:2], strings.ToUpper(hash)),
		filepath.Join("lost", sha("misplaced")),
	}
	mtime := e.clock.Now().Add(-longAgo)
	for _, name := range others {
		p := filepath.Join(e.blobDir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("not a blob"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}

	if rep, err := e.svc.SweepOrphans(e.ctx); err != nil || rep.Removed != 1 {
		t.Fatalf("SweepOrphans = %+v, %v; want the one blob removed", rep, err)
	}
	for _, name := range others {
		if _, err := os.Stat(filepath.Join(e.blobDir, name)); err != nil {
			t.Errorf("%s after the sweep: %v", name, err)
		}
	}
}

func TestSweepOrphansOncePerInterval(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	first := e.orphan(t, "first", longAgo)
	if rep, err := e.svc.SweepOrphans(e.ctx); err != nil || !rep.Ran || rep.Removed != 1 {
		t.Fatalf("first SweepOrphans = %+v, %v; want a run that removes one file", rep, err)
	}
	if e.stored(t, first) {
		t.Fatal("first orphan still stored")
	}

	second := e.orphan(t, "second", longAgo+artifact.OrphanSweepInterval)
	for _, wait := range []time.Duration{0, artifact.OrphanSweepInterval - time.Second} {
		e.clock.Advance(wait)
		if rep, err := e.svc.SweepOrphans(e.ctx); err != nil || rep != (artifact.OrphanReport{}) {
			t.Fatalf("SweepOrphans %v after the last = %+v, %v; want no run", wait, rep, err)
		}
	}
	if !e.stored(t, second) {
		t.Fatal("a sweep ran within the interval")
	}
	e.clock.Advance(time.Second)
	if rep, err := e.svc.SweepOrphans(e.ctx); err != nil || !rep.Ran || rep.Removed != 1 {
		t.Fatalf("SweepOrphans a day later = %+v, %v; want a run that removes one file", rep, err)
	}
	if e.stored(t, second) {
		t.Error("second orphan still stored")
	}
}

// agedPut stores like blob.FS, then makes the file old and parks once.
type agedPut struct {
	*blob.FS
	age     func(hash string) error
	once    sync.Once
	parked  chan struct{}
	release chan struct{}
}

func (p *agedPut) Put(ctx context.Context, r io.Reader) (string, int64, error) {
	hash, size, err := p.FS.Put(ctx, r)
	if err == nil {
		err = p.age(hash)
	}
	p.once.Do(func() {
		close(p.parked)
		<-p.release
	})
	return hash, size, err
}

// A publish that has stored a file and not yet recorded it keeps the file,
// however old it looks: the sweep waits for the publish, then finds its row.
func TestSweepOrphansSparesPublishInProgress(t *testing.T) {
	for _, tt := range []struct {
		name   string
		stored bool
	}{
		{"new content", false},
		{"content stored with no row", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, artifact.Options{})
			bob := e.user(t, "bob")
			const content = "<h1>in progress</h1>"
			if tt.stored {
				e.orphan(t, content, longAgo)
			}
			pb := &agedPut{FS: e.blobs, age: func(h string) error { return e.age(h, longAgo) },
				parked: make(chan struct{}), release: make(chan struct{})}
			svc := artifact.New(e.store, e.store, pb, artifact.NoQuotas{}, e.rec, artifact.Options{Now: e.clock.Now})
			created := make(chan error, 1)
			go func() {
				_, _, err := svc.Create(e.ctx, bob, artifact.CreateRequest{Slug: "new-page", Files: files("index.html", content)})
				created <- err
			}()
			<-pb.parked

			type result struct {
				rep artifact.OrphanReport
				err error
			}
			swept := make(chan result, 1)
			go func() {
				rep, err := svc.SweepOrphans(e.ctx)
				swept <- result{rep, err}
			}()
			var sw *result
			for sw == nil && !artifact.GCWaiting(svc) {
				select {
				case r := <-swept:
					sw = &r
				default:
					runtime.Gosched()
				}
			}
			close(pb.release)
			if err := <-created; err != nil {
				t.Fatalf("Create: %v", err)
			}
			if sw == nil {
				r := <-swept
				sw = &r
			}
			if sw.err != nil || sw.rep.Removed != 0 {
				t.Fatalf("SweepOrphans = %+v, %v; want nothing removed", sw.rep, sw.err)
			}
			if body := e.read(t, bob, "new-page", "index.html", 0); body != content {
				t.Fatalf("new-page index.html = %q, want %q", body, content)
			}
		})
	}
}

// knownHook runs after the first KnownBlobs, which the sweep makes before
// it takes the lock.
type knownHook struct {
	*sqlite.ArtifactStore
	once  sync.Once
	after func()
}

func (k *knownHook) KnownBlobs(ctx context.Context, hashes []string) (map[string]bool, error) {
	known, err := k.ArtifactStore.KnownBlobs(ctx, hashes)
	k.once.Do(k.after)
	return known, err
}

// A row recorded between the walk and the lock keeps its file: a publish of
// the same content stored nothing, so the file is its version's.
func TestSweepOrphansRechecksUnderLock(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice := e.user(t, "alice")
	const content = "<h1>published between the walk and the lock</h1>"
	e.orphan(t, content, longAgo)

	hook := &knownHook{ArtifactStore: e.store}
	svc := artifact.New(e.store, hook, e.blobs, artifact.NoQuotas{}, e.rec, artifact.Options{Now: e.clock.Now})
	hook.after = func() {
		if artifact.GCWaiting(svc) {
			t.Error("the first KnownBlobs runs under the write side of blobMu")
			return
		}
		if _, _, err := svc.Create(e.ctx, alice, artifact.CreateRequest{Slug: "new-page", Files: files("index.html", content)}); err != nil {
			t.Errorf("Create between the walk and the lock: %v", err)
		}
	}
	rep, err := svc.SweepOrphans(e.ctx)
	if err != nil || rep.Removed != 0 {
		t.Fatalf("SweepOrphans = %+v, %v; want nothing removed, the blob is recorded again", rep, err)
	}
	if body := e.read(t, alice, "new-page", "index.html", 0); body != content {
		t.Fatalf("new-page index.html = %q, want %q", body, content)
	}
}
