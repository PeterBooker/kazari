package artifact_test

import (
	"errors"
	"testing"

	"github.com/PeterBooker/kazari/internal/artifact"
)

func TestUpdateAfterRollback(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice := e.user(t, "alice")
	e.create(t, alice, "demo", "index.html", "v1", "a.txt", "a1")
	if _, _, err := e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "demo", Files: files("index.html", "v2")}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Publish(e.ctx, alice, "demo", 1); err != nil {
		t.Fatal(err)
	}

	_, _, err := e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "demo", Files: files("b.txt", "b")})
	if !errors.Is(err, artifact.ErrPublishedNotLatest) {
		t.Fatalf("Update after a rollback without base_version: err = %v, want published_not_latest", err)
	}
	a, v, err := e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "demo", BaseVersion: 1, Files: files("b.txt", "b")})
	if err != nil {
		t.Fatal(err)
	}
	if v.Number != 3 || a.PublishedVersion != 3 {
		t.Fatalf("Update on base 1 = version %d, published %d; want 3, 3", v.Number, a.PublishedVersion)
	}
	if got := e.read(t, alice, "demo", "index.html", 3); got != "v1" {
		t.Errorf("Update on base 1: index.html = %q, want the published %q", got, "v1")
	}

	if _, _, err := e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "demo", BaseVersion: 9, Files: files("c.txt", "c")}); !errors.Is(err, artifact.ErrVersionNotFound) {
		t.Errorf("Update on base 9: err = %v, want version_not_found", err)
	}
	if _, _, err := e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "demo", Name: new("Demo")}); err != nil {
		t.Errorf("Update of a field after a rollback: err = %v, want nil", err)
	}
}

func TestUpdateUnchanged(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice := e.user(t, "alice")
	e.create(t, alice, "demo", "index.html", "v1")

	a, v, err := e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "demo", Name: new("Demo"), Files: files("index.html", "v1")})
	if err != nil {
		t.Fatal(err)
	}
	if !v.Unchanged || v.Number != 1 || a.LatestVersion != 1 {
		t.Errorf("Update with the same files = version %d (unchanged %v), latest %d; want version 1, unchanged, latest 1", v.Number, v.Unchanged, a.LatestVersion)
	}
	if a.Name != "Demo" {
		t.Errorf("Update with the same files: name = %q, want the new %q", a.Name, "Demo")
	}
	if _, _, err := e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "demo", Files: []artifact.FileInput{del("nope.html")}}); !errors.Is(err, artifact.ErrFileNotFound) {
		t.Errorf("Update deleting a missing file: err = %v, want file_not_found", err)
	}
}

func TestUpdateDefaultLabel(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice := e.user(t, "alice")
	e.create(t, alice, "demo", "index.html", "v1", "old.css", "x")

	_, v, err := e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "demo",
		Files: append(files("index.html", "v2", "logo.png", "png"), del("old.css"))})
	if err != nil {
		t.Fatal(err)
	}
	if want := "~index.html +logo.png -old.css"; v.Label != want {
		t.Errorf("default label = %q, want %q", v.Label, want)
	}
	_, v, err = e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "demo", Label: "mine", Files: files("index.html", "v3")})
	if err != nil {
		t.Fatal(err)
	}
	if v.Label != "mine" {
		t.Errorf("given label = %q, want %q", v.Label, "mine")
	}
}
