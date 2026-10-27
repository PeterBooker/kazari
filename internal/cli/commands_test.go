package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/api"
)

func TestMain(m *testing.M) {
	// Human output prints local times.
	time.Local = time.FixedZone("CEST", 2*60*60)
	os.Exit(m.Run())
}

func envMap(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

type result struct {
	code           int
	stdout, stderr string
}

func runCLI(t *testing.T, env map[string]string, args ...string) result {
	t.Helper()
	if env == nil {
		env = map[string]string{}
	}
	if _, ok := env["HOME"]; !ok {
		env["HOME"] = t.TempDir()
	}
	var stdout, stderr bytes.Buffer
	code := Run(args, strings.NewReader(""), &stdout, &stderr, envMap(env))
	return result{code, stdout.String(), stderr.String()}
}

type fakeAPI struct {
	t      *testing.T
	mu     sync.Mutex
	routes map[string]http.HandlerFunc
	seen   []string
	auth   []string
}

func newFakeAPI(t *testing.T, routes map[string]http.HandlerFunc) (*fakeAPI, *httptest.Server) {
	f := &fakeAPI{t: t, routes: routes}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := r.Method + " " + r.URL.Path
	f.mu.Lock()
	f.seen = append(f.seen, key)
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	h, ok := f.routes[key]
	f.mu.Unlock()
	if !ok {
		f.t.Errorf("unexpected request %s", key)
		reply(w, http.StatusNotFound, api.ErrorResponse{Error: api.Error{Code: "not_found", Message: "no route"}})
		return
	}
	h(w, r)
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func replyWith(status int, v any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { reply(w, status, v) }
}

var demo = api.Artifact{
	Slug: "demo", Name: "Demo page", Owner: "peter", Visibility: "public",
	Metadata: map[string]string{"team": "web"}, URL: "https://demo.art.test",
	PublishedVersion: 2, LatestVersion: 3, SizeBytes: 1536, CreatedAt: t0, UpdatedAt: t0.Add(time.Hour),
}

func TestSettingsPrecedence(t *testing.T) {
	writeConfig := func(t *testing.T, dir, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, "kazari"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "kazari", "config.yaml"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const file = `url: https://api.file.test
token: file-token
admin_url: https://admin.file.test
admin_token: file-admin
`
	tests := []struct {
		name    string
		config  string
		env     map[string]string
		flags   CLI
		want    settings
		wantErr string
	}{
		{
			name:   "file only",
			config: file,
			want:   settings{URL: "https://api.file.test", Token: "file-token", AdminURL: "https://admin.file.test", AdminToken: "file-admin"},
		},
		{
			name:   "env beats file",
			config: file,
			env:    map[string]string{"KAZARI_URL": "https://api.env.test", "KAZARI_TOKEN": "env-token", "KAZARI_ADMIN_TOKEN": "env-admin"},
			want:   settings{URL: "https://api.env.test", Token: "env-token", AdminURL: "https://admin.file.test", AdminToken: "env-admin"},
		},
		{
			name:   "flag beats env",
			config: file,
			env:    map[string]string{"KAZARI_URL": "https://api.env.test", "KAZARI_TOKEN": "env-token"},
			flags:  CLI{URL: "https://api.flag.test", AdminURL: "https://admin.flag.test"},
			want:   settings{URL: "https://api.flag.test", Token: "env-token", AdminURL: "https://admin.flag.test", AdminToken: "file-admin"},
		},
		{
			name:  "admin defaults from user settings",
			flags: CLI{URL: "https://api.art.test:8443", Token: "t"},
			want:  settings{URL: "https://api.art.test:8443", Token: "t", AdminURL: "https://admin.art.test:8443", AdminToken: "t"},
		},
		{
			name:  "no admin url for a non-api host",
			flags: CLI{URL: "http://localhost:8080", Token: "t"},
			want:  settings{URL: "http://localhost:8080", Token: "t", AdminToken: "t"},
		},
		{
			name:    "explicit config must exist",
			flags:   CLI{ClientConfig: "/nonexistent/kazari.yaml"},
			wantErr: "config /nonexistent/kazari.yaml",
		},
		{
			name:    "unknown key",
			config:  "url: x\ntokn: y\n",
			wantErr: "line 2: field tokn not found",
		},
		{
			name:    "bad yaml",
			config:  "url: [unclosed\n",
			wantErr: "config ",
		},
		{
			name:    "wrong type",
			config:  "url: [a, b]\n",
			wantErr: "cannot unmarshal",
		},
		{
			name:   "comments only",
			config: "# url: https://api.file.test\n",
			want:   settings{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			xdg := t.TempDir()
			if tt.config != "" {
				writeConfig(t, xdg, tt.config)
			}
			env := map[string]string{"XDG_CONFIG_HOME": xdg}
			for k, v := range tt.env {
				env[k] = v
			}
			r := &runner{lookupEnv: envMap(env), g: &tt.flags}
			got, err := r.settings()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("settings (-want +got):\n%s", diff)
			}
		})
	}
}

func TestConfigFromHome(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "kazari")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("url: u\ntoken: home-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &runner{lookupEnv: envMap(map[string]string{"HOME": home}), g: &CLI{}}
	s, err := r.settings()
	if err != nil || s.Token != "home-token" {
		t.Errorf("settings = %+v, %v", s, err)
	}
}

func TestRunTokenPrecedence(t *testing.T) {
	f, srv := newFakeAPI(t, map[string]http.HandlerFunc{"GET /v1/me": replyWith(200, api.Me{Handle: "peter"})})
	env := map[string]string{"KAZARI_URL": srv.URL, "KAZARI_TOKEN": "env-token"}
	if r := runCLI(t, env, "me"); r.code != 0 {
		t.Fatalf("me: %+v", r)
	}
	if r := runCLI(t, env, "--token", "flag-token", "me"); r.code != 0 {
		t.Fatalf("me: %+v", r)
	}
	if diff := cmp.Diff([]string{"Bearer env-token", "Bearer flag-token"}, f.auth); diff != "" {
		t.Errorf("Authorization (-want +got):\n%s", diff)
	}
}

func TestRunUsageErrors(t *testing.T) {
	dir := writeTree(t, map[string]string{"index.html": "x"})
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStderr string
	}{
		{"no command", nil, 2, "Usage: kazari <command>"},
		{"unknown command", []string{"frobnicate"}, 2, "kazari: unexpected argument frobnicate"},
		{"unknown flag", []string{"get", "--bogus", "demo"}, 2, "kazari: unknown flag --bogus"},
		{"missing positional", []string{"get"}, 2, `kazari: expected "<slug>"`},
		{"too many positionals", []string{"rm", "a", "b"}, 2, "kazari: unexpected argument b"},
		{"publish without slug", []string{"publish", dir}, 2, "kazari: publish: --slug is required"},
		{"publish bad visibility", []string{"publish", dir, "--slug", "x", "--visibility", "secret"}, 2, "--visibility must be public, unlisted or members"},
		{"bad meta", []string{"publish", dir, "--slug", "x", "--meta", "novalue"}, 2, `expected "<key>=<value>"`},
		{"visibility value", []string{"visibility", "demo", "hidden"}, 2, "want public, unlisted, members or unpublished"},
		{"rollback version", []string{"rollback", "demo", "two"}, 2, "version must be a positive integer"},
		{"stale grace", []string{"stale", "demo", "--grace", "2w"}, 2, "stale: --grace"},
		{"update nothing", []string{"update", "demo"}, 2, "update: nothing to update"},
		{"files without cat", []string{"files", "ls"}, 2, "Usage: kazari files <command>"},
		{"admin unknown", []string{"admin", "nuke"}, 2, "kazari: unexpected argument nuke"},
		{"admin users missing action", []string{"admin", "users"}, 2, `expected one of "list", "create", "update"`},
		{"admin enable and disable", []string{"admin", "users", "update", "ann", "--enable", "--disable"}, 2, "are exclusive"},
		{"admin update nothing", []string{"admin", "users", "update", "ann"}, 2, "nothing to change"},
		{"admin bad quota", []string{"admin", "users", "create", "ann", "--quota", "lots"}, 2, `invalid size "lots"`},
		{"admin audit since", []string{"admin", "audit", "--since", "yesterday"}, 2, "--since"},
		{"missing url", []string{"me"}, 2, "kazari: no API URL: set --url, KAZARI_URL or url in"},
		{"missing token", []string{"--url", "http://x", "me"}, 2, "kazari: no API token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := runCLI(t, nil, tt.args...)
			if r.code != tt.wantCode {
				t.Errorf("exit = %d, want %d (stderr %q)", r.code, tt.wantCode, r.stderr)
			}
			if !strings.Contains(r.stderr, tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", r.stderr, tt.wantStderr)
			}
			if r.stdout != "" {
				t.Errorf("stdout = %q, want empty", r.stdout)
			}
		})
	}
}

func TestRunHelp(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantStdout string
	}{
		{"help", []string{"help"}, "Commands:"},
		{"global -h", []string{"-h"}, "--admin-socket=PATH"},
		{"command --help", []string{"publish", "--help"}, "--slug=SLUG"},
		{"help admin", []string{"help", "admin"}, "admin keys"},
		{"help admin keys", []string{"help", "admin", "keys"}, "admin keys issue"},
		{"subcommand -h", []string{"admin", "keys", "issue", "-h"}, "<handle>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := runCLI(t, nil, tt.args...)
			if r.code != 0 || !strings.Contains(r.stdout, tt.wantStdout) || r.stderr != "" {
				t.Errorf("%v: exit %d, stdout %q, stderr %q; want 0, stdout containing %q", tt.args, r.code, r.stdout, r.stderr, tt.wantStdout)
			}
		})
	}
}

func TestRunHTTPError(t *testing.T) {
	_, srv := newFakeAPI(t, map[string]http.HandlerFunc{
		"GET /v1/artifacts/demo": replyWith(404, api.ErrorResponse{Error: api.Error{Code: "not_found", Message: "no artifact demo"}}),
		"DELETE /v1/artifacts/demo": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			io.WriteString(w, "upstream down\n")
		},
	})
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"get", "demo"}, "kazari: not_found: no artifact demo\n"},
		{[]string{"--json", "get", "demo"}, "kazari: not_found: no artifact demo\n"},
		{[]string{"rm", "demo"}, "kazari: http_502: upstream down\n"},
	}
	for _, tt := range tests {
		r := runCLI(t, nil, append([]string{"--url", srv.URL, "--token", "t"}, tt.args...)...)
		if r.code != 1 || r.stderr != tt.want || r.stdout != "" {
			t.Errorf("%v: %+v, want exit 1 and stderr %q", tt.args, r, tt.want)
		}
	}
}

func TestRunGet(t *testing.T) {
	staleAt := t0.Add(48 * time.Hour)
	art := demo
	art.StaleAt = &staleAt
	detail := api.ArtifactDetail{Artifact: art, Versions: []api.Version{
		{Number: 3, Label: "draft", CreatedBy: "peter", CreatedAt: t0.Add(2 * time.Hour), SizeBytes: 2048, URL: "https://demo--v3.art.test",
			Files: []api.File{{Path: "index.html", Hash: "sha256:0123456789abcdef0123", Size: 2048, ContentType: "text/html"}}},
		{Number: 2, CreatedBy: "ann", CreatedAt: t0, SizeBytes: 100, URL: "https://demo--v2.art.test"},
	}}
	var gotQuery string
	_, srv := newFakeAPI(t, map[string]http.HandlerFunc{
		"GET /v1/artifacts/demo": func(w http.ResponseWriter, r *http.Request) {
			gotQuery = r.URL.RawQuery
			reply(w, 200, detail)
		},
	})
	base := []string{"--url", srv.URL, "--token", "t"}

	r := runCLI(t, nil, append(base, "get", "demo", "--files")...)
	if r.code != 0 {
		t.Fatalf("get: %+v", r)
	}
	if gotQuery != "files=true" {
		t.Errorf("query = %q", gotQuery)
	}
	want := `slug:        demo
name:        Demo page
owner:       peter
url:         https://demo.art.test
visibility:  public
state:       stale
version:     2 published, 3 latest
size:        1.5 KiB
metadata:    team=web
created:     2026-10-01T14:00:00+02:00
updated:     2026-10-01T15:00:00+02:00
stale at:    2026-10-03T14:00:00+02:00

VERSION  LABEL  CREATED BY  CREATED                    SIZE     URL
3        draft  peter       2026-10-01T16:00:00+02:00  2.0 KiB  https://demo--v3.art.test
2 *      -      ann         2026-10-01T14:00:00+02:00  100 B    https://demo--v2.art.test

version 3 files:
PATH        SIZE     TYPE       HASH
index.html  2.0 KiB  text/html  sha256:0123456789ab

version 2 files:
PATH  SIZE  TYPE  HASH
`
	if diff := cmp.Diff(want, r.stdout); diff != "" {
		t.Errorf("human output (-want +got):\n%s", diff)
	}

	for _, args := range [][]string{{"--json", "get", "demo"}, {"get", "demo", "--json"}} {
		r = runCLI(t, nil, append(base, args...)...)
		var got api.ArtifactDetail
		if err := json.Unmarshal([]byte(r.stdout), &got); err != nil || r.code != 0 {
			t.Fatalf("%v: %+v: %v", args, r, err)
		}
		if diff := cmp.Diff(detail, got); diff != "" {
			t.Errorf("%v json (-want +got):\n%s", args, diff)
		}
		if !strings.HasPrefix(r.stdout, "{\n  \"artifact\"") {
			t.Errorf("json is not pretty-printed: %q", r.stdout[:20])
		}
	}
}

func TestRunListAll(t *testing.T) {
	a2 := demo
	a2.Slug, a2.Name, a2.PublishedVersion, a2.LatestVersion, a2.Visibility = "other", "", 1, 1, "unlisted"
	var queries []string
	_, srv := newFakeAPI(t, map[string]http.HandlerFunc{
		"GET /v1/artifacts": func(w http.ResponseWriter, r *http.Request) {
			queries = append(queries, r.URL.RawQuery)
			if r.URL.Query().Get("cursor") == "" {
				reply(w, 200, api.ArtifactList{Items: []api.Artifact{demo}, NextCursor: "next"})
				return
			}
			reply(w, 200, api.ArtifactList{Items: []api.Artifact{a2}})
		},
	})
	r := runCLI(t, nil, "--url", srv.URL, "--token", "t", "list", "--all", "--mine", "any", "--meta", "team=web", "--stale", "exclude", "--limit", "1")
	if r.code != 0 {
		t.Fatalf("list: %+v", r)
	}
	wantQ := []string{
		"limit=1&metadata.team=web&mine=any&stale=exclude",
		"cursor=next&limit=1&metadata.team=web&mine=any&stale=exclude",
	}
	if diff := cmp.Diff(wantQ, queries); diff != "" {
		t.Errorf("queries (-want +got):\n%s", diff)
	}
	want := `SLUG   VERSION  VISIBILITY  STATE  SIZE     UPDATED                    NAME
demo   2/3      public      live   1.5 KiB  2026-10-01T15:00:00+02:00  Demo page
other  1        unlisted    live   1.5 KiB  2026-10-01T15:00:00+02:00  
`
	if diff := cmp.Diff(want, r.stdout); diff != "" {
		t.Errorf("output (-want +got):\n%s", diff)
	}
	if r.stderr != "" {
		t.Errorf("stderr = %q", r.stderr)
	}

	r = runCLI(t, nil, "--url", srv.URL, "--token", "t", "list")
	if !strings.Contains(r.stderr, "more results: --cursor next") {
		t.Errorf("stderr = %q", r.stderr)
	}
}

func TestRunPublish(t *testing.T) {
	dir := writeTree(t, map[string]string{"index.html": "<h1>hi</h1>", ".DS_Store": "x"})
	res := api.PublishResult{URL: "https://demo.art.test", Version: 1, Artifact: demo}
	var got bundleRequest
	_, srv := newFakeAPI(t, map[string]http.HandlerFunc{
		"POST /v1/artifacts": func(w http.ResponseWriter, r *http.Request) {
			got = decodeBundleRequest(t, r)
			reply(w, 201, res)
		},
	})
	r := runCLI(t, nil, "--url", srv.URL, "--token", "t", "publish", dir, "--slug", "demo", "--name", "Demo",
		"--label", "v1", "--visibility", "unlisted", "--meta", "team=web", "--meta", "env=prod")
	if r.code != 0 || r.stdout != "https://demo.art.test\n" {
		t.Fatalf("publish: %+v", r)
	}
	wantManifest := map[string]any{"slug": "demo", "name": "Demo", "label": "v1", "visibility": "unlisted",
		"metadata": map[string]any{"team": "web", "env": "prod"}}
	if diff := cmp.Diff(wantManifest, got.manifest); diff != "" {
		t.Errorf("manifest (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(map[string]string{"index.html": "<h1>hi</h1>"}, got.files); diff != "" {
		t.Errorf("bundle (-want +got):\n%s", diff)
	}

	r = runCLI(t, nil, "--url", srv.URL, "--token", "t", "--json", "publish", "--slug", "demo", dir)
	var gotRes api.PublishResult
	if err := json.Unmarshal([]byte(r.stdout), &gotRes); err != nil || r.code != 0 {
		t.Fatalf("publish --json: %+v: %v", r, err)
	}
	if diff := cmp.Diff(res, gotRes); diff != "" {
		t.Errorf("json (-want +got):\n%s", diff)
	}
}

func TestRunPublishEmptyDir(t *testing.T) {
	r := runCLI(t, nil, "--url", "http://unused", "--token", "t", "publish", t.TempDir(), "--slug", "demo")
	if r.code != 1 || !strings.Contains(r.stderr, "no files to publish") {
		t.Errorf("%+v", r)
	}
}

func TestRunPublishUpdate(t *testing.T) {
	dir := writeTree(t, map[string]string{"index.html": "new", "app.js": "js"})
	existing := api.ArtifactDetail{Artifact: demo, Versions: []api.Version{
		{Number: 3, Files: []api.File{{Path: "app.js"}, {Path: "index.html"}, {Path: "old.html"}}},
		{Number: 2, Files: []api.File{{Path: "ancient.html"}}},
	}}
	tests := []struct {
		name       string
		getStatus  int
		postStatus int
		wantSeen   []string
	}{
		{"exists", 200, 0, []string{"GET /v1/artifacts/demo", "PUT /v1/artifacts/demo"}},
		{"new", 404, 201, []string{"GET /v1/artifacts/demo", "POST /v1/artifacts"}},
		{"created concurrently", 404, 409, []string{"GET /v1/artifacts/demo", "POST /v1/artifacts", "GET /v1/artifacts/demo", "PUT /v1/artifacts/demo"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gets := 0
			var put bundleRequest
			f, srv := newFakeAPI(t, map[string]http.HandlerFunc{
				"GET /v1/artifacts/demo": func(w http.ResponseWriter, r *http.Request) {
					gets++
					if r.URL.Query().Get("files") != "true" {
						t.Errorf("get without files")
					}
					if gets == 1 && tt.getStatus == 404 {
						reply(w, 404, api.ErrorResponse{Error: api.Error{Code: "not_found", Message: "nope"}})
						return
					}
					reply(w, 200, existing)
				},
				"POST /v1/artifacts": func(w http.ResponseWriter, r *http.Request) {
					io.Copy(io.Discard, r.Body)
					if tt.postStatus == 409 {
						reply(w, 409, api.ErrorResponse{Error: api.Error{Code: "slug_taken", Message: "taken"}})
						return
					}
					reply(w, 201, api.PublishResult{URL: "https://demo.art.test", Version: 1})
				},
				"PUT /v1/artifacts/demo": func(w http.ResponseWriter, r *http.Request) {
					put = decodeBundleRequest(t, r)
					reply(w, 200, api.PublishResult{URL: "https://demo.art.test", Version: 4})
				},
			})
			r := runCLI(t, nil, "--url", srv.URL, "--token", "t", "publish", dir, "--slug", "demo", "--update", "--label", "l")
			if r.code != 0 || r.stdout != "https://demo.art.test\n" {
				t.Fatalf("%+v", r)
			}
			if diff := cmp.Diff(tt.wantSeen, f.seen); diff != "" {
				t.Errorf("requests (-want +got):\n%s", diff)
			}
			if put.method == "" {
				return
			}
			wantManifest := map[string]any{"label": "l", "expected_version": 3.0,
				"files": []any{map[string]any{"path": "old.html", "delete": true}}}
			if diff := cmp.Diff(wantManifest, put.manifest); diff != "" {
				t.Errorf("manifest (-want +got):\n%s", diff)
			}
			if put.header.Get("If-Match") != `"3"` {
				t.Errorf("If-Match = %q", put.header.Get("If-Match"))
			}
		})
	}
}

func TestRunUpdate(t *testing.T) {
	dir := writeTree(t, map[string]string{"index.html": "v2"})
	var put bundleRequest
	var jsonBody map[string]any
	_, srv := newFakeAPI(t, map[string]http.HandlerFunc{
		"PUT /v1/artifacts/demo": func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
				json.NewDecoder(r.Body).Decode(&jsonBody)
			} else {
				put = decodeBundleRequest(t, r)
			}
			reply(w, 200, api.PublishResult{URL: "https://demo.art.test", Version: 3})
		},
	})
	base := []string{"--url", srv.URL, "--token", "t"}
	r := runCLI(t, nil, append(base, "update", "demo", dir, "--expected", "2", "--keep-stale", "--name", "",
		"--meta", "k=v", "--delete", "a.html", "--delete", "b/c.html")...)
	if r.code != 0 || r.stdout != "https://demo.art.test\n" {
		t.Fatalf("%+v", r)
	}
	wantManifest := map[string]any{"name": "", "expected_version": 2.0, "keep_stale": true,
		"metadata": map[string]any{"k": "v"},
		"files":    []any{map[string]any{"path": "a.html", "delete": true}, map[string]any{"path": "b/c.html", "delete": true}}}
	if diff := cmp.Diff(wantManifest, put.manifest); diff != "" {
		t.Errorf("manifest (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(map[string]string{"index.html": "v2"}, put.files); diff != "" {
		t.Errorf("bundle (-want +got):\n%s", diff)
	}

	r = runCLI(t, nil, append(base, "update", "demo", "--delete", "a.html")...)
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if diff := cmp.Diff(map[string]any{"files": []any{map[string]any{"path": "a.html", "delete": true}}}, jsonBody); diff != "" {
		t.Errorf("json body (-want +got):\n%s", diff)
	}
}

func TestRunSimpleCommands(t *testing.T) {
	staleAt := t0
	expires := t0.Add(336 * time.Hour)
	lastUsed := t0.Add(time.Minute)
	var body string
	record := func(status int, v any) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			body = strings.TrimSpace(string(b))
			if q := r.URL.RawQuery; q != "" {
				if body != "" {
					body += " "
				}
				body += "?" + q
			}
			reply(w, status, v)
		}
	}
	_, srv := newFakeAPI(t, map[string]http.HandlerFunc{
		"DELETE /v1/artifacts/demo":       record(200, api.OK{OK: true}),
		"POST /v1/artifacts/demo/stale":   record(200, api.StaleResult{StaleAt: staleAt, ExpiresAt: expires, Artifact: demo}),
		"DELETE /v1/artifacts/demo/stale": record(200, demo),
		"PATCH /v1/artifacts/demo":        record(200, api.PublishResult{URL: demo.URL, Version: 2, Artifact: demo}),
		"POST /v1/artifacts/demo/publish": record(200, api.PublishResult{URL: demo.URL, Version: 1, Artifact: demo}),
		"GET /v1/me":                      record(200, api.Me{Handle: "peter", Scopes: []string{"read", "write"}, Source: "api_key", QuotaBytes: 100 << 20, UsedBytes: 1536}),
		"GET /v1/me/keys":                 record(200, []api.APIKey{{Prefix: "abc123", Label: "laptop", Scopes: []string{"read", "write"}, CreatedAt: t0, LastUsedAt: &lastUsed}}),
		"GET /v1/artifacts/demo/files/css/site.css": func(w http.ResponseWriter, r *http.Request) {
			body = r.URL.RawQuery
			w.Header().Set("Content-Type", "text/css")
			io.WriteString(w, "body{color:red}")
		},
	})
	tests := []struct {
		args     []string
		wantOut  string
		wantBody string
	}{
		{[]string{"rm", "demo"}, "deleted demo\n", ""},
		{[]string{"rm", "demo", "--hard"}, "purged demo\n", "?hard=true"},
		{[]string{"stale", "demo", "--grace", "336h"}, "demo is stale since 2026-10-01T14:00:00+02:00 and expires at 2026-10-15T14:00:00+02:00\n", `{"grace":"336h"}`},
		{[]string{"unstale", "demo"}, "demo is no longer stale\n", ""},
		{[]string{"visibility", "demo", "unpublished"}, "demo is now unpublished\n", `{"visibility":"unpublished"}`},
		{[]string{"rollback", "demo", "1"}, "demo now serves version 1: https://demo.art.test\n", `{"version":1}`},
		{[]string{"files", "cat", "demo", "css/site.css", "--version", "2"}, "body{color:red}", "version=2"},
		{[]string{"me"}, "handle:   peter\nadmin:    false\nscopes:   read, write\nsource:   api_key\nstorage:  1.5 KiB of 100.0 MiB used\n", ""},
		{[]string{"keys"}, "PREFIX  LABEL   SCOPES      CREATED                    LAST USED                  EXPIRES  REVOKED\n" +
			"abc123  laptop  read,write  2026-10-01T14:00:00+02:00  2026-10-01T14:01:00+02:00  -        -\n", ""},
		{[]string{"--json", "rm", "demo"}, "{\n  \"ok\": true\n}\n", ""},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			body = ""
			r := runCLI(t, nil, append([]string{"--url", srv.URL, "--token", "t"}, tt.args...)...)
			if r.code != 0 || r.stderr != "" {
				t.Fatalf("%+v", r)
			}
			if diff := cmp.Diff(tt.wantOut, r.stdout); diff != "" {
				t.Errorf("stdout (-want +got):\n%s", diff)
			}
			if body != tt.wantBody {
				t.Errorf("request body = %q, want %q", body, tt.wantBody)
			}
		})
	}
}

func TestRunVersion(t *testing.T) {
	r := runCLI(t, nil, "version")
	if r.code != 0 || !strings.HasPrefix(r.stdout, "kazari ") {
		t.Errorf("%+v", r)
	}
	r = runCLI(t, nil, "--json", "version")
	var info map[string]string
	if err := json.Unmarshal([]byte(r.stdout), &info); err != nil || info["version"] == "" {
		t.Errorf("%+v: %v", r, err)
	}
}

func TestRunAdmin(t *testing.T) {
	disabled := t0
	now := time.Now()
	var body, query string
	record := func(v any) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			body, query = strings.TrimSpace(string(b)), r.URL.RawQuery
			reply(w, 200, v)
		}
	}
	f, srv := newFakeAPI(t, map[string]http.HandlerFunc{
		"GET /v1/users":                    record([]api.User{{Handle: "ann", Email: "ann@x.test", Role: "admin", QuotaBytes: 1 << 30, UsedBytes: 2048, DisabledAt: &disabled, CreatedAt: t0}}),
		"POST /v1/users":                   record(api.User{Handle: "bob", Role: "member", QuotaBytes: 500 << 20, CreatedAt: t0}),
		"PATCH /v1/users/bob":              record(api.User{Handle: "bob", Role: "admin", CreatedAt: t0}),
		"POST /v1/users/bob/keys":          record(api.IssuedKey{Key: "kzk_pfx_SECRET", APIKey: api.APIKey{Prefix: "pfx", CreatedAt: t0}}),
		"DELETE /v1/users/bob/keys/pfx":    record(api.OK{OK: true}),
		"POST /v1/artifacts/demo/transfer": record(demo),
		"POST /v1/artifacts/demo/hide":     record(demo),
		"GET /v1/settings":                 record(map[string]any{"server": map[string]any{"base_domain": "art.test", "listen": []string{":443"}}, "auth": map[string]any{"admin_token": "[redacted]"}, "debug": false}),
		"GET /v1/jobs":                     record([]api.JobStatus{{Name: "gc", Every: "1h0m0s", LastRun: &disabled, LastError: "disk full"}}),
		"POST /v1/jobs/gc/run":             record(api.OK{OK: true}),
		"GET /v1/audit":                    record([]api.AuditEvent{{At: t0, Actor: "ann", Action: "artifact.create", Slug: "demo", Details: map[string]string{"version": "1", "a": "b"}}}),
		"GET /v1/health":                   record(api.Health{Status: "ok", Version: "1.2.3"}),
		"GET /v1/artifacts":                record(api.ArtifactList{Items: []api.Artifact{demo}}),
	})
	tests := []struct {
		args       []string
		wantOut    string
		wantStderr string
		wantBody   string
		wantQuery  string
	}{
		{args: []string{"users", "list"}, wantOut: "HANDLE  EMAIL       ROLE   USED     QUOTA    DISABLED                   CREATED\n" +
			"ann     ann@x.test  admin  2.0 KiB  1.0 GiB  2026-10-01T14:00:00+02:00  2026-10-01T14:00:00+02:00\n"},
		{args: []string{"users", "create", "bob", "--quota", "500MiB", "--role", "member"},
			wantOut:  "handle:   bob\nemail:    -\nrole:     member\nstorage:  0 B of 500.0 MiB used\ncreated:  2026-10-01T14:00:00+02:00\n",
			wantBody: `{"handle":"bob","role":"member","quota_bytes":524288000}`},
		{args: []string{"users", "update", "bob", "--enable", "--role", "admin", "--quota", "0"},
			wantOut:  "handle:   bob\nemail:    -\nrole:     admin\nstorage:  0 B used (no quota)\ncreated:  2026-10-01T14:00:00+02:00\n",
			wantBody: `{"role":"admin","quota_bytes":0,"disabled":false}`},
		{args: []string{"keys", "issue", "bob", "--label", "ci", "--scopes", "read, write", "--expires", "720h"},
			wantOut: "kzk_pfx_SECRET\n", wantStderr: "issued key pfx for bob; the key is shown only once\n",
			wantBody: `{"label":"ci","scopes":["read","write"],"expires_in":"720h"}`},
		{args: []string{"keys", "revoke", "bob", "pfx"}, wantOut: "revoked pfx\n"},
		{args: []string{"transfer", "demo", "bob"}, wantOut: "demo now belongs to bob\n", wantBody: `{"owner":"bob"}`},
		{args: []string{"hide", "demo"}, wantOut: "demo is hidden (unpublished)\n"},
		{args: []string{"settings"}, wantOut: "auth.admin_token   = \"[redacted]\"\ndebug              = false\n" +
			"server.base_domain = \"art.test\"\nserver.listen      = [\":443\"]\n"},
		{args: []string{"jobs", "list"}, wantOut: "NAME  EVERY   RUNNING  LAST RUN                   NEXT RUN  LAST ERROR\n" +
			"gc    1h0m0s  false    2026-10-01T14:00:00+02:00  -         disk full\n"},
		{args: []string{"jobs", "run", "gc"}, wantOut: "triggered gc\n"},
		{args: []string{"audit", "--since", "2026-10-01T00:00:00+02:00", "--slug", "demo", "--limit", "5"},
			wantOut: "AT                         ACTOR  ACTION           SLUG  DETAILS\n" +
				"2026-10-01T14:00:00+02:00  ann    artifact.create  demo  a=b, version=1\n",
			wantQuery: "limit=5&since=2026-10-01T00%3A00%3A00%2B02%3A00&slug=demo"},
		{args: []string{"health"}, wantOut: "ok (server 1.2.3)\n"},
		{args: []string{"artifacts", "list", "--deleted", "only"}, wantQuery: "deleted=only",
			wantOut: "OWNER  SLUG  VERSION  VISIBILITY  STATE  SIZE     UPDATED                    NAME\n" +
				"peter  demo  2/3      public      live   1.5 KiB  2026-10-01T15:00:00+02:00  Demo page\n"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			body, query = "", ""
			r := runCLI(t, nil, append([]string{"--admin-url", srv.URL, "--admin-token", "adm", "admin"}, tt.args...)...)
			if r.code != 0 {
				t.Fatalf("%+v", r)
			}
			if diff := cmp.Diff(tt.wantOut, r.stdout); diff != "" {
				t.Errorf("stdout (-want +got):\n%s", diff)
			}
			if r.stderr != tt.wantStderr {
				t.Errorf("stderr = %q, want %q", r.stderr, tt.wantStderr)
			}
			if body != tt.wantBody {
				t.Errorf("body = %s, want %s", body, tt.wantBody)
			}
			if query != tt.wantQuery {
				t.Errorf("query = %s, want %s", query, tt.wantQuery)
			}
		})
	}
	for _, a := range f.auth {
		if a != "Bearer adm" {
			t.Errorf("Authorization = %q", a)
		}
	}

	r := runCLI(t, nil, "--admin-url", srv.URL, "--admin-token", "adm", "admin", "audit", "--since", "24h")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	q, _ := url.ParseQuery(query)
	since, err := time.Parse(time.RFC3339, q.Get("since"))
	if err != nil || since.Sub(now.Add(-24*time.Hour)).Abs() > time.Minute {
		t.Errorf("since query = %q (%v)", query, err)
	}

	// The issued key is printed once and the admin token never.
	r = runCLI(t, nil, "--admin-url", srv.URL, "--admin-token", "adm", "--json", "admin", "keys", "issue", "bob")
	if strings.Count(r.stdout, "kzk_pfx_SECRET") != 1 || strings.Contains(r.stdout+r.stderr, "adm\"") {
		t.Errorf("%+v", r)
	}
}

// Other members write these fields, so escapes must not reach the terminal
// nor line breaks fake rows.
func TestRunGetTerminalSafe(t *testing.T) {
	art := demo
	art.Name = "Demo\x1b]52;c;cm0gLXJmIH4=\x07 page"
	art.Description = "line one\nslug:        forged\t\u202egnp.exe"
	art.Metadata = map[string]string{"team": "web\x1b[2J"}
	detail := api.ArtifactDetail{Artifact: art, Versions: []api.Version{
		{Number: 2, Label: "v\x9b31m", CreatedBy: "ann", CreatedAt: t0, SizeBytes: 100, URL: "https://demo--v2.art.test"},
	}}
	_, srv := newFakeAPI(t, map[string]http.HandlerFunc{"GET /v1/artifacts/demo": replyWith(200, detail)})
	r := runCLI(t, nil, "--url", srv.URL, "--token", "t", "get", "demo")
	if r.code != 0 {
		t.Fatalf("get: %+v", r)
	}
	if strings.ContainsAny(r.stdout, "\x1b\x07\u009b\u202e") {
		t.Errorf("control characters reached the output: %q", r.stdout)
	}
	for _, want := range []string{
		"  Demo\uFFFD]52;c;cm0gLXJmIH4=\uFFFD page\n",
		"  line one slug:        forged \uFFFDgnp.exe\n",
		"  team=web\uFFFD[2J\n",
		"2 *      v\uFFFD31m",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, r.stdout)
		}
	}
}

func TestRunEdit(t *testing.T) {
	var got api.EditRequest
	_, srv := newFakeAPI(t, map[string]http.HandlerFunc{
		"POST /v1/artifacts/demo/edit": func(w http.ResponseWriter, r *http.Request) {
			got = api.EditRequest{}
			json.NewDecoder(r.Body).Decode(&got)
			reply(w, 200, api.EditResult{URL: "https://demo.art.test", Version: 4, Path: "index.html", Edits: []api.EditOutcome{
				{Match: "indentation", Replacements: 1, StartLine: 3, EndLine: 4},
				{Match: "exact", Replacements: 2, StartLine: 9, EndLine: 8},
			}})
		},
		"GET /v1/skill": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
			io.WriteString(w, "---\nname: kazari\n---\n")
		},
		"GET /v1/artifacts/demo/files/index.html": func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, r.URL.RawQuery)
		},
	})
	base := []string{"--url", srv.URL, "--token", "t"}

	r := runCLI(t, nil, append(base, "edit", "demo", "index.html", "--old", "Q3", "--new", "Q4", "--all", "--expected", "3", "--label", "q4")...)
	want := "https://demo.art.test\nedit 1: lines 3-4 (indentation)\nedit 2: removed at line 9 (exact, 2 places)\n"
	if r.code != 0 || r.stdout != want {
		t.Fatalf("edit = %+v", r)
	}
	if diff := cmp.Diff(api.EditRequest{Path: "index.html", Label: "q4", ExpectedVersion: 3,
		Edits: []api.Edit{{OldText: "Q3", NewText: "Q4", ReplaceAll: true}}}, got); diff != "" {
		t.Errorf("request (-want +got):\n%s", diff)
	}

	r = runCLI(t, nil, append(base, "edit", "demo", "index.html", "--start", "3:abcd", "--end", "4:0123", "--new", "")...)
	if r.code != 0 || len(got.Edits) != 1 || got.Edits[0] != (api.Edit{Start: "3:abcd", End: "4:0123"}) {
		t.Errorf("anchor edit = %+v, request %+v", r, got)
	}

	file := filepath.Join(t.TempDir(), "edits.json")
	if err := os.WriteFile(file, []byte(`[{"old_text":"a","new_text":"b"},{"old_text":"c","new_text":""}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	r = runCLI(t, nil, append(base, "edit", "demo", "index.html", "--edits", file)...)
	if r.code != 0 || len(got.Edits) != 2 || got.Edits[1].OldText != "c" {
		t.Errorf("edits file = %+v, request %+v", r, got)
	}

	for _, args := range [][]string{
		{"edit", "demo", "index.html", "--new", "x"},
		{"edit", "demo", "index.html", "--old", "x"},
		{"edit", "demo", "index.html", "--edits", file, "--old", "x"},
		{"edit", "demo"},
	} {
		if r := runCLI(t, nil, append(base, args...)...); r.code != 2 {
			t.Errorf("%v: code %d, want 2 (%s)", args, r.code, r.stderr)
		}
	}

	if r := runCLI(t, nil, append(base, "skill")...); r.code != 0 || r.stdout != "---\nname: kazari\n---\n" {
		t.Errorf("skill = %+v", r)
	}
	if r := runCLI(t, nil, append(base, "files", "cat", "demo", "index.html", "--anchors", "--version", "2")...); r.code != 0 ||
		r.stdout != "anchors=true&version=2" {
		t.Errorf("files cat --anchors = %+v", r)
	}
}
