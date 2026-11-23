package cli_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/cli"
)

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// deployAPI is an artifact whose latest version 2 holds base, and that
// keeps what a deploy sends.
func deployAPI(t *testing.T, base map[string]string, res api.PublishResult) (*fakeAPI, string, *bundleRequest) {
	t.Helper()
	cur := demo
	cur.PublishedVersion, cur.LatestVersion = 2, 2
	v := api.Version{Number: 2}
	for _, p := range []string{"app.js", "index.html", "logo.png", "old.css"} {
		if content, ok := base[p]; ok {
			v.Files = append(v.Files, api.File{Path: p, Size: int64(len(content)), Hash: sha(content)})
		}
	}
	var put bundleRequest
	f, srv := newFakeAPI(t, map[string]http.HandlerFunc{
		"GET /v1/artifacts/demo": replyWith(200, api.ArtifactDetail{Artifact: cur, Versions: []api.Version{v, {Number: 1}}}),
		"PUT /v1/artifacts/demo": func(w http.ResponseWriter, r *http.Request) {
			put = decodeBundleRequest(t, r)
			reply(w, 200, res)
		},
	})
	return f, srv.URL, &put
}

func TestRunDeploySendsOnlyChanges(t *testing.T) {
	dir := writeTree(t, map[string]string{"index.html": "new", "app.js": "js", "logo.png": "png"})
	base := map[string]string{"index.html": "old", "app.js": "js", "logo.png": "png", "old.css": "a{}"}
	_, url, put := deployAPI(t, base, api.PublishResult{URL: "https://demo.art.test/", Version: 3})

	r := runCLI(t, nil, "--url", url, "--token", "t", "deploy", dir, "--slug", "demo", "--dry-run")
	wantOut := "send       index.html  3 B\n" +
		"unchanged  app.js      2 B\n" +
		"unchanged  logo.png    3 B\n" +
		"delete     old.css     \n"
	if r.code != 0 || r.stdout != wantOut || r.stderr != "dry run, nothing sent: 1 file, 3 B; 2 unchanged; removed: old.css\n" {
		t.Errorf("deploy --dry-run: %+v\nwant stdout\n%s", r, wantOut)
	}

	r = runCLI(t, nil, "--url", url, "--token", "t", "deploy", dir, "--slug", "demo")
	if r.code != 0 || r.stderr != "1 file, 3 B; 2 unchanged; removed: old.css\n" {
		t.Errorf("deploy: %+v", r)
	}
	if diff := cmp.Diff(map[string]string{"index.html": "new"}, put.files); diff != "" {
		t.Errorf("deploy sent (-want +got):\n%s", diff)
	}
	wantManifest := map[string]any{"expected_version": 2.0, "files": []any{map[string]any{"path": "old.css", "delete": true}}}
	if diff := cmp.Diff(wantManifest, put.manifest); diff != "" {
		t.Errorf("deploy manifest (-want +got):\n%s", diff)
	}
}

// With nothing to send, the smallest file goes all the same, so the server
// says the version is unchanged, or adds one when the base is not the
// latest, as for any deploy.
func TestRunDeployNothingChanged(t *testing.T) {
	files := map[string]string{"index.html": "<h1>x</h1>", "app.js": "js", "logo.png": "png"}
	_, url, put := deployAPI(t, files, api.PublishResult{URL: "https://demo.art.test/", Version: 2, Unchanged: true})
	r := runCLI(t, nil, "--url", url, "--token", "t", "deploy", writeTree(t, files), "--slug", "demo")
	want := "1 file, 2 B; 2 unchanged\nno changes: version 2 is already this content\n"
	if r.code != 0 || r.stderr != want {
		t.Errorf("deploy of what version 2 holds: %+v, want stderr %q", r, want)
	}
	if diff := cmp.Diff(map[string]string{"app.js": "js"}, put.files); diff != "" {
		t.Errorf("deploy sent (-want +got):\n%s", diff)
	}
	if _, ok := put.manifest["files"]; ok {
		t.Errorf("deploy manifest %v deletes files", put.manifest)
	}
}

// A file that changes after deploy has checked it fails the deploy rather
// than send bytes it did not check.
func TestRunDeployFileChangedAfterPlanning(t *testing.T) {
	tests := []struct {
		name   string
		change func(t *testing.T, dir string)
	}{
		{"rewritten in place", func(t *testing.T, dir string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>y</h1>"), 0o600); err != nil {
				t.Error(err)
			}
		}},
		{"replaced by a link to a dotfile", func(t *testing.T, dir string) {
			t.Helper()
			if err := os.Remove(filepath.Join(dir, "index.html")); err != nil {
				t.Error(err)
			}
			if err := os.Symlink(".env", filepath.Join(dir, "index.html")); err != nil {
				t.Error(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const secret = "TOKEN=hush-hush"
			dir := writeTree(t, map[string]string{"index.html": "<h1>x</h1>", ".env": secret})
			var sent []byte
			cur := demo
			_, srv := newFakeAPI(t, map[string]http.HandlerFunc{
				// Between the plan and the archive.
				"GET /v1/artifacts/demo": func(w http.ResponseWriter, _ *http.Request) {
					tt.change(t, dir)
					reply(w, 200, api.ArtifactDetail{Artifact: cur, Versions: []api.Version{{Number: cur.LatestVersion}}})
				},
				"PUT /v1/artifacts/demo": func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					if i := bytes.Index(body, []byte{0x1f, 0x8b}); i >= 0 {
						if zr, err := gzip.NewReader(bytes.NewReader(body[i:])); err == nil {
							sent, _ = io.ReadAll(zr)
						}
					}
					reply(w, 200, api.PublishResult{Version: 9})
				},
			})
			r := runCLI(t, nil, "--url", srv.URL, "--token", "t", "deploy", dir, "--slug", "demo")
			srv.Close() // waits for the handler, which may still read what was sent
			if r.code != exitError || !strings.Contains(r.stderr, "index.html: the file changed after it was checked") {
				t.Errorf("deploy: %+v, want a failure that names index.html", r)
			}
			if bytes.Contains(sent, []byte(secret)) {
				t.Errorf("deploy sent the dotfile's content: %q", sent)
			}
		})
	}
}

// A deploy against a running server keeps the files it does not send.
func TestRunDeployAgainstServer(t *testing.T) {
	_, env, addr := startLocal(t)
	if r := runCLI(t, env, "admin", "users", "create", "ana"); r.code != 0 {
		t.Fatalf("admin users create: %+v", r)
	}
	r := runCLI(t, env, "admin", "keys", "issue", "ana")
	if r.code != 0 {
		t.Fatalf("admin keys issue: %+v", r)
	}
	key := strings.TrimSpace(r.stdout)
	var d net.Dialer
	e := cli.Env{
		LookupEnv: envMap(map[string]string{"HOME": t.TempDir(), "KAZARI_URL": "http://api.kazari.localhost", "KAZARI_TOKEN": key}),
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return d.DialContext(ctx, "tcp", addr)
		}},
	}
	dir := writeTree(t, map[string]string{"index.html": "<h1>one</h1>", "app.js": "js", "logo.png": "png"})
	deploy := func(wantErr string) {
		t.Helper()
		if r := runEnv(t, e, "deploy", dir, "--slug", "site"); r.code != 0 || r.stderr != wantErr {
			t.Fatalf("deploy: %+v, want stderr %q", r, wantErr)
		}
	}
	files := func() string {
		t.Helper()
		r := runEnv(t, e, "files", "ls", "site")
		if r.code != 0 {
			t.Fatalf("files ls: %+v", r)
		}
		var paths []string
		for _, line := range strings.Split(strings.TrimSpace(r.stdout), "\n")[1:] {
			paths = append(paths, strings.Fields(line)[0])
		}
		return strings.Join(paths, " ")
	}

	deploy("3 files, 17 B\n")
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>two</h1>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "app.js")); err != nil {
		t.Fatal(err)
	}
	deploy("1 file, 12 B; 1 unchanged; removed: app.js\n")
	if got := files(); got != "index.html logo.png" {
		t.Errorf("files after a deploy that sent index.html alone: %s", got)
	}
	if r := runEnv(t, e, "files", "cat", "site", "logo.png"); r.stdout != "png" {
		t.Errorf("logo.png, which the deploy did not send: %+v", r)
	}
	deploy("1 file, 3 B; 1 unchanged\nno changes: version 2 is already this content\n")
	if err := os.Remove(filepath.Join(dir, "logo.png")); err != nil {
		t.Fatal(err)
	}
	deploy("0 files, 0 B; 1 unchanged; removed: logo.png\n")
	if got := files(); got != "index.html" {
		t.Errorf("files after a deploy that only deleted: %s", got)
	}
}
