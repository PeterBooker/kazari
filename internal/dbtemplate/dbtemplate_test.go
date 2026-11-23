package dbtemplate_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/PeterBooker/kazari/internal/dbtemplate"
)

func TestCopyBuildsOnce(t *testing.T) {
	builds := 0
	tp := dbtemplate.New(func(_ context.Context, path string) error {
		builds++
		return os.WriteFile(path, []byte("schema"), 0o600)
	})
	for _, name := range []string{"a.db", "b.db"} {
		path := filepath.Join(t.TempDir(), name)
		tp.Copy(t, path)
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "schema" {
			t.Errorf("Copy to %s wrote %q, want %q", name, got, "schema")
		}
	}
	if builds != 1 {
		t.Errorf("build ran %d times for two copies, want 1", builds)
	}
}
