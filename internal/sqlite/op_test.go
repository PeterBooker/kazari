package sqlite

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/instrument"
)

// TestEveryStoreMethodTraced keeps new store methods from going untraced:
// each exported method of a *...Store starts with
// ctx, end := s.db.op(ctx, "<Store>.<Method>") and defer func() { end(err) }().
func TestEveryStoreMethodTraced(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	methods := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || !fn.Name.IsExported() {
				continue
			}
			star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			store, ok := star.X.(*ast.Ident)
			if !ok || !strings.HasSuffix(store.Name, "Store") {
				continue
			}
			methods++
			want := store.Name + "." + fn.Name.Name
			if !startsWithOp(fn, want) {
				t.Errorf("%s: %s must begin with ctx, end := s.db.op(ctx, %q) and defer func() { end(err) }()", fset.Position(fn.Pos()), want, want)
			}
		}
	}
	if methods < 70 {
		t.Errorf("found %d store methods; the check is not seeing the stores", methods)
	}
}

func startsWithOp(fn *ast.FuncDecl, name string) bool {
	if fn.Body == nil || len(fn.Body.List) < 2 {
		return false
	}
	assign, ok := fn.Body.List[0].(*ast.AssignStmt)
	if !ok || len(assign.Rhs) != 1 {
		return false
	}
	call, ok := assign.Rhs[0].(*ast.CallExpr)
	if !ok || len(call.Args) != 2 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "op" {
		return false
	}
	lit, ok := call.Args[1].(*ast.BasicLit)
	if !ok {
		return false
	}
	got, err := strconv.Unquote(lit.Value)
	if err != nil || got != name {
		return false
	}
	def, ok := fn.Body.List[1].(*ast.DeferStmt)
	if !ok {
		return false
	}
	deferred, ok := def.Call.Fun.(*ast.FuncLit)
	if !ok || len(deferred.Body.List) != 1 {
		return false
	}
	stmt, ok := deferred.Body.List[0].(*ast.ExprStmt)
	if !ok {
		return false
	}
	endCall, ok := stmt.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	ident, ok := endCall.Fun.(*ast.Ident)
	return ok && ident.Name == "end"
}

func TestStoreSpans(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	defer tp.Shutdown(context.Background())
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "kazari.db"), Options{Telemetry: instrument.Providers{TracerProvider: tp}})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewArtifactStore(db)

	if _, err := store.Get(ctx, "missing"); err == nil {
		t.Fatal("Get of a missing slug succeeded")
	}
	if n := len(rec.Ended()); n != 0 {
		t.Fatalf("spans without a recording parent = %d, want 0", n)
	}

	parentCtx, parent := tp.Tracer("test").Start(ctx, "artifact.get")
	_, err = store.Get(parentCtx, "missing")
	parent.End()
	if !artifactNotFound(err) {
		t.Fatalf("err = %v", err)
	}
	var got sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		if s.Name() == "ArtifactStore.Get" {
			got = s
		}
	}
	if got == nil || got.Parent().SpanID() != parent.SpanContext().SpanID() {
		t.Fatalf("store span = %v", got)
	}
	attrs := map[string]string{}
	for _, kv := range got.Attributes() {
		attrs[string(kv.Key)] = kv.Value.String()
	}
	if attrs["code.function.name"] != "github.com/PeterBooker/kazari/internal/sqlite.(*ArtifactStore).Get" || attrs["error.type"] != "not_found" {
		t.Errorf("attributes = %v", attrs)
	}
	if len(got.Events()) != 0 {
		t.Errorf("a not-found recorded an exception: %v", got.Events())
	}
}

func artifactNotFound(err error) bool { return artifact.CodeOf(err) == "not_found" }
