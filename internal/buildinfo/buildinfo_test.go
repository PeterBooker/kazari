package buildinfo_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/buildinfo"
)

func TestGetStamped(t *testing.T) {
	buildinfo.Stamp(t, "1.2.3", "0123456789abcdef0123456789abcdef01234567", "2026-11-01T12:00:00Z")
	got := buildinfo.Get()
	want := buildinfo.Info{Version: "1.2.3", Commit: "0123456789abcdef0123456789abcdef01234567", Date: "2026-11-01T12:00:00Z", Go: runtime.Version()}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Get() of a stamped build (-want +got):\n%s", diff)
	}
	if got, want := got.String(), "1.2.3 (0123456789ab)"; got != want {
		t.Errorf("Get().String() = %q, want %q", got, want)
	}
}

func TestGetUnstamped(t *testing.T) {
	buildinfo.Stamp(t, "", "", "")
	got := buildinfo.Get()
	if got.Version == "" || got.Go != runtime.Version() {
		t.Errorf("Get() of an unstamped build = %+v, want a version and Go %s", got, runtime.Version())
	}
}

func TestString(t *testing.T) {
	tests := []struct {
		name string
		info buildinfo.Info
		want string
	}{
		{name: "no commit", info: buildinfo.Info{Version: "dev"}, want: "dev"},
		{name: "a short commit", info: buildinfo.Info{Version: "1.2.3", Commit: "abc123"}, want: "1.2.3 (abc123)"},
		{name: "a full commit", info: buildinfo.Info{Version: "1.2.3", Commit: "0123456789abcdef0123"}, want: "1.2.3 (0123456789ab)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.info.String(); got != tt.want {
				t.Errorf("%+v.String() = %q, want %q", tt.info, got, tt.want)
			}
		})
	}
}

// stampable lists the package's string variables, the only ones the linker's
// -X can set.
func stampable(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "buildinfo.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, s := range gd.Specs {
			vs := s.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					names = append(names, name.Name)
				}
			}
		}
	}
	return names
}

// The linker ignores an -X that names a variable that does not exist, and
// the build then reports no version without failing.
func TestStampFlagsNameVariables(t *testing.T) {
	pkg := reflect.TypeFor[buildinfo.Info]().PkgPath()
	vars := stampable(t)
	flag := regexp.MustCompile(`-X ([^\s="]+)\.(\w+)=`)
	for _, file := range []string{".goreleaser.yaml", "deploy/Dockerfile", "docs/development.md"} {
		t.Run(file, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(file)))
			if err != nil {
				t.Fatal(err)
			}
			flags := flag.FindAllStringSubmatch(string(data), -1)
			var stamped []string
			for _, m := range flags {
				if m[1] != pkg || !slices.Contains(vars, m[2]) {
					t.Errorf("%s sets %s.%s, want one of %v in %s", file, m[1], m[2], vars, pkg)
				}
				stamped = append(stamped, m[2])
			}
			if !slices.Contains(stamped, "version") {
				t.Errorf("%s stamps %v, want the version among them", file, stamped)
			}
		})
	}
}
