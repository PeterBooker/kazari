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
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/PeterBooker/kazari/internal/errcode"
	"github.com/PeterBooker/kazari/internal/instrument"
	"github.com/PeterBooker/kazari/internal/oauth"
	"github.com/PeterBooker/kazari/internal/signin"
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
	stmt   statements
}

// statements are the queries of serving a page, prepared once: the driver
// otherwise parses a statement on every call, which cost a page request more
// than running it.
type statements struct {
	resolvePublished *sql.Stmt
	resolveVersion   *sql.Stmt
	file             *sql.Stmt
	fileOrigin       *sql.Stmt
}

func (s *statements) prepare(ctx context.Context, db *sql.DB) error {
	for _, q := range []struct {
		stmt  **sql.Stmt
		query string
	}{
		{&s.resolvePublished, artifactResolvePublished},
		{&s.resolveVersion, artifactResolveVersion},
		{&s.file, artifactFile},
		{&s.fileOrigin, artifactFileOrigin},
	} {
		stmt, err := db.PrepareContext(ctx, q.query)
		if err != nil {
			return errors.Join(fmt.Errorf("sqlite: prepare: %w", err), s.close())
		}
		*q.stmt = stmt
	}
	return nil
}

func (s *statements) close() error {
	var errs []error
	for _, stmt := range []*sql.Stmt{s.resolvePublished, s.resolveVersion, s.file, s.fileOrigin} {
		if stmt != nil {
			errs = append(errs, stmt.Close())
		}
	}
	return errors.Join(errs...)
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
	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(4)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: open %s: %w", path, err)
	}
	if err := Migrate(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	out := &DB{DB: db, tracer: o.Telemetry.Tracer(scope)}
	if err := out.stmt.prepare(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	observePool(o.Telemetry.Meter(scope), db)
	return out, nil
}

// Close closes the prepared statements and then the database.
func (db *DB) Close() error {
	return errors.Join(db.stmt.close(), db.DB.Close())
}

const maxOpenConns = 8

// VacuumInto writes a compacted copy of the database to path, which must
// not exist. The copy is one consistent snapshot, taken while other
// connections go on reading and writing.
func (db *DB) VacuumInto(ctx context.Context, path string) (err error) {
	ctx, end := db.op(ctx, "DB.VacuumInto")
	defer func() { end(err) }()
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		return fmt.Errorf("sqlite: vacuum into %s: %w", path, err)
	}
	return nil
}

// observePool reports the connection pool as the database client
// conventions describe it, from sql.DBStats at each collection.
func observePool(m metric.Meter, db *sql.DB) {
	count := instrument.Handle(m.Int64ObservableUpDownCounter("db.client.connection.count",
		metric.WithUnit("{connection}"), metric.WithDescription("The number of connections that are currently in state described by the state attribute.")))
	limit := instrument.Handle(m.Int64ObservableUpDownCounter("db.client.connection.max",
		metric.WithUnit("{connection}"), metric.WithDescription("The maximum number of open connections allowed.")))
	pool := semconv.DBClientConnectionPoolName("kazari.db")
	idle := metric.WithAttributes(pool, semconv.DBClientConnectionStateIdle)
	used := metric.WithAttributes(pool, semconv.DBClientConnectionStateUsed)
	poolOnly := metric.WithAttributes(pool)
	_ = instrument.Handle(m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		st := db.Stats()
		o.ObserveInt64(count, int64(st.Idle), idle)
		o.ObserveInt64(count, int64(st.InUse), used)
		o.ObserveInt64(limit, int64(st.MaxOpenConnections), poolOnly)
		return nil
	}, count, limit))
}

// op starts the span of the store method name ("ArtifactStore.Get") under a
// recording parent, and returns the function that ends it with the method's
// error. It is not a database client span, which describes one statement:
// a store method runs several in one transaction, and the method is where a
// request's time goes. Callers defer a closure that passes err by value:
// passing &err to the returned function would move err to the heap in
// every call, sampled or not.
func (db *DB) op(ctx context.Context, name string) (context.Context, func(error)) {
	if !trace.SpanFromContext(ctx).IsRecording() {
		return ctx, endNothing
	}
	store, method, _ := strings.Cut(name, ".")
	ctx, span := db.tracer.Start(ctx, name,
		trace.WithAttributes(semconv.CodeFunctionName(scope+".(*"+store+")."+method)))
	return ctx, func(err error) { instrument.End(span, err, errorCode(err)) }
}

func endNothing(error) {}

// errorCode names the expected outcomes a store reports, such as a missing
// row, so only real faults count as internal.
func errorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errcode.CodeOf(err) != "":
		return errcode.CodeOf(err)
	case errors.Is(err, oauth.ErrNotFound), errors.Is(err, signin.ErrNotFound):
		return errcode.NotFound
	case errors.Is(err, oauth.ErrCodeUsed), errors.Is(err, signin.ErrCodeUsed):
		return errcode.InvalidArgument
	default:
		return instrument.CodeInternal
	}
}

// beginner is a *sql.DB, or a *sql.Conn that a migration has set up.
type beginner interface {
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

// inTx runs fn in a transaction that takes the write lock as it begins
// (_txlock=immediate), so it never fails part way when another writer
// commits first.
func inTx(ctx context.Context, db beginner, fn func(tx *sql.Tx) error) error {
	return runTx(ctx, db, nil, fn)
}

// readTx runs fn in a deferred, query-only transaction: one snapshot that
// neither waits for a writer nor holds one up.
func readTx(ctx context.Context, db beginner, fn func(tx *sql.Tx) error) error {
	return runTx(ctx, db, &sql.TxOptions{ReadOnly: true}, fn)
}

func runTx(ctx context.Context, db beginner, opts *sql.TxOptions, fn func(tx *sql.Tx) error) (err error) {
	tx, err := db.BeginTx(ctx, opts)
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
