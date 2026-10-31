// Package sqlite implements the artifact, user, upload, audit and oauth
// stores on one SQLite database. Every public method owns its transaction.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"

	_ "github.com/ncruces/go-sqlite3/driver"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/instrument"
	"github.com/PeterBooker/kazari/internal/oauth"
	"github.com/PeterBooker/kazari/internal/user"
)

const scope = "github.com/PeterBooker/kazari/internal/sqlite"

// Options configure a DB. The zero value is valid.
type Options struct {
	Telemetry instrument.Providers
}

// DB is an open, migrated database.
type DB struct {
	*sql.DB
	tracer trace.Tracer
}

// Open opens or creates the database at path and applies pending migrations.
func Open(ctx context.Context, path string, o Options) (*DB, error) {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Set("_txlock", "immediate")
	dsn := "file:" + path + "?" + q.Encode()
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: open %s: %w", path, err)
	}
	if err := Migrate(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return &DB{DB: db, tracer: o.Telemetry.Tracer(scope)}, nil
}

// op starts the span of the store method name ("ArtifactStore.Get") under a
// recording parent, and returns the function that ends it with the method's
// error. It is not a database client span, which describes one statement:
// a store method runs several in one transaction, and the method is where a
// request's time goes.
func (db *DB) op(ctx context.Context, name string) (context.Context, func(*error)) {
	if !trace.SpanFromContext(ctx).IsRecording() {
		return ctx, endNothing
	}
	store, method, _ := strings.Cut(name, ".")
	ctx, span := db.tracer.Start(ctx, name,
		trace.WithAttributes(semconv.CodeFunctionName(scope+".(*"+store+")."+method)))
	return ctx, func(err *error) { instrument.End(span, *err, errorCode(*err)) }
}

func endNothing(*error) {}

// errorCode names the expected outcomes a store reports, such as a missing
// row, so only real faults count as internal.
func errorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case artifact.CodeOf(err) != "":
		return artifact.CodeOf(err)
	case errors.Is(err, user.ErrNotFound), errors.Is(err, user.ErrKeyNotFound), errors.Is(err, oauth.ErrNotFound):
		return api.CodeNotFound
	case errors.Is(err, user.ErrHandleTaken):
		return api.CodeHandleTaken
	case errors.Is(err, user.ErrEmailTaken):
		return api.CodeEmailTaken
	case errors.Is(err, oauth.ErrCodeUsed):
		return api.CodeInvalidArgument
	default:
		return instrument.CodeInternal
	}
}

// beginner is a *sql.DB, or a *sql.Conn that a migration has set up.
type beginner interface {
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

func inTx(ctx context.Context, db beginner, fn func(tx *sql.Tx) error) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback()
			return
		}
		err = tx.Commit()
	}()
	return fn(tx)
}
