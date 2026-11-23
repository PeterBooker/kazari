package errcode_test

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/errcode"
)

var (
	errThing      = errcode.New(errcode.NotFound, "thing not found")
	errOtherThing = errcode.Text(errcode.NotFound, "other: not found")
	errShown      = errcode.Shown(errcode.Forbidden, "pkg: refused", "you may not")
)

func copyOf(sentinel *errcode.Error) *errcode.Error {
	e, _ := errors.AsType[*errcode.Error](errcode.Errorf(sentinel, "a copy"))
	return e
}

func TestIs(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		target error
		want   bool
	}{
		{name: "a sentinel is itself", err: errThing, target: errThing, want: true},
		{name: "a wrapped sentinel", err: fmt.Errorf("get: %w", errThing), target: errThing, want: true},
		{name: "a copy is its sentinel", err: errcode.Errorf(errThing, "thing %q not found", "a"), target: errThing, want: true},
		{name: "a copy of a copy is its sentinel", err: errcode.Errorf(copyOf(errThing), "b"), target: errThing, want: true},
		{name: "two copies of one sentinel", err: errcode.Errorf(errThing, "a"), target: errcode.Errorf(errThing, "b"), want: true},
		{name: "another sentinel with the same code", err: errOtherThing, target: errThing, want: false},
		{name: "the other way around", err: errThing, target: errOtherThing, want: false},
		{name: "a copy of another sentinel with the same code", err: errcode.Errorf(errOtherThing, "x"), target: errThing, want: false},
		{name: "a literal with the same code", err: &errcode.Error{Code: errcode.NotFound, Message: "thing not found"}, target: errThing, want: false},
		{name: "another code", err: errShown, target: errThing, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := errors.Is(tt.err, tt.target); got != tt.want {
				t.Errorf("errors.Is(%v, %v) = %v, want %v", tt.err, tt.target, got, tt.want)
			}
		})
	}
}

func TestError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "new", err: errThing, want: "not_found: thing not found"},
		{name: "text", err: errOtherThing, want: "other: not found"},
		{name: "shown", err: errShown, want: "pkg: refused"},
		{name: "a copy", err: errcode.Errorf(errShown, "no %s", "way"), want: "forbidden: no way"},
		{name: "a literal without a message", err: &errcode.Error{Code: errcode.Gone}, want: "gone"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.Error(); got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPublic(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode string
		wantMsg  string
		wantOK   bool
	}{
		{name: "new shows its message", err: fmt.Errorf("get: %w", errThing), wantCode: errcode.NotFound, wantMsg: "thing not found", wantOK: true},
		{name: "text shows the whole error", err: fmt.Errorf("%w: %q", errOtherThing, "a"), wantCode: errcode.NotFound,
			wantMsg: `other: not found: "a"`, wantOK: true},
		{name: "shown keeps its words", err: fmt.Errorf("%w: %q", errShown, "a"), wantCode: errcode.Forbidden, wantMsg: "you may not", wantOK: true},
		{name: "a copy shows its message", err: errcode.Errorf(errOtherThing, "no %s", "way"), wantCode: errcode.NotFound, wantMsg: "no way", wantOK: true},
		{name: "an error without a code", err: errors.New("boom"), wantOK: false},
		{name: "no error", err: nil, wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, msg, ok := errcode.Public(tt.err)
			if code != tt.wantCode || msg != tt.wantMsg || ok != tt.wantOK {
				t.Errorf("Public(%v) = %q, %q, %v, want %q, %q, %v", tt.err, code, msg, ok, tt.wantCode, tt.wantMsg, tt.wantOK)
			}
			if got := errcode.CodeOf(tt.err); got != tt.wantCode {
				t.Errorf("CodeOf(%v) = %q, want %q", tt.err, got, tt.wantCode)
			}
		})
	}
}

// declaredCodes reads the code constants from the package's source, which
// is the only place that lists them without Codes.
func declaredCodes(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var codes []string
	fset := token.NewFileSet()
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, parser.SkipObjectResolution)
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
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Fatalf("%s is not a string literal", name.Name)
					}
					code, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatalf("%s: %v", name.Name, err)
					}
					codes = append(codes, code)
				}
			}
		}
	}
	if len(codes) == 0 {
		t.Fatal("found no constants in the package's source")
	}
	return codes
}

func TestCodesListsEveryCode(t *testing.T) {
	want := declaredCodes(t)
	slices.Sort(want)
	got := slices.Clone(errcode.Codes)
	slices.Sort(got)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Codes differs from the constants (-constants +Codes):\n%s", diff)
	}
}
