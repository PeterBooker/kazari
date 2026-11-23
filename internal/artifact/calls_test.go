package artifact_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/sqlite"
)

// countingStore counts the store calls a service call makes.
type countingStore struct {
	*sqlite.ArtifactStore
	mu    sync.Mutex
	calls map[string]int
}

func (c *countingStore) count(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls[name]++
}

func (c *countingStore) Get(ctx context.Context, slug string) (*artifact.Artifact, error) {
	c.count("Get")
	return c.ArtifactStore.Get(ctx, slug)
}

func (c *countingStore) GetVersion(ctx context.Context, slug string, number int) (*artifact.Version, error) {
	c.count("GetVersion")
	return c.ArtifactStore.GetVersion(ctx, slug, number)
}

func (c *countingStore) Involved(ctx context.Context, slug, handle string) (bool, error) {
	c.count("Involved")
	return c.ArtifactStore.Involved(ctx, slug, handle)
}

func (c *countingStore) Usage(ctx context.Context, owner string) (int64, error) {
	c.count("Usage")
	return c.ArtifactStore.Usage(ctx, owner)
}

func (c *countingStore) AddVersion(ctx context.Context, slug string, v *artifact.Version, expected int, quota int64,
	fn func(a *artifact.Artifact) error) error {
	c.count("AddVersion")
	return c.ArtifactStore.AddVersion(ctx, slug, v, expected, quota, fn)
}

func (c *countingStore) Update(ctx context.Context, slug, by string, fn func(a *artifact.Artifact) error) error {
	c.count("Update")
	return c.ArtifactStore.Update(ctx, slug, by, fn)
}

func (c *countingStore) RecordBlob(ctx context.Context, hash string, size int64, at time.Time) error {
	c.count("RecordBlob")
	return c.ArtifactStore.RecordBlob(ctx, hash, size, at)
}

func (c *countingStore) Files(ctx context.Context, versionID int64) (map[string]artifact.File, error) {
	c.count("Files")
	return c.ArtifactStore.Files(ctx, versionID)
}

func TestEditStoreCalls(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	bob := e.user(t, "bob")
	e.create(t, bob, "demo", "index.html", page, "app.js", "run()")
	cs := &countingStore{ArtifactStore: e.store, calls: map[string]int{}}
	svc := artifact.New(cs, e.blobs, artifact.NoQuotas{}, e.rec, artifact.Options{Now: e.clock.Now})
	if _, _, _, err := svc.Edit(e.ctx, bob, artifact.EditRequest{Slug: "demo", Path: "index.html",
		Edits: []artifact.Edit{{OldText: "Q3", NewText: "Q4"}}}); err != nil {
		t.Fatal(err)
	}
	// One read each of the row, the actor and the base before the
	// transaction, the base again under the blob hold, the reload after.
	want := map[string]int{"Get": 2, "Involved": 1, "GetVersion": 2, "AddVersion": 1, "RecordBlob": 1}
	if diff := cmp.Diff(want, cs.calls); diff != "" {
		t.Errorf("store calls of one Edit (-want +got):\n%s", diff)
	}
}
