package httpapi_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"path/filepath"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/blob"
	"github.com/PeterBooker/kazari/internal/fetch"
	"github.com/PeterBooker/kazari/internal/httpapi"
	"github.com/PeterBooker/kazari/internal/present"
	"github.com/PeterBooker/kazari/internal/ratelimit"
	"github.com/PeterBooker/kazari/internal/scheduler"
	"github.com/PeterBooker/kazari/internal/snapshot"
	"github.com/PeterBooker/kazari/internal/sqlite"
	"github.com/PeterBooker/kazari/internal/upload"
	"github.com/PeterBooker/kazari/internal/user"
)

var t0 = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

const adminToken = "admin-secret"

type config struct {
	artifact artifact.Options
	upload   upload.Options
	user     httpapi.UserOptions
	admin    httpapi.AdminOptions
	grants   httpapi.GrantStore
	// writeRate is per credential; 0 disables it.
	writeRate int
}

type env struct {
	t       *testing.T
	ctx     context.Context
	art     *artifact.Service
	users   *user.Service
	uploads *upload.Service
	jobRuns *atomic.Int32
	userURL string
	admURL  string
	// alice is the key of a member with every member scope.
	alice string
}

func newEnv(t *testing.T, c config) *env {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := sqlite.Open(ctx, filepath.Join(dir, "kazari.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	blobs, err := blob.NewFS(filepath.Join(dir, "blobs"), blob.Options{})
	if err != nil {
		t.Fatal(err)
	}
	now := func() time.Time { return t0 }
	rec := sqlite.NewAuditStore(db)

	c.artifact.Now = now
	// Loopback is allowed so tests can fetch from httptest servers.
	fc := fetch.New(fetch.Options{Transport: http.DefaultTransport, Dir: dir})
	c.artifact.Pages = snapshot.New(fc, snapshot.Options{})
	art := artifact.New(sqlite.NewArtifactStore(db), blobs, rec, c.artifact)
	c.upload.Dir = filepath.Join(dir, "uploads")
	c.upload.Now = now
	uploads := upload.New(sqlite.NewUploadStore(db), c.upload)
	users := user.New(sqlite.NewUserStore(db), rec, user.Options{Now: now})

	var runs atomic.Int32
	jobs := scheduler.New(scheduler.Options{Now: now}, scheduler.Job{
		Name:  "count",
		Every: time.Hour,
		Run:   func(context.Context) error { runs.Add(1); return nil },
	})
	links := present.Links{URL: func(sub string) string { return "https://" + sub + ".art.test/" }}

	if c.user.IPRatePerMinute == 0 {
		c.user.IPRatePerMinute = -1 // tests make many requests from one IP
	}
	if c.admin.IPRatePerMinute == 0 {
		c.admin.IPRatePerMinute = -1
	}
	c.admin.AdminToken = adminToken
	c.admin.Now = now
	c.user.TempDir = dir
	c.user.Now = now
	userSrv := httptest.NewServer(httpapi.NewUserAPI(httpapi.UserDeps{
		Artifacts: art, Uploads: uploads, Users: users, Auth: users, Links: links,
		Writes: ratelimit.New(c.writeRate, ratelimit.Options{Now: now, Scope: ratelimit.ScopeCredential}), Fetch: fc,
	}, c.user))
	t.Cleanup(userSrv.Close)
	admSrv := httptest.NewServer(httpapi.NewAdminAPI(httpapi.AdminDeps{
		Artifacts: art,
		Users:     users,
		Auth:      users,
		Audit:     rec,
		Jobs:      jobs,
		Grants:    c.grants,
		Settings:  func() map[string]any { return map[string]any{"base_domain": "art.test", "admin_token": "***"} },
		Metrics: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			io.WriteString(w, "kazari_up 1\n")
		}),
		Links: links,
	}, c.admin))
	t.Cleanup(admSrv.Close)

	e := &env{t: t, ctx: ctx, art: art, users: users, uploads: uploads, jobRuns: &runs,
		userURL: userSrv.URL, admURL: admSrv.URL}
	e.alice = e.member("alice", nil)
	return e
}

// member returns a key with scopes, nil meaning every member scope.
func (e *env) member(handle string, scopes []string) string {
	e.t.Helper()
	if _, err := e.users.GetUser(e.ctx, handle); err != nil {
		if _, err := e.users.CreateUser(e.ctx, user.CreateUserRequest{Handle: handle}); err != nil {
			e.t.Fatal(err)
		}
	}
	_, key, err := e.users.IssueKey(e.ctx, handle, "test", scopes, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	return key
}

type req struct {
	method, url, token string
	body               io.Reader
	contentType        string
	header             map[string]string
}

func (e *env) send(r req) *http.Response {
	e.t.Helper()
	hr, err := http.NewRequestWithContext(e.ctx, r.method, r.url, r.body)
	if err != nil {
		e.t.Fatal(err)
	}
	if r.token != "" {
		hr.Header.Set("Authorization", "Bearer "+r.token)
	}
	if r.contentType != "" {
		hr.Header.Set("Content-Type", r.contentType)
	}
	for k, v := range r.header {
		hr.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (e *env) call(method, url, token string, in any, want int, out any, header ...string) *http.Response {
	e.t.Helper()
	var body io.Reader
	ct := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			e.t.Fatal(err)
		}
		body, ct = bytes.NewReader(b), "application/json"
	}
	h := map[string]string{}
	for i := 0; i+1 < len(header); i += 2 {
		h[header[i]] = header[i+1]
	}
	resp := e.send(req{method: method, url: url, token: token, body: body, contentType: ct, header: h})
	return e.expect(resp, want, out)
}

func (e *env) expect(resp *http.Response, want int, out any) *http.Response {
	e.t.Helper()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	if resp.StatusCode != want {
		e.t.Fatalf("%s %s: status %d, want %d; body %s", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, want, raw)
	}
	if out != nil {
		reflect.ValueOf(out).Elem().SetZero() // decode into a clean value even when reused
		if err := json.Unmarshal(raw, out); err != nil {
			e.t.Fatalf("%s %s: decode %s: %v", resp.Request.Method, resp.Request.URL.Path, raw, err)
		}
	}
	return resp
}

func (e *env) apiErr(resp *http.Response, status int, code string) api.Error {
	e.t.Helper()
	var body api.ErrorResponse
	e.expect(resp, status, &body)
	if body.Error.Code != code {
		e.t.Fatalf("%s %s: code %q, want %q (%s)", resp.Request.Method, resp.Request.URL.Path, body.Error.Code, code, body.Error.Message)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		e.t.Errorf("error Content-Type = %q", ct)
	}
	return body.Error
}

// with reports to t, for use inside subtests.
func (e *env) with(t *testing.T) *env {
	c := *e
	c.t = t
	return &c
}

func (e *env) u(path string) string { return e.userURL + path }
func (e *env) a(path string) string { return e.admURL + path }

func (e *env) create(token, slug string, extra ...func(*api.CreateArtifactRequest)) api.PublishResult {
	e.t.Helper()
	r := api.CreateArtifactRequest{Slug: slug, Files: []api.FileInput{{Path: "index.html", Content: "<h1>" + slug + "</h1>"}}}
	for _, f := range extra {
		f(&r)
	}
	var out api.PublishResult
	e.call(http.MethodPost, e.u("/v1/artifacts"), token, r, http.StatusCreated, &out)
	return out
}

func (e *env) read(token, slug, path string, version int) string {
	e.t.Helper()
	url := e.u("/v1/artifacts/" + slug + "/files/" + path)
	if version > 0 {
		url += "?version=" + strconv.Itoa(version)
	}
	resp := e.send(req{method: http.MethodGet, url: url, token: token})
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		e.t.Fatalf("read %s/%s: status %d: %s", slug, path, resp.StatusCode, b)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

type tarEntry struct {
	name   string
	body   string
	typ    byte
	linkTo string
}

func tarGz(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, en := range entries {
		typ := en.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		hdr := &tar.Header{Name: en.name, Typeflag: typ, Mode: 0o644, Linkname: en.linkTo}
		if typ == tar.TypeReg {
			hdr.Size = int64(len(en.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := io.WriteString(tw, en.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type part struct {
	name string
	body []byte
}

func multipartBody(t *testing.T, parts ...part) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="`+p.name+`"`)
		w, err := mw.CreatePart(h)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(p.body)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, mw.FormDataContentType()
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func ptr[T any](v T) *T { return &v }

var readOnly = []string{auth.ScopeRead}

func httptestServer(t *testing.T, h http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

func userReq(handle, role string) user.CreateUserRequest {
	return user.CreateUserRequest{Handle: handle, Role: user.Role(role)}
}
