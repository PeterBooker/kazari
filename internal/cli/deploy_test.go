package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/api"
)

func TestRunDeploy(t *testing.T) {
	dir := writeTree(t, map[string]string{"hello/index.html": "new", "hello/app.js": "js"})
	site := filepath.Join(dir, "hello")
	rolledBack := demo
	rolledBack.Slug, rolledBack.PublishedVersion, rolledBack.LatestVersion = "hello", 1, 3
	detail := api.ArtifactDetail{Artifact: rolledBack, Versions: []api.Version{
		{Number: 3, Files: []api.File{{Path: "app.js"}, {Path: "index.html"}, {Path: "bad.html"}}},
		{Number: 2},
		{Number: 1},
	}}
	older := api.ArtifactDetail{Artifact: rolledBack, Versions: []api.Version{
		{Number: 3}, {Number: 2}, {Number: 1, Files: []api.File{{Path: "index.html"}, {Path: "old.css"}}},
	}}
	var put bundleRequest
	var query []string
	f, srv := newFakeAPI(t, map[string]http.HandlerFunc{
		"GET /v1/artifacts/hello": func(w http.ResponseWriter, r *http.Request) {
			query = append(query, r.URL.RawQuery)
			if r.URL.Query().Get("files_before") == "2" {
				reply(w, 200, older)
				return
			}
			reply(w, 200, detail)
		},
		"PUT /v1/artifacts/hello": func(w http.ResponseWriter, r *http.Request) {
			put = decodeBundleRequest(t, r)
			if put.manifest["base_version"] == nil {
				reply(w, 409, api.ErrorResponse{Error: api.Error{Code: api.CodePublishedNotLatest,
					Message: "version 1 of hello is published but the latest is 3"}})
				return
			}
			reply(w, 200, api.PublishResult{URL: "https://hello.art.test/", Version: 4, VersionURL: "https://hello--v4.art.test/",
				Warnings: []string{"index.html loads app.js, which is not in this version"}})
		},
	})
	base := []string{"--url", srv.URL, "--token", "secret-key"}

	r := runCLI(t, nil, append(base, "deploy", site)...)
	wantErr := "kazari: published_not_latest: version 1 of hello is published but the latest is 3\n" +
		"to change the published version 1: kazari --url " + srv.URL + " deploy " + site + " --base 1\n" +
		"to build on the latest version 3 and publish it: kazari --url " + srv.URL + " deploy " + site + " --base 3\n" +
		"to add to the latest version 3 without publishing: kazari --url " + srv.URL + " deploy " + site + " --stage\n"
	if r.code != exitConflict || r.stderr != wantErr || r.stdout != "" {
		t.Errorf("deploy after a rollback: %+v\nwant exit %d and stderr\n%s", r, exitConflict, wantErr)
	}

	r = runCLI(t, nil, append(base, "deploy", site, "--base", "1")...)
	if want := "hello v4 → https://hello.art.test/ (pinned: https://hello--v4.art.test/)\n"; r.code != 0 || r.stdout != want {
		t.Fatalf("deploy --base 1: %+v, want stdout %q", r, want)
	}
	if want := "2 files, 5 B; removed: old.css\nwarning: index.html loads app.js, which is not in this version\n"; r.stderr != want {
		t.Errorf("deploy --base 1 stderr = %q, want %q", r.stderr, want)
	}
	wantManifest := map[string]any{"expected_version": 3.0, "base_version": 1.0,
		"files": []any{map[string]any{"path": "old.css", "delete": true}}}
	if diff := cmp.Diff(wantManifest, put.manifest); diff != "" {
		t.Errorf("deploy --base 1 manifest (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(map[string]string{"app.js": "js", "index.html": "new"}, put.files); diff != "" {
		t.Errorf("deploy --base 1 bundle (-want +got):\n%s", diff)
	}
	if query[len(query)-1] != "files=true&files_before=2" {
		t.Errorf("deploy --base 1 read %q, want the files of version 1", query[len(query)-1])
	}

	seen := len(f.seen)
	r = runCLI(t, nil, append(base, "deploy", site, "--base", "3", "--dry-run")...)
	wantOut := "send    app.js      2 B\nsend    index.html  3 B\ndelete  bad.html    \n"
	if r.code != 0 || r.stdout != wantOut || r.stderr != "dry run, nothing sent: 2 files, 5 B; removed: bad.html\n" {
		t.Errorf("deploy --dry-run: %+v, want stdout %q", r, wantOut)
	}
	if diff := cmp.Diff([]string{"GET /v1/artifacts/hello"}, f.seen[seen:]); diff != "" {
		t.Errorf("deploy --dry-run requests (-want +got):\n%s", diff)
	}

	r = runCLI(t, nil, append(base, "--json", "deploy", site, "--base", "3", "--dry-run")...)
	var plan bundlePlan
	if err := json.Unmarshal([]byte(r.stdout), &plan); err != nil || r.code != 0 {
		t.Fatalf("deploy --dry-run --json: %+v: %v", r, err)
	}
	if diff := cmp.Diff([]string{"bad.html"}, plan.Removed); diff != "" || len(plan.Files) != 2 {
		t.Errorf("deploy --dry-run --json = %+v (removed -want +got):\n%s", plan, diff)
	}
}

func TestRunDeployStage(t *testing.T) {
	dir := writeTree(t, map[string]string{"hello/index.html": "new"})
	site := filepath.Join(dir, "hello")
	cur := demo
	cur.Slug, cur.PublishedVersion, cur.LatestVersion = "hello", 2, 2
	staged := cur
	staged.LatestVersion = 3
	var put bundleRequest
	_, srv := newFakeAPI(t, map[string]http.HandlerFunc{
		"GET /v1/artifacts/hello": func(w http.ResponseWriter, _ *http.Request) {
			reply(w, 200, api.ArtifactDetail{Artifact: cur, Versions: []api.Version{
				{Number: 2, Files: []api.File{{Path: "index.html"}}}, {Number: 1}}})
		},
		"PUT /v1/artifacts/hello": func(w http.ResponseWriter, r *http.Request) {
			put = decodeBundleRequest(t, r)
			reply(w, 200, api.PublishResult{URL: "https://hello.art.test/", Version: 3, VersionURL: "https://hello--v3.art.test/",
				Staged: true, Artifact: staged})
		},
		"GET /v1/artifacts/fresh": func(w http.ResponseWriter, _ *http.Request) {
			reply(w, 404, api.ErrorResponse{Error: api.Error{Code: api.CodeNotFound, Message: "artifact not found"}})
		},
	})
	base := []string{"--url", srv.URL, "--token", "secret-key"}

	r := runCLI(t, nil, append(base, "deploy", site, "--stage")...)
	want := "hello v3 staged → https://hello--v3.art.test/ (members only; https://hello.art.test/ serves v2 until kazari rollback hello 3)\n"
	if r.code != 0 || r.stdout != want {
		t.Fatalf("deploy --stage: %+v, want stdout %q", r, want)
	}
	if put.manifest["stage"] != true {
		t.Errorf("deploy --stage sent manifest %v, want stage: true", put.manifest)
	}

	r = runCLI(t, nil, append(base, "deploy", site, "--slug", "fresh", "--stage")...)
	wantErr := "kazari: deploy: --stage needs an existing artifact; fresh does not exist, and a new one is published as version 1\n"
	if r.code != exitUsage || r.stderr != wantErr {
		t.Errorf("deploy --stage of a new slug: %+v, want exit %d and stderr %q", r, exitUsage, wantErr)
	}
}

func TestRunPublishSlugTaken(t *testing.T) {
	dir := writeTree(t, map[string]string{"index.html": "x"})
	_, srv := newFakeAPI(t, map[string]http.HandlerFunc{
		"POST /v1/artifacts": func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			reply(w, 409, api.ErrorResponse{Error: api.Error{Code: api.CodeSlugTaken, Message: `slug "demo" is already taken`}})
		},
	})
	r := runCLI(t, nil, "--url", srv.URL, "--token", "t", "publish", dir, "--slug", "demo")
	want := "kazari: slug_taken: slug \"demo\" is already taken\nto make it match " + dir + ": kazari deploy " + dir + " --slug demo\n"
	if r.code != exitConflict || r.stderr != want {
		t.Errorf("publish to a taken slug: %+v, want stderr %q", r, want)
	}
}

func TestRunPublishDryRun(t *testing.T) {
	dir := writeTree(t, map[string]string{"index.html": "<h1>x</h1>", ".env": "A=1"})
	r := runCLI(t, nil, "publish", dir, "--slug", "demo", "--dry-run")
	if want := "send  index.html  10 B\nskip  .env        dotfile\n"; r.code != 0 || r.stdout != want {
		t.Errorf("publish --dry-run with no API configured: %+v, want stdout %q", r, want)
	}
}

func TestRunPublishFile(t *testing.T) {
	dir := writeTree(t, map[string]string{"Weekly Report.html": "<h1>w</h1>"})
	var got bundleRequest
	_, srv := newFakeAPI(t, map[string]http.HandlerFunc{
		"POST /v1/artifacts": func(w http.ResponseWriter, r *http.Request) {
			got = decodeBundleRequest(t, r)
			reply(w, 201, api.PublishResult{URL: "https://weekly-report.art.test/", Version: 1})
		},
	})
	r := runCLI(t, nil, "--url", srv.URL, "--token", "t", "publish", filepath.Join(dir, "Weekly Report.html"))
	if r.code != 0 || r.stdout != "weekly-report v1 → https://weekly-report.art.test/\n" {
		t.Fatalf("publish of one file: %+v", r)
	}
	if got.manifest["slug"] != "weekly-report" {
		t.Errorf("slug = %v, want weekly-report", got.manifest["slug"])
	}
	if diff := cmp.Diff(map[string]string{"index.html": "<h1>w</h1>"}, got.files); diff != "" {
		t.Errorf("bundle (-want +got):\n%s", diff)
	}
}

func TestRunUpdateUnchanged(t *testing.T) {
	dir := writeTree(t, map[string]string{"index.html": "x"})
	_, srv := newFakeAPI(t, map[string]http.HandlerFunc{
		"PUT /v1/artifacts/demo": func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			reply(w, 200, api.PublishResult{URL: "https://demo.art.test/", Version: 4, Unchanged: true})
		},
	})
	r := runCLI(t, nil, "--url", srv.URL, "--token", "t", "update", "demo", dir)
	if want := "1 file, 1 B\nno changes: version 4 is already this content\n"; r.code != 0 || r.stderr != want {
		t.Errorf("update with the same files: %+v, want stderr %q", r, want)
	}
}

func TestRunRmHardOnATerminal(t *testing.T) {
	_, srv := newFakeAPI(t, map[string]http.HandlerFunc{
		"DELETE /v1/artifacts/demo": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) },
	})
	terminal := func() bool { return true }
	args := []string{"--url", srv.URL, "--token", "t", "rm", "demo", "--hard"}
	r := runCLIWith(t, nil, strings.NewReader("demo\n"), terminal, args...)
	if r.code != 0 || r.stdout != "purged demo\n" || !strings.Contains(r.stderr, "Type the slug to confirm") {
		t.Errorf("rm --hard confirmed: %+v", r)
	}
	r = runCLIWith(t, nil, strings.NewReader("dmeo\n"), terminal, args...)
	if r.code != exitUsage || !strings.Contains(r.stderr, "not purged: the slug did not match") {
		t.Errorf("rm --hard with the wrong slug: %+v", r)
	}
}

func TestRunFilesLs(t *testing.T) {
	detail := api.ArtifactDetail{Artifact: demo, NextFilesBefore: 3, Versions: []api.Version{
		{Number: 3, Files: []api.File{{Path: "new.html", Size: 10, ContentType: "text/html", Hash: "sha256:" + strings.Repeat("a", 64)}}},
		{Number: 2},
	}}
	page2 := api.ArtifactDetail{Artifact: demo, Versions: []api.Version{
		{Number: 3},
		{Number: 2, Files: []api.File{{Path: "index.html", Size: 2048, ContentType: "text/html", Hash: "sha256:" + strings.Repeat("b", 64)}}},
	}}
	var queries []string
	_, srv := newFakeAPI(t, map[string]http.HandlerFunc{
		"GET /v1/artifacts/demo": func(w http.ResponseWriter, r *http.Request) {
			queries = append(queries, r.URL.RawQuery)
			if r.URL.Query().Get("files_before") == "3" {
				reply(w, 200, page2)
				return
			}
			reply(w, 200, detail)
		},
	})
	base := []string{"--url", srv.URL, "--token", "t", "files", "ls", "demo"}
	r := runCLI(t, nil, base...)
	want := "PATH        SIZE     TYPE       HASH\nindex.html  2.0 KiB  text/html  sha256:bbbbbbbbbbbb\n"
	if r.code != 0 || r.stdout != want {
		t.Errorf("files ls (the published version 2): %+v, want %q", r, want)
	}
	if diff := cmp.Diff([]string{"files=true", "files=true&files_before=3"}, queries); diff != "" {
		t.Errorf("files ls queries (-want +got):\n%s", diff)
	}
	r = runCLI(t, nil, append(base, "--version", "3")...)
	if r.code != 0 || !strings.Contains(r.stdout, "new.html") {
		t.Errorf("files ls --version 3: %+v", r)
	}
	r = runCLI(t, nil, append(base, "--version", "9")...)
	if r.code != exitNotFound || !strings.Contains(r.stderr, "version_not_found: demo has no version 9; its latest is 3") {
		t.Errorf("files ls --version 9: %+v", r)
	}
}

func TestRunMeRole(t *testing.T) {
	tests := []struct {
		me   api.Me
		want string
	}{
		{api.Me{Handle: "ann", Role: "admin", Admin: false}, "role:     admin (this key has no admin scope)\n"},
		{api.Me{Handle: "ann", Role: "admin", Admin: true}, "role:     admin\n"},
		{api.Me{Handle: "bob", Role: "member"}, "role:     member\n"},
	}
	for _, tt := range tests {
		_, srv := newFakeAPI(t, map[string]http.HandlerFunc{"GET /v1/me": replyWith(200, tt.me)})
		r := runCLI(t, nil, "--url", srv.URL, "--token", "t", "me")
		if r.code != 0 || !strings.Contains(r.stdout, tt.want) {
			t.Errorf("me as %+v: %+v, want a line %q", tt.me, r, tt.want)
		}
	}
}

func TestRunRollbackNotesBase(t *testing.T) {
	_, srv := newFakeAPI(t, map[string]http.HandlerFunc{
		"POST /v1/artifacts/demo/publish": replyWith(200, api.PublishResult{URL: demo.URL, Version: 1, Artifact: demo}),
	})
	r := runCLI(t, nil, "--url", srv.URL, "--token", "t", "rollback", "demo", "1")
	if want := "the next change needs --base: 1 to build on this version, 3 to build on the latest\n"; r.code != 0 || r.stderr != want {
		t.Errorf("rollback: %+v, want stderr %q", r, want)
	}
}

func TestRunUnknownCommand(t *testing.T) {
	r := runCLI(t, nil, "pubish")
	if r.code != exitUsage || strings.Contains(r.stderr, "Commands:") || strings.Contains(r.stderr, "Usage:") ||
		!strings.Contains(r.stderr, `did you mean "publish"?`) {
		t.Errorf("a mistyped command: %+v, want the mistake and a suggestion only", r)
	}
}

func TestRunURLForms(t *testing.T) {
	f, srv := newFakeAPI(t, map[string]http.HandlerFunc{"GET /v1/me": replyWith(200, api.Me{Handle: "peter"})})
	host := strings.TrimPrefix(srv.URL, "http://")
	for _, u := range []string{srv.URL + "/v1", srv.URL + "/v1/", srv.URL + "/"} {
		if r := runCLI(t, nil, "--url", u, "--token", "t", "me"); r.code != 0 {
			t.Errorf("--url %s: %+v", u, r)
		}
	}
	if diff := cmp.Diff([]string{"GET /v1/me", "GET /v1/me", "GET /v1/me"}, f.seen); diff != "" {
		t.Errorf("requests (-want +got):\n%s", diff)
	}
	if r := runCLI(t, nil, "--url", host, "--token", "t", "me"); r.code != 0 {
		t.Errorf("--url %s, a local host without a scheme: %+v", host, r)
	}
	for in, want := range map[string]string{
		"api.art.test":                    "https://api.art.test",
		"api.art.test/v1/":                "https://api.art.test",
		"api.kazari.localhost:8080/v1":    "http://api.kazari.localhost:8080",
		"HTTPS://api.art.test:8443/v1/v1": "https://api.art.test:8443",
	} {
		if got, err := normalizeURL(in); got != want || err != nil {
			t.Errorf("normalizeURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestRunPlainHTTP(t *testing.T) {
	tests := []struct {
		name string
		url  string
		env  map[string]string
		args []string
		ok   bool
	}{
		{"loopback name", "http://localhost:1", nil, nil, true},
		{"localhost subdomain", "http://api.kazari.localhost:1", nil, nil, true},
		{"loopback address", "http://127.0.0.1:1", nil, nil, true},
		{"ipv6 loopback", "http://[::1]:1", nil, nil, true},
		{"another host", "http://api.example.com:1", nil, nil, false},
		{"another host with --allow-http", "http://api.example.com:1", nil, []string{"--allow-http"}, true},
		{"another host with KAZARI_ALLOW_HTTP", "http://api.example.com:1", map[string]string{"KAZARI_ALLOW_HTTP": "1"}, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &runner{lookupEnv: envMap(tt.env), g: &CLI{}}
			r.g.AllowHTTP = len(tt.args) > 0
			err := r.checkScheme(tt.url, "t")
			if (err == nil) != tt.ok {
				t.Errorf("checkScheme(%q) = %v, want ok %t", tt.url, err, tt.ok)
			}
		})
	}
}
