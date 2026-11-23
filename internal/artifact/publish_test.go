package artifact_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/textedit"
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

// Edits to several files in one update make one version, built on the base
// version's text of each file.
func TestUpdateWithEdits(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice := e.user(t, "alice")
	e.create(t, alice, "demo", "index.html", page, "style.css", "body { color: red; }", "logo.png", "\x89PNG\x00")

	_, v, err := e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "demo", ExpectedVersion: 1, Files: []artifact.FileInput{
		{Path: "index.html", Edits: []textedit.Edit{{OldText: "Q3", NewText: "Q4"}}},
		{Path: "/style.css", Edits: []textedit.Edit{{OldText: "red", NewText: "blue"}}},
		{Path: "about.html", Content: strings.NewReader("<p>about</p>")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if v.Number != 2 {
		t.Fatalf("Update with edits = version %d, want 2", v.Number)
	}
	if got := e.read(t, alice, "demo", "index.html", 2); got != strings.Replace(page, "Q3", "Q4", 1) {
		t.Errorf("index.html = %q", got)
	}
	if got := e.read(t, alice, "demo", "style.css", 2); got != "body { color: blue; }" {
		t.Errorf("style.css = %q", got)
	}
	var paths []string
	for _, fe := range v.Edited {
		paths = append(paths, fe.Path)
		if len(fe.Outcomes) != 1 || fe.Outcomes[0].Match != textedit.MatchExact {
			t.Errorf("outcomes of %s = %+v", fe.Path, fe.Outcomes)
		}
	}
	if diff := cmp.Diff([]string{"index.html", "style.css"}, paths); diff != "" {
		t.Errorf("edited paths (-want +got):\n%s", diff)
	}

	cases := []struct {
		name string
		in   artifact.FileInput
		want *artifact.Error
	}{
		{"missing file", artifact.FileInput{Path: "nope.html", Edits: []textedit.Edit{{OldText: "a", NewText: "b"}}}, artifact.ErrFileNotFound},
		{"binary file", artifact.FileInput{Path: "logo.png", Edits: []textedit.Edit{{OldText: "PNG", NewText: "GIF"}}}, artifact.ErrInvalidArgument},
		{"text not found", artifact.FileInput{Path: "index.html", Edits: []textedit.Edit{{OldText: "Q3", NewText: "Q1"}}}, textedit.ErrNotFound},
		{"edits and content", artifact.FileInput{Path: "index.html", Content: strings.NewReader("x"),
			Edits: []textedit.Edit{{OldText: "Q4", NewText: "Q1"}}}, artifact.ErrInvalidArgument},
	}
	for _, c := range cases {
		_, _, err := e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "demo", Files: []artifact.FileInput{c.in}})
		if !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %s", c.name, err, c.want.Code)
		}
	}
	if _, _, err := e.svc.Create(e.ctx, alice, artifact.CreateRequest{Slug: "fresh", Files: []artifact.FileInput{
		{Path: "index.html", Edits: []textedit.Edit{{OldText: "a", NewText: "b"}}}}}); !errors.Is(err, artifact.ErrInvalidArgument) {
		t.Errorf("Create with edits: err = %v, want invalid_argument", err)
	}
}

// A conflict says whether the edits still apply to the newer version, and
// the number to resend with, so the caller need not read again.
func TestEditConflictSaysWhetherEditsApply(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice := e.user(t, "alice")
	e.create(t, alice, "demo", "index.html", page)
	if _, _, _, err := e.svc.Edit(e.ctx, alice, artifact.EditRequest{Slug: "demo", Path: "index.html",
		Edits: []textedit.Edit{{OldText: "Revenue grew.", NewText: "Revenue doubled."}}}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		edits []textedit.Edit
		want  string
	}{
		{"still applies", []textedit.Edit{{OldText: "Costs fell.", NewText: "Costs held."}},
			"latest version is 2, expected 1; every edit still applies to version 2 as sent: resend the same edits with expected_version 2"},
		{"no longer applies", []textedit.Edit{{OldText: "Revenue grew.", NewText: "Revenue fell."}},
			"latest version is 2, expected 1; the edits no longer apply as sent to version 2: read index.html again and redo them"},
	}
	for _, c := range cases {
		_, _, _, err := e.svc.Edit(e.ctx, alice, artifact.EditRequest{Slug: "demo", Path: "index.html", ExpectedVersion: 1, Edits: c.edits})
		ae, ok := errors.AsType[*artifact.Error](err)
		if !ok || ae.Code != artifact.ErrVersionConflict.Code || ae.Message != c.want {
			t.Errorf("%s: err = %v, want version_conflict: %s", c.name, err, c.want)
		}
	}
}

// Where the instance ignores kazari.csp, setting it is refused rather than
// accepted and ignored; removing it and other keys still work.
func TestCSPOverrideRefusedWhenOff(t *testing.T) {
	csp := map[string]string{artifact.MetaCSP: "frame-src https://www.youtube.com"}
	for _, allow := range []bool{false, true} {
		e := newEnv(t, artifact.Options{AllowCSPOverride: allow})
		alice := e.user(t, "alice")
		_, _, err := e.svc.Create(e.ctx, alice, artifact.CreateRequest{Slug: "demo", Metadata: csp, Files: files("index.html", "x")})
		if allow != (err == nil) || !allow && !errors.Is(err, artifact.ErrInvalidMetadata) {
			t.Errorf("Create with kazari.csp, overrides %v: err = %v", allow, err)
		}
		e.create(t, alice, "plain", "index.html", "x")
		_, _, err = e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "plain",
			Metadata: map[string]*string{artifact.MetaCSP: new("frame-src https://www.youtube.com")}})
		if allow != (err == nil) || !allow && !errors.Is(err, artifact.ErrInvalidMetadata) {
			t.Errorf("Update with kazari.csp, overrides %v: err = %v", allow, err)
		}
		if _, _, err := e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "plain",
			Metadata: map[string]*string{artifact.MetaCSP: nil, "team": new("web")}}); err != nil {
			t.Errorf("Update removing kazari.csp, overrides %v: err = %v", allow, err)
		}
	}
}

func TestStagedVersion(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice := e.user(t, "alice")
	e.create(t, alice, "demo", "index.html", "v1")

	a, v, err := e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "demo", Stage: true, Files: files("index.html", "v2")})
	if err != nil {
		t.Fatal(err)
	}
	if !v.Staged || v.Number != 2 || a.PublishedVersion != 1 || a.LatestVersion != 2 {
		t.Fatalf("staged Update = version %d (staged %t), published %d, latest %d; want 2, staged, 1, 2",
			v.Number, v.Staged, a.PublishedVersion, a.LatestVersion)
	}
	if got := e.rec.Events[len(e.rec.Events)-1].Details["staged"]; got != "true" {
		t.Errorf("staged Update audit detail staged = %q, want %q", got, "true")
	}

	_, v, err = e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "demo", Stage: true, Files: files("a.txt", "a")})
	if err != nil {
		t.Fatalf("second staged Update without base_version: %v", err)
	}
	if got := e.read(t, alice, "demo", "index.html", v.Number); !v.Staged || got != "v2" {
		t.Errorf("second staged Update = version %d (staged %t) with index.html %q; want staged, built on the staged %q",
			v.Number, v.Staged, got, "v2")
	}
	_, v, _, err = e.svc.Edit(e.ctx, alice, artifact.EditRequest{Slug: "demo", Path: "index.html", Stage: true,
		Edits: []textedit.Edit{{OldText: "v2", NewText: "v4"}}})
	if err != nil {
		t.Fatalf("staged Edit without base_version: %v", err)
	}
	if !v.Staged || v.Number != 4 {
		t.Errorf("staged Edit = version %d (staged %t), want 4, staged", v.Number, v.Staged)
	}

	_, _, err = e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "demo", Files: files("b.txt", "b")})
	wantErr(t, "publishing Update over a staged version without base_version", err, artifact.ErrPublishedNotLatest)
	_, _, _, err = e.svc.Edit(e.ctx, alice, artifact.EditRequest{Slug: "demo", Path: "index.html",
		Edits: []textedit.Edit{{OldText: "v4", NewText: "v5"}}})
	wantErr(t, "publishing Edit over a staged version without base_version", err, artifact.ErrPublishedNotLatest)

	a, err = e.svc.Publish(e.ctx, alice, "demo", 4)
	if err != nil {
		t.Fatal(err)
	}
	_, vs, err := e.svc.Get(e.ctx, alice, "demo")
	if err != nil {
		t.Fatal(err)
	}
	var staged []int
	for _, v := range vs {
		if v.Staged {
			staged = append(staged, v.Number)
		}
	}
	if a.PublishedVersion != 4 || !cmp.Equal(staged, []int{3, 2}) {
		t.Errorf("after publishing version 4: published %d, staged %v; want 4 and [3 2]", a.PublishedVersion, staged)
	}
}

func TestStageNeedsFiles(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	alice := e.user(t, "alice")
	e.create(t, alice, "demo", "index.html", "v1")

	_, _, err := e.svc.Update(e.ctx, alice, artifact.UpdateRequest{Slug: "demo", Stage: true, Name: new("Demo")})
	wantErr(t, "staged Update without files", err, artifact.ErrInvalidArgument)
	if a, _, _ := e.svc.Get(e.ctx, alice, "demo"); a.Name == "Demo" {
		t.Errorf("a refused staged Update changed the name to %q", a.Name)
	}
}
