package dashboard

import (
	"io/fs"
	"os"
	"testing"
)

// Every template ships inside the binary: a file the embed pattern does not
// match would render from a checkout and be missing from a release.
func TestEveryTemplateIsEmbedded(t *testing.T) {
	err := fs.WalkDir(os.DirFS("."), "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if _, err := fs.Stat(templateFS, path); err != nil {
			t.Errorf("%s is not embedded: the go:embed pattern takes templates/*.html", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
