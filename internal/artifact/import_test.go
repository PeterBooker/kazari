package artifact_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/audit"
)

type fakePages struct {
	calls int
	err   error
}

func (f *fakePages) Take(_ context.Context, url string, keepScripts bool) (*artifact.Snapshot, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	removed := 3
	if keepScripts {
		removed = 0
	}
	return &artifact.Snapshot{
		Title:       "  Example\tsite\n",
		Description: "A page",
		Files: []artifact.FileInput{
			{Path: "index.html", Content: strings.NewReader("<h1>hi</h1>"), ContentType: "text/html; charset=utf-8"},
			{Path: "assets/a.css", Content: strings.NewReader("body{}")},
		},
		Report: artifact.ImportReport{Source: url, Fetched: 2, ScriptsRemoved: removed,
			Outline: []artifact.Landmark{{Line: 1, Tag: "<h1>", Heading: "hi"}}},
	}, nil
}

func TestImport(t *testing.T) {
	pages := &fakePages{}
	e := newEnv(t, artifact.Options{Pages: pages})
	ana := e.user(t, "ana")

	a, v, rep, err := e.svc.Import(e.ctx, ana, artifact.ImportRequest{
		URL: "https://example.test/", Slug: "copy",
		Files: []artifact.FileInput{{Path: "assets/a.css", Content: strings.NewReader("h1{color:red}")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "Example site" || a.Description != "A page" || v.Number != 1 || rep.Source != "https://example.test/" || rep.ScriptsRemoved != 3 {
		t.Errorf("imported %+v version %d report %+v", a, v.Number, rep)
	}
	_, rc, err := e.svc.OpenFile(e.ctx, ana, "copy", "assets/a.css", 1)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "h1{color:red}" {
		t.Errorf("inline file did not win: %q", got)
	}
	var seen bool
	for _, ev := range e.rec.Events {
		if ev.Action == audit.ArtifactImport && ev.Slug == "copy" && ev.Details["source"] == "https://example.test/" {
			seen = true
		}
	}
	if !seen {
		t.Errorf("no import audit event: %+v", e.rec.Events)
	}

	_, _, _, err = e.svc.Import(e.ctx, ana, artifact.ImportRequest{URL: "https://example.test/", Slug: "copy"})
	if !errors.Is(err, artifact.ErrSlugTaken) || pages.calls != 1 {
		t.Errorf("taken slug: %v after %d fetches", err, pages.calls)
	}
	_, _, _, err = e.svc.Import(e.ctx, ana, artifact.ImportRequest{URL: "https://example.test/", Slug: "copy-2", Name: "Mine", KeepScripts: true})
	if err != nil {
		t.Fatal(err)
	}
	if got, _, _ := e.svc.Get(e.ctx, ana, "copy-2"); got.Name != "Mine" {
		t.Errorf("name = %q", got.Name)
	}

	pages.err = artifact.Errorf(artifact.ErrFetchFailed, "https://example.test/ answered 403 Forbidden")
	if _, _, _, err := e.svc.Import(e.ctx, ana, artifact.ImportRequest{URL: "https://example.test/", Slug: "copy-3"}); !errors.Is(err, artifact.ErrFetchFailed) {
		t.Errorf("fetch failure: %v", err)
	}
}

func TestImportDisabled(t *testing.T) {
	e := newEnv(t, artifact.Options{})
	ana := e.user(t, "ana")
	_, _, _, err := e.svc.Import(e.ctx, ana, artifact.ImportRequest{URL: "https://example.test/", Slug: "copy"})
	if !errors.Is(err, artifact.ErrForbidden) {
		t.Errorf("disabled: %v", err)
	}
}
