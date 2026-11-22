package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// fakeKey has the shape of an API key, so the scan must find it.
var fakeKey = "kzk_" + strings.Repeat("a", 8) + "_" + strings.Repeat("b", 52)

func TestPlanBundle(t *testing.T) {
	tests := []struct {
		name        string
		files       map[string]string
		wantFiles   []string
		wantSkipped []skippedFile
	}{
		{
			name:      "dotfiles but .well-known",
			files:     map[string]string{"index.html": "x", ".env": "A=1", ".git/HEAD": "ref", ".well-known/security.txt": "s"},
			wantFiles: []string{".well-known/security.txt", "index.html"},
			wantSkipped: []skippedFile{
				{Path: ".env", Reason: "dotfile"},
				{Path: ".git", Reason: "dotfile"},
			},
		},
		{
			name: "secrets by name",
			files: map[string]string{"index.html": "x", "server.pem": "p", "tls.KEY": "k", "id_rsa.pub": "pub",
				"id_ed25519": "k", "credentials.json": "{}", "vault.kdbx": "v", "cert.p12": "c", "keyboard.html": "fine"},
			wantFiles: []string{"index.html", "keyboard.html"},
			wantSkipped: []skippedFile{
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
			wantSkipped: []skippedFile{{Path: "big.js", Reason: "looks like a secret"}, {Path: "deploy.sh", Reason: "looks like a secret"}},
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
			p, err := planBundle(writeTree(t, tt.files))
			if err != nil {
				t.Fatal(err)
			}
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
	p, err := planBundle(link)
	if err != nil {
		t.Fatalf("planBundle(%s), a symlink to a directory: %v", link, err)
	}
	if diff := cmp.Diff([]string{"index.html", "real.png"}, p.paths()); diff != "" {
		t.Errorf("files (-want +got):\n%s", diff)
	}
	if want := "2 files, 14 B; skipped: logo.png (symlink)"; p.summary() != want {
		t.Errorf("summary = %q, want %q", p.summary(), want)
	}
}

func TestPlanBundleSingleFile(t *testing.T) {
	dir := writeTree(t, map[string]string{"Report.HTML": "<h1>r</h1>", "data.csv": "a,b", "id_rsa": "k"})
	tests := []struct {
		file        string
		wantFiles   []string
		wantSkipped []skippedFile
	}{
		{"Report.HTML", []string{"index.html"}, nil},
		{"data.csv", []string{"data.csv"}, nil},
		{"id_rsa", []string{}, []skippedFile{{Path: "id_rsa", Reason: "looks like a secret"}}},
	}
	for _, tt := range tests {
		p, err := planBundle(filepath.Join(dir, tt.file))
		if err != nil {
			t.Fatalf("planBundle(%s): %v", tt.file, err)
		}
		if diff := cmp.Diff(tt.wantFiles, p.paths()); diff != "" {
			t.Errorf("planBundle(%s) files (-want +got):\n%s", tt.file, diff)
		}
		if diff := cmp.Diff(tt.wantSkipped, p.Skipped); diff != "" {
			t.Errorf("planBundle(%s) skipped (-want +got):\n%s", tt.file, diff)
		}
	}
}

func TestPlanSummary(t *testing.T) {
	p := &bundlePlan{Bytes: 312 << 10, Removed: []string{"sub/a.txt"}}
	for range 14 {
		p.Files = append(p.Files, bundleFile{})
	}
	for i := range 12 {
		p.Skipped = append(p.Skipped, skippedFile{Path: string(rune('a' + i)), Reason: "dotfile"})
	}
	want := "14 files, 312.0 KiB; skipped: a (dotfile), b (dotfile), c (dotfile), d (dotfile), e (dotfile), " +
		"f (dotfile), g (dotfile), h (dotfile), i (dotfile), j (dotfile) and 2 more; removed: sub/a.txt"
	if got := p.summary(); got != want {
		t.Errorf("summary =\n %q\nwant\n %q", got, want)
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
		{filepath.Join(dir, "a"), "", `publish: "a" does not make a slug; give --slug`},
	}
	for _, tt := range tests {
		got, err := slugFor("publish", "", tt.src)
		if tt.wantErr != "" {
			if err == nil || err.Error() != tt.wantErr {
				t.Errorf("slugFor(%q) = %q, %v; want error %q", tt.src, got, err, tt.wantErr)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("slugFor(%q) = %q, %v; want %q", tt.src, got, err, tt.want)
		}
	}
}
