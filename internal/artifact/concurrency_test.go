package artifact_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/blob"
	"github.com/PeterBooker/kazari/internal/sqlite"
)

type parkBlobs struct {
	*blob.FS
	once    sync.Once
	parked  chan struct{}
	release chan struct{}
}

func (p *parkBlobs) Put(ctx context.Context, r io.Reader) (string, int64, error) {
	hash, size, err := p.FS.Put(ctx, r)
	p.once.Do(func() {
		close(p.parked)
		<-p.release
	})
	return hash, size, err
}

func TestCollectGarbageSparesBlobOfPublishInProgress(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice, bob := e.user(t, "alice"), e.user(t, "bob")
	const content = "<h1>shared content</h1>"
	e.create(t, alice, "old-page", "index.html", content)
	if err := e.svc.Delete(e.ctx, alice, "old-page", true); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(2 * time.Hour)

	pb := &parkBlobs{FS: e.blobs, parked: make(chan struct{}), release: make(chan struct{})}
	svc := artifact.New(e.store, e.store, pb, artifact.NoQuotas{}, e.rec, artifact.Options{Now: e.clock.Now})
	created := make(chan error, 1)
	go func() {
		_, _, err := svc.Create(e.ctx, bob, artifact.CreateRequest{Slug: "new-page", Files: files("index.html", content)})
		created <- err
	}()
	<-pb.parked

	type result struct {
		n   int
		err error
	}
	collected := make(chan result, 1)
	go func() {
		n, err := svc.CollectGarbage(e.ctx)
		collected <- result{n, err}
	}()
	// Release the publish once the collector waits for it or has finished
	// without waiting.
	var gc *result
	for gc == nil && !artifact.GCWaiting(svc) {
		select {
		case r := <-collected:
			gc = &r
		default:
			runtime.Gosched()
		}
	}
	close(pb.release)
	if err := <-created; err != nil {
		t.Fatalf("Create: %v", err)
	}
	if gc == nil {
		r := <-collected
		gc = &r
	}
	if gc.err != nil || gc.n != 0 {
		t.Fatalf("CollectGarbage = %d, %v; want 0 deleted, the blob is referenced again", gc.n, gc.err)
	}
	if body := e.read(t, bob, "new-page", "index.html", 0); body != content {
		t.Fatalf("new-page index.html = %q", body)
	}
}

// rowsHook runs before the collector deletes a batch's rows.
type rowsHook struct {
	*sqlite.ArtifactStore
	before func()
}

func (r *rowsHook) DeleteBlobs(ctx context.Context, hashes []string, before time.Time) (int, error) {
	r.before()
	return r.ArtifactStore.DeleteBlobs(ctx, hashes, before)
}

// The collector lets publishes in before it deletes a batch's rows, and a
// publish of collected content then keeps its blob, file and row.
func TestCollectGarbageRowsAfterLock(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice := e.user(t, "alice")
	const content = "<h1>collected, then published again</h1>"
	e.create(t, alice, "old-page", "index.html", content)
	if err := e.svc.Delete(e.ctx, alice, "old-page", true); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(2 * time.Hour)

	hook := &rowsHook{ArtifactStore: e.store}
	svc := artifact.New(e.store, hook, e.blobs, artifact.NoQuotas{}, e.rec, artifact.Options{Now: e.clock.Now})
	hook.before = func() {
		if artifact.GCWaiting(svc) {
			t.Error("DeleteBlobs runs under the write side of blobMu")
			return
		}
		if _, _, err := svc.Create(e.ctx, alice, artifact.CreateRequest{Slug: "new-page", Files: files("index.html", content)}); err != nil {
			t.Errorf("Create between the files and the rows: %v", err)
		}
	}
	n, err := svc.CollectGarbage(e.ctx)
	if err != nil || n != 0 {
		t.Fatalf("CollectGarbage = %d, %v; want 0 rows deleted, the blob is referenced again", n, err)
	}
	if body := e.read(t, alice, "new-page", "index.html", 0); body != content {
		t.Fatalf("new-page index.html = %q", body)
	}
	if referenced, err := e.store.BlobReferenced(e.ctx, sha(content)); err != nil || !referenced {
		t.Fatalf("BlobReferenced = %v, %v; want true", referenced, err)
	}
	totals, err := e.store.BlobTotals(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if totals.Count != 1 {
		t.Errorf("BlobTotals.Count = %d, want 1: the row of the published blob", totals.Count)
	}
}

// A publish records its blobs in batches as it stores them, so a crash
// leaves rows the collector finds for all but the last batch.
func TestPublishRecordsBlobsInBatches(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice := e.user(t, "alice")
	cs := &countingStore{ArtifactStore: e.store, calls: map[string]int{}}
	svc := artifact.New(cs, cs, e.blobs, artifact.NoQuotas{}, e.rec, artifact.Options{Now: e.clock.Now})

	const stored = 150
	in := make([]artifact.FileInput, 0, stored+1)
	for i := range stored {
		in = append(in, artifact.FileInput{Path: fmt.Sprintf("f/%03d.txt", i), Content: strings.NewReader(fmt.Sprintf("file %d", i))})
	}
	br := &blockingReader{data: strings.NewReader("tail"), reached: make(chan struct{}), release: make(chan struct{})}
	in = append(in, artifact.FileInput{Path: "tail.txt", Content: br})
	created := make(chan error, 1)
	go func() {
		_, _, err := svc.Create(e.ctx, alice, artifact.CreateRequest{Slug: "many", Files: in})
		created <- err
	}()
	<-br.reached
	totals, err := e.store.BlobTotals(e.ctx)
	close(br.release)
	if err != nil {
		t.Fatal(err)
	}
	if totals.Count != 100 {
		t.Errorf("blob rows while the publish is parked on file %d = %d, want 100, the first batch", stored+1, totals.Count)
	}
	if err := <-created; err != nil {
		t.Fatalf("Create: %v", err)
	}
	if totals, err = e.store.BlobTotals(e.ctx); err != nil || totals.Count != stored+1 {
		t.Errorf("blob rows after the publish = %d, %v; want %d", totals.Count, err, stored+1)
	}
	if got := cs.calls["RecordBlobs"]; got != 2 {
		t.Errorf("RecordBlobs calls for %d files = %d, want 2", stored+1, got)
	}
}

func TestFailedPublishSparesBlobOfPublishInProgress(t *testing.T) {
	e := newEnv(t, artifact.Options{MaxFileBytes: 100})
	alice, bob := e.user(t, "alice"), e.user(t, "bob")
	const content = "<h1>shared content</h1>"

	// Parked on its second file: the first is stored but not yet referenced.
	br := &blockingReader{data: strings.NewReader("tail"), reached: make(chan struct{}), release: make(chan struct{})}
	created := make(chan error, 1)
	go func() {
		_, _, err := e.svc.Create(e.ctx, bob, artifact.CreateRequest{Slug: "new-page", Files: []artifact.FileInput{
			{Path: "index.html", Content: strings.NewReader(content)},
			{Path: "tail.txt", Content: br},
		}})
		created <- err
	}()
	<-br.reached

	failed := make(chan error, 1)
	go func() {
		_, _, err := e.svc.Create(e.ctx, alice, artifact.CreateRequest{
			Slug: "failed", Files: files("index.html", content, "big.txt", strings.Repeat("x", 101)),
		})
		failed <- err
	}()
	// Release the publish once the cleanup waits for it or has finished
	// without waiting.
	var failure error
	for finished := false; !finished && !artifact.GCWaiting(e.svc); {
		select {
		case failure = <-failed:
			finished = true
			failed <- failure
		default:
			runtime.Gosched()
		}
	}
	close(br.release)
	if err := <-created; err != nil {
		t.Fatalf("Create: %v", err)
	}
	wantErr(t, "failed create", <-failed, artifact.ErrTooLarge)
	if body := e.read(t, bob, "new-page", "index.html", 0); body != content {
		t.Fatalf("new-page index.html = %q", body)
	}
}

type blockingReader struct {
	data    *strings.Reader
	reached chan struct{}
	release chan struct{}
}

func (b *blockingReader) Read(p []byte) (int, error) {
	n, err := b.data.Read(p)
	if errors.Is(err, io.EOF) {
		close(b.reached)
		<-b.release
	}
	return n, err
}

func TestQuotaHoldsAcrossConcurrentPublishes(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	e.user(t, "alice")
	alice := artifact.Actor{Handle: "alice", QuotaBytes: 100 + 2*artifact.FileChargeBytes}
	body := strings.Repeat("x", 60)

	br := &blockingReader{data: strings.NewReader(body), reached: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, _, err := e.svc.Create(e.ctx, alice, artifact.CreateRequest{Slug: "one-page",
			Files: []artifact.FileInput{{Path: "index.html", Content: br}}})
		done <- err
	}()
	<-br.reached
	e.create(t, alice, "two-page", "index.html", body)
	close(br.release)
	wantErr(t, "second publish over quota", <-done, artifact.ErrQuotaExceeded)

	used, err := e.svc.Usage(e.ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if used > alice.QuotaBytes {
		t.Fatalf("usage %d exceeds quota %d", used, alice.QuotaBytes)
	}
	_, err = e.store.Get(e.ctx, "one-page")
	wantErr(t, "refused publish", err, artifact.ErrNotFound)

	// 30 bytes fit in the 40 left until another publish takes 30.
	br = &blockingReader{data: strings.NewReader(strings.Repeat("y", 30)), reached: make(chan struct{}), release: make(chan struct{})}
	go func() {
		_, _, err := e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "two-page",
			Files: []artifact.FileInput{del("index.html"), {Path: "more.txt", Content: br}}})
		done <- err
	}()
	<-br.reached
	e.create(t, alice, "three-page", "index.html", strings.Repeat("z", 30))
	close(br.release)
	wantErr(t, "new version over quota", <-done, artifact.ErrQuotaExceeded)
	if a, _, err := e.svc.Get(e.ctx, alice, "two-page"); err != nil || a.LatestVersion != 1 {
		t.Fatalf("two-page after refused version: %+v, %v", a, err)
	}
}

// hookStore calls before once, between the service's checks and its write.
type hookStore struct {
	*sqlite.ArtifactStore
	before func()
}

func (h *hookStore) fire() {
	if f := h.before; f != nil {
		h.before = nil
		f()
	}
}

func (h *hookStore) Update(ctx context.Context, slug, by string, fn func(*artifact.Artifact) error) error {
	h.fire()
	return h.ArtifactStore.Update(ctx, slug, by, fn)
}

func (h *hookStore) AddVersion(ctx context.Context, slug string, v *artifact.Version, expected int, quota int64,
	fn func(*artifact.Artifact) error) error {
	h.fire()
	return h.ArtifactStore.AddVersion(ctx, slug, v, expected, quota, fn)
}

func (h *hookStore) Purge(ctx context.Context, slug string, at time.Time, fn func(*artifact.Artifact) error) error {
	h.fire()
	return h.ArtifactStore.Purge(ctx, slug, at, fn)
}

func TestUpdateKeepsConcurrentChanges(t *testing.T) {
	root := artifact.Actor{Handle: "root", Admin: true}
	tests := []struct {
		name    string
		landing func(e *env, alice artifact.Actor) error // the concurrent request
		update  artifact.UpdateRequest
		wantErr error
		check   func(a *artifact.Artifact) error
	}{
		{
			name: "rename keeps a transfer",
			landing: func(e *env, _ artifact.Actor) error {
				_, err := e.svc.Transfer(e.ctx, root, "demo", "bob")
				return err
			},
			update: artifact.UpdateRequest{Slug: "demo", Name: new("renamed")},
			check: func(a *artifact.Artifact) error {
				if a.OwnerHandle != "bob" || a.Name != "renamed" {
					return fmt.Errorf("owner %q name %q, want bob and renamed", a.OwnerHandle, a.Name)
				}
				return nil
			},
		},
		{
			name: "new version keeps a transfer",
			landing: func(e *env, _ artifact.Actor) error {
				_, err := e.svc.Transfer(e.ctx, root, "demo", "bob")
				return err
			},
			update: artifact.UpdateRequest{Slug: "demo", Files: files("v2.txt", "two")},
			check: func(a *artifact.Artifact) error {
				if a.OwnerHandle != "bob" || a.LatestVersion != 2 {
					return fmt.Errorf("owner %q latest %d, want bob and 2", a.OwnerHandle, a.LatestVersion)
				}
				return nil
			},
		},
		{
			name:    "rename does not undo a delete",
			landing: func(e *env, alice artifact.Actor) error { return e.svc.Delete(e.ctx, alice, "demo", false) },
			update:  artifact.UpdateRequest{Slug: "demo", Name: new("renamed")},
			wantErr: artifact.ErrNotFound,
			check: func(a *artifact.Artifact) error {
				if a.DeletedAt == nil || a.Name != "" {
					return fmt.Errorf("deleted %v name %q, want still deleted and unnamed", a.DeletedAt, a.Name)
				}
				return nil
			},
		},
		{
			name: "new version does not undo an unpublish",
			landing: func(e *env, alice artifact.Actor) error {
				_, err := e.svc.SetVisibility(e.ctx, alice, "demo", artifact.Unpublished)
				return err
			},
			update: artifact.UpdateRequest{Slug: "demo", Files: files("v2.txt", "two")},
			check: func(a *artifact.Artifact) error {
				if a.UnpublishedAt == nil || a.LatestVersion != 2 {
					return fmt.Errorf("unpublished %v latest %d, want still unpublished and 2", a.UnpublishedAt, a.LatestVersion)
				}
				return nil
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, artifact.Options{})
			alice := e.user(t, "alice")
			e.user(t, "bob")
			e.user(t, "root")
			e.create(t, alice, "demo")
			hs := &hookStore{ArtifactStore: e.store}
			svc := artifact.New(hs, hs, e.blobs, artifact.NoQuotas{}, e.rec, artifact.Options{Now: e.clock.Now})
			hs.before = func() {
				if err := tt.landing(e, alice); err != nil {
					t.Errorf("concurrent request: %v", err)
				}
			}
			_, _, err := svc.Update(e.ctx, alice, tt.update)
			if tt.wantErr != nil {
				wantErr(t, "Update", err, tt.wantErr)
			} else if err != nil {
				t.Fatalf("Update: %v", err)
			}
			a, err := e.store.Get(e.ctx, "demo")
			if err != nil {
				t.Fatal(err)
			}
			if err := tt.check(a); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestSweepSparesArtifactRestoredSinceListing(t *testing.T) {
	e := newEnv(t, artifact.Options{Retention: time.Hour})
	alice := e.user(t, "alice")
	e.create(t, alice, "demo")
	if err := e.svc.Delete(e.ctx, alice, "demo", false); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(2 * time.Hour)
	hs := &hookStore{ArtifactStore: e.store}
	svc := artifact.New(hs, hs, e.blobs, artifact.NoQuotas{}, e.rec, artifact.Options{Retention: time.Hour, Now: e.clock.Now})
	hs.before = func() {
		if _, _, err := e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "demo", Restore: true}); err != nil {
			t.Errorf("restore: %v", err)
		}
	}
	rep, err := svc.Sweep(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Purged != 0 {
		t.Fatalf("Sweep = %+v, want nothing purged", rep)
	}
	a, err := e.store.Get(e.ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if a.PurgedAt != nil || a.DeletedAt != nil || a.LatestVersion != 1 {
		t.Fatalf("after sweep: %+v, want restored with its version", a)
	}
}

func TestSweepSkipsArtifactChangedSinceListing(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice := e.user(t, "alice")
	e.create(t, alice, "demo")
	if _, err := e.svc.MarkStale(e.ctx, alice, "demo", time.Hour); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(2 * time.Hour)
	hs := &hookStore{ArtifactStore: e.store}
	svc := artifact.New(hs, hs, e.blobs, artifact.NoQuotas{}, e.rec, artifact.Options{Now: e.clock.Now})
	hs.before = func() {
		if _, err := e.svc.UnmarkStale(e.ctx, alice, "demo"); err != nil {
			t.Errorf("UnmarkStale: %v", err)
		}
	}
	rep, err := svc.Sweep(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.SoftDeleted != 0 {
		t.Fatalf("Sweep = %+v, want nothing soft-deleted", rep)
	}
	a, err := e.store.Get(e.ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if a.DeletedAt != nil || a.StaleAt != nil {
		t.Fatalf("after sweep: deleted %v stale %v, want live and not stale", a.DeletedAt, a.StaleAt)
	}
}

// getVersionHook calls before once, as the store is asked for a version for
// the nth time.
type getVersionHook struct {
	*sqlite.ArtifactStore
	n      int
	before func()
}

func (h *getVersionHook) GetVersion(ctx context.Context, slug string, number int) (*artifact.Version, error) {
	if h.n--; h.n == 0 {
		h.before()
	}
	return h.ArtifactStore.GetVersion(ctx, slug, number)
}

// A version built on a base that is deleted, and its blobs collected, after
// the base was read would hold files whose content is gone.
func TestUpdateOnBaseDeletedMeanwhile(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice := e.user(t, "alice")
	e.create(t, alice, "demo", "index.html", "v1", "old.css", "only in version 1")
	if _, _, err := e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "demo",
		Files: []artifact.FileInput{files("index.html", "v2")[0], del("old.css")}}); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(2 * time.Hour) // past the collector's grace for version 1's blobs
	hs := &getVersionHook{ArtifactStore: e.store, n: 2}
	svc := artifact.New(hs, hs, e.blobs, artifact.NoQuotas{}, e.rec, artifact.Options{Now: e.clock.Now})
	// The base is read first, the latest second.
	hs.before = func() {
		if _, err := svc.DeleteVersion(e.ctx, alice, "demo", 1); err != nil {
			t.Errorf("delete version 1: %v", err)
		}
		if n, err := svc.CollectGarbage(e.ctx); err != nil || n == 0 {
			t.Errorf("CollectGarbage = %d, %v; want version 1's blobs collected", n, err)
		}
	}
	_, _, err := svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "demo", BaseVersion: 1, Files: files("new.txt", "new")})
	wantErr(t, "Update on a base deleted meanwhile", err, artifact.ErrVersionNotFound)
	if a, err := e.store.Get(e.ctx, "demo"); err != nil || a.LatestVersion != 2 {
		t.Errorf("after the refused update: %+v, %v; want latest version 2", a, err)
	}
}
