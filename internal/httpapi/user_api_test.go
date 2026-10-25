package httpapi_test

import (
	"bytes"
	"encoding/base64"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"kazari/internal/api"
	"kazari/internal/artifact"
	"kazari/internal/auth"
	"kazari/internal/httpapi"
	"kazari/internal/upload"
	"kazari/internal/user"
)

func TestCreateJSON(t *testing.T) {
	e := newEnv(t, config{})
	png := []byte{0x89, 'P', 'N', 'G', 0, 1, 2, 3}
	var res api.PublishResult
	e.call(http.MethodPost, e.u("/v1/artifacts"), e.alice, api.CreateArtifactRequest{
		Slug:     "demo",
		Name:     "Demo",
		Metadata: map[string]string{"team": "web"},
		Files: []api.FileInput{
			{Path: "index.html", Content: "<h1>hi</h1>"},
			{Path: "img/logo.png", Content: base64.StdEncoding.EncodeToString(png), Encoding: "base64"},
			{Path: "notes.txt", Content: "plain", Encoding: "utf8"},
		},
	}, http.StatusCreated, &res)

	if res.URL != "https://demo.art.test/" || res.Version != 1 {
		t.Errorf("result url %q version %d", res.URL, res.Version)
	}
	if a := res.Artifact; a.Owner != "alice" || a.Name != "Demo" || a.Metadata["team"] != "web" || a.PublishedVersion != 1 {
		t.Errorf("artifact = %+v", a)
	}
	if got := e.read(e.alice, "demo", "img/logo.png", 0); got != string(png) {
		t.Errorf("logo.png = %q, want %q", got, png)
	}
	if got := e.read(e.alice, "demo", "notes.txt", 1); got != "plain" {
		t.Errorf("notes.txt = %q", got)
	}
}

func TestCreateMultipart(t *testing.T) {
	e := newEnv(t, config{})
	html := "<!doctype html><h1>bundle</h1>"
	bundle := tarGz(t,
		tarEntry{name: "./", typ: '5'},
		tarEntry{name: "./index.html", body: html},
		tarEntry{name: "css/", typ: '5'},
		tarEntry{name: "css/site.css", body: "h1{}"},
		tarEntry{name: "link.html", typ: '2', linkTo: "index.html"},
	)
	body, ct := multipartBody(t,
		part{"manifest", mustJSON(t, api.CreateManifest{Slug: "bundle", Name: "Bundle", Label: "first"})},
		part{"bundle", bundle},
	)
	var res api.PublishResult
	e.expect(e.send(req{method: http.MethodPost, url: e.u("/v1/artifacts"), token: e.alice, body: body, contentType: ct}),
		http.StatusCreated, &res)
	if res.URL != "https://bundle.art.test/" || res.Version != 1 || res.Artifact.Name != "Bundle" {
		t.Errorf("result = %+v", res)
	}

	resp := e.send(req{method: http.MethodGet, url: e.u("/v1/artifacts/bundle/files/index.html"), token: e.alice})
	var got bytes.Buffer
	got.ReadFrom(resp.Body)
	if resp.StatusCode != http.StatusOK || got.String() != html {
		t.Fatalf("index.html: %d %q", resp.StatusCode, got.String())
	}
	wantHeaders := map[string]string{
		"Content-Type":           "text/html; charset=utf-8",
		"ETag":                   `"` + sha(html) + `"`,
		"X-Content-Type-Options": "nosniff",
		"Cache-Control":          "private, no-cache",
		"Content-Disposition":    "inline; filename=index.html",
		// Artifact HTML must not run on the api origin.
		"Content-Security-Policy": "default-src 'none'; sandbox",
	}
	for k, v := range wantHeaders {
		if g := resp.Header.Get(k); g != v {
			t.Errorf("%s = %q, want %q", k, g, v)
		}
	}

	resp = e.send(req{method: http.MethodGet, url: e.u("/v1/artifacts/bundle/files/index.html"), token: e.alice,
		header: map[string]string{"If-None-Match": `"` + sha(html) + `"`}})
	if resp.StatusCode != http.StatusNotModified {
		t.Errorf("conditional GET: status %d, want 304", resp.StatusCode)
	}

	var d api.ArtifactDetail
	e.call(http.MethodGet, e.u("/v1/artifacts/bundle?files=true"), e.alice, nil, http.StatusOK, &d)
	if len(d.Versions) != 1 {
		t.Fatalf("versions = %+v", d.Versions)
	}
	var paths []string
	for _, f := range d.Versions[0].Files {
		paths = append(paths, f.Path)
	}
	if !slices.Equal(paths, []string{"css/site.css", "index.html"}) {
		t.Errorf("files = %v", paths)
	}
	if d.Versions[0].Label != "first" || d.Versions[0].URL != "https://bundle--v1.art.test/" {
		t.Errorf("version = %+v", d.Versions[0])
	}

	e.call(http.MethodGet, e.u("/v1/artifacts/bundle"), e.alice, nil, http.StatusOK, &d)
	if len(d.Versions[0].Files) != 0 {
		t.Errorf("files listed without files=true: %+v", d.Versions[0].Files)
	}
}

func TestCreateErrors(t *testing.T) {
	e := newEnv(t, config{artifact: artifact.Options{MaxFileBytes: 64}})
	e.create(e.alice, "taken")

	file := []api.FileInput{{Path: "index.html", Content: "x"}}
	tests := []struct {
		name   string
		body   string
		ct     string
		status int
		code   string
	}{
		{"duplicate slug", string(mustJSON(t, api.CreateArtifactRequest{Slug: "taken", Files: file})), "application/json", 409, api.CodeSlugTaken},
		{"invalid slug", string(mustJSON(t, api.CreateArtifactRequest{Slug: "Bad_Slug!", Files: file})), "application/json", 422, api.CodeInvalidSlug},
		{"oversize file", string(mustJSON(t, api.CreateArtifactRequest{Slug: "big", Files: []api.FileInput{{Path: "a.txt", Content: strings.Repeat("x", 65)}}})), "application/json", 413, api.CodeTooLarge},
		{"delete on create", string(mustJSON(t, api.CreateArtifactRequest{Slug: "del", Files: []api.FileInput{{Path: "a.txt", Delete: true}}})), "application/json", 422, api.CodeInvalidArgument},
		{"bad encoding", string(mustJSON(t, api.CreateArtifactRequest{Slug: "enc", Files: []api.FileInput{{Path: "a.txt", Content: "x", Encoding: "hex"}}})), "application/json", 422, api.CodeInvalidArgument},
		{"bad base64", string(mustJSON(t, api.CreateArtifactRequest{Slug: "b64", Files: []api.FileInput{{Path: "a.txt", Content: "!!", Encoding: "base64"}}})), "application/json", 422, api.CodeInvalidArgument},
		{"invalid path", string(mustJSON(t, api.CreateArtifactRequest{Slug: "path", Files: []api.FileInput{{Path: "../x", Content: "x"}}})), "application/json", 422, api.CodeInvalidPath},
		{"no files", `{"slug":"empty"}`, "application/json", 422, api.CodeInvalidArgument},
		{"unknown field", `{"slug":"x","bogus":1}`, "application/json", 422, api.CodeInvalidArgument},
		{"trailing data", `{"slug":"x"} {}`, "application/json", 422, api.CodeInvalidArgument},
		{"empty body", ``, "application/json", 422, api.CodeInvalidArgument},
		{"malformed", `{"slug":`, "application/json", 422, api.CodeInvalidArgument},
		{"unknown upload", `{"slug":"up","upload_id":"AAAAAAAAAAAAAAAAAAAAAA"}`, "application/json", 404, api.CodeUploadNotFound},
		{"unsupported type", `slug=x`, "application/x-www-form-urlencoded", 415, api.CodeInvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := e.with(t)
			resp := e.send(req{method: http.MethodPost, url: e.u("/v1/artifacts"), token: e.alice,
				body: strings.NewReader(tt.body), contentType: tt.ct})
			e.apiErr(resp, tt.status, tt.code)
		})
	}

	multipartTests := []struct {
		name   string
		parts  []part
		status int
		code   string
	}{
		{"bundle first", []part{{"bundle", tarGz(t, tarEntry{name: "a", body: "a"})}, {"manifest", []byte(`{"slug":"mp-one"}`)}}, 422, api.CodeInvalidArgument},
		{"no bundle", []part{{"manifest", []byte(`{"slug":"mp-two"}`)}}, 422, api.CodeInvalidArgument},
		{"not gzip", []part{{"manifest", []byte(`{"slug":"mp-three"}`)}, {"bundle", []byte("plain")}}, 422, api.CodeInvalidArgument},
		{"bad manifest", []part{{"manifest", []byte(`{"slug":"mp-four","x":1}`)}, {"bundle", tarGz(t)}}, 422, api.CodeInvalidArgument},
		{"oversize file in bundle", []part{{"manifest", []byte(`{"slug":"mp-five"}`)}, {"bundle", tarGz(t, tarEntry{name: "a", body: strings.Repeat("x", 65)})}}, 413, api.CodeTooLarge},
		{"traversal in bundle", []part{{"manifest", []byte(`{"slug":"mp-six"}`)}, {"bundle", tarGz(t, tarEntry{name: "../a", body: "a"})}}, 422, api.CodeInvalidPath},
	}
	for _, tt := range multipartTests {
		t.Run(tt.name, func(t *testing.T) {
			e := e.with(t)
			body, ct := multipartBody(t, tt.parts...)
			resp := e.send(req{method: http.MethodPost, url: e.u("/v1/artifacts"), token: e.alice, body: body, contentType: ct})
			e.apiErr(resp, tt.status, tt.code)
		})
	}
}

func TestBodyCaps(t *testing.T) {
	e := newEnv(t, config{user: httpapi.UserOptions{MaxBodyBytes: 512}})
	body := mustJSON(t, api.CreateArtifactRequest{Slug: "big", Files: []api.FileInput{{Path: "a.txt", Content: strings.Repeat("x", 600)}}})
	e.apiErr(e.send(req{method: http.MethodPost, url: e.u("/v1/artifacts"), token: e.alice,
		body: bytes.NewReader(body), contentType: "application/json"}), 413, api.CodeTooLarge)

	mp, ct := multipartBody(t, part{"manifest", []byte(`{"slug":"bomb"}`)},
		part{"bundle", tarGz(t, tarEntry{name: "a.txt", body: strings.Repeat("0", 4096)})})
	e.apiErr(e.send(req{method: http.MethodPost, url: e.u("/v1/artifacts"), token: e.alice, body: mp, contentType: ct}),
		413, api.CodeTooLarge)
}

func TestList(t *testing.T) {
	e := newEnv(t, config{})
	bob := e.member("bob", nil)
	e.create(e.alice, "alpha", func(r *api.CreateArtifactRequest) { r.Metadata = map[string]string{"k": "v"} })
	e.create(e.alice, "beta", func(r *api.CreateArtifactRequest) { r.Metadata = map[string]string{"k": "w"} })
	e.create(bob, "gamma", func(r *api.CreateArtifactRequest) { r.Metadata = map[string]string{"k": "v"} })

	slugs := func(query string) []string {
		t.Helper()
		var l api.ArtifactList
		e.call(http.MethodGet, e.u("/v1/artifacts"+query), e.alice, nil, http.StatusOK, &l)
		var out []string
		for _, a := range l.Items {
			out = append(out, a.Slug)
		}
		slices.Sort(out)
		return out
	}
	tests := []struct {
		query string
		want  []string
	}{
		{"", []string{"alpha", "beta", "gamma"}},
		{"?mine=created", []string{"alpha", "beta"}},
		{"?metadata.k=v", []string{"alpha", "gamma"}},
		{"?mine=created&metadata.k=v", []string{"alpha"}},
		{"?stale=only", nil},
	}
	for _, tt := range tests {
		if got := slugs(tt.query); !slices.Equal(got, tt.want) {
			t.Errorf("list %q = %v, want %v", tt.query, got, tt.want)
		}
	}

	var page api.ArtifactList
	e.call(http.MethodGet, e.u("/v1/artifacts?limit=2"), e.alice, nil, http.StatusOK, &page)
	if len(page.Items) != 2 || page.NextCursor == "" {
		t.Fatalf("first page = %+v", page)
	}
	var next api.ArtifactList
	e.call(http.MethodGet, e.u("/v1/artifacts?limit=2&cursor="+page.NextCursor), e.alice, nil, http.StatusOK, &next)
	if len(next.Items) != 1 || next.NextCursor != "" {
		t.Errorf("second page = %+v", next)
	}

	for _, q := range []string{"?limit=x", "?mine=nobody", "?stale=maybe"} {
		e.apiErr(e.send(req{method: http.MethodGet, url: e.u("/v1/artifacts" + q), token: e.alice}), 422, api.CodeInvalidArgument)
	}
}

func TestUpdate(t *testing.T) {
	e := newEnv(t, config{})
	e.create(e.alice, "demo", func(r *api.CreateArtifactRequest) {
		r.Files = append(r.Files, api.FileInput{Path: "old.txt", Content: "old"})
	})
	url := e.u("/v1/artifacts/demo")
	change := api.UpdateArtifactRequest{
		Label: "second",
		Files: []api.FileInput{{Path: "new.txt", Content: "new"}, {Path: "old.txt", Delete: true}},
	}

	e.apiErr(e.send(req{method: http.MethodPut, url: url, token: e.alice, body: bytes.NewReader(mustJSON(t, change)),
		contentType: "application/json", header: map[string]string{"If-Match": `"5"`}}), 412, api.CodeVersionConflict)
	e.apiErr(e.send(req{method: http.MethodPut, url: url, token: e.alice, body: bytes.NewReader(mustJSON(t, change)),
		contentType: "application/json", header: map[string]string{"If-Match": `"abc"`}}), 422, api.CodeInvalidArgument)

	var res api.PublishResult
	e.call(http.MethodPut, url, e.alice, change, http.StatusOK, &res, "If-Match", `"1"`)
	if res.Version != 2 || res.Artifact.LatestVersion != 2 || res.Artifact.PublishedVersion != 2 {
		t.Fatalf("update result = %+v", res)
	}
	var d api.ArtifactDetail
	e.call(http.MethodGet, url+"?files=true", e.alice, nil, http.StatusOK, &d)
	var paths []string
	for _, f := range d.Versions[0].Files { // newest first
		paths = append(paths, f.Path)
	}
	if d.Versions[0].Number != 2 || !slices.Equal(paths, []string{"index.html", "new.txt"}) {
		t.Errorf("version 2 = %d %v", d.Versions[0].Number, paths)
	}

	stale := api.UpdateArtifactRequest{ExpectedVersion: 1, Files: []api.FileInput{{Path: "x.txt", Content: "x"}}}
	e.apiErr(e.send(req{method: http.MethodPut, url: url, token: e.alice, body: bytes.NewReader(mustJSON(t, stale)),
		contentType: "application/json"}), 412, api.CodeVersionConflict)

	manifest := api.UpdateArtifactRequest{Name: ptr("Renamed"), Files: []api.FileInput{{Path: "new.txt", Delete: true}}}
	body, ct := multipartBody(t, part{"manifest", mustJSON(t, manifest)},
		part{"bundle", tarGz(t, tarEntry{name: "./about.html", body: "about"})})
	e.expect(e.send(req{method: http.MethodPut, url: url, token: e.alice, body: body, contentType: ct,
		header: map[string]string{"If-Match": "2"}}), http.StatusOK, &res)
	if res.Version != 3 || res.Artifact.Name != "Renamed" {
		t.Errorf("multipart update = %+v", res)
	}
	if got := e.read(e.alice, "demo", "about.html", 3); got != "about" {
		t.Errorf("about.html = %q", got)
	}
	e.apiErr(e.send(req{method: http.MethodGet, url: e.u("/v1/artifacts/demo/files/new.txt"), token: e.alice}), 404, api.CodeFileNotFound)

	body, ct = multipartBody(t, part{"manifest", []byte(`{"description":"d"}`)})
	e.expect(e.send(req{method: http.MethodPut, url: url, token: e.alice, body: body, contentType: ct}), http.StatusOK, &res)
	if res.Artifact.Description != "d" || res.Artifact.LatestVersion != 3 {
		t.Errorf("fields-only multipart update = %+v", res.Artifact)
	}

	e.apiErr(e.send(req{method: http.MethodPut, url: e.u("/v1/artifacts/missing"), token: e.alice,
		body: strings.NewReader(`{"name":"x"}`), contentType: "application/json"}), 404, api.CodeNotFound)
}

func TestPatch(t *testing.T) {
	e := newEnv(t, config{})
	e.create(e.alice, "demo", func(r *api.CreateArtifactRequest) { r.Metadata = map[string]string{"a": "1", "b": "2"} })
	url := e.u("/v1/artifacts/demo")

	var res api.PublishResult
	e.call(http.MethodPatch, url, e.alice, api.UpdateArtifactRequest{
		Name:     ptr("New name"),
		Metadata: map[string]*string{"a": nil, "c": ptr("3")},
	}, http.StatusOK, &res)
	if res.Artifact.Name != "New name" || res.Version != 1 || res.Artifact.LatestVersion != 1 {
		t.Errorf("patch = %+v", res)
	}
	if md := res.Artifact.Metadata; md["a"] != "" || md["b"] != "2" || md["c"] != "3" {
		t.Errorf("metadata = %v", md)
	}

	e.apiErr(e.send(req{method: http.MethodPatch, url: url, token: e.alice,
		body: strings.NewReader(`{"files":[{"path":"x","content":"x"}]}`), contentType: "application/json"}), 422, api.CodeInvalidArgument)
	e.apiErr(e.send(req{method: http.MethodPatch, url: url, token: e.alice,
		body: strings.NewReader(`{"upload_id":"x"}`), contentType: "application/json"}), 422, api.CodeInvalidArgument)
	e.apiErr(e.send(req{method: http.MethodPatch, url: url, token: e.alice,
		body: strings.NewReader(`{"visibility":"secret"}`), contentType: "application/json"}), 422, api.CodeInvalidArgument)

	e.call(http.MethodPatch, url, e.alice, api.UpdateArtifactRequest{Visibility: ptr("unpublished")}, http.StatusOK, &res)
	if res.Artifact.UnpublishedAt == nil || !res.Artifact.UnpublishedAt.Equal(t0) {
		t.Errorf("unpublished_at = %v", res.Artifact.UnpublishedAt)
	}
	e.call(http.MethodPatch, url, e.alice, api.UpdateArtifactRequest{Visibility: ptr("unlisted")}, http.StatusOK, &res)
	if res.Artifact.UnpublishedAt != nil || res.Artifact.Visibility != "unlisted" {
		t.Errorf("after unlisted: %+v", res.Artifact)
	}

	e.call(http.MethodDelete, url, e.alice, nil, http.StatusNoContent, nil)
	e.apiErr(e.send(req{method: http.MethodPatch, url: url, token: e.alice,
		body: strings.NewReader(`{"name":"x"}`), contentType: "application/json"}), 404, api.CodeNotFound)
	e.call(http.MethodPatch, url, e.alice, api.UpdateArtifactRequest{Restore: true}, http.StatusOK, &res)
	if res.Artifact.DeletedAt != nil {
		t.Errorf("restored artifact still deleted: %+v", res.Artifact)
	}
}

func TestPublishAndStale(t *testing.T) {
	e := newEnv(t, config{artifact: artifact.Options{DefaultGrace: 24 * time.Hour}})
	e.create(e.alice, "demo")
	e.call(http.MethodPut, e.u("/v1/artifacts/demo"), e.alice,
		api.UpdateArtifactRequest{Files: []api.FileInput{{Path: "index.html", Content: "v2"}}}, http.StatusOK, nil)

	var res api.PublishResult
	e.call(http.MethodPost, e.u("/v1/artifacts/demo/publish"), e.alice, api.PublishRequest{Version: 1}, http.StatusOK, &res)
	if res.Version != 1 || res.Artifact.PublishedVersion != 1 || res.Artifact.LatestVersion != 2 {
		t.Errorf("rollback = %+v", res)
	}
	if got := e.read(e.alice, "demo", "index.html", 0); got != "<h1>demo</h1>" {
		t.Errorf("published index.html = %q", got)
	}
	e.apiErr(e.send(req{method: http.MethodPost, url: e.u("/v1/artifacts/demo/publish"), token: e.alice,
		body: strings.NewReader(`{"version":9}`), contentType: "application/json"}), 404, api.CodeVersionNotFound)
	e.apiErr(e.send(req{method: http.MethodPost, url: e.u("/v1/artifacts/demo/publish"), token: e.alice,
		body: strings.NewReader(`{}`), contentType: "application/json"}), 422, api.CodeInvalidArgument)

	var st api.StaleResult
	e.call(http.MethodPost, e.u("/v1/artifacts/demo/stale"), e.alice, api.StaleRequest{Grace: "48h"}, http.StatusOK, &st)
	if !st.StaleAt.Equal(t0) || !st.ExpiresAt.Equal(t0.Add(48*time.Hour)) {
		t.Errorf("stale = %+v", st)
	}
	e.call(http.MethodPost, e.u("/v1/artifacts/demo/stale"), e.alice, nil, http.StatusOK, &st)
	if !st.ExpiresAt.Equal(t0.Add(24 * time.Hour)) {
		t.Errorf("default grace expires_at = %v", st.ExpiresAt)
	}
	e.apiErr(e.send(req{method: http.MethodPost, url: e.u("/v1/artifacts/demo/stale"), token: e.alice,
		body: strings.NewReader(`{"grace":"soon"}`), contentType: "application/json"}), 422, api.CodeInvalidArgument)

	e.call(http.MethodDelete, e.u("/v1/artifacts/demo/stale"), e.alice, nil, http.StatusOK, &res)
	if res.Artifact.StaleAt != nil || res.Artifact.ExpiresAt != nil || res.Version != 1 {
		t.Errorf("unstale = %+v", res)
	}
}

func TestChunkedUpload(t *testing.T) {
	e := newEnv(t, config{upload: upload.Options{MaxChunkBytes: 16}})
	var up api.Upload
	e.call(http.MethodPost, e.u("/v1/uploads"), e.alice, nil, http.StatusCreated, &up)
	if up.UploadID == "" || up.MaxChunkBytes != 16 || up.MaxTotalBytes != upload.DefaultMaxTotalBytes || !up.ExpiresAt.After(t0) {
		t.Fatalf("upload = %+v", up)
	}
	chunk := func(path, offset, body, ct string) *http.Response {
		t.Helper()
		url := e.u("/v1/uploads/" + up.UploadID + "/files/" + path)
		if offset != "" {
			url += "?offset=" + offset
		}
		return e.send(req{method: http.MethodPatch, url: url, token: e.alice, body: strings.NewReader(body), contentType: ct})
	}

	var cr api.ChunkResult
	e.expect(chunk("big/data.txt", "0", "hello ", "application/octet-stream"), http.StatusOK, &cr)
	if cr.Path != "big/data.txt" || cr.BytesReceived != 6 {
		t.Errorf("chunk 1 = %+v", cr)
	}
	e.expect(chunk("big/data.txt", "6", "world", ""), http.StatusOK, &cr)
	if cr.BytesReceived != 11 {
		t.Errorf("chunk 2 = %+v", cr)
	}
	e.expect(chunk("big/data.txt", "6", "world", ""), http.StatusOK, &cr)
	if cr.BytesReceived != 11 {
		t.Errorf("retried chunk = %+v", cr)
	}
	ae := e.apiErr(chunk("big/data.txt", "3", "xx", ""), http.StatusConflict, api.CodeOffsetMismatch)
	if ae.Current == nil || *ae.Current != 11 {
		t.Errorf("mismatch current = %v", ae.Current)
	}
	e.apiErr(chunk("big/data.txt", "", "x", ""), 422, api.CodeInvalidArgument)
	e.apiErr(chunk("big/data.txt", "-1", "x", ""), 422, api.CodeInvalidArgument)
	e.apiErr(chunk("big/other.txt", "0", strings.Repeat("x", 17), ""), 413, api.CodeTooLarge)
	e.expect(chunk("page.html", "0", "<p>staged</p>", "text/html"), http.StatusOK, &cr)

	bob := e.member("bob", nil)
	e.apiErr(e.send(req{method: http.MethodPatch, url: e.u("/v1/uploads/" + up.UploadID + "/files/x?offset=0"), token: bob,
		body: strings.NewReader("x")}), 404, api.CodeUploadNotFound)

	var res api.PublishResult
	e.call(http.MethodPost, e.u("/v1/artifacts"), e.alice, api.CreateArtifactRequest{
		Slug:     "chunked",
		UploadID: up.UploadID,
		Files:    []api.FileInput{{Path: "index.html", Content: "inline"}, {Path: "page.html", Content: "inline wins"}},
	}, http.StatusCreated, &res)
	if got := e.read(e.alice, "chunked", "big/data.txt", 0); got != "hello world" {
		t.Errorf("staged file = %q", got)
	}
	if got := e.read(e.alice, "chunked", "page.html", 0); got != "inline wins" {
		t.Errorf("page.html = %q", got)
	}

	e.apiErr(chunk("late.txt", "0", "x", ""), 404, api.CodeUploadNotFound)

	var up2 api.Upload
	e.call(http.MethodPost, e.u("/v1/uploads"), e.alice, nil, http.StatusCreated, &up2)
	e.call(http.MethodPatch, e.u("/v1/uploads/"+up2.UploadID+"/files/a.txt?offset=0"), e.alice, nil, http.StatusOK, nil)
	e.expect(e.send(req{method: http.MethodPatch, url: e.u("/v1/uploads/" + up2.UploadID + "/files/b.txt?offset=0"),
		token: e.alice, body: strings.NewReader("b")}), http.StatusOK, nil)
	e.apiErr(e.send(req{method: http.MethodPost, url: e.u("/v1/artifacts"), token: e.alice, contentType: "application/json",
		body: bytes.NewReader(mustJSON(t, api.CreateArtifactRequest{Slug: "chunked", UploadID: up2.UploadID}))}),
		409, api.CodeSlugTaken)
	if _, err := e.uploads.Get(e.ctx, "alice", up2.UploadID); err == nil {
		t.Error("upload survived a failed publish")
	}

	var up3 api.Upload
	e.call(http.MethodPost, e.u("/v1/uploads"), e.alice, nil, http.StatusCreated, &up3)
	e.call(http.MethodDelete, e.u("/v1/uploads/"+up3.UploadID), e.alice, nil, http.StatusNoContent, nil)
	e.apiErr(e.send(req{method: http.MethodDelete, url: e.u("/v1/uploads/" + up3.UploadID), token: e.alice}), 404, api.CodeUploadNotFound)
}

func TestUpdateWithUpload(t *testing.T) {
	e := newEnv(t, config{})
	e.create(e.alice, "demo", func(r *api.CreateArtifactRequest) {
		r.Files = append(r.Files, api.FileInput{Path: "gone.txt", Content: "x"})
	})
	var up api.Upload
	e.call(http.MethodPost, e.u("/v1/uploads"), e.alice, nil, http.StatusCreated, &up)
	e.expect(e.send(req{method: http.MethodPatch, url: e.u("/v1/uploads/" + up.UploadID + "/files/data.bin?offset=0"),
		token: e.alice, body: strings.NewReader("data")}), http.StatusOK, nil)
	var res api.PublishResult
	e.call(http.MethodPut, e.u("/v1/artifacts/demo"), e.alice, api.UpdateArtifactRequest{
		UploadID: up.UploadID,
		Files:    []api.FileInput{{Path: "gone.txt", Delete: true}},
	}, http.StatusOK, &res)
	if res.Version != 2 {
		t.Fatalf("update = %+v", res)
	}
	if got := e.read(e.alice, "demo", "data.bin", 2); got != "data" {
		t.Errorf("data.bin = %q", got)
	}
	e.apiErr(e.send(req{method: http.MethodGet, url: e.u("/v1/artifacts/demo/files/gone.txt"), token: e.alice}), 404, api.CodeFileNotFound)
}

func TestDelete(t *testing.T) {
	e := newEnv(t, config{})
	noDelete := e.member("alice", []string{auth.ScopeRead, auth.ScopeWrite})
	e.create(e.alice, "demo")
	e.create(e.alice, "other")

	e.apiErr(e.send(req{method: http.MethodDelete, url: e.u("/v1/artifacts/demo"), token: noDelete}), 403, api.CodeForbidden)
	e.apiErr(e.send(req{method: http.MethodDelete, url: e.u("/v1/artifacts/demo?hard=true"), token: noDelete}), 403, api.CodeForbidden)
	e.apiErr(e.send(req{method: http.MethodDelete, url: e.u("/v1/artifacts/demo?hard=maybe"), token: e.alice}), 422, api.CodeInvalidArgument)

	e.call(http.MethodDelete, e.u("/v1/artifacts/demo"), e.alice, nil, http.StatusNoContent, nil)
	var d api.ArtifactDetail
	e.call(http.MethodGet, e.u("/v1/artifacts/demo"), e.alice, nil, http.StatusOK, &d)
	if d.Artifact.DeletedAt == nil {
		t.Errorf("soft-deleted artifact has no deleted_at: %+v", d.Artifact)
	}
	if got := e.read(e.alice, "demo", "index.html", 0); got != "<h1>demo</h1>" {
		t.Errorf("soft-deleted content = %q", got)
	}

	e.call(http.MethodDelete, e.u("/v1/artifacts/other?hard=true"), e.alice, nil, http.StatusNoContent, nil)
	e.apiErr(e.send(req{method: http.MethodGet, url: e.u("/v1/artifacts/other"), token: e.alice}), 410, api.CodeGone)
	e.apiErr(e.send(req{method: http.MethodDelete, url: e.u("/v1/artifacts/missing"), token: e.alice}), 404, api.CodeNotFound)
}

func TestReadFileErrors(t *testing.T) {
	e := newEnv(t, config{})
	e.create(e.alice, "demo")
	e.apiErr(e.send(req{method: http.MethodGet, url: e.u("/v1/artifacts/demo/files/nope.html"), token: e.alice}), 404, api.CodeFileNotFound)
	e.apiErr(e.send(req{method: http.MethodGet, url: e.u("/v1/artifacts/demo/files/index.html?version=7"), token: e.alice}), 404, api.CodeVersionNotFound)
	e.apiErr(e.send(req{method: http.MethodGet, url: e.u("/v1/artifacts/demo/files/index.html?version=x"), token: e.alice}), 422, api.CodeInvalidArgument)
	e.apiErr(e.send(req{method: http.MethodGet, url: e.u("/v1/artifacts/missing/files/index.html"), token: e.alice}), 404, api.CodeNotFound)
}

func TestAuthAndRouting(t *testing.T) {
	e := newEnv(t, config{})
	ro := e.member("alice", readOnly)
	e.create(e.alice, "demo")

	for _, token := range []string{"", "kzk_bogus"} {
		resp := e.send(req{method: http.MethodGet, url: e.u("/v1/artifacts"), token: token})
		e.apiErr(resp, http.StatusUnauthorized, api.CodeUnauthorized)
		if !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Bearer") {
			t.Errorf("token %q: WWW-Authenticate = %q", token, resp.Header.Get("WWW-Authenticate"))
		}
	}

	e.call(http.MethodGet, e.u("/v1/artifacts/demo"), ro, nil, http.StatusOK, nil)
	e.apiErr(e.send(req{method: http.MethodPost, url: e.u("/v1/artifacts"), token: ro, contentType: "application/json",
		body: bytes.NewReader(mustJSON(t, api.CreateArtifactRequest{Slug: "x"}))}), 403, api.CodeForbidden)
	e.apiErr(e.send(req{method: http.MethodPost, url: e.u("/v1/uploads"), token: ro}), 403, api.CodeForbidden)

	e.apiErr(e.send(req{method: http.MethodGet, url: e.u("/v1/nothing"), token: e.alice}), 404, api.CodeNotFound)
	e.apiErr(e.send(req{method: http.MethodGet, url: e.u("/elsewhere"), token: e.alice}), 404, api.CodeNotFound)
	resp := e.send(req{method: http.MethodPost, url: e.u("/v1/me"), token: e.alice})
	e.apiErr(resp, http.StatusMethodNotAllowed, api.CodeMethodNotAllowed)
	if allow := resp.Header.Get("Allow"); !strings.Contains(allow, "GET") {
		t.Errorf("Allow = %q", allow)
	}
	resp = e.send(req{method: http.MethodPost, url: e.u("/v1/artifacts/demo/files/index.html"), token: e.alice})
	e.apiErr(resp, http.StatusMethodNotAllowed, api.CodeMethodNotAllowed)
}

func TestRateLimits(t *testing.T) {
	t.Run("per key writes", func(t *testing.T) {
		e := newEnv(t, config{writeRate: 1})
		e.call(http.MethodPost, e.u("/v1/uploads"), e.alice, nil, http.StatusCreated, nil)
		resp := e.send(req{method: http.MethodPost, url: e.u("/v1/uploads"), token: e.alice})
		e.apiErr(resp, http.StatusTooManyRequests, api.CodeRateLimited)
		if ra := resp.Header.Get("Retry-After"); ra != "60" {
			t.Errorf("Retry-After = %q, want 60", ra)
		}
		for range 3 {
			e.call(http.MethodGet, e.u("/v1/me"), e.alice, nil, http.StatusOK, nil)
		}
		e.call(http.MethodPost, e.u("/v1/uploads"), e.member("bob", nil), nil, http.StatusCreated, nil)
	})
	t.Run("per IP", func(t *testing.T) {
		e := newEnv(t, config{user: httpapi.UserOptions{IPRatePerMinute: 2}})
		e.call(http.MethodGet, e.u("/v1/me"), e.alice, nil, http.StatusOK, nil)
		// Counts too: the IP limit runs before authentication.
		e.apiErr(e.send(req{method: http.MethodGet, url: e.u("/v1/me")}), http.StatusUnauthorized, api.CodeUnauthorized)
		resp := e.send(req{method: http.MethodGet, url: e.u("/v1/me"), token: e.alice})
		e.apiErr(resp, http.StatusTooManyRequests, api.CodeRateLimited)
		if ra := resp.Header.Get("Retry-After"); ra != "30" {
			t.Errorf("Retry-After = %q, want 30", ra)
		}
	})
}

func TestMe(t *testing.T) {
	e := newEnv(t, config{})
	var me api.Me
	e.call(http.MethodGet, e.u("/v1/me"), e.alice, nil, http.StatusOK, &me)
	if me.Handle != "alice" || me.Admin || me.Source != auth.SourceAPIKey ||
		!slices.Equal(me.Scopes, auth.MemberScopes) {
		t.Errorf("me = %+v", me)
	}
	ro := e.member("alice", readOnly)
	var keys []api.APIKey
	e.call(http.MethodGet, e.u("/v1/me/keys"), ro, nil, http.StatusOK, &keys)
	if len(keys) != 2 {
		t.Fatalf("keys = %+v", keys)
	}
	var roScopes []string
	for _, k := range keys {
		if k.Prefix == "" || k.Label != "test" {
			t.Errorf("key = %+v", k)
		}
		if strings.HasPrefix(ro, user.KeyPrefix+k.Prefix+"_") {
			roScopes = k.Scopes
		}
	}
	if !slices.Equal(roScopes, readOnly) {
		t.Errorf("read-only key scopes = %v", roScopes)
	}
}

func TestEdit(t *testing.T) {
	e := newEnv(t, config{})
	e.create(e.alice, "demo")

	var res api.EditResult
	e.call(http.MethodPost, e.u("/v1/artifacts/demo/edit"), e.alice, api.EditRequest{
		Path:  "./index.html",
		Label: "rename",
		Edits: []api.Edit{{OldText: "<h1>demo", NewText: "<h1>Demo"}},
	}, http.StatusOK, &res, "If-Match", `"1"`)
	if res.Version != 2 || res.Path != "index.html" || len(res.Edits) != 1 || res.Edits[0].Match != artifact.MatchExact ||
		res.Edits[0].StartLine != 1 || !strings.Contains(res.Edits[0].Excerpt, "|<h1>Demo</h1>") {
		t.Fatalf("edit = %+v", res)
	}
	if got := e.read(e.alice, "demo", "index.html", 0); got != "<h1>Demo</h1>" {
		t.Errorf("published index.html = %q", got)
	}

	resp := e.send(req{method: http.MethodGet, url: e.u("/v1/artifacts/demo/files/index.html?anchors=true"), token: e.alice})
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/plain; charset=utf-8" ||
		string(body) != artifact.AnchorLines("<h1>Demo</h1>") || resp.Header.Get("Content-Security-Policy") == "" {
		t.Errorf("anchored read = %d %q %q", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}

	anchor := artifact.Anchor(1, "<h1>Demo</h1>")
	e.call(http.MethodPost, e.u("/v1/artifacts/demo/edit"), e.alice, api.EditRequest{
		Path: "index.html", ExpectedVersion: 2, Edits: []api.Edit{{Start: anchor, NewText: "<h1>Anchored</h1>"}},
	}, http.StatusOK, &res)
	if res.Version != 3 || res.Edits[0].Match != artifact.MatchAnchor {
		t.Errorf("anchor edit = %+v", res)
	}

	edit := func(r api.EditRequest) *http.Response {
		return e.send(req{method: http.MethodPost, url: e.u("/v1/artifacts/demo/edit"), token: e.alice,
			body: bytes.NewReader(mustJSON(t, r)), contentType: "application/json"})
	}
	e.apiErr(edit(api.EditRequest{Path: "index.html", ExpectedVersion: 2,
		Edits: []api.Edit{{OldText: "Anchored", NewText: "x"}}}), http.StatusPreconditionFailed, api.CodeVersionConflict)
	e.apiErr(edit(api.EditRequest{Path: "index.html",
		Edits: []api.Edit{{Start: anchor, NewText: "x"}}}), http.StatusConflict, api.CodeAnchorMismatch)
	e.apiErr(edit(api.EditRequest{Path: "index.html",
		Edits: []api.Edit{{OldText: "<h2>", NewText: "x"}}}), http.StatusUnprocessableEntity, api.CodeEditNotFound)
	e.apiErr(edit(api.EditRequest{Path: "index.html",
		Edits: []api.Edit{{OldText: "h1", NewText: "h2"}}}), http.StatusUnprocessableEntity, api.CodeEditAmbiguous)
	e.apiErr(edit(api.EditRequest{Path: "missing.html",
		Edits: []api.Edit{{OldText: "a", NewText: "b"}}}), http.StatusNotFound, api.CodeFileNotFound)

	e.call(http.MethodPut, e.u("/v1/artifacts/demo"), e.alice, api.UpdateArtifactRequest{Files: []api.FileInput{
		{Path: "logo.png", Content: base64.StdEncoding.EncodeToString([]byte{0x89, 'P', 0, 0xff}), Encoding: "base64"}}}, http.StatusOK, nil)
	e.apiErr(e.send(req{method: http.MethodGet, url: e.u("/v1/artifacts/demo/files/logo.png?anchors=true"), token: e.alice}),
		http.StatusUnprocessableEntity, api.CodeInvalidArgument)

	reader := e.member("rita", []string{auth.ScopeRead})
	e.apiErr(e.send(req{method: http.MethodPost, url: e.u("/v1/artifacts/demo/edit"), token: reader,
		body:        bytes.NewReader(mustJSON(t, api.EditRequest{Path: "index.html", Edits: []api.Edit{{OldText: "h1", NewText: "h2", ReplaceAll: true}}})),
		contentType: "application/json"}), http.StatusForbidden, api.CodeForbidden)
}

func TestSkill(t *testing.T) {
	e := newEnv(t, config{user: httpapi.UserOptions{Skill: "---\nname: kazari\n---\n# Kazari\n"}})
	resp := e.send(req{method: http.MethodGet, url: e.u("/v1/skill"), token: e.alice})
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/markdown; charset=utf-8" ||
		string(body) != "---\nname: kazari\n---\n# Kazari\n" {
		t.Errorf("GET /v1/skill = %d %q %q", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	e.apiErr(e.send(req{method: http.MethodGet, url: e.u("/v1/skill")}), http.StatusUnauthorized, api.CodeUnauthorized)

	none := newEnv(t, config{})
	none.apiErr(none.send(req{method: http.MethodGet, url: none.u("/v1/skill"), token: none.alice}), http.StatusNotFound, api.CodeNotFound)
}
