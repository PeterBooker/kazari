package httpapi_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/httpapi"
	"github.com/PeterBooker/kazari/internal/upload"
)

const apiBase = "https://api.art.test/v1"

func zipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// beginArchive asks for an archive URL and returns it rebased onto the test
// server, since the response carries the instance's public address.
func (e *env) beginArchive(t *testing.T, archive []byte) (api.Upload, string) {
	t.Helper()
	var up api.Upload
	e.call(http.MethodPost, e.u("/v1/uploads"), e.alice,
		api.BeginUploadRequest{ArchiveSHA256: sha(string(archive)), ArchiveBytes: int64(len(archive))}, http.StatusCreated, &up)
	prefix := apiBase + "/uploads/" + up.UploadID + "/archive?token="
	if !strings.HasPrefix(up.ArchiveURL, prefix) || len(up.ArchiveURL) != len(prefix)+43 {
		t.Fatalf("archive_url = %q, want %s<43 characters>", up.ArchiveURL, prefix)
	}
	return up, e.u(strings.TrimPrefix(up.ArchiveURL, "https://api.art.test"))
}

func (e *env) put(url string, body []byte) *http.Response {
	e.t.Helper()
	return e.send(req{method: http.MethodPut, url: url, body: bytes.NewReader(body)})
}

func TestArchiveUpload(t *testing.T) {
	e := newEnv(t, config{user: httpapi.UserOptions{APIBaseURL: apiBase}})
	archive := zipBytes(t, map[string]string{"index.html": "<h1>zipped</h1>", "assets/app.js": "js"})
	up, put := e.beginArchive(t, archive)

	e.apiErr(e.send(req{method: http.MethodGet, url: put}), http.StatusUnauthorized, api.CodeUnauthorized)
	e.apiErr(e.put(put+"x", archive), http.StatusNotFound, api.CodeUploadNotFound)
	e.apiErr(e.put(e.u("/v1/uploads/AAAAAAAAAAAAAAAAAAAAAA/archive?token="+strings.Repeat("A", 43)), archive),
		http.StatusNotFound, api.CodeUploadNotFound)

	miss := e.apiErr(e.put(put, []byte("not the archive")), http.StatusUnprocessableEntity, api.CodeDigestMismatch)
	if !strings.Contains(miss.Message, "2 more attempts") {
		t.Errorf("mismatch message = %q", miss.Message)
	}
	long := e.apiErr(e.put(put, append(slices.Clone(archive), 'x')), http.StatusUnprocessableEntity, api.CodeDigestMismatch)
	if !strings.Contains(long.Message, "longer than") || !strings.Contains(long.Message, "1 more attempts") {
		t.Errorf("overlong message = %q", long.Message)
	}

	var res api.ArchiveResult
	e.expect(e.put(put, archive), http.StatusOK, &res)
	if res.UploadID != up.UploadID || res.Files != 2 || res.Bytes != int64(len("<h1>zipped</h1>")+len("js")) {
		t.Fatalf("archive result = %+v", res)
	}
	e.apiErr(e.put(put, archive), http.StatusNotFound, api.CodeUploadNotFound)

	var pub api.PublishResult
	e.call(http.MethodPost, e.u("/v1/artifacts"), e.alice, api.CreateArtifactRequest{Slug: "zipped", UploadID: up.UploadID},
		http.StatusCreated, &pub)
	if got := e.read(e.alice, "zipped", "index.html", 0); got != "<h1>zipped</h1>" {
		t.Errorf("index.html = %q", got)
	}
	if got := e.read(e.alice, "zipped", "assets/app.js", 0); got != "js" {
		t.Errorf("assets/app.js = %q", got)
	}
}

func TestArchiveUploadTarGz(t *testing.T) {
	e := newEnv(t, config{user: httpapi.UserOptions{APIBaseURL: apiBase}})
	archive := tarGz(t, tarEntry{name: "./index.html", body: "<h1>tar</h1>"}, tarEntry{name: "link", typ: tar.TypeSymlink, linkTo: "index.html"})
	up, put := e.beginArchive(t, archive)
	var res api.ArchiveResult
	e.expect(e.put(put, archive), http.StatusOK, &res)
	if res.Files != 1 || res.Bytes != int64(len("<h1>tar</h1>")) {
		t.Fatalf("archive result = %+v", res)
	}
	e.call(http.MethodPost, e.u("/v1/artifacts"), e.alice, api.CreateArtifactRequest{Slug: "tarred", UploadID: up.UploadID},
		http.StatusCreated, nil)
	if got := e.read(e.alice, "tarred", "index.html", 0); got != "<h1>tar</h1>" {
		t.Errorf("index.html = %q", got)
	}
}

func TestArchiveUploadAttemptsExhausted(t *testing.T) {
	e := newEnv(t, config{user: httpapi.UserOptions{APIBaseURL: apiBase}})
	archive := zipBytes(t, map[string]string{"index.html": "x"})
	up, put := e.beginArchive(t, archive)
	for i := 1; i < upload.MaxArchiveAttempts; i++ {
		e.apiErr(e.put(put, []byte("wrong")), http.StatusUnprocessableEntity, api.CodeDigestMismatch)
	}
	last := e.apiErr(e.put(put, []byte("wrong")), http.StatusUnprocessableEntity, api.CodeDigestMismatch)
	if !strings.Contains(last.Message, "the upload is removed") {
		t.Errorf("last message = %q", last.Message)
	}
	e.apiErr(e.put(put, archive), http.StatusNotFound, api.CodeUploadNotFound)
	if _, err := e.uploads.Get(e.ctx, "alice", up.UploadID); !errors.Is(err, artifact.ErrUploadNotFound) {
		t.Errorf("upload kept: %v", err)
	}
}

// A body that matches but is no archive Kazari can stage removes the upload.
func TestArchiveUploadUnreadable(t *testing.T) {
	tests := []struct {
		name    string
		archive []byte
		code    string
	}{
		{"neither zip nor gzip", []byte("plain text"), api.CodeInvalidArgument},
		{"broken zip", []byte("PK\x03\x04 and then nothing"), api.CodeInvalidArgument},
		{"escaping path", zipBytes(t, map[string]string{"../evil.html": "x"}), api.CodeInvalidPath},
		{"empty zip", zipBytes(t, nil), api.CodeInvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, config{user: httpapi.UserOptions{APIBaseURL: apiBase}})
			up, put := e.beginArchive(t, tt.archive)
			got := e.apiErr(e.put(put, tt.archive), http.StatusUnprocessableEntity, tt.code)
			if !strings.Contains(got.Message, "the upload is removed") {
				t.Errorf("message = %q", got.Message)
			}
			if _, err := e.uploads.Get(e.ctx, "alice", up.UploadID); !errors.Is(err, artifact.ErrUploadNotFound) {
				t.Errorf("upload kept: %v", err)
			}
		})
	}
}

func TestBeginUploadArchiveErrors(t *testing.T) {
	e := newEnv(t, config{user: httpapi.UserOptions{APIBaseURL: apiBase}})
	for _, tt := range []struct {
		name   string
		req    api.BeginUploadRequest
		status int
		code   string
	}{
		{"digest alone", api.BeginUploadRequest{ArchiveSHA256: sha("x")}, http.StatusUnprocessableEntity, api.CodeInvalidArgument},
		{"size alone", api.BeginUploadRequest{ArchiveBytes: 1}, http.StatusUnprocessableEntity, api.CodeInvalidArgument},
		{"not a digest", api.BeginUploadRequest{ArchiveSHA256: "abc", ArchiveBytes: 1}, http.StatusUnprocessableEntity, api.CodeInvalidArgument},
		{"over the limit", api.BeginUploadRequest{ArchiveSHA256: sha("x"), ArchiveBytes: upload.DefaultMaxTotalBytes + 1},
			http.StatusRequestEntityTooLarge, api.CodeTooLarge},
	} {
		resp := e.send(req{method: http.MethodPost, url: e.u("/v1/uploads"), token: e.alice,
			body: bytes.NewReader(mustJSON(t, tt.req)), contentType: "application/json"})
		e.with(t).apiErr(resp, tt.status, tt.code)
	}
}
