package cli_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PeterBooker/kazari/internal/cli"
)

func FuzzIgnore(f *testing.F) {
	f.Add("*.map\nnode_modules/\n!keep.map\n", "a/keep.map", false)
	f.Add("/docs/**/draft-*\n# c\n", "docs/x/draft-1", false)
	f.Add("**\n!**/", "a/b", true)
	f.Add("[", "[", false)
	f.Fuzz(func(t *testing.T, rules, rel string, dir bool) {
		matched, include := cli.IgnoreMatch(rules, rel, dir)
		if include && !matched {
			t.Errorf("IgnoreMatch(%q, %q) includes without a matching rule", rules, rel)
		}
		// A last rule that names the path exactly decides it.
		if rel == "" || strings.ContainsAny(rel, "*?[\\\n\r") || strings.HasPrefix(rel, "!") || strings.HasPrefix(rel, "#") ||
			strings.HasSuffix(rel, "/") || strings.Contains(rel, "//") || strings.TrimRight(rel, " \t") != rel {
			return
		}
		if m, inc := cli.IgnoreMatch(rules+"\n/"+rel+"\n", rel, dir); !m || inc {
			t.Errorf("rules %q then /%s: match(%q) = %t, %t; want left out", rules, rel, rel, m, inc)
		}
		if m, inc := cli.IgnoreMatch(rules+"\n!/"+rel+"\n", rel, dir); !m || !inc {
			t.Errorf("rules %q then !/%s: match(%q) = %t, %t; want sent", rules, rel, rel, m, inc)
		}
	})
}

func FuzzNormalizeURL(f *testing.F) {
	for _, s := range []string{"api.art.test", "https://api.art.test/v1/", "http://127.0.0.1:8080", "ftp://x", "", " https://a/b/v1 ", "https://[::1]:1/"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		got, err := cli.NormalizeURL(raw)
		if err != nil {
			return
		}
		again, err := cli.NormalizeURL(got)
		if err != nil || again != got {
			t.Errorf("cli.NormalizeURL(%q) = %q, then %q, %v; want the same again", raw, got, again, err)
		}
		if strings.HasSuffix(got, "/") {
			t.Errorf("cli.NormalizeURL(%q) = %q, which ends in /", raw, got)
		}
	})
}

// FuzzExtractBundle mutates the tar inside the gzip stream, where the names
// and types are, rather than the compressed bytes.
func FuzzExtractBundle(f *testing.F) {
	f.Add(rawTar(f, tarEntry{Name: "index.html", Content: "<h1>x</h1>"}, tarEntry{Name: "a/b.css", Content: "b{}"}))
	f.Add(rawTar(f, tarEntry{Name: "../escape", Content: "x"}))
	f.Add(rawTar(f, tarEntry{Name: "link", Type: tar.TypeSymlink}))
	f.Add(rawTar(f, tarEntry{Name: "a", Type: tar.TypeDir}, tarEntry{Name: "a", Content: "x"}))
	f.Add(rawTar(f, tarEntry{Name: "a", Content: "xx"}, tarEntry{Name: "./a", Content: "y"}))
	f.Fuzz(func(t *testing.T, raw []byte) {
		archive := gz(t, raw)
		parent := t.TempDir()
		dir := filepath.Join(parent, "out")
		pulled, size, err := cli.ExtractBundle(bytes.NewReader(archive), dir)
		entries, rerr := os.ReadDir(parent)
		if rerr != nil {
			t.Fatal(rerr)
		}
		if len(entries) > 1 || (len(entries) == 1 && entries[0].Name() != "out") {
			t.Fatalf("extractBundle wrote beside its directory: %v", entries)
		}
		if err != nil {
			return
		}
		var sum int64
		for _, p := range pulled {
			fi, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(p)))
			if err != nil || !fi.Mode().IsRegular() {
				t.Fatalf("extractBundle reported %q, which is not a regular file in the directory: %v", p, err)
			}
			sum += fi.Size()
		}
		if sum != size {
			t.Errorf("extractBundle reported %d bytes, the files hold %d", size, sum)
		}
		if _, _, err := cli.ExtractBundle(bytes.NewReader(archive), dir); errors.Is(err, cli.ErrUnsafeEntry) {
			t.Errorf("extractBundle refused an archive the second time that it took the first: %v", err)
		}
	})
}
