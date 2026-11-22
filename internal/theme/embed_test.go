package theme

import (
	"io/fs"
	"os"
	"strings"
	"testing"
)

// Every file of the look ships inside the binary: a file added beside the
// others that the embed pattern does not match would work from a checkout
// and be missing from a release.
func TestEveryFileIsEmbedded(t *testing.T) {
	err := fs.WalkDir(os.DirFS("."), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch {
		case d.IsDir() && path == "testdata":
			return fs.SkipDir
		case d.IsDir(), strings.HasSuffix(path, ".go"):
			return nil
		// The list of glyphs the font subset is built from, read by
		// site/subset-fonts.sh and never served.
		case path == "fonts/japanese.txt":
			return nil
		}
		if _, err := fs.Stat(files, path); err != nil {
			t.Errorf("%s is not embedded: add it to the go:embed pattern in theme.go", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
