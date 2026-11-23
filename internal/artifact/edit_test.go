package artifact_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/sqlite"
	"github.com/PeterBooker/kazari/internal/textedit"
)

const page = `<html>
  <body>
    <h1>Q3 report</h1>
    <p>Revenue grew.</p>
    <p>Costs fell.</p>
  </body>
</html>
`

func TestServiceEdit(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice := e.user(t, "alice")
	e.create(t, alice, "demo", "index.html", page, "app.js", "run()", "logo.png", "\x89PNG\x00")

	a, v, outs, err := e.svc.Edit(e.ctx, alice, artifact.EditRequest{
		Slug: "demo", Path: "index.html", Label: "q4",
		Edits: []textedit.Edit{{OldText: "Q3", NewText: "Q4"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.Number != 2 || a.LatestVersion != 2 || a.PublishedVersion != 2 || v.Label != "q4" || outs[0].StartLine != 3 {
		t.Fatalf("edit = artifact %+v version %+v outcomes %+v", a, v, outs)
	}
	if got := e.read(t, alice, "demo", "index.html", 2); got != strings.Replace(page, "Q3", "Q4", 1) {
		t.Errorf("v2 index.html = %q", got)
	}
	if got := e.read(t, alice, "demo", "app.js", 2); got != "run()" {
		t.Errorf("v2 app.js = %q, want it carried over", got)
	}

	cases := []struct {
		name string
		req  artifact.EditRequest
		want *artifact.Error
	}{
		{"stale expected version", artifact.EditRequest{Slug: "demo", Path: "index.html", ExpectedVersion: 1,
			Edits: []textedit.Edit{{OldText: "Q4", NewText: "Q1"}}}, artifact.ErrVersionConflict},
		{"binary file", artifact.EditRequest{Slug: "demo", Path: "logo.png",
			Edits: []textedit.Edit{{OldText: "PNG", NewText: "GIF"}}}, artifact.ErrInvalidArgument},
		{"missing file", artifact.EditRequest{Slug: "demo", Path: "nope.html",
			Edits: []textedit.Edit{{OldText: "a", NewText: "b"}}}, artifact.ErrFileNotFound},
		{"bad path", artifact.EditRequest{Slug: "demo", Path: "../x",
			Edits: []textedit.Edit{{OldText: "a", NewText: "b"}}}, artifact.ErrInvalidPath},
		{"missing artifact", artifact.EditRequest{Slug: "nope", Path: "index.html",
			Edits: []textedit.Edit{{OldText: "a", NewText: "b"}}}, artifact.ErrNotFound},
		{"text not found", artifact.EditRequest{Slug: "demo", Path: "index.html",
			Edits: []textedit.Edit{{OldText: "Q3", NewText: "Q1"}}}, textedit.ErrNotFound},
		{"unchanged", artifact.EditRequest{Slug: "demo", Path: "app.js",
			Edits: []textedit.Edit{{OldText: "run()", NewText: "x"}, {OldText: "x", NewText: "run()"}}}, artifact.ErrInvalidArgument},
	}
	for _, c := range cases {
		if _, _, _, err := e.svc.Edit(e.ctx, alice, c.req); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %s", c.name, err, c.want.Code)
		}
	}

	if _, err := e.svc.Publish(e.ctx, alice, "demo", 1); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := e.svc.Edit(e.ctx, alice, artifact.EditRequest{Slug: "demo", Path: "index.html", ExpectedVersion: 2,
		Edits: []textedit.Edit{{OldText: "Q4", NewText: "Q1"}}}); !errors.Is(err, artifact.ErrPublishedNotLatest) {
		t.Fatalf("edit after rollback without a base: err = %v, want published_not_latest", err)
	}
	_, v, _, err = e.svc.Edit(e.ctx, alice, artifact.EditRequest{Slug: "demo", Path: "index.html", ExpectedVersion: 2, BaseVersion: 2,
		Edits: []textedit.Edit{{OldText: "Q4", NewText: "Q1"}}})
	if err != nil || v.Number != 3 {
		t.Fatalf("edit after rollback on the latest = %v, %v", v, err)
	}
	if v.Label != "edit index.html (1 edit)" {
		t.Errorf("edit label = %q, want %q", v.Label, "edit index.html (1 edit)")
	}

	if err := e.svc.Delete(e.ctx, alice, "demo", false); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := e.svc.Edit(e.ctx, alice, artifact.EditRequest{Slug: "demo", Path: "index.html",
		Edits: []textedit.Edit{{OldText: "Q1", NewText: "Q2"}}}); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("edit of a deleted artifact: err = %v, want not_found", err)
	}
}

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

func (c *countingStore) Files(ctx context.Context, versionID int64) ([]artifact.File, error) {
	c.count("Files")
	return c.ArtifactStore.Files(ctx, versionID)
}

func TestEditStoreCalls(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	bob := e.user(t, "bob")
	e.create(t, bob, "demo", "index.html", page, "app.js", "run()")
	cs := &countingStore{ArtifactStore: e.store, calls: map[string]int{}}
	svc := artifact.New(cs, cs, e.blobs, artifact.NoQuotas{}, e.rec, artifact.Options{Now: e.clock.Now})
	if _, _, _, err := svc.Edit(e.ctx, bob, artifact.EditRequest{Slug: "demo", Path: "index.html",
		Edits: []textedit.Edit{{OldText: "Q3", NewText: "Q4"}}}); err != nil {
		t.Fatal(err)
	}
	// One read each of the row, the actor and the base before the
	// transaction, the base again under the blob hold, the reload after.
	want := map[string]int{"Get": 2, "Involved": 1, "GetVersion": 2, "AddVersion": 1, "RecordBlob": 1}
	if diff := cmp.Diff(want, cs.calls); diff != "" {
		t.Errorf("store calls of one Edit (-want +got):\n%s", diff)
	}
}
