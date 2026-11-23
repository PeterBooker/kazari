package cli_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/api"
)

type plannedFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

type skipped struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// dryPlan is what --dry-run --json prints.
type dryPlan struct {
	Files     []plannedFile `json:"files"`
	Unchanged []plannedFile `json:"unchanged"`
	Skipped   []skipped     `json:"skipped"`
	Removed   []string      `json:"removed"`
	Bytes     int64         `json:"bytes"`
}

func (p dryPlan) paths() []string {
	out := []string{}
	for _, f := range p.Files {
		out = append(out, f.Path)
	}
	return out
}

// planOf is the plan publish --dry-run prints for src, and the summary.
func planOf(t *testing.T, src string) (dryPlan, string) {
	t.Helper()
	r := runCLI(t, nil, "--json", "publish", src, "--slug", "demo", "--dry-run")
	var p dryPlan
	if err := json.Unmarshal([]byte(r.stdout), &p); err != nil || r.code != 0 {
		t.Fatalf("publish %s --dry-run --json: %+v: %v", src, r, err)
	}
	r = runCLI(t, nil, "publish", src, "--slug", "demo", "--dry-run")
	return p, strings.TrimPrefix(strings.TrimSuffix(r.stderr, "\n"), "dry run, nothing sent: ")
}

// fakeKey has the shape of an API key, so the scan must find it.
var fakeKey = "kzk_" + strings.Repeat("a", 8) + "_" + strings.Repeat("b", 52)

func TestPlanBundle(t *testing.T) {
	tests := []struct {
		name        string
		files       map[string]string
		wantFiles   []string
		wantSkipped []skipped
	}{
		{
			name:      "dotfiles but .well-known",
			files:     map[string]string{"index.html": "x", ".env": "A=1", ".git/HEAD": "ref", ".well-known/security.txt": "s"},
			wantFiles: []string{".well-known/security.txt", "index.html"},
			wantSkipped: []skipped{
				{Path: ".env", Reason: "dotfile"},
				{Path: ".git", Reason: "dotfile"},
			},
		},
		{
			name: "secrets by name",
			files: map[string]string{"index.html": "x", "server.pem": "p", "tls.KEY": "k", "id_rsa.pub": "pub",
				"id_ed25519": "k", "credentials.json": "{}", "vault.kdbx": "v", "cert.p12": "c", "keyboard.html": "fine"},
			wantFiles: []string{"index.html", "keyboard.html"},
			wantSkipped: []skipped{
				{Path: "cert.p12", Reason: "looks like a secret"},
				{Path: "credentials.json", Reason: "looks like a secret"},
				{Path: "id_ed25519", Reason: "looks like a secret"},
				{Path: "id_rsa.pub", Reason: "looks like a secret"},
				{Path: "server.pem", Reason: "looks like a secret"},
				{Path: "tls.KEY", Reason: "looks like a secret"},
				{Path: "vault.kdbx", Reason: "looks like a secret"},
			},
		},
		{
			name: "a key in the content",
			files: map[string]string{"index.html": "x", "deploy.sh": "export KAZARI_TOKEN=" + fakeKey + "\n",
				"big.js": strings.Repeat("x", 64<<10-10) + fakeKey, "docs.html": "kzk_ab12cd34_..."},
			wantFiles:   []string{"docs.html", "index.html"},
			wantSkipped: []skipped{{Path: "big.js", Reason: "looks like a secret"}, {Path: "deploy.sh", Reason: "looks like a secret"}},
		},
		{
			name: "kazariignore",
			files: map[string]string{
				".kazariignore": "# build leftovers\n*.map\nnode_modules/\n/notes.md\ndocs/**/draft-*\n!keep.map\n!.htaccess\n!server.pem\n",
				"index.html":    "x", "app.js.map": "m", "keep.map": "k", "sub/notes.md": "n", "notes.md": "n",
				"node_modules/a/b.js": "b", "docs/x/y/draft-1.html": "d", "docs/final.html": "f",
				".htaccess": "h", "server.pem": "p",
			},
			wantFiles: []string{".htaccess", "docs/final.html", "index.html", "keep.map", "server.pem", "sub/notes.md"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, _ := planOf(t, writeTree(t, tt.files))
			if diff := cmp.Diff(tt.wantFiles, p.paths()); diff != "" {
				t.Errorf("files (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tt.wantSkipped, p.Skipped); diff != "" {
				t.Errorf("skipped (-want +got):\n%s", diff)
			}
		})
	}
}

func TestPlanBundleSymlinks(t *testing.T) {
	dir := writeTree(t, map[string]string{"site/index.html": "<h1>hi</h1>", "site/real.png": "png"})
	site := filepath.Join(dir, "site")
	if err := os.Symlink(filepath.Join(site, "real.png"), filepath.Join(site, "logo.png")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "dist")
	if err := os.Symlink(site, link); err != nil {
		t.Fatal(err)
	}
	p, summary := planOf(t, link)
	if diff := cmp.Diff([]string{"index.html", "real.png"}, p.paths()); diff != "" {
		t.Errorf("files (-want +got):\n%s", diff)
	}
	if want := "2 files, 14 B; skipped: logo.png (symlink)"; summary != want {
		t.Errorf("summary = %q, want %q", summary, want)
	}
}

func TestPlanBundleSingleFile(t *testing.T) {
	dir := writeTree(t, map[string]string{"Report.HTML": "<h1>r</h1>", "data.csv": "a,b", "id_rsa": "k"})
	tests := []struct {
		file        string
		wantFiles   []string
		wantSkipped []skipped
	}{
		{"Report.HTML", []string{"index.html"}, nil},
		{"data.csv", []string{"data.csv"}, nil},
	}
	if r := runCLI(t, nil, "publish", filepath.Join(dir, "id_rsa"), "--slug", "demo", "--dry-run"); r.code != exitError ||
		!strings.Contains(r.stderr, "nothing to publish in "+filepath.Join(dir, "id_rsa")+" (0 files, 0 B; skipped: id_rsa (looks like a secret))") {
		t.Errorf("publish id_rsa: %+v, want nothing to publish", r)
	}
	for _, tt := range tests {
		p, _ := planOf(t, filepath.Join(dir, tt.file))
		if diff := cmp.Diff(tt.wantFiles, p.paths()); diff != "" {
			t.Errorf("publish %s: files (-want +got):\n%s", tt.file, diff)
		}
		if diff := cmp.Diff(tt.wantSkipped, p.Skipped); diff != "" {
			t.Errorf("publish %s: skipped (-want +got):\n%s", tt.file, diff)
		}
	}
}

func TestPlanSummary(t *testing.T) {
	files := map[string]string{}
	for i := range 14 {
		files[fmt.Sprintf("f%02d.txt", i)] = strings.Repeat("x", 312<<10/14)
	}
	for i := range 12 {
		files[fmt.Sprintf(".%c", 'a'+i)] = "x"
	}
	_, summary := planOf(t, writeTree(t, files))
	want := "14 files, 312 KiB; skipped: .a (dotfile), .b (dotfile), .c (dotfile), .d (dotfile), .e (dotfile), " +
		".f (dotfile), .g (dotfile), .h (dotfile), .i (dotfile), .j (dotfile) and 2 more"
	if summary != want {
		t.Errorf("summary =\n %q\nwant\n %q", summary, want)
	}
}

func TestSlugFor(t *testing.T) {
	dir := writeTree(t, map[string]string{"My Site/index.html": "x", "Q3 Report.html": "x", "a/index.html": "x"})
	tests := []struct {
		src, want, wantErr string
	}{
		{filepath.Join(dir, "My Site"), "my-site", ""},
		{filepath.Join(dir, "My Site") + "/", "my-site", ""},
		{filepath.Join(dir, "Q3 Report.html"), "q3-report", ""},
		{filepath.Join(dir, "a"), "", `kazari: publish: "a" does not make a slug; give --slug` + "\n"},
	}
	for _, tt := range tests {
		var slug any
		_, srv := newFakeAPI(t, map[string]http.HandlerFunc{
			"POST /v1/artifacts": func(w http.ResponseWriter, r *http.Request) {
				slug = decodeBundleRequest(t, r).manifest["slug"]
				reply(w, 201, api.PublishResult{URL: "https://x.art.test", Version: 1})
			},
		})
		r := runCLI(t, nil, "--url", srv.URL, "--token", "t", "publish", tt.src)
		if tt.wantErr != "" {
			if r.code != exitUsage || r.stderr != tt.wantErr {
				t.Errorf("publish %s: %+v, want exit %d and %q", tt.src, r, exitUsage, tt.wantErr)
			}
			continue
		}
		if r.code != 0 || slug != tt.want {
			t.Errorf("publish %s: %+v, slug %v; want %q", tt.src, r, slug, tt.want)
		}
	}
}
