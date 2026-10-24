// Package storetest is a conformance suite for artifact.Store
// implementations. A store passes when Run succeeds against a fresh store
// per subtest.
package storetest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"agalma/internal/artifact"
)

// Run exercises every artifact.Store method. open returns an empty store
// and a hook that creates a user row for a handle, since artifacts need an
// existing owner.
func Run(t *testing.T, open func(t *testing.T) (artifact.Store, func(handle string))) {
	tests := []struct {
		name string
		fn   func(t *testing.T, h *harness)
	}{
		{"CreateGet", testCreateGet},
		{"CreateSlugTaken", testCreateSlugTaken},
		{"CreateUnknownOwner", testCreateUnknownOwner},
		{"GetNotFound", testGetNotFound},
		{"Versions", testVersions},
		{"AddVersion", testAddVersion},
		{"AddVersionConflict", testAddVersionConflict},
		{"AddVersionConcurrent", testAddVersionConcurrent},
		{"Update", testUpdate},
		{"UpdateErrors", testUpdateErrors},
		{"ListFilters", testListFilters},
		{"ListPagination", testListPagination},
		{"ListInvalid", testListInvalid},
		{"Resolve", testResolve},
		{"Files", testFiles},
		{"Usage", testUsage},
		{"LifecycleQueries", testLifecycleQueries},
		{"PurgeAndDeleteTombstone", testPurgeAndDeleteTombstone},
		{"Blobs", testBlobs},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, mkUser := open(t)
			tt.fn(t, &harness{store: store, mkUser: mkUser, ctx: context.Background()})
		})
	}
}

// t0 is the base of every timestamp in the suite.
var t0 = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

func at(d time.Duration) time.Time { return t0.Add(d) }

func ptr(t time.Time) *time.Time { return &t }

// cmpOpts lets a store return nil or empty maps and slices interchangeably.
var cmpOpts = []cmp.Option{cmpopts.EquateEmpty()}

type harness struct {
	store  artifact.Store
	mkUser func(handle string)
	ctx    context.Context
}

func hashOf(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}

func file(p, content string) artifact.File {
	return artifact.File{Path: p, Hash: hashOf(content), Size: int64(len(content)), ContentType: "text/plain; charset=utf-8"}
}

func newArtifact(slug, owner string, created time.Time) *artifact.Artifact {
	return &artifact.Artifact{
		Slug:        slug,
		Name:        "Name of " + slug,
		Description: "About " + slug,
		OwnerHandle: owner,
		Visibility:  artifact.Public,
		Metadata:    map[string]string{},
		CreatedAt:   created,
		UpdatedAt:   created,
	}
}

func newVersion(by string, created time.Time, files ...artifact.File) *artifact.Version {
	v := &artifact.Version{Label: "label", CreatedBy: by, CreatedAt: created, Files: files}
	for _, f := range files {
		v.SizeBytes += f.Size
	}
	return v
}

// create stores a fresh artifact with one file and returns it and its
// version.
func (h *harness) create(t *testing.T, a *artifact.Artifact, files ...artifact.File) *artifact.Version {
	t.Helper()
	if len(files) == 0 {
		files = []artifact.File{file("index.html", "<h1>"+a.Slug+"</h1>")}
	}
	v := newVersion(a.OwnerHandle, a.CreatedAt, files...)
	if err := h.store.Create(h.ctx, a, v); err != nil {
		t.Fatalf("Create(%s): %v", a.Slug, err)
	}
	return v
}

func (h *harness) get(t *testing.T, slug string) *artifact.Artifact {
	t.Helper()
	a, err := h.store.Get(h.ctx, slug)
	if err != nil {
		t.Fatalf("Get(%s): %v", slug, err)
	}
	return a
}

func (h *harness) update(t *testing.T, a *artifact.Artifact, by string) {
	t.Helper()
	if err := h.store.Update(h.ctx, a, by); err != nil {
		t.Fatalf("Update(%s): %v", a.Slug, err)
	}
}

func wantErr(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: err = %v, want %v", what, err, want)
	}
}

func testCreateGet(t *testing.T, h *harness) {
	h.mkUser("alice")
	a := newArtifact("demo", "alice", at(0))
	a.Visibility = artifact.Unlisted
	a.Metadata = map[string]string{"team": "core", artifact.MetaSPA: "true"}
	a.StaleAt = ptr(at(time.Hour))
	a.ExpiresAt = ptr(at(2 * time.Hour))
	v := h.create(t, a, file("b.txt", "bee"), file("a.txt", "a"))
	if v.ID == 0 || v.Number != 1 {
		t.Fatalf("Create set ID=%d Number=%d, want non-zero and 1", v.ID, v.Number)
	}

	want := &artifact.Artifact{
		Slug:             "demo",
		Name:             "Name of demo",
		Description:      "About demo",
		OwnerHandle:      "alice",
		Visibility:       artifact.Unlisted,
		Metadata:         map[string]string{"team": "core", artifact.MetaSPA: "true"},
		PublishedVersion: 1,
		LatestVersion:    1,
		SizeBytes:        4,
		StaleAt:          ptr(at(time.Hour)),
		ExpiresAt:        ptr(at(2 * time.Hour)),
		CreatedAt:        at(0),
		UpdatedAt:        at(0),
	}
	got := h.get(t, "demo")
	if diff := cmp.Diff(want, got, cmpOpts...); diff != "" {
		t.Fatalf("Get mismatch (-want +got):\n%s", diff)
	}
	if got.CreatedAt.Location() != time.UTC {
		t.Errorf("CreatedAt location = %v, want UTC", got.CreatedAt.Location())
	}
}

func testCreateSlugTaken(t *testing.T, h *harness) {
	h.mkUser("alice")
	h.mkUser("bob")
	h.create(t, newArtifact("taken", "alice", at(0)))

	a := newArtifact("taken", "bob", at(time.Hour))
	err := h.store.Create(h.ctx, a, newVersion("bob", at(time.Hour), file("x", "x")))
	wantErr(t, "Create duplicate", err, artifact.ErrSlugTaken)
	if got := h.get(t, "taken"); got.OwnerHandle != "alice" {
		t.Fatalf("owner after failed create = %q, want alice", got.OwnerHandle)
	}

	// A tombstone still reserves the slug.
	if err := h.store.Purge(h.ctx, "taken", at(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	err = h.store.Create(h.ctx, a, newVersion("bob", at(time.Hour), file("x", "x")))
	wantErr(t, "Create over tombstone", err, artifact.ErrSlugTaken)

	// Removing the tombstone frees it.
	if err := h.store.DeleteTombstone(h.ctx, "taken"); err != nil {
		t.Fatal(err)
	}
	h.create(t, newArtifact("taken", "bob", at(3*time.Hour)))
	if got := h.get(t, "taken"); got.OwnerHandle != "bob" || got.LatestVersion != 1 {
		t.Fatalf("recreated artifact: owner %q latest %d, want bob 1", got.OwnerHandle, got.LatestVersion)
	}
}

func testCreateUnknownOwner(t *testing.T, h *harness) {
	a := newArtifact("orphan", "nobody", at(0))
	if err := h.store.Create(h.ctx, a, newVersion("nobody", at(0), file("x", "x"))); err == nil {
		t.Fatal("Create with unknown owner succeeded")
	}
	_, err := h.store.Get(h.ctx, "orphan")
	wantErr(t, "Get after failed create", err, artifact.ErrNotFound)
}

func testGetNotFound(t *testing.T, h *harness) {
	_, err := h.store.Get(h.ctx, "missing")
	wantErr(t, "Get", err, artifact.ErrNotFound)
	_, err = h.store.ListVersions(h.ctx, "missing")
	wantErr(t, "ListVersions", err, artifact.ErrNotFound)
	_, err = h.store.GetVersion(h.ctx, "missing", 1)
	if !errors.Is(err, artifact.ErrNotFound) && !errors.Is(err, artifact.ErrVersionNotFound) {
		t.Fatalf("GetVersion: err = %v, want ErrNotFound or ErrVersionNotFound", err)
	}
}

func testVersions(t *testing.T, h *harness) {
	h.mkUser("alice")
	h.mkUser("bob")
	a := newArtifact("demo", "alice", at(0))
	v1 := h.create(t, a, file("z/last.css", "zz"), file("index.html", "<p>"), file("a/first.js", "1"))

	got, err := h.store.GetVersion(h.ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	want := &artifact.Version{
		ID: v1.ID, Number: 1, Label: "label", CreatedBy: "alice", CreatedAt: at(0), SizeBytes: 6,
		Files: []artifact.File{file("a/first.js", "1"), file("index.html", "<p>"), file("z/last.css", "zz")},
	}
	if diff := cmp.Diff(want, got, cmpOpts...); diff != "" {
		t.Fatalf("GetVersion mismatch (-want +got):\n%s", diff)
	}

	v2 := newVersion("bob", at(time.Hour), file("index.html", "<p>v2"))
	v2.Label = "second"
	if err := h.store.AddVersion(h.ctx, a, v2, 0); err != nil {
		t.Fatal(err)
	}
	list, err := h.store.ListVersions(h.ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	wantList := []artifact.Version{
		{ID: v2.ID, Number: 2, Label: "second", CreatedBy: "bob", CreatedAt: at(time.Hour), SizeBytes: 5},
		{ID: v1.ID, Number: 1, Label: "label", CreatedBy: "alice", CreatedAt: at(0), SizeBytes: 6},
	}
	if diff := cmp.Diff(wantList, list, cmpOpts...); diff != "" {
		t.Fatalf("ListVersions mismatch (-want +got):\n%s", diff)
	}

	_, err = h.store.GetVersion(h.ctx, "demo", 3)
	wantErr(t, "GetVersion(3)", err, artifact.ErrVersionNotFound)
}

func testAddVersion(t *testing.T, h *harness) {
	h.mkUser("alice")
	h.mkUser("bob")
	a := newArtifact("demo", "alice", at(0))
	a.StaleAt, a.ExpiresAt = ptr(at(time.Minute)), ptr(at(time.Hour))
	h.create(t, a)

	for n := 2; n <= 3; n++ {
		a.Name = "rev"
		a.Description = "changed"
		a.Visibility = artifact.Unlisted
		a.Metadata = map[string]string{"rev": "x"}
		a.StaleAt, a.ExpiresAt = nil, nil
		a.UpdatedAt = at(time.Duration(n) * time.Hour)
		v := newVersion("bob", a.UpdatedAt, file("index.html", strings.Repeat("v", n)))
		if err := h.store.AddVersion(h.ctx, a, v, n-1); err != nil {
			t.Fatalf("AddVersion %d: %v", n, err)
		}
		if v.Number != n || v.ID == 0 {
			t.Fatalf("AddVersion set Number=%d ID=%d, want %d and non-zero", v.Number, v.ID, n)
		}
	}
	want := &artifact.Artifact{
		Slug: "demo", Name: "rev", Description: "changed", OwnerHandle: "alice",
		Visibility: artifact.Unlisted, Metadata: map[string]string{"rev": "x"},
		PublishedVersion: 3, LatestVersion: 3,
		SizeBytes: int64(len("<h1>demo</h1>")) + 2 + 3,
		CreatedAt: at(0), UpdatedAt: at(3 * time.Hour),
	}
	if diff := cmp.Diff(want, h.get(t, "demo"), cmpOpts...); diff != "" {
		t.Fatalf("after AddVersion (-want +got):\n%s", diff)
	}

	// The version's author is recorded as an updater.
	page, err := h.store.List(h.ctx, artifact.ListQuery{Viewer: "bob", Mine: "updated"})
	if err != nil {
		t.Fatal(err)
	}
	if got := slugs(page.Items); !slices.Equal(got, []string{"demo"}) {
		t.Fatalf("bob mine=updated = %v, want [demo]", got)
	}

	// AddVersion clears deleted_at when asked to (restore with a new version).
	cur := h.get(t, "demo")
	cur.DeletedAt = ptr(at(4 * time.Hour))
	h.update(t, cur, "")
	cur.DeletedAt = nil
	if err := h.store.AddVersion(h.ctx, cur, newVersion("alice", at(5*time.Hour), file("x", "x")), 0); err != nil {
		t.Fatal(err)
	}
	if got := h.get(t, "demo"); got.DeletedAt != nil || got.LatestVersion != 4 {
		t.Fatalf("after restore AddVersion: deleted %v latest %d", got.DeletedAt, got.LatestVersion)
	}

	err = h.store.AddVersion(h.ctx, newArtifact("missing", "alice", at(0)), newVersion("alice", at(0), file("x", "x")), 0)
	wantErr(t, "AddVersion missing", err, artifact.ErrNotFound)
}

func testAddVersionConflict(t *testing.T, h *harness) {
	h.mkUser("alice")
	a := newArtifact("demo", "alice", at(0))
	h.create(t, a)
	before := h.get(t, "demo")

	changed := *before
	changed.Name = "should not be written"
	changed.UpdatedAt = at(time.Hour)
	v := newVersion("alice", at(time.Hour), file("new.txt", "new"))
	err := h.store.AddVersion(h.ctx, &changed, v, 5)
	wantErr(t, "AddVersion expected=5", err, artifact.ErrVersionConflict)

	if diff := cmp.Diff(before, h.get(t, "demo"), cmpOpts...); diff != "" {
		t.Fatalf("conflicting AddVersion changed the artifact (-before +after):\n%s", diff)
	}
	list, err := h.store.ListVersions(h.ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("versions after conflict = %d, want 1", len(list))
	}
	// The conflicting version's blob was not recorded as referenced.
	blobs, err := h.store.UnreferencedBlobs(h.ctx, at(24*time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(blobs, hashOf("new")) {
		t.Fatalf("blob of a rejected version is referenced or recorded: %v", blobs)
	}

	// The matching expectation succeeds.
	if err := h.store.AddVersion(h.ctx, &changed, v, 1); err != nil {
		t.Fatal(err)
	}
	if got := h.get(t, "demo"); got.LatestVersion != 2 || got.Name != "should not be written" {
		t.Fatalf("after AddVersion expected=1: latest %d name %q", got.LatestVersion, got.Name)
	}
}

func testAddVersionConcurrent(t *testing.T, h *harness) {
	h.mkUser("alice")
	h.create(t, newArtifact("demo", "alice", at(0)))
	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a := newArtifact("demo", "alice", at(0))
			a.UpdatedAt = at(time.Hour)
			v := newVersion("alice", at(time.Hour), file("f.txt", fmt.Sprint(i)))
			errs[i] = h.store.AddVersion(h.ctx, a, v, 1)
		}()
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, artifact.ErrVersionConflict):
		default:
			t.Errorf("concurrent AddVersion: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d concurrent AddVersion calls with expected=1 succeeded, want 1", wins)
	}
	if got := h.get(t, "demo"); got.LatestVersion != 2 {
		t.Fatalf("LatestVersion = %d, want 2", got.LatestVersion)
	}
}

func testUpdate(t *testing.T, h *harness) {
	h.mkUser("alice")
	h.mkUser("bob")
	a := newArtifact("demo", "alice", at(0))
	h.create(t, a)
	if err := h.store.AddVersion(h.ctx, a, newVersion("alice", at(time.Minute), file("v2", "v2")), 0); err != nil {
		t.Fatal(err)
	}

	got := h.get(t, "demo")
	got.Name = "new name"
	got.Description = "new description"
	got.OwnerHandle = "bob"
	got.Visibility = artifact.Unlisted
	got.Metadata = map[string]string{"k": "v", "agalma.indexable": "true"}
	got.PublishedVersion = 1 // roll back
	got.StaleAt = ptr(at(time.Hour))
	got.ExpiresAt = ptr(at(2 * time.Hour))
	got.UnpublishedAt = ptr(at(3 * time.Hour))
	got.DeletedAt = ptr(at(4 * time.Hour))
	got.UpdatedAt = at(5 * time.Hour)
	h.update(t, got, "bob")

	want := *got
	if diff := cmp.Diff(&want, h.get(t, "demo"), cmpOpts...); diff != "" {
		t.Fatalf("after Update (-want +got):\n%s", diff)
	}
	p, err := h.store.ResolvePublished(h.ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if p.VersionNumber != 1 {
		t.Fatalf("published after rollback = %d, want 1", p.VersionNumber)
	}

	// Pointer fields clear back to nil.
	got.StaleAt, got.ExpiresAt, got.UnpublishedAt, got.DeletedAt = nil, nil, nil, nil
	got.Metadata = nil
	got.PublishedVersion = 2
	got.UpdatedAt = at(6 * time.Hour)
	h.update(t, got, "")
	want = *got
	if diff := cmp.Diff(&want, h.get(t, "demo"), cmpOpts...); diff != "" {
		t.Fatalf("after clearing Update (-want +got):\n%s", diff)
	}

	// The updater is recorded; an empty by records nothing.
	page, err := h.store.List(h.ctx, artifact.ListQuery{Viewer: "bob", Mine: "updated"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(slugs(page.Items), []string{"demo"}) {
		t.Fatalf("bob mine=updated = %v, want [demo]", slugs(page.Items))
	}
}

func testUpdateErrors(t *testing.T, h *harness) {
	h.mkUser("alice")
	a := newArtifact("demo", "alice", at(0))
	h.create(t, a)

	missing := newArtifact("missing", "alice", at(0))
	wantErr(t, "Update missing", h.store.Update(h.ctx, missing, "alice"), artifact.ErrNotFound)

	bad := h.get(t, "demo")
	bad.PublishedVersion = 9
	wantErr(t, "Update to missing version", h.store.Update(h.ctx, bad, "alice"), artifact.ErrVersionNotFound)

	bad = h.get(t, "demo")
	bad.OwnerHandle = "nobody"
	if err := h.store.Update(h.ctx, bad, "alice"); err == nil {
		t.Fatal("Update to unknown owner succeeded")
	}
	if got := h.get(t, "demo"); got.OwnerHandle != "alice" || got.PublishedVersion != 1 {
		t.Fatalf("failed updates changed the artifact: %+v", got)
	}
}

func slugs(items []artifact.Artifact) []string {
	out := make([]string, len(items))
	for i, a := range items {
		out[i] = a.Slug
	}
	return out
}

func testListFilters(t *testing.T, h *harness) {
	for _, u := range []string{"alice", "bob", "carol", "dave"} {
		h.mkUser(u)
	}
	// Each artifact is created an hour after the previous one, so the
	// listing order (updated_at descending) is the reverse of this list.
	type setup struct {
		slug, owner string
		mutate      func(a *artifact.Artifact)
	}
	setups := []setup{
		{"pub-alice", "alice", nil},
		{"unl-alice", "alice", func(a *artifact.Artifact) { a.Visibility = artifact.Unlisted }},
		{"unl-bob", "bob", func(a *artifact.Artifact) { a.Visibility = artifact.Unlisted }},
		{"stale-bob", "bob", func(a *artifact.Artifact) { a.StaleAt, a.ExpiresAt = ptr(t0), ptr(t0.Add(time.Hour)) }},
		{"deleted-bob", "bob", func(a *artifact.Artifact) { a.DeletedAt = ptr(t0) }},
		{"meta-carol", "carol", func(a *artifact.Artifact) {
			a.Metadata = map[string]string{"team": "x", artifact.MetaSPA: "true"}
		}},
		{"unpub-carol", "carol", func(a *artifact.Artifact) { a.UnpublishedAt = ptr(t0) }},
		{"tomb-carol", "carol", nil},
	}
	for i, s := range setups {
		a := newArtifact(s.slug, s.owner, at(time.Duration(i)*time.Hour))
		if s.mutate != nil {
			s.mutate(a)
		}
		h.create(t, a)
	}
	if err := h.store.Purge(h.ctx, "tomb-carol", at(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// alice updates bob's unlisted artifact without changing its position.
	ub := h.get(t, "unl-bob")
	h.update(t, ub, "alice")

	tests := []struct {
		name string
		q    artifact.ListQuery
		want []string
	}{
		{"stranger sees live public", artifact.ListQuery{Viewer: "dave"},
			[]string{"meta-carol", "stale-bob", "pub-alice"}},
		{"viewer sees own and updated", artifact.ListQuery{Viewer: "alice"},
			[]string{"meta-carol", "stale-bob", "unl-bob", "unl-alice", "pub-alice"}},
		{"owner sees own unpublished", artifact.ListQuery{Viewer: "carol"},
			[]string{"unpub-carol", "meta-carol", "stale-bob", "pub-alice"}},
		{"all", artifact.ListQuery{Viewer: "dave", All: true},
			[]string{"unpub-carol", "meta-carol", "stale-bob", "unl-bob", "unl-alice", "pub-alice"}},
		{"mine created", artifact.ListQuery{Viewer: "alice", Mine: "created"},
			[]string{"unl-alice", "pub-alice"}},
		{"mine updated", artifact.ListQuery{Viewer: "alice", Mine: "updated"},
			[]string{"unl-bob"}},
		{"mine any", artifact.ListQuery{Viewer: "alice", Mine: "any"},
			[]string{"unl-bob", "unl-alice", "pub-alice"}},
		{"mine with all", artifact.ListQuery{Viewer: "bob", All: true, Mine: "created"},
			[]string{"stale-bob", "unl-bob"}},
		{"owner", artifact.ListQuery{Viewer: "dave", All: true, Owner: "bob"},
			[]string{"stale-bob", "unl-bob"}},
		{"owner respects visibility", artifact.ListQuery{Viewer: "dave", Owner: "bob"},
			[]string{"stale-bob"}},
		{"metadata", artifact.ListQuery{Viewer: "dave", Metadata: map[string]string{"team": "x"}},
			[]string{"meta-carol"}},
		{"metadata dotted key", artifact.ListQuery{Viewer: "dave", Metadata: map[string]string{artifact.MetaSPA: "true"}},
			[]string{"meta-carol"}},
		{"metadata every pair", artifact.ListQuery{Viewer: "dave", Metadata: map[string]string{"team": "x", artifact.MetaSPA: "false"}},
			nil},
		{"metadata no match", artifact.ListQuery{Viewer: "dave", Metadata: map[string]string{"team": "y"}},
			nil},
		{"stale include", artifact.ListQuery{Viewer: "dave", Stale: "include"},
			[]string{"meta-carol", "stale-bob", "pub-alice"}},
		{"stale exclude", artifact.ListQuery{Viewer: "dave", Stale: "exclude"},
			[]string{"meta-carol", "pub-alice"}},
		{"stale only", artifact.ListQuery{Viewer: "dave", Stale: "only"},
			[]string{"stale-bob"}},
		{"deleted exclude", artifact.ListQuery{Viewer: "bob", Deleted: "exclude"},
			[]string{"meta-carol", "stale-bob", "unl-bob", "pub-alice"}},
		{"deleted include", artifact.ListQuery{Viewer: "bob", Deleted: "include"},
			[]string{"meta-carol", "deleted-bob", "stale-bob", "unl-bob", "pub-alice"}},
		{"deleted only", artifact.ListQuery{Viewer: "bob", Deleted: "only"},
			[]string{"deleted-bob"}},
		{"deleted hidden from strangers", artifact.ListQuery{Viewer: "dave", Deleted: "only"},
			nil},
		{"deleted only all", artifact.ListQuery{Viewer: "dave", All: true, Deleted: "only"},
			[]string{"deleted-bob"}},
		{"tombstones never listed", artifact.ListQuery{Viewer: "carol", All: true, Deleted: "include", Owner: "carol"},
			[]string{"unpub-carol", "meta-carol"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			page, err := h.store.List(h.ctx, tt.q)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tt.want, slugs(page.Items), cmpOpts...); diff != "" {
				t.Fatalf("List mismatch (-want +got):\n%s", diff)
			}
			if page.NextCursor != "" {
				t.Fatalf("NextCursor = %q on a single page", page.NextCursor)
			}
		})
	}

	// Listed items carry the full record.
	page, err := h.store.List(h.ctx, artifact.ListQuery{Viewer: "dave", Metadata: map[string]string{"team": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]artifact.Artifact{*h.get(t, "meta-carol")}, page.Items, cmpOpts...); diff != "" {
		t.Fatalf("listed item differs from Get (-get +list):\n%s", diff)
	}
}

func testListPagination(t *testing.T, h *harness) {
	h.mkUser("alice")
	// Several artifacts share an updated_at so the tie-break is exercised.
	for i := range 11 {
		a := newArtifact("page-"+string(rune('a'+i)), "alice", at(time.Duration(i/3)*time.Hour))
		h.create(t, a)
	}
	full, err := h.store.List(h.ctx, artifact.ListQuery{Viewer: "alice", Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Items) != 11 || full.NextCursor != "" {
		t.Fatalf("full listing: %d items, cursor %q", len(full.Items), full.NextCursor)
	}
	for i := 1; i < len(full.Items); i++ {
		if full.Items[i].UpdatedAt.After(full.Items[i-1].UpdatedAt) {
			t.Fatalf("listing not ordered by updated_at descending: %v", slugs(full.Items))
		}
	}

	for _, limit := range []int{1, 3, 4, 11} {
		var (
			got    []string
			cursor string
			pages  int
		)
		for {
			page, err := h.store.List(h.ctx, artifact.ListQuery{Viewer: "alice", Limit: limit, Cursor: cursor})
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Items) > limit {
				t.Fatalf("limit %d: page of %d", limit, len(page.Items))
			}
			got = append(got, slugs(page.Items)...)
			pages++
			if page.NextCursor == "" {
				break
			}
			if pages > 20 {
				t.Fatalf("limit %d: pagination does not terminate", limit)
			}
			cursor = page.NextCursor
		}
		if diff := cmp.Diff(slugs(full.Items), got); diff != "" {
			t.Fatalf("limit %d: paged listing differs from full (-full +paged):\n%s", limit, diff)
		}
	}

	// The default limit is 50.
	for i := range 45 {
		h.create(t, newArtifact(fmt.Sprintf("bulk-%02d", i), "alice", at(48*time.Hour)))
	}
	page, err := h.store.List(h.ctx, artifact.ListQuery{Viewer: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 50 || page.NextCursor == "" {
		t.Fatalf("default page: %d items, cursor %q; want 50 and a cursor", len(page.Items), page.NextCursor)
	}
}

func testListInvalid(t *testing.T, h *harness) {
	for _, q := range []artifact.ListQuery{
		{Mine: "mine"},
		{Stale: "maybe"},
		{Deleted: "yes"},
		{Cursor: "not a cursor!"},
		{Metadata: map[string]string{"bad key'": "x"}},
	} {
		if _, err := h.store.List(h.ctx, q); err == nil {
			t.Errorf("List(%+v) succeeded, want an error", q)
		}
	}
}

func testResolve(t *testing.T, h *harness) {
	h.mkUser("alice")
	a := newArtifact("demo", "alice", at(0))
	a.Metadata = map[string]string{artifact.MetaSPA: "true", "x": "y"}
	v1 := h.create(t, a)
	v2 := newVersion("alice", at(time.Hour), file("index.html", "two"))
	if err := h.store.AddVersion(h.ctx, a, v2, 0); err != nil {
		t.Fatal(err)
	}

	want := &artifact.Published{
		Slug: "demo", VersionID: v2.ID, VersionNumber: 2, Visibility: artifact.Public,
		Metadata: map[string]string{artifact.MetaSPA: "true", "x": "y"},
	}
	p, err := h.store.ResolvePublished(h.ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, p, cmpOpts...); diff != "" {
		t.Fatalf("ResolvePublished (-want +got):\n%s", diff)
	}
	p, err = h.store.ResolveVersion(h.ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	want1 := *want
	want1.VersionID, want1.VersionNumber = v1.ID, 1
	if diff := cmp.Diff(&want1, p, cmpOpts...); diff != "" {
		t.Fatalf("ResolveVersion(1) (-want +got):\n%s", diff)
	}
	_, err = h.store.ResolveVersion(h.ctx, "demo", 3)
	wantErr(t, "ResolveVersion(3)", err, artifact.ErrVersionNotFound)
	_, err = h.store.ResolvePublished(h.ctx, "missing")
	wantErr(t, "ResolvePublished missing", err, artifact.ErrNotFound)
	_, err = h.store.ResolveVersion(h.ctx, "missing", 1)
	wantErr(t, "ResolveVersion missing", err, artifact.ErrNotFound)

	// Unpublished and deleted states are reported, not hidden.
	got := h.get(t, "demo")
	got.UnpublishedAt = ptr(at(2 * time.Hour))
	h.update(t, got, "")
	p, err = h.store.ResolvePublished(h.ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if p.UnpublishedAt == nil || !p.UnpublishedAt.Equal(at(2*time.Hour)) || p.VersionID != v2.ID {
		t.Fatalf("unpublished resolve: %+v", p)
	}
	got.UnpublishedAt = nil
	got.DeletedAt = ptr(at(3 * time.Hour))
	h.update(t, got, "")
	p, err = h.store.ResolveVersion(h.ctx, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if p.DeletedAt == nil || !p.DeletedAt.Equal(at(3*time.Hour)) || p.UnpublishedAt != nil {
		t.Fatalf("deleted resolve: %+v", p)
	}

	// A tombstone resolves with no version and purged_at set.
	if err := h.store.Purge(h.ctx, "demo", at(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	for name, resolve := range map[string]func() (*artifact.Published, error){
		"published": func() (*artifact.Published, error) { return h.store.ResolvePublished(h.ctx, "demo") },
		"version":   func() (*artifact.Published, error) { return h.store.ResolveVersion(h.ctx, "demo", 1) },
	} {
		p, err := resolve()
		if err != nil {
			t.Fatalf("resolve %s after purge: %v", name, err)
		}
		if p.VersionID != 0 || p.PurgedAt == nil || !p.PurgedAt.Equal(at(4*time.Hour)) {
			t.Fatalf("resolve %s after purge: %+v", name, p)
		}
		if p.Servable() {
			t.Fatalf("resolve %s after purge is servable", name)
		}
	}
}

func testFiles(t *testing.T, h *harness) {
	h.mkUser("alice")
	files := []artifact.File{file("index.html", "<p>"), file("css/site.css", "body{}"), file("img/a.png", "png")}
	files[2].ContentType = "image/png"
	v := h.create(t, newArtifact("demo", "alice", at(0)), files...)
	got, err := h.store.Files(h.ctx, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]artifact.File{}
	for _, f := range files {
		want[f.Path] = f
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Files (-want +got):\n%s", diff)
	}
	empty, err := h.store.Files(h.ctx, v.ID+1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("Files of unknown version = %v", empty)
	}
}

func testUsage(t *testing.T, h *harness) {
	h.mkUser("alice")
	h.mkUser("bob")
	a := newArtifact("one", "alice", at(0))
	h.create(t, a, file("a", "12345"))
	if err := h.store.AddVersion(h.ctx, a, newVersion("bob", at(time.Hour), file("a", "123")), 0); err != nil {
		t.Fatal(err)
	}
	h.create(t, newArtifact("two", "alice", at(0)), file("b", "1234567"))
	h.create(t, newArtifact("three", "bob", at(0)), file("c", "1"))

	for handle, want := range map[string]int64{"alice": 15, "bob": 1, "nobody": 0} {
		got, err := h.store.Usage(h.ctx, handle)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("Usage(%s) = %d, want %d", handle, got, want)
		}
	}
	if got := h.get(t, "one"); got.SizeBytes != 8 {
		t.Errorf("SizeBytes of one = %d, want 8", got.SizeBytes)
	}

	// Purged versions no longer count.
	if err := h.store.Purge(h.ctx, "two", at(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.store.Usage(h.ctx, "alice"); got != 8 {
		t.Errorf("Usage(alice) after purge = %d, want 8", got)
	}
}

func testLifecycleQueries(t *testing.T, h *harness) {
	h.mkUser("alice")
	mk := func(slug string, mutate func(a *artifact.Artifact)) {
		a := newArtifact(slug, "alice", at(0))
		mutate(a)
		h.create(t, a)
	}
	mk("exp-2", func(a *artifact.Artifact) { a.StaleAt, a.ExpiresAt = ptr(t0), ptr(at(2*time.Hour)) })
	mk("exp-1", func(a *artifact.Artifact) { a.StaleAt, a.ExpiresAt = ptr(t0), ptr(at(1*time.Hour)) })
	mk("exp-3", func(a *artifact.Artifact) { a.StaleAt, a.ExpiresAt = ptr(t0), ptr(at(3*time.Hour)) })
	mk("exp-deleted", func(a *artifact.Artifact) {
		a.StaleAt, a.ExpiresAt, a.DeletedAt = ptr(t0), ptr(at(time.Hour)), ptr(at(time.Hour))
	})
	mk("del-2", func(a *artifact.Artifact) { a.DeletedAt = ptr(at(20 * time.Hour)) })
	mk("del-1", func(a *artifact.Artifact) { a.DeletedAt = ptr(at(10 * time.Hour)) })
	mk("del-3", func(a *artifact.Artifact) { a.DeletedAt = ptr(at(30 * time.Hour)) })
	mk("purged-1", func(a *artifact.Artifact) { a.DeletedAt = ptr(at(time.Hour)) })
	mk("purged-2", func(a *artifact.Artifact) { a.DeletedAt = ptr(at(time.Hour)) })
	mk("live", func(a *artifact.Artifact) {})
	if err := h.store.Purge(h.ctx, "purged-2", at(200*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Purge(h.ctx, "purged-1", at(100*time.Hour)); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		fn   func() ([]artifact.Artifact, error)
		want []string
	}{
		{"expired none", func() ([]artifact.Artifact, error) { return h.store.ListExpired(h.ctx, at(30*time.Minute)) }, nil},
		{"expired boundary", func() ([]artifact.Artifact, error) { return h.store.ListExpired(h.ctx, at(2*time.Hour)) },
			[]string{"exp-1", "exp-2"}},
		{"expired all", func() ([]artifact.Artifact, error) { return h.store.ListExpired(h.ctx, at(99*time.Hour)) },
			[]string{"exp-1", "exp-2", "exp-3"}},
		{"retention boundary", func() ([]artifact.Artifact, error) { return h.store.ListRetentionDue(h.ctx, at(20*time.Hour)) },
			[]string{"exp-deleted", "del-1", "del-2"}},
		{"retention all", func() ([]artifact.Artifact, error) { return h.store.ListRetentionDue(h.ctx, at(999*time.Hour)) },
			[]string{"exp-deleted", "del-1", "del-2", "del-3"}},
		{"tombstones none", func() ([]artifact.Artifact, error) { return h.store.ListTombstonesDue(h.ctx, at(99*time.Hour)) }, nil},
		{"tombstones boundary", func() ([]artifact.Artifact, error) { return h.store.ListTombstonesDue(h.ctx, at(100*time.Hour)) },
			[]string{"purged-1"}},
		{"tombstones all", func() ([]artifact.Artifact, error) { return h.store.ListTombstonesDue(h.ctx, at(999*time.Hour)) },
			[]string{"purged-1", "purged-2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.fn()
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tt.want, slugs(got), cmpOpts...); diff != "" {
				t.Fatalf("(-want +got):\n%s", diff)
			}
		})
	}
}

func testPurgeAndDeleteTombstone(t *testing.T, h *harness) {
	h.mkUser("alice")
	a := newArtifact("demo", "alice", at(0))
	v1 := h.create(t, a)
	if err := h.store.AddVersion(h.ctx, a, newVersion("alice", at(time.Hour), file("x", "x")), 0); err != nil {
		t.Fatal(err)
	}
	wantErr(t, "DeleteTombstone of live artifact", h.store.DeleteTombstone(h.ctx, "demo"), artifact.ErrInvalidArgument)
	h.get(t, "demo")

	if err := h.store.Purge(h.ctx, "demo", at(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got := h.get(t, "demo")
	if got.PurgedAt == nil || !got.PurgedAt.Equal(at(2*time.Hour)) {
		t.Fatalf("PurgedAt = %v, want %v", got.PurgedAt, at(2*time.Hour))
	}
	if got.PublishedVersion != 0 || got.LatestVersion != 0 || got.SizeBytes != 0 {
		t.Fatalf("tombstone keeps versions: published %d latest %d size %d",
			got.PublishedVersion, got.LatestVersion, got.SizeBytes)
	}
	if got.DeletedAt == nil {
		t.Fatalf("purge left DeletedAt nil")
	}
	versions, err := h.store.ListVersions(h.ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 0 {
		t.Fatalf("versions after purge = %d", len(versions))
	}
	_, err = h.store.GetVersion(h.ctx, "demo", 1)
	wantErr(t, "GetVersion after purge", err, artifact.ErrVersionNotFound)
	files, err := h.store.Files(h.ctx, v1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("files after purge = %v", files)
	}

	// Purging a tombstone again keeps the first purge time.
	if err := h.store.Purge(h.ctx, "demo", at(5*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := h.get(t, "demo"); !got.PurgedAt.Equal(at(2 * time.Hour)) {
		t.Fatalf("second purge moved PurgedAt to %v", got.PurgedAt)
	}

	if err := h.store.DeleteTombstone(h.ctx, "demo"); err != nil {
		t.Fatal(err)
	}
	_, err = h.store.Get(h.ctx, "demo")
	wantErr(t, "Get after DeleteTombstone", err, artifact.ErrNotFound)
	wantErr(t, "DeleteTombstone missing", h.store.DeleteTombstone(h.ctx, "demo"), artifact.ErrNotFound)
	wantErr(t, "Purge missing", h.store.Purge(h.ctx, "demo", at(0)), artifact.ErrNotFound)
}

func testBlobs(t *testing.T, h *harness) {
	h.mkUser("alice")
	free1, free2, used := hashOf("free1"), hashOf("free2"), hashOf("used")
	for _, hash := range []string{free2, free1} {
		if err := h.store.RecordBlob(h.ctx, hash, 5, at(0)); err != nil {
			t.Fatal(err)
		}
	}
	// Create records the blobs of its files; they are referenced.
	h.create(t, newArtifact("demo", "alice", at(0)), file("used.txt", "used"))

	unref := func(before time.Time, limit int) []string {
		t.Helper()
		got, err := h.store.UnreferencedBlobs(h.ctx, before, limit)
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(got)
		return got
	}
	sorted := func(s ...string) []string { slices.Sort(s); return s }

	if got := unref(at(0), 10); len(got) != 0 {
		t.Fatalf("UnreferencedBlobs before creation = %v, want none (strictly before)", got)
	}
	if diff := cmp.Diff(sorted(free1, free2), unref(at(time.Hour), 10)); diff != "" {
		t.Fatalf("UnreferencedBlobs (-want +got):\n%s", diff)
	}
	if got := unref(at(time.Hour), 1); len(got) != 1 {
		t.Fatalf("UnreferencedBlobs limit 1 returned %d", len(got))
	}

	// Re-recording refreshes created_at, protecting the blob for the grace.
	if err := h.store.RecordBlob(h.ctx, free1, 5, at(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{free2}, unref(at(time.Hour), 10)); diff != "" {
		t.Fatalf("after re-record (-want +got):\n%s", diff)
	}
	// An older re-record never moves created_at backwards.
	if err := h.store.RecordBlob(h.ctx, free1, 5, at(0)); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{free2}, unref(at(time.Hour), 10)); diff != "" {
		t.Fatalf("after older re-record (-want +got):\n%s", diff)
	}

	if err := h.store.DeleteBlob(h.ctx, free2); err != nil {
		t.Fatal(err)
	}
	if got := unref(at(3*time.Hour), 10); !slices.Equal(got, []string{free1}) {
		t.Fatalf("after DeleteBlob = %v, want [free1]", got)
	}
	if err := h.store.DeleteBlob(h.ctx, free2); err != nil {
		t.Fatalf("DeleteBlob of missing row: %v", err)
	}

	// Purging the only version that references a blob releases it.
	if err := h.store.Purge(h.ctx, "demo", at(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(sorted(free1, used), unref(at(3*time.Hour), 10)); diff != "" {
		t.Fatalf("after purge (-want +got):\n%s", diff)
	}
}
