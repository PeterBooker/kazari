package httpapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PeterBooker/kazari/internal/api"
)

func pageServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			io.WriteString(w, `<!doctype html><html><head><title>Charity</title><link rel="stylesheet" href="/s.css"><script>t()</script></head>`+
				`<body><header><h1>Give</h1></header><p>`+strings.Repeat("words ", 50)+`</p><img src="img.png"></body></html>`)
		case "/s.css":
			w.Header().Set("Content-Type", "text/css")
			io.WriteString(w, "body{}")
		case "/img.png":
			w.Header().Set("Content-Type", "image/png")
			io.WriteString(w, "PNG")
		case "/data.json":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"ok":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (e *env) postJSON(token string, body any) *http.Response {
	e.t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.send(req{method: http.MethodPost, url: e.u("/v1/artifacts"), token: token, body: bytes.NewReader(b), contentType: "application/json"})
}

func TestFilesFromURL(t *testing.T) {
	e := newEnv(t, config{})
	srv := pageServer(t)

	e.create(e.alice, "demo", func(r *api.CreateArtifactRequest) {
		r.Files = append(r.Files, api.FileInput{Path: "data.json", URL: srv.URL + "/data.json"})
	})
	if got := e.read(e.alice, "demo", "data.json", 0); got != `{"ok":true}` {
		t.Errorf("fetched file = %q", got)
	}

	var out api.PublishResult
	e.call(http.MethodPut, e.u("/v1/artifacts/demo"), e.alice, api.UpdateArtifactRequest{
		Files: []api.FileInput{{Path: "s.css", URL: srv.URL + "/s.css"}}}, http.StatusOK, &out)
	if out.Version != 2 || e.read(e.alice, "demo", "s.css", 2) != "body{}" {
		t.Errorf("updated from url: %+v", out)
	}

	e.apiErr(e.postJSON(e.alice, api.CreateArtifactRequest{Slug: "bad", Files: []api.FileInput{{Path: "x.json", URL: srv.URL + "/missing"}}}),
		http.StatusBadGateway, api.CodeFetchFailed)
	e.apiErr(e.postJSON(e.alice, api.CreateArtifactRequest{Slug: "bad", Files: []api.FileInput{{Path: "x.json", URL: srv.URL + "/data.json", Content: "{}"}}}),
		http.StatusUnprocessableEntity, api.CodeInvalidArgument)
	e.send(req{method: http.MethodGet, url: e.u("/v1/artifacts/bad"), token: e.alice})
	resp := e.send(req{method: http.MethodGet, url: e.u("/v1/artifacts/bad"), token: e.alice})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("failed create left an artifact: %d", resp.StatusCode)
	}
}

func TestImportPage(t *testing.T) {
	e := newEnv(t, config{})
	srv := pageServer(t)

	var out api.PublishResult
	e.expect(e.postJSON(e.alice, api.CreateArtifactRequest{Slug: "copy", SourceURL: srv.URL + "/",
		Files: []api.FileInput{{Path: "extra.txt", Content: "mine"}}}), http.StatusCreated, &out)
	if out.Version != 1 || out.Artifact.Name != "Charity" || out.Import == nil || out.Import.Fetched != 2 || out.Import.ScriptsRemoved != 1 ||
		len(out.Import.Outline) == 0 || out.Import.Outline[0].Tag != "<header>" {
		t.Fatalf("import = %+v", out)
	}
	page := e.read(e.alice, "copy", "index.html", 0)
	if strings.Contains(page, "<script") || !strings.Contains(page, `href="assets/`) || e.read(e.alice, "copy", "extra.txt", 0) != "mine" {
		t.Errorf("copy = %s", page)
	}

	e.apiErr(e.postJSON(e.alice, api.CreateArtifactRequest{Slug: "copy-2", SourceURL: srv.URL + "/missing"}), http.StatusBadGateway, api.CodeFetchFailed)
	e.apiErr(e.postJSON(e.alice, api.CreateArtifactRequest{Slug: "copy-2", SourceURL: srv.URL + "/", UploadID: "x"}), http.StatusUnprocessableEntity, api.CodeInvalidArgument)
	e.apiErr(e.postJSON(e.alice, api.CreateArtifactRequest{Slug: "copy", SourceURL: srv.URL + "/"}), http.StatusConflict, api.CodeSlugTaken)
}
