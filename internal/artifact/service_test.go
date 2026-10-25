package artifact_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"kazari/internal/artifact"
	"kazari/internal/audit"
	"kazari/internal/blob"
	"kazari/internal/sqlite"
)

var start = time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type env struct {
	ctx   context.Context
	svc   *artifact.Service
	db    *sqlite.DB
	store *sqlite.ArtifactStore
	blobs *blob.FS
	rec   *audit.Memory
	clock *clock
}

func newEnv(t *testing.T, opts artifact.Options) *env {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := sqlite.Open(ctx, filepath.Join(dir, "kazari.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	blobs, err := blob.NewFS(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{t: start}
	opts.Now = c.Now
	store := sqlite.NewArtifactStore(db)
	rec := &audit.Memory{}
	return &env{ctx: ctx, svc: artifact.New(store, blobs, rec, opts), db: db, store: store, blobs: blobs, rec: rec, clock: c}
}

func (e *env) user(t *testing.T, handle string) artifact.Actor {
	t.Helper()
	_, err := e.db.ExecContext(e.ctx,
		`INSERT INTO user (handle, role, quota_bytes, created_at) VALUES (?, 'member', 0, ?)`,
		handle, start.UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	return artifact.Actor{Handle: handle}
}

func files(pairs ...string) []artifact.FileInput {
	var out []artifact.FileInput
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, artifact.FileInput{Path: pairs[i], Content: strings.NewReader(pairs[i+1])})
	}
	return out
}

func del(p string) artifact.FileInput { return artifact.FileInput{Path: p, Delete: true} }

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func ptr[T any](v T) *T { return &v }

func (e *env) create(t *testing.T, actor artifact.Actor, slug string, pairs ...string) *artifact.Artifact {
	t.Helper()
	if len(pairs) == 0 {
		pairs = []string{"index.html", "<h1>" + slug + "</h1>"}
	}
	a, _, err := e.svc.Create(e.ctx, actor, artifact.CreateRequest{Slug: slug, Files: files(pairs...)})
	if err != nil {
		t.Fatalf("Create(%s): %v", slug, err)
	}
	return a
}

func (e *env) read(t *testing.T, actor artifact.Actor, slug, p string, version int) string {
	t.Helper()
	_, rc, err := e.svc.OpenFile(e.ctx, actor, slug, p, version)
	if err != nil {
		t.Fatalf("OpenFile(%s, %s, %d): %v", slug, p, version, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (e *env) actions() []string {
	var out []string
	for _, ev := range e.rec.Events {
		out = append(out, ev.Action)
	}
	return out
}

func wantErr(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: err = %v, want %v", what, err, want)
	}
}

func TestCreateAndGet(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice := e.user(t, "alice")
	a, v, err := e.svc.Create(e.ctx, alice, artifact.CreateRequest{
		Slug:        "demo",
		Name:        "Demo",
		Description: "A demo",
		Visibility:  artifact.Unlisted,
		Metadata:    map[string]string{"team": "core"},
		Label:       "first",
		Files:       files("index.html", "<h1>hi</h1>", "app.js", "go()"),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := &artifact.Artifact{
		Slug: "demo", Name: "Demo", Description: "A demo", OwnerHandle: "alice",
		Visibility: artifact.Unlisted, Metadata: map[string]string{"team": "core"},
		PublishedVersion: 1, LatestVersion: 1, SizeBytes: 15,
		CreatedAt: start, UpdatedAt: start,
	}
	if diff := cmp.Diff(want, a); diff != "" {
		t.Fatalf("created artifact (-want +got):\n%s", diff)
	}
	wantV := &artifact.Version{
		ID: v.ID, Number: 1, Label: "first", CreatedBy: "alice", CreatedAt: start, SizeBytes: 15,
		Files: []artifact.File{
			{Path: "app.js", Hash: sha("go()"), Size: 4, ContentType: "text/javascript; charset=utf-8"},
			{Path: "index.html", Hash: sha("<h1>hi</h1>"), Size: 11, ContentType: "text/html; charset=utf-8"},
		},
	}
	if diff := cmp.Diff(wantV, v); diff != "" {
		t.Fatalf("created version (-want +got):\n%s", diff)
	}

	got, versions, err := e.svc.Get(e.ctx, alice, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Get (-want +got):\n%s", diff)
	}
	if len(versions) != 1 || versions[0].Number != 1 || versions[0].Label != "first" {
		t.Fatalf("Get versions = %+v", versions)
	}
	if body := e.read(t, alice, "demo", "index.html", 0); body != "<h1>hi</h1>" {
		t.Fatalf("published index.html = %q", body)
	}

	wantEvents := []audit.Event{{
		At: start, Actor: "alice", Action: audit.ArtifactCreate, Slug: "demo",
		Details: map[string]string{"version": "1", "size": "15"},
	}}
	if diff := cmp.Diff(wantEvents, e.rec.Events); diff != "" {
		t.Fatalf("audit (-want +got):\n%s", diff)
	}

	_, _, err = e.svc.Get(e.ctx, alice, "missing")
	wantErr(t, "Get missing", err, artifact.ErrNotFound)
}

func TestCreateValidation(t *testing.T) {
	tests := []struct {
		name  string
		actor artifact.Actor
		req   artifact.CreateRequest
		want  error
	}{
		{"bad slug", artifact.Actor{}, artifact.CreateRequest{Slug: "Bad_Slug", Files: files("a", "a")}, artifact.ErrInvalidSlug},
		{"reserved slug", artifact.Actor{}, artifact.CreateRequest{Slug: "admin", Files: files("a", "a")}, artifact.ErrInvalidSlug},
		{"version separator", artifact.Actor{}, artifact.CreateRequest{Slug: "demo--v2", Files: files("a", "a")}, artifact.ErrInvalidSlug},
		{"bad visibility", artifact.Actor{}, artifact.CreateRequest{Slug: "demo", Visibility: "secret", Files: files("a", "a")}, artifact.ErrInvalidArgument},
		{"bad metadata", artifact.Actor{}, artifact.CreateRequest{Slug: "demo", Metadata: map[string]string{"kazari.nope": "x"}, Files: files("a", "a")}, artifact.ErrInvalidMetadata},
		{"no files", artifact.Actor{}, artifact.CreateRequest{Slug: "demo"}, artifact.ErrInvalidArgument},
		{"bad path", artifact.Actor{}, artifact.CreateRequest{Slug: "demo", Files: files("../etc/passwd", "x")}, artifact.ErrInvalidPath},
		{"duplicate path", artifact.Actor{}, artifact.CreateRequest{Slug: "demo", Files: files("a/b", "1", "a//b", "2")}, artifact.ErrInvalidArgument},
		{"delete on create", artifact.Actor{}, artifact.CreateRequest{Slug: "demo", Files: []artifact.FileInput{del("a")}}, artifact.ErrInvalidArgument},
		{"nil content", artifact.Actor{}, artifact.CreateRequest{Slug: "demo", Files: []artifact.FileInput{{Path: "a"}}}, artifact.ErrInvalidArgument},
		{"too many files", artifact.Actor{}, artifact.CreateRequest{Slug: "demo", Files: files("a", "1", "b", "2", "c", "3")}, artifact.ErrTooManyFiles},
		{"anonymous", artifact.Actor{Handle: ""}, artifact.CreateRequest{Slug: "demo", Files: files("a", "a")}, artifact.ErrForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, artifact.Options{MaxFiles: 2, ReservedSlugs: []string{"admin"}})
			actor := e.user(t, "alice")
			if tt.name == "anonymous" {
				actor = tt.actor
			}
			_, _, err := e.svc.Create(e.ctx, actor, tt.req)
			wantErr(t, "Create", err, tt.want)
			if _, err := e.store.Get(e.ctx, tt.req.Slug); !errors.Is(err, artifact.ErrNotFound) {
				t.Fatalf("failed Create left a row: %v", err)
			}
			if len(e.rec.Events) != 0 {
				t.Fatalf("failed Create recorded %v", e.actions())
			}
		})
	}
}

func TestCreateDuplicateSlug(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice, bob := e.user(t, "alice"), e.user(t, "bob")
	e.create(t, alice, "demo")
	_, _, err := e.svc.Create(e.ctx, bob, artifact.CreateRequest{Slug: "demo", Files: files("a", "a")})
	wantErr(t, "duplicate Create", err, artifact.ErrSlugTaken)
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func TestSizeLimits(t *testing.T) {
	e := newEnv(t, artifact.Options{MaxFileBytes: 10, MaxVersionBytes: 25})
	alice := e.user(t, "alice")
	ten := strings.Repeat("x", 10)

	big := &countingReader{r: strings.NewReader(strings.Repeat("y", 1<<20))}
	_, _, err := e.svc.Create(e.ctx, alice, artifact.CreateRequest{
		Slug: "big", Files: []artifact.FileInput{{Path: "big.txt", Content: big}},
	})
	wantErr(t, "file over limit", err, artifact.ErrTooLarge)
	if big.n > 64<<10 {
		t.Fatalf("read %d bytes of an oversized file; the limit should fail fast", big.n)
	}
	_, _, err = e.svc.Create(e.ctx, alice, artifact.CreateRequest{Slug: "big", Files: files("noext", ten+"z")})
	wantErr(t, "sniffed file over limit", err, artifact.ErrTooLarge)

	_, _, err = e.svc.Create(e.ctx, alice, artifact.CreateRequest{
		Slug: "wide", Files: files("a.txt", ten, "b.txt", ten, "c.txt", ten),
	})
	wantErr(t, "version over limit", err, artifact.ErrTooLarge)
	for _, slug := range []string{"big", "wide"} {
		if _, err := e.store.Get(e.ctx, slug); !errors.Is(err, artifact.ErrNotFound) {
			t.Fatalf("failed Create of %s left a row: %v", slug, err)
		}
	}

	e.create(t, alice, "edge", "a.txt", ten, "b.txt", ten)

	_, _, err = e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "edge", Files: files("c.txt", "123456")})
	wantErr(t, "update over version limit", err, artifact.ErrTooLarge)
	a, versions, err := e.svc.Get(e.ctx, alice, "edge")
	if err != nil {
		t.Fatal(err)
	}
	if a.LatestVersion != 1 || len(versions) != 1 {
		t.Fatalf("failed update wrote a version: latest %d, %d versions", a.LatestVersion, len(versions))
	}
	if _, _, err := e.svc.Update(e.ctx, alice, artifact.UpdateRequest{
		Slug: "edge", Files: files("b.txt", "12345", "c.txt", "12345"),
	}); err != nil {
		t.Fatalf("update within limit: %v", err)
	}
}

func TestQuota(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	e.user(t, "alice")
	alice := artifact.Actor{Handle: "alice", QuotaBytes: 20}

	e.create(t, alice, "one", "a.txt", strings.Repeat("a", 15))
	_, _, err := e.svc.Create(e.ctx, alice, artifact.CreateRequest{Slug: "two", Files: files("b.txt", strings.Repeat("b", 6))})
	wantErr(t, "create over quota", err, artifact.ErrQuotaExceeded)

	if _, _, err := e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "one", Files: files("a.txt", "abc")}); err != nil {
		t.Fatalf("update within quota: %v", err)
	}
	// 3 carried + 1 new byte would make usage 22.
	_, _, err = e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "one", Files: files("b.txt", "b")})
	wantErr(t, "update over quota", err, artifact.ErrQuotaExceeded)

	unlimited := artifact.Actor{Handle: "alice"}
	e.create(t, unlimited, "three", "c.txt", strings.Repeat("c", 100))
}

func TestContentType(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice := e.user(t, "alice")
	png := "\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 600)
	html := "<!DOCTYPE html><html><body>" + strings.Repeat("long page ", 100) + "</body></html>"
	_, v, err := e.svc.Create(e.ctx, alice, artifact.CreateRequest{
		Slug: "types",
		Files: []artifact.FileInput{
			{Path: "data.css", Content: strings.NewReader("{}"), ContentType: "application/json"},
			{Path: "style.css", Content: strings.NewReader("body{}")},
			{Path: "README", Content: strings.NewReader(html)},
			{Path: "pic.unknownext", Content: strings.NewReader(png)},
			{Path: "short", Content: strings.NewReader("hi")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"data.css":       "application/json",
		"style.css":      "text/css; charset=utf-8",
		"README":         "text/html; charset=utf-8",
		"pic.unknownext": "image/png",
		"short":          "text/plain; charset=utf-8",
	}
	got := map[string]string{}
	for _, f := range v.Files {
		got[f.Path] = f.ContentType
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("content types (-want +got):\n%s", diff)
	}
	if body := e.read(t, alice, "types", "README", 0); body != html {
		t.Fatalf("sniffed file stored %d bytes, want %d", len(body), len(html))
	}
	if body := e.read(t, alice, "types", "pic.unknownext", 0); body != png {
		t.Fatalf("sniffed binary stored %d bytes, want %d", len(body), len(png))
	}
}

func TestUpdateAddsVersion(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice, bob := e.user(t, "alice"), e.user(t, "bob")
	e.create(t, alice, "demo", "a.txt", "aaa", "b.txt", "bbb", "c.txt", "ccc")
	e.clock.Advance(time.Minute)

	a, v, err := e.svc.Update(e.ctx, bob, artifact.UpdateRequest{
		Slug:  "demo",
		Label: "second",
		Files: append(files("c.txt", "new c", "d/e.txt", "ddd"), del("b.txt"), del("never-existed")),
	})
	if err != nil {
		t.Fatal(err)
	}
	if v == nil || v.Number != 2 || v.Label != "second" || v.CreatedBy != "bob" || !v.CreatedAt.Equal(e.clock.Now()) {
		t.Fatalf("new version = %+v", v)
	}
	wantFiles := []artifact.File{
		{Path: "a.txt", Hash: sha("aaa"), Size: 3, ContentType: "text/plain; charset=utf-8"},
		{Path: "c.txt", Hash: sha("new c"), Size: 5, ContentType: "text/plain; charset=utf-8"},
		{Path: "d/e.txt", Hash: sha("ddd"), Size: 3, ContentType: "text/plain; charset=utf-8"},
	}
	if diff := cmp.Diff(wantFiles, v.Files); diff != "" {
		t.Fatalf("version 2 files (-want +got):\n%s", diff)
	}
	if v.SizeBytes != 11 {
		t.Fatalf("version 2 size = %d, want 11", v.SizeBytes)
	}
	if a.PublishedVersion != 2 || a.LatestVersion != 2 || a.SizeBytes != 9+11 {
		t.Fatalf("artifact after update: published %d latest %d size %d", a.PublishedVersion, a.LatestVersion, a.SizeBytes)
	}
	if !a.UpdatedAt.Equal(e.clock.Now()) || !a.CreatedAt.Equal(start) {
		t.Fatalf("timestamps after update: created %v updated %v", a.CreatedAt, a.UpdatedAt)
	}
	if body := e.read(t, alice, "demo", "b.txt", 1); body != "bbb" {
		t.Fatalf("v1 b.txt = %q", body)
	}
	if body := e.read(t, alice, "demo", "c.txt", 0); body != "new c" {
		t.Fatalf("published c.txt = %q", body)
	}
	_, _, err = e.svc.OpenFile(e.ctx, alice, "demo", "b.txt", 0)
	wantErr(t, "deleted file in v2", err, artifact.ErrFileNotFound)

	_, _, err = e.svc.Update(e.ctx, alice, artifact.UpdateRequest{
		Slug: "demo", Files: []artifact.FileInput{del("a.txt"), del("c.txt"), del("d/e.txt")},
	})
	wantErr(t, "empty version", err, artifact.ErrInvalidArgument)

	page, err := e.svc.List(e.ctx, bob, artifact.ListQuery{Mine: "updated"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Slug != "demo" {
		t.Fatalf("bob mine=updated = %+v", page.Items)
	}
	if got := e.rec.Events[len(e.rec.Events)-1]; got.Action != audit.ArtifactUpdate || got.Actor != "bob" ||
		got.Details["version"] != "2" || got.Details["size"] != "11" {
		t.Fatalf("update audit = %+v", got)
	}
}

func TestUpdateExpectedVersion(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice := e.user(t, "alice")
	e.create(t, alice, "demo")

	_, _, err := e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "demo", ExpectedVersion: 5, Files: files("x", "x")})
	wantErr(t, "files with stale expectation", err, artifact.ErrVersionConflict)
	_, _, err = e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "demo", ExpectedVersion: 2, Name: ptr("n")})
	wantErr(t, "fields with stale expectation", err, artifact.ErrVersionConflict)
	a, _, _ := e.svc.Get(e.ctx, alice, "demo")
	if a.LatestVersion != 1 || a.Name != "" {
		t.Fatalf("conflicting updates wrote: %+v", a)
	}

	if _, _, err := e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "demo", ExpectedVersion: 1, Files: files("x", "x")}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "demo", ExpectedVersion: 2, Name: ptr("n")}); err != nil {
		t.Fatal(err)
	}

	const n = 6
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, errs[i] = e.svc.Update(e.ctx, alice, artifact.UpdateRequest{
				Slug: "demo", ExpectedVersion: 2, Files: files("race.txt", strings.Repeat("r", i+1)),
			})
		}()
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		if err == nil {
			wins++
		} else if !errors.Is(err, artifact.ErrVersionConflict) {
			t.Errorf("concurrent update: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d concurrent updates won, want 1", wins)
	}
}

func TestFieldsOnlyUpdate(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice := e.user(t, "alice")
	e.create(t, alice, "demo")
	e.clock.Advance(time.Hour)

	a, v, err := e.svc.Update(e.ctx, alice, artifact.UpdateRequest{
		Slug: "demo", Name: ptr("New"), Description: ptr("Desc"), Visibility: ptr(artifact.Unlisted),
	})
	if err != nil {
		t.Fatal(err)
	}
	if v != nil {
		t.Fatalf("fields-only update returned version %+v", v)
	}
	if a.Name != "New" || a.Description != "Desc" || a.Visibility != artifact.Unlisted ||
		a.LatestVersion != 1 || !a.UpdatedAt.Equal(e.clock.Now()) {
		t.Fatalf("after fields-only update: %+v", a)
	}
	a, _, err = e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "demo", Name: ptr("")})
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "" || a.Description != "Desc" {
		t.Fatalf("after clearing name: %+v", a)
	}
	_, _, err = e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "demo", Visibility: ptr(artifact.Visibility("secret"))})
	wantErr(t, "bad visibility", err, artifact.ErrInvalidArgument)
	_, _, err = e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "missing", Name: ptr("x")})
	wantErr(t, "missing artifact", err, artifact.ErrNotFound)
}

func TestMetadataMerge(t *testing.T) {
	e := newEnv(t, artifact.Options{MaxMetadataPairs: 3})
	alice := e.user(t, "alice")
	if _, _, err := e.svc.Create(e.ctx, alice, artifact.CreateRequest{
		Slug: "demo", Metadata: map[string]string{"a": "1", "b": "2"}, Files: files("x", "x"),
	}); err != nil {
		t.Fatal(err)
	}
	a, _, err := e.svc.Update(e.ctx, alice, artifact.UpdateRequest{
		Slug: "demo", Metadata: map[string]*string{"b": nil, "c": ptr("3"), "missing": nil},
	})
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(map[string]string{"a": "1", "c": "3"}, a.Metadata); diff != "" {
		t.Fatalf("merged metadata (-want +got):\n%s", diff)
	}
	_, _, err = e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "demo", Metadata: map[string]*string{"kazari.x": ptr("1")}})
	wantErr(t, "unknown kazari key", err, artifact.ErrInvalidMetadata)
	_, _, err = e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "demo", Metadata: map[string]*string{"d": ptr("4"), "e": ptr("5")}})
	wantErr(t, "pair cap", err, artifact.ErrInvalidMetadata)
	a, _, _ = e.svc.Get(e.ctx, alice, "demo")
	if diff := cmp.Diff(map[string]string{"a": "1", "c": "3"}, a.Metadata); diff != "" {
		t.Fatalf("metadata after rejected updates (-want +got):\n%s", diff)
	}
}

func TestWritePolicy(t *testing.T) {
	tests := []struct {
		policy string
		actor  artifact.Actor
		ok     bool
	}{
		{"", artifact.Actor{Handle: "bob"}, true}, // default is shared
		{artifact.WriteShared, artifact.Actor{Handle: "bob"}, true},
		{artifact.WriteShared, artifact.Actor{Handle: "alice"}, true},
		{artifact.WriteOwner, artifact.Actor{Handle: "alice"}, true},
		{artifact.WriteOwner, artifact.Actor{Handle: "bob"}, false},
		{artifact.WriteOwner, artifact.Actor{Handle: "root", Admin: true}, true},
	}
	for _, tt := range tests {
		t.Run(tt.policy+"/"+tt.actor.Handle, func(t *testing.T) {
			e := newEnv(t, artifact.Options{WritePolicy: tt.policy})
			alice := e.user(t, "alice")
			e.user(t, "bob")
			e.user(t, "root")
			e.create(t, alice, "demo")
			e.create(t, alice, "demo-two")

			mutations := []struct {
				name string
				fn   func() error
			}{
				{"update", func() error {
					_, _, err := e.svc.Update(e.ctx, tt.actor, artifact.UpdateRequest{Slug: "demo", Name: ptr("x")})
					return err
				}},
				{"update files", func() error {
					_, _, err := e.svc.Update(e.ctx, tt.actor, artifact.UpdateRequest{Slug: "demo", Files: files("y", "y")})
					return err
				}},
				{"publish", func() error { _, err := e.svc.Publish(e.ctx, tt.actor, "demo", 1); return err }},
				{"stale", func() error { _, err := e.svc.MarkStale(e.ctx, tt.actor, "demo", 0); return err }},
				{"unstale", func() error { _, err := e.svc.UnmarkStale(e.ctx, tt.actor, "demo"); return err }},
				{"visibility", func() error {
					_, err := e.svc.SetVisibility(e.ctx, tt.actor, "demo", "unlisted")
					return err
				}},
				{"delete", func() error { return e.svc.Delete(e.ctx, tt.actor, "demo-two", false) }},
				{"hard delete", func() error { return e.svc.Delete(e.ctx, tt.actor, "demo-two", true) }},
			}
			for _, m := range mutations {
				name, err := m.name, m.fn()
				if tt.ok && err != nil {
					t.Errorf("%s: %v", name, err)
				}
				if !tt.ok && !errors.Is(err, artifact.ErrForbidden) {
					t.Errorf("%s: err = %v, want ErrForbidden", name, err)
				}
			}
		})
	}
}

func TestStale(t *testing.T) {
	e := newEnv(t, artifact.Options{DefaultGrace: 48 * time.Hour})
	alice := e.user(t, "alice")
	e.create(t, alice, "demo")

	a, err := e.svc.MarkStale(e.ctx, alice, "demo", 0)
	if err != nil {
		t.Fatal(err)
	}
	if a.StaleAt == nil || !a.StaleAt.Equal(start) || a.ExpiresAt == nil || !a.ExpiresAt.Equal(start.Add(48*time.Hour)) {
		t.Fatalf("default grace: stale %v expires %v", a.StaleAt, a.ExpiresAt)
	}
	a, err = e.svc.MarkStale(e.ctx, alice, "demo", 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !a.ExpiresAt.Equal(start.Add(2 * time.Hour)) {
		t.Fatalf("explicit grace: expires %v", a.ExpiresAt)
	}
	p, err := e.svc.ResolvePublished(e.ctx, "demo")
	if err != nil || !p.Servable() {
		t.Fatalf("stale artifact not servable: %+v, %v", p, err)
	}

	a, _, err = e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "demo", KeepStale: true, Files: files("k", "k")})
	if err != nil {
		t.Fatal(err)
	}
	if a.StaleAt == nil || a.ExpiresAt == nil {
		t.Fatalf("KeepStale cleared the timer")
	}
	a, _, err = e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "demo", Name: ptr("n")})
	if err != nil {
		t.Fatal(err)
	}
	if a.StaleAt == nil {
		t.Fatalf("fields-only update cleared the timer")
	}
	a, _, err = e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "demo", Files: files("k", "k2")})
	if err != nil {
		t.Fatal(err)
	}
	if a.StaleAt != nil || a.ExpiresAt != nil {
		t.Fatalf("new version kept the timer: %v %v", a.StaleAt, a.ExpiresAt)
	}

	if _, err := e.svc.MarkStale(e.ctx, alice, "demo", time.Hour); err != nil {
		t.Fatal(err)
	}
	a, err = e.svc.UnmarkStale(e.ctx, alice, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if a.StaleAt != nil || a.ExpiresAt != nil {
		t.Fatalf("UnmarkStale left %v %v", a.StaleAt, a.ExpiresAt)
	}
	want := []string{audit.ArtifactCreate, audit.ArtifactStale, audit.ArtifactStale, audit.ArtifactUpdate,
		audit.ArtifactUpdate, audit.ArtifactUpdate, audit.ArtifactStale, audit.ArtifactUnstale}
	if diff := cmp.Diff(want, e.actions()); diff != "" {
		t.Fatalf("audit actions (-want +got):\n%s", diff)
	}
}

func TestVisibility(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice := e.user(t, "alice")
	e.create(t, alice, "demo")

	a, err := e.svc.SetVisibility(e.ctx, alice, "demo", "unlisted")
	if err != nil {
		t.Fatal(err)
	}
	if a.Visibility != artifact.Unlisted || a.UnpublishedAt != nil {
		t.Fatalf("unlisted: %+v", a)
	}
	e.clock.Advance(time.Minute)
	a, err = e.svc.SetVisibility(e.ctx, alice, "demo", artifact.Unpublished)
	if err != nil {
		t.Fatal(err)
	}
	if a.Visibility != artifact.Unlisted || a.UnpublishedAt == nil || !a.UnpublishedAt.Equal(e.clock.Now()) {
		t.Fatalf("unpublished: %+v", a)
	}
	p, err := e.svc.ResolvePublished(e.ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if p.Servable() || p.UnpublishedAt == nil {
		t.Fatalf("unpublished artifact resolves as servable: %+v", p)
	}
	e.clock.Advance(time.Minute)
	a, _ = e.svc.SetVisibility(e.ctx, alice, "demo", artifact.Unpublished)
	if !a.UnpublishedAt.Equal(start.Add(time.Minute)) {
		t.Fatalf("second unpublish moved UnpublishedAt to %v", a.UnpublishedAt)
	}
	a, err = e.svc.SetVisibility(e.ctx, alice, "demo", "public")
	if err != nil {
		t.Fatal(err)
	}
	if a.Visibility != artifact.Public || a.UnpublishedAt != nil {
		t.Fatalf("public: %+v", a)
	}
	_, err = e.svc.SetVisibility(e.ctx, alice, "demo", "private")
	wantErr(t, "bad visibility", err, artifact.ErrInvalidArgument)

	ev := e.rec.Events[1]
	if ev.Action != audit.ArtifactVisibility || ev.Details["visibility"] != "unlisted" {
		t.Fatalf("visibility audit = %+v", ev)
	}
}

func TestDeleteAndRestore(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice := e.user(t, "alice")
	e.create(t, alice, "demo", "index.html", "keep me")

	if err := e.svc.Delete(e.ctx, alice, "demo", false); err != nil {
		t.Fatal(err)
	}
	a, _, err := e.svc.Get(e.ctx, alice, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if a.DeletedAt == nil || !a.DeletedAt.Equal(start) {
		t.Fatalf("DeletedAt = %v", a.DeletedAt)
	}
	e.clock.Advance(time.Minute)
	if err := e.svc.Delete(e.ctx, alice, "demo", false); err != nil {
		t.Fatal(err)
	}
	if a, _, _ := e.svc.Get(e.ctx, alice, "demo"); !a.DeletedAt.Equal(start) {
		t.Fatalf("second soft delete moved DeletedAt to %v", a.DeletedAt)
	}

	_, _, err = e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "demo", Name: ptr("x")})
	wantErr(t, "update deleted", err, artifact.ErrNotFound)
	_, err = e.svc.MarkStale(e.ctx, alice, "demo", 0)
	wantErr(t, "stale deleted", err, artifact.ErrNotFound)
	_, err = e.svc.Publish(e.ctx, alice, "demo", 1)
	wantErr(t, "publish deleted", err, artifact.ErrNotFound)
	page, err := e.svc.List(e.ctx, alice, artifact.ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("deleted artifact listed: %+v", page.Items)
	}
	if body := e.read(t, alice, "demo", "index.html", 0); body != "keep me" {
		t.Fatalf("recovered content = %q", body)
	}
	p, err := e.svc.ResolvePublished(e.ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if p.Servable() || p.DeletedAt == nil {
		t.Fatalf("deleted artifact resolves as servable: %+v", p)
	}

	a, _, err = e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "demo", Restore: true, Name: ptr("back")})
	if err != nil {
		t.Fatal(err)
	}
	if a.DeletedAt != nil || a.Name != "back" {
		t.Fatalf("after restore: %+v", a)
	}
	want := []string{audit.ArtifactCreate, audit.ArtifactDelete, audit.ArtifactRestore, audit.ArtifactUpdate}
	if diff := cmp.Diff(want, e.actions()); diff != "" {
		t.Fatalf("audit actions (-want +got):\n%s", diff)
	}

	if err := e.svc.Delete(e.ctx, alice, "demo", true); err != nil {
		t.Fatal(err)
	}
	_, _, err = e.svc.Get(e.ctx, alice, "demo")
	wantErr(t, "Get purged", err, artifact.ErrGone)
	_, _, err = e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "demo", Restore: true})
	wantErr(t, "restore purged", err, artifact.ErrGone)
	_, _, err = e.svc.OpenFile(e.ctx, alice, "demo", "index.html", 0)
	wantErr(t, "OpenFile purged", err, artifact.ErrGone)
	wantErr(t, "delete purged", e.svc.Delete(e.ctx, alice, "demo", false), artifact.ErrGone)
	p, err = e.svc.ResolvePublished(e.ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if p.PurgedAt == nil || p.VersionID != 0 {
		t.Fatalf("purged resolve: %+v", p)
	}
	_, _, err = e.svc.Create(e.ctx, alice, artifact.CreateRequest{Slug: "demo", Files: files("a", "a")})
	wantErr(t, "reuse tombstoned slug", err, artifact.ErrSlugTaken)
	if got := e.actions(); got[len(got)-1] != audit.ArtifactPurge {
		t.Fatalf("last audit action = %s, want purge", got[len(got)-1])
	}
}

func TestDeleteSoftDeletedArtifactHard(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice := e.user(t, "alice")
	e.create(t, alice, "demo")
	if err := e.svc.Delete(e.ctx, alice, "demo", false); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Delete(e.ctx, alice, "demo", true); err != nil {
		t.Fatalf("hard delete of a soft-deleted artifact: %v", err)
	}
	a, err := e.store.Get(e.ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if a.PurgedAt == nil {
		t.Fatal("not purged")
	}
}

func TestPublishRollback(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice := e.user(t, "alice")
	e.create(t, alice, "demo", "index.html", "one")
	if _, _, err := e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "demo", Files: files("index.html", "two")}); err != nil {
		t.Fatal(err)
	}

	a, err := e.svc.Publish(e.ctx, alice, "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if a.PublishedVersion != 1 || a.LatestVersion != 2 {
		t.Fatalf("after rollback: published %d latest %d", a.PublishedVersion, a.LatestVersion)
	}
	if body := e.read(t, alice, "demo", "index.html", 0); body != "one" {
		t.Fatalf("served after rollback = %q", body)
	}
	p, err := e.svc.ResolvePublished(e.ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if p.VersionNumber != 1 {
		t.Fatalf("resolved version = %d", p.VersionNumber)
	}
	if _, err := e.svc.Publish(e.ctx, alice, "demo", 2); err != nil {
		t.Fatal(err)
	}
	if body := e.read(t, alice, "demo", "index.html", 0); body != "two" {
		t.Fatalf("served after roll-forward = %q", body)
	}
	for _, n := range []int{0, -1, 3} {
		_, err := e.svc.Publish(e.ctx, alice, "demo", n)
		wantErr(t, "publish missing version", err, artifact.ErrVersionNotFound)
	}
	ev := e.rec.Events[2]
	if ev.Action != audit.ArtifactPublish || ev.Details["version"] != "1" {
		t.Fatalf("publish audit = %+v", ev)
	}
}

func TestOpenFile(t *testing.T) {
	e := newEnv(t, artifact.Options{WritePolicy: artifact.WriteOwner})
	alice, bob := e.user(t, "alice"), e.user(t, "bob")
	e.create(t, alice, "demo", "index.html", "v1", "css/a.css", "a{}")
	if _, _, err := e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "demo", Files: files("index.html", "v2")}); err != nil {
		t.Fatal(err)
	}

	f, rc, err := e.svc.OpenFile(e.ctx, bob, "demo", "./css//a.css", 0)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if f.Path != "css/a.css" || f.ContentType != "text/css; charset=utf-8" || string(b) != "a{}" {
		t.Fatalf("OpenFile = %+v %q", f, b)
	}
	if body := e.read(t, bob, "demo", "index.html", 0); body != "v2" {
		t.Fatalf("published = %q", body)
	}
	if body := e.read(t, bob, "demo", "index.html", 1); body != "v1" {
		t.Fatalf("pinned v1 = %q", body)
	}

	_, rc, err = e.svc.OpenFile(e.ctx, bob, "demo", "index.html", 1)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := rc.Seek(0, io.SeekEnd); err != nil || n != 2 {
		t.Fatalf("Seek = %d, %v", n, err)
	}
	rc.Close()

	_, _, err = e.svc.OpenFile(e.ctx, bob, "demo", "nope.html", 0)
	wantErr(t, "missing file", err, artifact.ErrFileNotFound)
	_, _, err = e.svc.OpenFile(e.ctx, bob, "demo", "index.html", 9)
	wantErr(t, "missing version", err, artifact.ErrVersionNotFound)
	_, _, err = e.svc.OpenFile(e.ctx, bob, "demo", "../x", 0)
	wantErr(t, "bad path", err, artifact.ErrInvalidPath)
	_, _, err = e.svc.OpenFile(e.ctx, bob, "missing", "index.html", 0)
	wantErr(t, "missing artifact", err, artifact.ErrNotFound)

	if err := e.svc.Delete(e.ctx, alice, "demo", false); err != nil {
		t.Fatal(err)
	}
	_, _, err = e.svc.OpenFile(e.ctx, bob, "demo", "index.html", 0)
	wantErr(t, "stranger reads deleted", err, artifact.ErrNotFound)
	if body := e.read(t, alice, "demo", "index.html", 0); body != "v2" {
		t.Fatalf("owner reads deleted = %q", body)
	}
}

func TestResolveDerivesFlags(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice := e.user(t, "alice")
	_, v, err := e.svc.Create(e.ctx, alice, artifact.CreateRequest{
		Slug: "spa",
		Metadata: map[string]string{
			artifact.MetaSPA: "true", artifact.MetaIndexable: "true", artifact.MetaCSP: "default-src 'self'",
		},
		Files: files("index.html", "<div id=app>"),
	})
	if err != nil {
		t.Fatal(err)
	}
	e.create(t, alice, "plain")

	p, err := e.svc.ResolvePublished(e.ctx, "spa")
	if err != nil {
		t.Fatal(err)
	}
	if !p.SPA || !p.Indexable || p.CSP != "default-src 'self'" || p.VersionID != v.ID || !p.Servable() {
		t.Fatalf("ResolvePublished = %+v", p)
	}
	p, err = e.svc.ResolveVersion(e.ctx, "spa", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !p.SPA || p.VersionNumber != 1 {
		t.Fatalf("ResolveVersion = %+v", p)
	}
	p, err = e.svc.ResolvePublished(e.ctx, "plain")
	if err != nil {
		t.Fatal(err)
	}
	if p.SPA || p.Indexable || p.CSP != "" {
		t.Fatalf("plain flags = %+v", p)
	}
	fm, err := e.svc.Files(e.ctx, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if f, ok := fm["index.html"]; !ok || f.Hash != sha("<div id=app>") {
		t.Fatalf("Files = %+v", fm)
	}
	_, err = e.svc.ResolveVersion(e.ctx, "spa", 2)
	wantErr(t, "ResolveVersion missing", err, artifact.ErrVersionNotFound)
	_, err = e.svc.ResolvePublished(e.ctx, "nope")
	wantErr(t, "ResolvePublished missing", err, artifact.ErrNotFound)
}

func TestList(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice, bob := e.user(t, "alice"), e.user(t, "bob")
	admin := artifact.Actor{Handle: "root", Admin: true}
	e.user(t, "root")
	e.create(t, alice, "pub-a")
	e.clock.Advance(time.Second)
	if _, _, err := e.svc.Create(e.ctx, alice, artifact.CreateRequest{
		Slug: "hidden-a", Visibility: artifact.Unlisted, Files: files("x", "x"),
	}); err != nil {
		t.Fatal(err)
	}

	list := func(actor artifact.Actor, q artifact.ListQuery) []string {
		t.Helper()
		page, err := e.svc.List(e.ctx, actor, q)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, a := range page.Items {
			out = append(out, a.Slug)
		}
		return out
	}
	if got := list(bob, artifact.ListQuery{All: true}); !slices.Equal(got, []string{"pub-a"}) {
		t.Fatalf("non-admin All = %v, want [pub-a]", got)
	}
	if got := list(admin, artifact.ListQuery{All: true}); !slices.Equal(got, []string{"hidden-a", "pub-a"}) {
		t.Fatalf("admin All = %v", got)
	}
	if got := list(bob, artifact.ListQuery{Viewer: "alice"}); !slices.Equal(got, []string{"pub-a"}) {
		t.Fatalf("spoofed viewer = %v", got)
	}
	if got := list(alice, artifact.ListQuery{Mine: "created"}); !slices.Equal(got, []string{"hidden-a", "pub-a"}) {
		t.Fatalf("alice mine=created = %v", got)
	}
	page, err := e.svc.List(e.ctx, alice, artifact.ListQuery{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.NextCursor == "" {
		t.Fatalf("limit 1: %d items, cursor %q", len(page.Items), page.NextCursor)
	}
	if _, err := e.svc.List(e.ctx, alice, artifact.ListQuery{Limit: 100000}); err != nil {
		t.Fatalf("huge limit is capped, not rejected: %v", err)
	}

	for _, q := range []artifact.ListQuery{
		{Mine: "yours"},
		{Stale: "sometimes"},
		{Deleted: "maybe"},
		{Metadata: map[string]string{"bad key": "x"}},
	} {
		_, err := e.svc.List(e.ctx, alice, q)
		if !errors.Is(err, artifact.ErrInvalidArgument) && !errors.Is(err, artifact.ErrInvalidMetadata) {
			t.Errorf("List(%+v): err = %v, want a validation error", q, err)
		}
	}
}

func TestSweepLifecycle(t *testing.T) {
	e := newEnv(t, artifact.Options{Retention: 24 * time.Hour, Tombstone: 48 * time.Hour})
	alice := e.user(t, "alice")
	e.create(t, alice, "doomed")
	e.create(t, alice, "keeper")
	if _, err := e.svc.MarkStale(e.ctx, alice, "doomed", time.Hour); err != nil {
		t.Fatal(err)
	}
	sweep := func(want artifact.SweepReport) {
		t.Helper()
		got, err := e.svc.Sweep(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("Sweep = %+v, want %+v", got, want)
		}
	}

	e.clock.Advance(30 * time.Minute)
	sweep(artifact.SweepReport{})

	e.clock.Advance(31 * time.Minute)
	sweep(artifact.SweepReport{SoftDeleted: 1})
	a, _, err := e.svc.Get(e.ctx, alice, "doomed")
	if err != nil {
		t.Fatal(err)
	}
	if a.DeletedAt == nil || !a.DeletedAt.Equal(e.clock.Now()) {
		t.Fatalf("DeletedAt = %v, want %v", a.DeletedAt, e.clock.Now())
	}
	p, _ := e.svc.ResolvePublished(e.ctx, "doomed")
	if p.Servable() {
		t.Fatal("expired artifact still servable")
	}
	sweep(artifact.SweepReport{})

	e.clock.Advance(24 * time.Hour)
	sweep(artifact.SweepReport{Purged: 1})
	_, _, err = e.svc.Get(e.ctx, alice, "doomed")
	wantErr(t, "Get after purge", err, artifact.ErrGone)

	e.clock.Advance(47 * time.Hour)
	sweep(artifact.SweepReport{})
	e.clock.Advance(time.Hour)
	sweep(artifact.SweepReport{Freed: 1})
	_, err = e.store.Get(e.ctx, "doomed")
	wantErr(t, "store Get after free", err, artifact.ErrNotFound)
	e.create(t, alice, "doomed")

	if a, _, err := e.svc.Get(e.ctx, alice, "keeper"); err != nil || a.DeletedAt != nil {
		t.Fatalf("keeper: %+v, %v", a, err)
	}

	var sys []audit.Event
	for _, ev := range e.rec.Events {
		if ev.Actor == artifact.System.Handle {
			sys = append(sys, ev)
		}
	}
	wantSys := []struct{ action, reason string }{
		{audit.ArtifactDelete, "expired"},
		{audit.ArtifactPurge, "retention"},
		{audit.ArtifactFree, ""},
	}
	if len(sys) != len(wantSys) {
		t.Fatalf("scheduler events = %+v", sys)
	}
	for i, w := range wantSys {
		if sys[i].Action != w.action || sys[i].Slug != "doomed" || sys[i].Details["reason"] != w.reason {
			t.Errorf("scheduler event %d = %+v, want %s %q", i, sys[i], w.action, w.reason)
		}
	}
}

func TestCollectGarbage(t *testing.T) {
	e := newEnv(t, artifact.Options{BlobGrace: time.Hour, MaxFileBytes: 100})
	alice := e.user(t, "alice")

	e.create(t, alice, "kept", "f.txt", "old content")
	if _, _, err := e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "kept", Files: files("f.txt", "new content")}); err != nil {
		t.Fatal(err)
	}
	e.create(t, alice, "purged", "p.txt", "purged content")
	if err := e.svc.Delete(e.ctx, alice, "purged", true); err != nil {
		t.Fatal(err)
	}
	_, _, err := e.svc.Create(e.ctx, alice, artifact.CreateRequest{
		Slug: "failed", Files: files("ok.txt", "orphan content", "big.txt", strings.Repeat("x", 101)),
	})
	wantErr(t, "failed create", err, artifact.ErrTooLarge)
	e.create(t, alice, "shared-live", "s.txt", "shared content")
	e.create(t, alice, "shared-dead", "s.txt", "shared content")
	if err := e.svc.Delete(e.ctx, alice, "shared-dead", true); err != nil {
		t.Fatal(err)
	}

	stat := func(content string) bool {
		t.Helper()
		_, err := e.blobs.Stat(e.ctx, sha(content))
		if err != nil && !errors.Is(err, blob.ErrNotFound) {
			t.Fatal(err)
		}
		return err == nil
	}

	n, err := e.svc.CollectGarbage(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("GC inside the grace window deleted %d blobs", n)
	}

	e.clock.Advance(2 * time.Hour)
	n, err = e.svc.CollectGarbage(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("GC deleted %d blobs, want 2", n)
	}
	for content, want := range map[string]bool{
		"old content":    true,
		"new content":    true,
		"shared content": true,
		"purged content": false,
		"orphan content": false,
	} {
		if got := stat(content); got != want {
			t.Errorf("blob %q present = %v, want %v", content, got, want)
		}
	}
	if n, err := e.svc.CollectGarbage(e.ctx); err != nil || n != 0 {
		t.Fatalf("second GC = %d, %v", n, err)
	}

	e.create(t, alice, "again", "p.txt", "purged content")
	if !stat("purged content") {
		t.Fatal("re-put blob missing")
	}
	if body := e.read(t, alice, "again", "p.txt", 0); body != "purged content" {
		t.Fatalf("re-put content = %q", body)
	}
}

func TestAuditTrail(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice := e.user(t, "alice")
	steps := []func() error{
		func() error {
			_, _, err := e.svc.Create(e.ctx, alice, artifact.CreateRequest{Slug: "demo", Files: files("a", "a")})
			return err
		},
		func() error {
			_, _, err := e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "demo", Files: files("b", "b")})
			return err
		},
		func() error { _, err := e.svc.Publish(e.ctx, alice, "demo", 1); return err },
		func() error { _, err := e.svc.MarkStale(e.ctx, alice, "demo", 0); return err },
		func() error { _, err := e.svc.UnmarkStale(e.ctx, alice, "demo"); return err },
		func() error { _, err := e.svc.SetVisibility(e.ctx, alice, "demo", "unpublished"); return err },
		func() error { return e.svc.Delete(e.ctx, alice, "demo", false) },
		func() error {
			_, _, err := e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "demo", Restore: true})
			return err
		},
		func() error { return e.svc.Delete(e.ctx, alice, "demo", true) },
	}
	for i, step := range steps {
		e.clock.Advance(time.Second)
		if err := step(); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	want := []string{
		audit.ArtifactCreate, audit.ArtifactUpdate, audit.ArtifactPublish, audit.ArtifactStale,
		audit.ArtifactUnstale, audit.ArtifactVisibility, audit.ArtifactDelete, audit.ArtifactRestore,
		audit.ArtifactUpdate, audit.ArtifactPurge,
	}
	if diff := cmp.Diff(want, e.actions()); diff != "" {
		t.Fatalf("audit actions (-want +got):\n%s", diff)
	}
	for i, ev := range e.rec.Events {
		if ev.Actor != "alice" || ev.Slug != "demo" || ev.At.IsZero() {
			t.Errorf("event %d = %+v", i, ev)
		}
	}
}

type failingRecorder struct{}

func (failingRecorder) Record(context.Context, audit.Event) error { return errors.New("audit down") }

func TestAuditFailureDoesNotFailOperation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := sqlite.Open(ctx, filepath.Join(dir, "kazari.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `INSERT INTO user (handle, role, quota_bytes, created_at) VALUES ('alice', 'member', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	blobs, err := blob.NewFS(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	svc := artifact.New(sqlite.NewArtifactStore(db), blobs, failingRecorder{}, artifact.Options{
		Logger: slog.New(slog.NewTextHandler(&logs, nil)),
	})
	if _, _, err := svc.Create(ctx, artifact.Actor{Handle: "alice"}, artifact.CreateRequest{Slug: "demo", Files: files("a", "a")}); err != nil {
		t.Fatalf("Create with failing audit: %v", err)
	}
	if !strings.Contains(logs.String(), "audit down") {
		t.Fatalf("audit failure not logged: %q", logs.String())
	}
}

func TestTransfer(t *testing.T) {
	e := newEnv(t, artifact.Options{WritePolicy: artifact.WriteShared})
	alice := e.user(t, "alice")
	bob := e.user(t, "bob")
	root := artifact.Actor{Handle: "root", Admin: true}
	e.create(t, alice, "demo")

	_, err := e.svc.Transfer(e.ctx, bob, "demo", "bob")
	wantErr(t, "transfer by non-owner", err, artifact.ErrForbidden)

	_, err = e.svc.Transfer(e.ctx, root, "demo", "nobody")
	wantErr(t, "transfer to unknown user", err, artifact.ErrInvalidArgument)

	_, err = e.svc.Transfer(e.ctx, root, "demo", "")
	wantErr(t, "transfer to empty handle", err, artifact.ErrInvalidArgument)

	_, err = e.svc.Transfer(e.ctx, root, "missing", "bob")
	wantErr(t, "transfer of unknown slug", err, artifact.ErrNotFound)

	e.clock.Advance(time.Minute)
	a, err := e.svc.Transfer(e.ctx, root, "demo", "bob")
	if err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	if a.OwnerHandle != "bob" || !a.UpdatedAt.Equal(start.Add(time.Minute)) {
		t.Errorf("after transfer: owner %q, updated %v", a.OwnerHandle, a.UpdatedAt)
	}
	ev := e.rec.Events[len(e.rec.Events)-1]
	if ev.Action != audit.ArtifactTransfer || ev.Actor != "root" || ev.Slug != "demo" ||
		ev.Details["from"] != "alice" || ev.Details["to"] != "bob" {
		t.Errorf("audit event = %+v", ev)
	}

	_, err = e.svc.Transfer(e.ctx, alice, "demo", "alice")
	wantErr(t, "transfer by previous owner", err, artifact.ErrForbidden)
	if _, err := e.svc.Transfer(e.ctx, bob, "demo", "alice"); err != nil {
		t.Fatalf("Transfer by owner: %v", err)
	}

	page, err := e.svc.List(e.ctx, root, artifact.ListQuery{All: true, Owner: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Slug != "demo" {
		t.Errorf("owner=alice listing = %+v", page.Items)
	}
}

func TestContentTypeValidation(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice := e.user(t, "alice")
	tests := []struct {
		name string
		ct   string
		ok   bool
	}{
		{"empty derives", "", true},
		{"plain", "text/html", true},
		{"with params", "text/plain; charset=utf-8", true},
		{"CRLF injection", "text/html\r\nSet-Cookie: a=b", false},
		{"NUL", "text/html\x00", false},
		{"non-ASCII", "text/html; charset=é", false},
		{"not a media type", "no slash here;;", false},
		{"too long", "text/" + strings.Repeat("a", artifact.ContentTypeMaxLen), false},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := e.svc.Create(e.ctx, alice, artifact.CreateRequest{
				Slug:  fmt.Sprintf("ct-%d", i),
				Files: []artifact.FileInput{{Path: "index.html", Content: strings.NewReader("x"), ContentType: tt.ct}},
			})
			if tt.ok && err != nil {
				t.Fatalf("Create: %v", err)
			}
			if !tt.ok && !errors.Is(err, artifact.ErrInvalidArgument) {
				t.Fatalf("Create err = %v, want invalid_argument", err)
			}
		})
	}
}

func TestTextFieldLimits(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice := e.user(t, "alice")
	e.create(t, alice, "demo")
	longName := strings.Repeat("n", artifact.NameMaxBytes+1)
	longDesc := strings.Repeat("d", artifact.DescriptionMaxBytes+1)
	longLabel := strings.Repeat("l", artifact.LabelMaxBytes+1)

	creates := map[string]artifact.CreateRequest{
		"name":        {Slug: "new-name", Name: longName},
		"description": {Slug: "new-desc", Description: longDesc},
		"label":       {Slug: "new-label", Label: longLabel},
		"bidi name":   {Slug: "new-bidi", Name: "invoice\u202efdp.exe"},
	}
	for what, req := range creates {
		big := &countingReader{r: strings.NewReader("<p>content</p>")}
		req.Files = []artifact.FileInput{{Path: "index.html", Content: big}}
		_, _, err := e.svc.Create(e.ctx, alice, req)
		wantErr(t, "Create with bad "+what, err, artifact.ErrInvalidArgument)
		if big.n != 0 {
			t.Errorf("Create with bad %s read %d bytes of content", what, big.n)
		}
		if _, err := e.store.Get(e.ctx, req.Slug); !errors.Is(err, artifact.ErrNotFound) {
			t.Errorf("Create with bad %s left a row: %v", what, err)
		}
	}

	updates := map[string]artifact.UpdateRequest{
		"name":               {Slug: "demo", Name: ptr(longName)},
		"description":        {Slug: "demo", Description: ptr(longDesc)},
		"name with files":    {Slug: "demo", Name: ptr("a\nb"), Files: files("x.txt", "x")},
		"label":              {Slug: "demo", Label: longLabel, Files: files("x.txt", "x")},
		"label, fields only": {Slug: "demo", Label: "v2\x00"},
	}
	for what, req := range updates {
		_, _, err := e.svc.Update(e.ctx, alice, req)
		wantErr(t, "Update with bad "+what, err, artifact.ErrInvalidArgument)
	}
	a, _, err := e.svc.Get(e.ctx, alice, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "" || a.Description != "" || a.LatestVersion != 1 {
		t.Fatalf("rejected updates changed demo: %+v", a)
	}

	desc := strings.Repeat("d", artifact.DescriptionMaxBytes-2) + "\n\t"
	a, v, err := e.svc.Update(e.ctx, alice, artifact.UpdateRequest{
		Slug: "demo", Name: ptr(longName[1:]), Description: ptr(desc), Label: longLabel[1:], Files: files("x.txt", "x"),
	})
	if err != nil {
		t.Fatalf("Update at the limits: %v", err)
	}
	if a.Name != longName[1:] || a.Description != desc || v.Label != longLabel[1:] {
		t.Fatalf("Update at the limits stored name %d, description %d, label %d bytes", len(a.Name), len(a.Description), len(v.Label))
	}
}

func TestGetAndOpenFileHideOfflineArtifacts(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice, bob, carol, dave := e.user(t, "alice"), e.user(t, "bob"), e.user(t, "carol"), e.user(t, "dave")
	root := artifact.Actor{Handle: "root", Admin: true}
	e.create(t, alice, "demo", "index.html", "hi")
	if _, _, err := e.svc.Update(e.ctx, bob, artifact.UpdateRequest{Slug: "demo", Name: ptr("by bob")}); err != nil {
		t.Fatal(err)
	}

	readable := func(actor artifact.Actor) bool {
		t.Helper()
		var oks []bool
		_, _, err := e.svc.Get(e.ctx, actor, "demo")
		errs := []error{err}
		for _, version := range []int{0, 1} {
			_, rc, err := e.svc.OpenFile(e.ctx, actor, "demo", "index.html", version)
			if err == nil {
				rc.Close()
			}
			errs = append(errs, err)
		}
		for _, err := range errs {
			if err != nil && !errors.Is(err, artifact.ErrNotFound) {
				t.Fatalf("read as %q: %v", actor.Handle, err)
			}
			oks = append(oks, err == nil)
		}
		if !slices.Equal(oks, []bool{oks[0], oks[0], oks[0]}) {
			t.Fatalf("Get and OpenFile disagree for %q: %v", actor.Handle, errs)
		}
		return oks[0]
	}
	want := func(state string, wants map[string]bool) {
		t.Helper()
		for _, actor := range []artifact.Actor{alice, bob, carol, dave, root, {}} {
			if got := readable(actor); got != wants[actor.Handle] {
				t.Errorf("%s: readable by %q = %v, want %v", state, actor.Handle, got, wants[actor.Handle])
			}
		}
	}
	everyone := map[string]bool{"alice": true, "bob": true, "carol": true, "dave": true, "root": true, "": true}

	if _, err := e.svc.SetVisibility(e.ctx, alice, "demo", string(artifact.Unlisted)); err != nil {
		t.Fatal(err)
	}
	want("unlisted", everyone)

	if _, err := e.svc.SetVisibility(e.ctx, alice, "demo", artifact.Unpublished); err != nil {
		t.Fatal(err)
	}
	want("unpublished", map[string]bool{"alice": true, "bob": true, "root": true})

	if _, err := e.svc.Transfer(e.ctx, root, "demo", "dave"); err != nil {
		t.Fatal(err)
	}
	want("transferred", map[string]bool{"alice": true, "bob": true, "dave": true, "root": true})

	if _, err := e.svc.SetVisibility(e.ctx, dave, "demo", string(artifact.Public)); err != nil {
		t.Fatal(err)
	}
	want("republished", everyone)

	if err := e.svc.Delete(e.ctx, dave, "demo", false); err != nil {
		t.Fatal(err)
	}
	want("soft-deleted", map[string]bool{"alice": true, "bob": true, "dave": true, "root": true})
}

func TestAuditRecordsCredential(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	e.user(t, "alice")
	alice := artifact.Actor{Handle: "alice", Credential: "api_key:kzk_abc123"}
	steps := []func() error{
		func() error {
			_, _, err := e.svc.Create(e.ctx, alice, artifact.CreateRequest{Slug: "demo", Files: files("a", "a")})
			return err
		},
		func() error {
			_, _, err := e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "demo", Name: ptr("n")})
			return err
		},
		func() error { _, err := e.svc.Publish(e.ctx, alice, "demo", 1); return err },
		func() error { _, err := e.svc.MarkStale(e.ctx, alice, "demo", time.Hour); return err },
	}
	for i, step := range steps {
		if err := step(); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	e.clock.Advance(2 * time.Hour)
	if _, err := e.svc.Sweep(e.ctx); err != nil {
		t.Fatal(err)
	}

	if len(e.rec.Events) != len(steps)+1 {
		t.Fatalf("events = %v", e.actions())
	}
	for _, ev := range e.rec.Events[:len(steps)] {
		if ev.Details["credential"] != "api_key:kzk_abc123" {
			t.Errorf("%s event details = %v, want the credential", ev.Action, ev.Details)
		}
	}
	if d := e.rec.Events[0].Details; d["version"] != "1" || d["size"] != "1" {
		t.Errorf("create details = %v", d)
	}
	if sys := e.rec.Events[len(steps)]; sys.Actor != artifact.System.Handle {
		t.Errorf("last event = %+v, want the sweep's", sys)
	} else if _, ok := sys.Details["credential"]; ok {
		t.Errorf("scheduler event has a credential: %v", sys.Details)
	}
}
