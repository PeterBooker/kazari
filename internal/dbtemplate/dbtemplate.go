// Package dbtemplate gives each test a database file of its own without
// migrating one for every test. The first test that asks builds the file
// once, and every test then gets a copy of its bytes: under the race
// detector, migrating an empty SQLite database costs far more than writing
// the few hundred kilobytes it ends up as.
//
// A package keeps one template for its tests, beside the helper that opens
// the database:
//
//	var schema = dbtemplate.New(func(ctx context.Context, path string) error {
//		db, err := sqlite.Open(ctx, path, sqlite.Options{})
//		if err != nil {
//			return err
//		}
//		return db.Close()
//	})
//
// and the helper copies it to the path it then opens:
//
//	schema.Copy(t, path)
//	db, err := sqlite.Open(ctx, path, sqlite.Options{})
package dbtemplate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Template is a database file built once and copied for each test.
type Template struct {
	build func(ctx context.Context, path string) error

	once sync.Once
	data []byte
	err  error
}

// New returns a template that build creates at path the first time a test
// copies it. build must leave the file whole when it returns: the database
// closed, with no write-ahead log or journal beside it.
func New(build func(ctx context.Context, path string) error) *Template {
	return &Template{build: build}
}

// Copy writes the template to path, which must not exist yet.
func (tp *Template) Copy(t testing.TB, path string) {
	t.Helper()
	tp.once.Do(func() { tp.data, tp.err = tp.make(t) })
	if tp.err != nil {
		t.Fatalf("build the database template: %v", tp.err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		t.Fatalf("copy the database template: %v", err)
	}
	_, err = f.Write(tp.data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatalf("copy the database template to %s: %v", path, err)
	}
}

// make builds the template in a directory of the test that first asks for
// it and returns its bytes, which outlive that directory.
func (tp *Template) make(t testing.TB) ([]byte, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "template.db")
	if err := tp.build(t.Context(), path); err != nil {
		return nil, err
	}
	for _, side := range []string{"-wal", "-journal"} {
		if fi, err := os.Stat(path + side); err == nil && fi.Size() > 0 {
			return nil, fmt.Errorf("build left %d bytes in %s%s: the database is not closed", fi.Size(), filepath.Base(path), side)
		}
	}
	return os.ReadFile(path)
}
