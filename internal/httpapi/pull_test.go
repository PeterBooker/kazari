package httpapi_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/httpapi"
)

func TestDownloadBundle(t *testing.T) {
	e := newEnv(t, config{})
	v1 := map[string]string{
		"index.html":                 "<h1>one</h1>",
		"css/site.css":               "body{}",
		"deep/" + longName + "/a.js": "1",
	}
	e.create(e.alice, "demo", func(r *api.CreateArtifactRequest) {
		r.Files = nil
		for p, c := range v1 {
			r.Files = append(r.Files, api.FileInput{Path: p, Content: c})
		}
	})
	e.call(http.MethodPut, e.u("/v1/artifacts/demo"), e.alice, api.UpdateArtifactRequest{
		Stage: true, Files: []api.FileInput{{Path: "index.html", Content: "<h1>two</h1>"}},
	}, http.StatusOK, nil)
	v2 := map[string]string{"index.html": "<h1>two</h1>", "css/site.css": "body{}", "deep/" + longName + "/a.js": "1"}

	tests := []struct {
		name    string
		query   string
		version string
		files   map[string]string
	}{
		{"published", "", "1", v1},
		{"staged version", "?version=2", "2", v2},
		{"version zero is published", "?version=0", "1", v1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := e.with(t).bundle(e.alice, "demo", tc.query)
			if got := resp.Header.Get(httpapi.VersionHeader); got != tc.version {
				t.Errorf("%s = %q, want %q", httpapi.VersionHeader, got, tc.version)
			}
			if got, want := resp.Header.Get("Content-Disposition"), `attachment; filename="demo-v`+tc.version+`.tar.gz"`; got != want {
				t.Errorf("Content-Disposition = %q, want %q", got, want)
			}
			if got := resp.Header.Get("Content-Type"); got != "application/gzip" {
				t.Errorf("Content-Type = %q, want application/gzip", got)
			}
			got, err := untarGz(bytes.NewReader(body))
			if err != nil {
				t.Fatalf("read bundle: %v", err)
			}
			if diff := cmp.Diff(tc.files, got); diff != "" {
				t.Errorf("bundle files (-want +got):\n%s", diff)
			}
		})
	}

	t.Run("same version same bytes", func(t *testing.T) {
		_, a := e.with(t).bundle(e.alice, "demo", "")
		_, b := e.with(t).bundle(e.alice, "demo", "")
		if len(a) == 0 || !bytes.Equal(a, b) {
			t.Errorf("two bundles of version 1 differ: %d and %d bytes", len(a), len(b))
		}
	})

	t.Run("refused", func(t *testing.T) {
		writeOnly := e.member("alice", []string{auth.ScopeWrite})
		e.apiErr(e.jsonReq(http.MethodGet, e.u("/v1/artifacts/demo/bundle?version=9"), e.alice, nil), http.StatusNotFound, api.CodeVersionNotFound)
		e.apiErr(e.jsonReq(http.MethodGet, e.u("/v1/artifacts/demo/bundle?version=x"), e.alice, nil), http.StatusUnprocessableEntity, api.CodeInvalidArgument)
		e.apiErr(e.jsonReq(http.MethodGet, e.u("/v1/artifacts/missing/bundle"), e.alice, nil), http.StatusNotFound, api.CodeNotFound)
		e.apiErr(e.jsonReq(http.MethodGet, e.u("/v1/artifacts/demo/bundle"), writeOnly, nil), http.StatusForbidden, api.CodeForbidden)
	})
}

func TestDownloadBundleOfUnpublishedArtifact(t *testing.T) {
	e := newEnv(t, config{})
	e.create(e.alice, "demo")
	e.call(http.MethodPatch, e.u("/v1/artifacts/demo"), e.alice, map[string]any{"visibility": "unpublished"}, http.StatusOK, nil)
	bob := e.member("bob", nil)

	_, body := e.bundle(e.alice, "demo", "")
	got, err := untarGz(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}
	if diff := cmp.Diff(map[string]string{"index.html": "<h1>demo</h1>"}, got); diff != "" {
		t.Errorf("owner's bundle of an unpublished page (-want +got):\n%s", diff)
	}
	e.apiErr(e.jsonReq(http.MethodGet, e.u("/v1/artifacts/demo/bundle"), bob, nil), http.StatusNotFound, api.CodeNotFound)
}

func TestDownloadBundleMissingBlob(t *testing.T) {
	tests := []struct {
		name    string
		missing string
		started bool
	}{
		{"before the first byte", "a.html", false},
		{"mid-stream", "b.html", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, config{})
			e.create(e.alice, "demo", func(r *api.CreateArtifactRequest) {
				r.Files = []api.FileInput{{Path: "a.html", Content: "first"}, {Path: "b.html", Content: "second"}}
			})
			var d api.ArtifactDetail
			e.call(http.MethodGet, e.u("/v1/artifacts/demo?files=true"), e.alice, nil, http.StatusOK, &d)
			var hash string
			for _, f := range d.Versions[0].Files {
				if f.Path == tc.missing {
					hash = f.Hash
				}
			}
			if err := e.blobs.Delete(e.ctx, hash); err != nil {
				t.Fatal(err)
			}
			if !tc.started {
				resp := e.send(req{method: http.MethodGet, url: e.u("/v1/artifacts/demo/bundle"), token: e.alice})
				if resp.StatusCode != http.StatusInternalServerError {
					t.Errorf("bundle with %s missing: status %d, want 500", tc.missing, resp.StatusCode)
				}
				return
			}
			// Whether the client sees a refused request or a cut body depends
			// on how much the server had flushed; neither is a whole archive.
			hr, err := http.NewRequestWithContext(t.Context(), http.MethodGet, e.u("/v1/artifacts/demo/bundle"), nil)
			if err != nil {
				t.Fatal(err)
			}
			hr.Header.Set("Authorization", "Bearer "+e.alice)
			resp, err := http.DefaultClient.Do(hr)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			if _, err := untarGz(resp.Body); err == nil {
				t.Errorf("bundle with %s missing read as a whole archive (status %d), want an error", tc.missing, resp.StatusCode)
			}
		})
	}
}

// longName is past the 100 bytes a plain tar header holds.
const longName = "a-directory-name-long-enough-that-the-path-of-a-file-under-it-needs-more-than-a-ustar-name-field"

// bundle downloads the bundle at query and fails the test on any status
// but 200.
func (e *env) bundle(token, slug, query string) (*http.Response, []byte) {
	e.t.Helper()
	resp := e.send(req{method: http.MethodGet, url: e.u("/v1/artifacts/" + slug + "/bundle" + query), token: token})
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("GET %s bundle%s: status %d, want 200; body %s", slug, query, resp.StatusCode, b)
	}
	return resp, b
}

func untarGz(r io.Reader) (map[string]string, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(zr)
	files := map[string]string{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg || hdr.Mode != 0o644 {
			return nil, errors.New(hdr.Name + " is not a regular file with mode 0644")
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		files[hdr.Name] = string(b)
	}
}
