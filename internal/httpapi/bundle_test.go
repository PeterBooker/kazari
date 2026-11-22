package httpapi_test

import (
	"archive/tar"
	"bytes"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/api"
)

func (e *env) paths(slug string, version int) []string {
	e.t.Helper()
	var d api.ArtifactDetail
	e.call(http.MethodGet, e.u("/v1/artifacts/"+slug+"?files=true"), e.alice, nil, http.StatusOK, &d)
	for _, v := range d.Versions {
		if v.Number == version {
			var out []string
			for _, f := range v.Files {
				out = append(out, f.Path)
			}
			return out
		}
	}
	e.t.Fatalf("%s has no version %d", slug, version)
	return nil
}

func TestCreateMultipartBundleFormats(t *testing.T) {
	site := []archived{{"index.html", tar.TypeReg, "<h1>hi</h1>"}, {"css/a.css", tar.TypeReg, "a{}"}}
	tests := []struct {
		name   string
		bundle []byte
	}{
		{"tar.gz", gzipOf(t, tarOf(t, site))},
		{"tar", tarOf(t, site)},
		{"zip", zipOf(t, site)},
	}
	e := newEnv(t, config{})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := e.with(t)
			slug := "fmt-" + strings.ReplaceAll(tt.name, ".", "")
			body, ct := multipartBody(t, part{"manifest", mustJSON(t, api.CreateManifest{Slug: slug})}, part{"bundle", tt.bundle})
			e.expect(e.send(req{method: http.MethodPost, url: e.u("/v1/artifacts"), token: e.alice, body: body, contentType: ct}),
				http.StatusCreated, nil)
			if diff := cmp.Diff([]string{"css/a.css", "index.html"}, e.paths(slug, 1)); diff != "" {
				t.Errorf("files of a %s bundle (-want +got):\n%s", tt.name, diff)
			}
			if got := e.read(e.alice, slug, "index.html", 1); got != "<h1>hi</h1>" {
				t.Errorf("index.html of a %s bundle = %q", tt.name, got)
			}
		})
	}

	body, ct := multipartBody(t, part{"manifest", []byte(`{"slug":"fmt-junk"}`)}, part{"bundle", []byte("not an archive")})
	got := e.apiErr(e.send(req{method: http.MethodPost, url: e.u("/v1/artifacts"), token: e.alice, body: body, contentType: ct}),
		422, api.CodeInvalidArgument)
	if !strings.Contains(got.Message, "neither a gzip-compressed tar, a tar nor a zip") {
		t.Errorf("junk bundle message = %q", got.Message)
	}
}

func TestBundleTopDirectory(t *testing.T) {
	e := newEnv(t, config{})
	wrapped := gzipOf(t, tarOf(t, []archived{
		{"site/index.html", tar.TypeReg, "<h1>site</h1>"}, {"site/css/a.css", tar.TypeReg, "a{}"},
	}))
	body, ct := multipartBody(t, part{"manifest", []byte(`{"slug":"wrapped"}`)}, part{"bundle", wrapped})
	var res api.PublishResult
	e.expect(e.send(req{method: http.MethodPost, url: e.u("/v1/artifacts"), token: e.alice, body: body, contentType: ct}),
		http.StatusCreated, &res)
	if diff := cmp.Diff([]string{"css/a.css", "index.html"}, e.paths("wrapped", 1)); diff != "" {
		t.Errorf("create from site/... (-want +got):\n%s", diff)
	}

	assets := gzipOf(t, tarOf(t, []archived{{"assets/app.js", tar.TypeReg, "1"}}))
	body, ct = multipartBody(t, part{"manifest", []byte(`{}`)}, part{"bundle", assets})
	e.expect(e.send(req{method: http.MethodPut, url: e.u("/v1/artifacts/wrapped"), token: e.alice, body: body, contentType: ct}),
		http.StatusOK, nil)
	if diff := cmp.Diff([]string{"assets/app.js", "css/a.css", "index.html"}, e.paths("wrapped", 2)); diff != "" {
		t.Errorf("update with assets/... only (-want +got):\n%s", diff)
	}
}

func TestPutCreate(t *testing.T) {
	e := newEnv(t, config{})
	in := api.UpdateArtifactRequest{
		Name: new("Fresh"), Visibility: new("unlisted"), Label: "first",
		Metadata: map[string]*string{"team": new("web"), "gone": nil},
		Files:    []api.FileInput{{Path: "index.html", Content: "<h1>one</h1>"}},
	}
	var res api.PublishResult
	e.call(http.MethodPut, e.u("/v1/artifacts/fresh?create=true"), e.alice, in, http.StatusCreated, &res)
	want := api.Artifact{Slug: "fresh", Name: "Fresh", Visibility: "unlisted", Metadata: map[string]string{"team": "web"}}
	got := api.Artifact{Slug: res.Artifact.Slug, Name: res.Artifact.Name, Visibility: res.Artifact.Visibility, Metadata: res.Artifact.Metadata}
	if diff := cmp.Diff(want, got); diff != "" || res.Version != 1 {
		t.Errorf("PUT ?create=true on a new slug: version %d (-want +got):\n%s", res.Version, diff)
	}

	in.Files = []api.FileInput{{Path: "two.html", Content: "two"}}
	e.call(http.MethodPut, e.u("/v1/artifacts/fresh?create=true"), e.alice, in, http.StatusOK, &res)
	if res.Version != 2 {
		t.Errorf("PUT ?create=true on an existing slug = version %d, want 2", res.Version)
	}

	e.apiErr(e.send(req{method: http.MethodPut, url: e.u("/v1/artifacts/other?create=true"), token: e.alice,
		body: bytes.NewReader(mustJSON(t, in)), contentType: "application/json", header: map[string]string{"If-Match": `"1"`}}),
		404, api.CodeNotFound)

	e.create(e.alice, "binned")
	e.call(http.MethodDelete, e.u("/v1/artifacts/binned"), e.alice, nil, http.StatusNoContent, nil)
	bob := e.member("bob", nil)
	e.apiErr(e.send(req{method: http.MethodPut, url: e.u("/v1/artifacts/binned?create=true"), token: bob,
		body: bytes.NewReader(mustJSON(t, in)), contentType: "application/json"}), 409, api.CodeSlugTaken)
}

func TestPutMirror(t *testing.T) {
	e := newEnv(t, config{})
	e.create(e.alice, "site", func(r *api.CreateArtifactRequest) {
		r.Files = append(r.Files, api.FileInput{Path: "old.js", Content: "old"}, api.FileInput{Path: "keep.css", Content: "k"})
	})
	bundle := gzipOf(t, tarOf(t, []archived{
		{"index.html", tar.TypeReg, "<h1>new</h1>"}, {"keep.css", tar.TypeReg, "k"}, {"new.js", tar.TypeReg, "new"},
	}))
	body, ct := multipartBody(t, part{"manifest", []byte(`{"mirror":true}`)}, part{"bundle", bundle})
	var res api.PublishResult
	e.expect(e.send(req{method: http.MethodPut, url: e.u("/v1/artifacts/site"), token: e.alice, body: body, contentType: ct}),
		http.StatusOK, &res)
	if diff := cmp.Diff([]string{"index.html", "keep.css", "new.js"}, e.paths("site", res.Version)); diff != "" {
		t.Errorf("mirror (-want +got):\n%s", diff)
	}

	in := api.UpdateArtifactRequest{Mirror: true, Files: []api.FileInput{{Path: "index.html", Content: "fresh"}}}
	e.call(http.MethodPut, e.u("/v1/artifacts/born?create=true"), e.alice, in, http.StatusCreated, nil)
	in.Files = append(in.Files, api.FileInput{Path: "b.txt", Content: "b"})
	e.call(http.MethodPut, e.u("/v1/artifacts/born?create=true"), e.alice, in, http.StatusOK, &res)
	in.Files = in.Files[1:]
	e.call(http.MethodPut, e.u("/v1/artifacts/born"), e.alice, in, http.StatusOK, &res)
	if got := e.paths("born", res.Version); !slices.Equal(got, []string{"b.txt"}) {
		t.Errorf("mirror with create = %v, want [b.txt]", got)
	}

	e.apiErr(e.send(req{method: http.MethodPatch, url: e.u("/v1/artifacts/born"), token: e.alice,
		body: strings.NewReader(`{"mirror":true}`), contentType: "application/json"}), 422, api.CodeInvalidArgument)
}

func TestGetArtifactETag(t *testing.T) {
	e := newEnv(t, config{})
	e.create(e.alice, "tagged")
	e.call(http.MethodPut, e.u("/v1/artifacts/tagged"), e.alice,
		api.UpdateArtifactRequest{Files: []api.FileInput{{Path: "b.txt", Content: "b"}}}, http.StatusOK, nil)
	resp := e.call(http.MethodGet, e.u("/v1/artifacts/tagged"), e.alice, nil, http.StatusOK, nil)
	if got := resp.Header.Get("ETag"); got != `"2"` {
		t.Errorf("GET /v1/artifacts/tagged ETag = %q, want %q", got, `"2"`)
	}
}
