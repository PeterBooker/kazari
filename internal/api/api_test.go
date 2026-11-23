package api_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// constants reads the constants declared in dir's source with their values,
// which a leaf's tests read rather than import.
func constants(t *testing.T, dir string) map[string]ast.Expr {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]ast.Expr{}
	fset := token.NewFileSet()
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, s := range gd.Specs {
				vs := s.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if i < len(vs.Values) {
						out[name.Name] = vs.Values[i]
					}
				}
			}
		}
	}
	return out
}

func isSelector(e ast.Expr, pkg, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == pkg && sel.Sel.Name == name
}

// Every error code has a Code constant on the wire that is the code itself.
func TestCodeForEveryErrorCode(t *testing.T) {
	var want []string
	for name := range constants(t, filepath.Join("..", "errcode")) {
		want = append(want, "Code"+name)
	}
	if len(want) == 0 {
		t.Fatal("found no constants in internal/errcode's source")
	}
	var got []string
	for name, value := range constants(t, ".") {
		if !strings.HasPrefix(name, "Code") {
			continue
		}
		got = append(got, name)
		if !isSelector(value, "errcode", strings.TrimPrefix(name, "Code")) {
			t.Errorf("%s is not errcode.%s", name, strings.TrimPrefix(name, "Code"))
		}
	}
	slices.Sort(want)
	slices.Sort(got)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("the Code constants differ from internal/errcode's (-errcode +api):\n%s", diff)
	}
}
