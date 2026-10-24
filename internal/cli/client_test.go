package cli

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"kazari/internal/api"
)

// captured is what a fake server saw.
type captured struct {
	method string
	path   string // escaped
	query  url.Values
	header http.Header
	body   []byte
}

// fakeServer answers every request with status and body and records it.
func fakeServer(t *testing.T, status int, body string) (*httptest.Server, *captured) {
	t.Helper()
	got := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*got = captured{method: r.Method, path: r.URL.EscapedPath(), query: r.URL.Query(), header: r.Header.Clone(), body: b}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// jsonEqual compares two JSON documents structurally.
func jsonEqual(t *testing.T, got []byte, want string) {
	t.Helper()
	var g, w any
	if len(got) == 0 && want == "" {
		return
	}
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("request body %q: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("want body %q: %v", want, err)
	}
	if diff := cmp.Diff(w, g); diff != "" {
		t.Errorf("request body (-want +got):\n%s", diff)
	}
}

func ptr[T any](v T) *T { return &v }

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func TestClientEndpoints(t *testing.T) {
	art := api.Artifact{Slug: "demo", Name: "Demo", URL: "https://demo.art.test", PublishedVersion: 2, LatestVersion: 2, CreatedAt: t0, UpdatedAt: t0}
	pub := api.PublishResult{URL: art.URL, Version: 2, Artifact: art}
	keys := []api.APIKey{{Prefix: "abc", Scopes: []string{"read"}, CreatedAt: t0}}

	tests := []struct {
		name    string
		call    func(ctx context.Context, c *Client) (any, error)
		reply   string
		method  string
		path    string
		query   url.Values
		reqBody string // JSON
		rawBody string // compared verbatim instead of reqBody
		header  map[string]string
		want    any
	}{
		{
			name: "create",
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.Create(ctx, api.CreateArtifactRequest{Slug: "demo", Files: []api.FileInput{{Path: "index.html", Content: "hi"}}})
			},
			reply:   mustJSON(t, pub),
			method:  "POST",
			path:    "/v1/artifacts",
			reqBody: `{"slug":"demo","files":[{"path":"index.html","content":"hi"}]}`,
			header:  map[string]string{"Content-Type": "application/json"},
			want:    &pub,
		},
		{
			name: "update json with expected version",
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.Update(ctx, "demo", api.UpdateArtifactRequest{Name: ptr("New"), ExpectedVersion: 1})
			},
			reply:   mustJSON(t, pub),
			method:  "PUT",
			path:    "/v1/artifacts/demo",
			reqBody: `{"name":"New","expected_version":1}`,
			header:  map[string]string{"If-Match": `"1"`},
			want:    &pub,
		},
		{
			name: "patch",
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.Patch(ctx, "demo", api.UpdateArtifactRequest{Metadata: map[string]*string{"a": ptr("1"), "b": nil}})
			},
			reply:   mustJSON(t, pub),
			method:  "PATCH",
			path:    "/v1/artifacts/demo",
			reqBody: `{"metadata":{"a":"1","b":null}}`,
			want:    &pub,
		},
		{
			name:   "get with files",
			call:   func(ctx context.Context, c *Client) (any, error) { return c.Get(ctx, "demo", true) },
			reply:  mustJSON(t, api.ArtifactDetail{Artifact: art}),
			method: "GET",
			path:   "/v1/artifacts/demo",
			query:  url.Values{"files": {"true"}},
			want:   &api.ArtifactDetail{Artifact: art},
		},
		{
			name: "list params",
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.List(ctx, ListParams{Mine: "created", Metadata: map[string]string{"team": "web"}, Stale: "only", Deleted: "include", Cursor: "c1", Limit: 5})
			},
			reply:  `{"items":[` + mustJSON(t, art) + `],"next_cursor":"c2"}`,
			method: "GET",
			path:   "/v1/artifacts",
			query: url.Values{"mine": {"created"}, "metadata.team": {"web"}, "stale": {"only"},
				"deleted": {"include"}, "cursor": {"c1"}, "limit": {"5"}},
			want: &api.ArtifactList{Items: []api.Artifact{art}, NextCursor: "c2"},
		},
		{
			name:    "publish version",
			call:    func(ctx context.Context, c *Client) (any, error) { return c.Publish(ctx, "demo", 1) },
			reply:   mustJSON(t, pub),
			method:  "POST",
			path:    "/v1/artifacts/demo/publish",
			reqBody: `{"version":1}`,
			want:    &pub,
		},
		{
			name:    "mark stale",
			call:    func(ctx context.Context, c *Client) (any, error) { return c.MarkStale(ctx, "demo", "24h") },
			reply:   mustJSON(t, api.StaleResult{StaleAt: t0, ExpiresAt: t0.Add(24 * time.Hour), Artifact: art}),
			method:  "POST",
			path:    "/v1/artifacts/demo/stale",
			reqBody: `{"grace":"24h"}`,
			want:    &api.StaleResult{StaleAt: t0, ExpiresAt: t0.Add(24 * time.Hour), Artifact: art},
		},
		{
			name:   "unmark stale",
			call:   func(ctx context.Context, c *Client) (any, error) { return c.UnmarkStale(ctx, "demo") },
			reply:  mustJSON(t, pub),
			method: "DELETE",
			path:   "/v1/artifacts/demo/stale",
			want:   &pub,
		},
		{
			name:    "set visibility",
			call:    func(ctx context.Context, c *Client) (any, error) { return c.SetVisibility(ctx, "demo", "unlisted") },
			reply:   mustJSON(t, pub),
			method:  "PATCH",
			path:    "/v1/artifacts/demo",
			reqBody: `{"visibility":"unlisted"}`,
			want:    &pub,
		},
		{
			name:   "hard delete",
			call:   func(ctx context.Context, c *Client) (any, error) { return nil, c.Delete(ctx, "demo", true) },
			reply:  `{"ok":true}`,
			method: "DELETE",
			path:   "/v1/artifacts/demo",
			query:  url.Values{"hard": {"true"}},
		},
		{
			name:   "me",
			call:   func(ctx context.Context, c *Client) (any, error) { return c.Me(ctx) },
			reply:  `{"handle":"peter","scopes":["read"],"quota_bytes":10}`,
			method: "GET",
			path:   "/v1/me",
			want:   &api.Me{Handle: "peter", Scopes: []string{"read"}, QuotaBytes: 10},
		},
		{
			name:   "my keys bare array",
			call:   func(ctx context.Context, c *Client) (any, error) { return c.MyKeys(ctx) },
			reply:  mustJSON(t, keys),
			method: "GET",
			path:   "/v1/me/keys",
			want:   keys,
		},
		{
			name:   "my keys enveloped",
			call:   func(ctx context.Context, c *Client) (any, error) { return c.MyKeys(ctx) },
			reply:  `{"keys":` + mustJSON(t, keys) + `}`,
			method: "GET",
			path:   "/v1/me/keys",
			want:   keys,
		},
		{
			name:   "begin upload",
			call:   func(ctx context.Context, c *Client) (any, error) { return c.BeginUpload(ctx) },
			reply:  `{"upload_id":"u1","max_chunk_bytes":100}`,
			method: "POST",
			path:   "/v1/uploads",
			want:   &api.Upload{UploadID: "u1", MaxChunkBytes: 100},
		},
		{
			name: "upload chunk",
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.UploadChunk(ctx, "u1", "css/site main.css", 10, strings.NewReader("body{}"), "text/css")
			},
			reply:   `{"path":"css/site main.css","bytes_received":16}`,
			method:  "PATCH",
			path:    "/v1/uploads/u1/files/css/site%20main.css",
			query:   url.Values{"offset": {"10"}},
			rawBody: "body{}",
			header:  map[string]string{"Content-Type": "text/css"},
			want:    &api.ChunkResult{Path: "css/site main.css", BytesReceived: 16},
		},
		{
			name:   "abort upload",
			call:   func(ctx context.Context, c *Client) (any, error) { return nil, c.AbortUpload(ctx, "u1") },
			reply:  `{"ok":true}`,
			method: "DELETE",
			path:   "/v1/uploads/u1",
		},
		// Admin API.
		{
			name:   "list users",
			call:   func(ctx context.Context, c *Client) (any, error) { return c.ListUsers(ctx) },
			reply:  `{"items":[{"handle":"ann","role":"admin","quota_bytes":0,"created_at":"2026-10-01T12:00:00Z"}]}`,
			method: "GET",
			path:   "/v1/users",
			want:   []api.User{{Handle: "ann", Role: "admin", CreatedAt: t0}},
		},
		{
			name: "create user",
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.CreateUser(ctx, api.CreateUserRequest{Handle: "ann", QuotaBytes: ptr(int64(1024))})
			},
			reply:   `{"handle":"ann","role":"member","quota_bytes":1024,"created_at":"2026-10-01T12:00:00Z"}`,
			method:  "POST",
			path:    "/v1/users",
			reqBody: `{"handle":"ann","quota_bytes":1024}`,
			want:    &api.User{Handle: "ann", Role: "member", QuotaBytes: 1024, CreatedAt: t0},
		},
		{
			name: "update user",
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.UpdateUser(ctx, "ann", api.UpdateUserRequest{Disabled: ptr(true)})
			},
			reply:   `{"handle":"ann","role":"member","quota_bytes":0,"created_at":"2026-10-01T12:00:00Z"}`,
			method:  "PATCH",
			path:    "/v1/users/ann",
			reqBody: `{"disabled":true}`,
			want:    &api.User{Handle: "ann", Role: "member", CreatedAt: t0},
		},
		{
			name:   "list keys",
			call:   func(ctx context.Context, c *Client) (any, error) { return c.ListKeys(ctx, "ann") },
			reply:  mustJSON(t, keys),
			method: "GET",
			path:   "/v1/users/ann/keys",
			want:   keys,
		},
		{
			name: "issue key",
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.IssueKey(ctx, "ann", api.IssueKeyRequest{Label: "ci", Scopes: []string{"read"}, ExpiresIn: "720h"})
			},
			reply:   `{"key":"kzk_abc_secret","api_key":` + mustJSON(t, keys[0]) + `}`,
			method:  "POST",
			path:    "/v1/users/ann/keys",
			reqBody: `{"label":"ci","scopes":["read"],"expires_in":"720h"}`,
			want:    &api.IssuedKey{Key: "kzk_abc_secret", APIKey: keys[0]},
		},
		{
			name:   "revoke key",
			call:   func(ctx context.Context, c *Client) (any, error) { return nil, c.RevokeKey(ctx, "ann", "abc") },
			reply:  `{"ok":true}`,
			method: "DELETE",
			path:   "/v1/users/ann/keys/abc",
		},
		{
			name: "admin list artifacts",
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.AdminListArtifacts(ctx, ListParams{Limit: 2})
			},
			reply:  `{"items":[]}`,
			method: "GET",
			path:   "/v1/artifacts",
			query:  url.Values{"limit": {"2"}},
			want:   &api.ArtifactList{Items: []api.Artifact{}},
		},
		{
			name:    "transfer",
			call:    func(ctx context.Context, c *Client) (any, error) { return c.Transfer(ctx, "demo", "ann") },
			reply:   mustJSON(t, art),
			method:  "POST",
			path:    "/v1/artifacts/demo/transfer",
			reqBody: `{"owner":"ann"}`,
			want:    &art,
		},
		{
			name:   "hide",
			call:   func(ctx context.Context, c *Client) (any, error) { return c.Hide(ctx, "demo") },
			reply:  mustJSON(t, art),
			method: "POST",
			path:   "/v1/artifacts/demo/hide",
			want:   &art,
		},
		{
			name:   "settings",
			call:   func(ctx context.Context, c *Client) (any, error) { return c.Settings(ctx) },
			reply:  `{"server":{"base_domain":"art.test"}}`,
			method: "GET",
			path:   "/v1/settings",
			want:   map[string]any{"server": map[string]any{"base_domain": "art.test"}},
		},
		{
			name:   "jobs",
			call:   func(ctx context.Context, c *Client) (any, error) { return c.Jobs(ctx) },
			reply:  `[{"name":"gc","every":"1h0m0s","running":false}]`,
			method: "GET",
			path:   "/v1/jobs",
			want:   []api.JobStatus{{Name: "gc", Every: "1h0m0s"}},
		},
		{
			name:   "run job",
			call:   func(ctx context.Context, c *Client) (any, error) { return nil, c.RunJob(ctx, "gc") },
			reply:  `{"ok":true}`,
			method: "POST",
			path:   "/v1/jobs/gc/run",
		},
		{
			name: "audit",
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.Audit(ctx, "2026-10-01T00:00:00Z", "demo", 10)
			},
			reply:  `[{"at":"2026-10-01T12:00:00Z","actor":"ann","action":"create","slug":"demo"}]`,
			method: "GET",
			path:   "/v1/audit",
			query:  url.Values{"since": {"2026-10-01T00:00:00Z"}, "slug": {"demo"}, "limit": {"10"}},
			want:   []api.AuditEvent{{At: t0, Actor: "ann", Action: "create", Slug: "demo"}},
		},
		{
			name:   "health",
			call:   func(ctx context.Context, c *Client) (any, error) { return c.Health(ctx) },
			reply:  `{"status":"ok","version":"1.0"}`,
			method: "GET",
			path:   "/v1/health",
			want:   &api.Health{Status: "ok", Version: "1.0"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, got := fakeServer(t, http.StatusOK, tt.reply)
			c := NewClient(srv.URL+"/", "tok", srv.Client())
			res, err := tt.call(t.Context(), c)
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			if got.method != tt.method || got.path != tt.path {
				t.Errorf("request = %s %s, want %s %s", got.method, got.path, tt.method, tt.path)
			}
			wantQuery := tt.query
			if wantQuery == nil {
				wantQuery = url.Values{}
			}
			if diff := cmp.Diff(wantQuery, got.query); diff != "" {
				t.Errorf("query (-want +got):\n%s", diff)
			}
			if tt.rawBody != "" {
				if string(got.body) != tt.rawBody {
					t.Errorf("request body = %q, want %q", got.body, tt.rawBody)
				}
			} else {
				jsonEqual(t, got.body, tt.reqBody)
			}
			if h := got.header.Get("Authorization"); h != "Bearer tok" {
				t.Errorf("Authorization = %q", h)
			}
			if h := got.header.Get("Accept"); h != "application/json" {
				t.Errorf("Accept = %q", h)
			}
			if h := got.header.Get("User-Agent"); !strings.HasPrefix(h, "kazari-cli/") {
				t.Errorf("User-Agent = %q", h)
			}
			for k, v := range tt.header {
				if h := got.header.Get(k); h != v {
					t.Errorf("%s = %q, want %q", k, h, v)
				}
			}
			if tt.want == nil {
				return
			}
			if diff := cmp.Diff(tt.want, res); diff != "" {
				t.Errorf("result (-want +got):\n%s", diff)
			}
		})
	}
}

func TestClientNoTokenNoAuthorization(t *testing.T) {
	srv, got := fakeServer(t, http.StatusOK, `{"status":"ok"}`)
	if _, err := NewClient(srv.URL, "", nil).Health(t.Context()); err != nil {
		t.Fatal(err)
	}
	if h, ok := got.header["Authorization"]; ok {
		t.Errorf("Authorization sent without a token: %q", h)
	}
}

func TestClientErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   HTTPError
	}{
		{"api error", 409, `{"error":{"code":"slug_taken","message":"slug demo is taken"}}`,
			HTTPError{Status: 409, Err: api.Error{Code: "slug_taken", Message: "slug demo is taken"}}},
		{"offset mismatch", 409, `{"error":{"code":"upload_offset_mismatch","message":"bad offset","current":12}}`,
			HTTPError{Status: 409, Err: api.Error{Code: "upload_offset_mismatch", Message: "bad offset", Current: ptr(int64(12))}}},
		{"plain text", 502, "  bad gateway\n", HTTPError{Status: 502, Err: api.Error{Code: "http_502", Message: "bad gateway"}}},
		{"empty", 503, "", HTTPError{Status: 503, Err: api.Error{Code: "http_503", Message: "Service Unavailable"}}},
		{"json without code", 400, `{"oops":1}`, HTTPError{Status: 400, Err: api.Error{Code: "http_400", Message: `{"oops":1}`}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := fakeServer(t, tt.status, tt.body)
			_, err := NewClient(srv.URL, "tok", nil).Get(t.Context(), "demo", false)
			var he *HTTPError
			if !errors.As(err, &he) {
				t.Fatalf("err = %v (%T), want *HTTPError", err, err)
			}
			if diff := cmp.Diff(tt.want, *he); diff != "" {
				t.Errorf("error (-want +got):\n%s", diff)
			}
			var ae *api.Error
			if !errors.As(err, &ae) || ae.Code != tt.want.Err.Code {
				t.Errorf("errors.As api.Error = %v", ae)
			}
			if !IsCode(err, tt.want.Err.Code) {
				t.Errorf("IsCode(%q) = false", tt.want.Err.Code)
			}
		})
	}
}

func TestReadFile(t *testing.T) {
	var gotPath string
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.EscapedPath(), r.URL.Query()
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte{0x89, 'P', 'N', 'G'})
	}))
	defer srv.Close()
	body, ct, err := NewClient(srv.URL, "tok", nil).ReadFile(t.Context(), "demo", "img/a b.png", 3)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	b, _ := io.ReadAll(body)
	if string(b) != "\x89PNG" || ct != "image/png" {
		t.Errorf("got %q %q", b, ct)
	}
	if gotPath != "/v1/artifacts/demo/files/img/a%20b.png" || gotQuery.Get("version") != "3" {
		t.Errorf("request %s ?%s", gotPath, gotQuery.Encode())
	}
}

// bundleRequest is a decoded multipart publish.
type bundleRequest struct {
	method, path string
	header       http.Header
	manifest     map[string]any
	bundleType   string
	files        map[string]string
}

// decodeBundleRequest parses a multipart publish: the manifest JSON and the
// gzip tar entries of the bundle.
func decodeBundleRequest(t *testing.T, r *http.Request) bundleRequest {
	t.Helper()
	out := bundleRequest{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), files: map[string]string{}}
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" {
		t.Errorf("Content-Type = %q", r.Header.Get("Content-Type"))
		return out
	}
	mr, err := r.MultipartReader()
	if err != nil {
		t.Errorf("multipart: %v", err)
		return out
	}
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Errorf("next part: %v", err)
			return out
		}
		switch p.FormName() {
		case "manifest":
			if err := json.NewDecoder(p).Decode(&out.manifest); err != nil {
				t.Errorf("manifest: %v", err)
			}
		case "bundle":
			out.bundleType = p.Header.Get("Content-Type")
			out.files = readTarGz(t, p)
		default:
			t.Errorf("unexpected part %q", p.FormName())
		}
	}
	return out
}

func readTarGz(t *testing.T, r io.Reader) map[string]string {
	t.Helper()
	files := map[string]string{}
	zr, err := gzip.NewReader(r)
	if err != nil {
		t.Errorf("gzip: %v", err)
		return files
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Errorf("tar: %v", err)
			return files
		}
		b, _ := io.ReadAll(tr)
		files[h.Name] = string(b)
	}
	return files
}

func TestCreateAndUpdateBundle(t *testing.T) {
	dir := writeTree(t, map[string]string{"index.html": "<h1>hi</h1>", "css/a.css": "a{}"})
	var got bundleRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = decodeBundleRequest(t, r)
		io.WriteString(w, `{"url":"https://demo.art.test","version":1}`)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "tok", nil)

	bundle := TarGzReader(dir)
	res, err := c.CreateBundle(t.Context(), api.CreateManifest{Slug: "demo", Label: "first"}, bundle)
	bundle.Close()
	if err != nil {
		t.Fatal(err)
	}
	if res.URL != "https://demo.art.test" || got.method != "POST" || got.path != "/v1/artifacts" {
		t.Errorf("res %+v, request %s %s", res, got.method, got.path)
	}
	if diff := cmp.Diff(map[string]any{"slug": "demo", "label": "first"}, got.manifest); diff != "" {
		t.Errorf("manifest (-want +got):\n%s", diff)
	}
	if got.bundleType != "application/gzip" {
		t.Errorf("bundle Content-Type = %q", got.bundleType)
	}
	wantFiles := map[string]string{"index.html": "<h1>hi</h1>", "css/a.css": "a{}"}
	if diff := cmp.Diff(wantFiles, got.files); diff != "" {
		t.Errorf("bundle (-want +got):\n%s", diff)
	}
	if got.header.Get("If-Match") != "" {
		t.Errorf("If-Match on create: %q", got.header.Get("If-Match"))
	}

	bundle = TarGzReader(dir)
	_, err = c.UpdateBundle(t.Context(), "demo", api.UpdateArtifactRequest{ExpectedVersion: 4,
		Files: []api.FileInput{{Path: "old.html", Delete: true}}}, bundle)
	bundle.Close()
	if err != nil {
		t.Fatal(err)
	}
	if got.method != "PUT" || got.path != "/v1/artifacts/demo" || got.header.Get("If-Match") != `"4"` {
		t.Errorf("request %s %s If-Match %q", got.method, got.path, got.header.Get("If-Match"))
	}
	wantManifest := map[string]any{"expected_version": 4.0, "files": []any{map[string]any{"path": "old.html", "delete": true}}}
	if diff := cmp.Diff(wantManifest, got.manifest); diff != "" {
		t.Errorf("manifest (-want +got):\n%s", diff)
	}
}

// An error response that arrives before the bundle is fully sent still
// surfaces as an HTTPError.
func TestCreateBundleEarlyError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		io.WriteString(w, `{"error":{"code":"too_large","message":"bundle over 1 MiB"}}`)
	}))
	defer srv.Close()
	_, err := NewClient(srv.URL, "tok", nil).CreateBundle(t.Context(), api.CreateManifest{Slug: "demo"},
		io.LimitReader(zeroReader{}, 1<<20))
	if !IsCode(err, api.CodeTooLarge) {
		t.Fatalf("err = %v, want too_large", err)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
