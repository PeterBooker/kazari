package instrument

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestStartChild(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	defer tp.Shutdown(context.Background())
	tracer := tp.Tracer("test")

	ctx, span := StartChild(context.Background(), tracer, "orphan")
	if span.IsRecording() || ctx != context.Background() {
		t.Error("StartChild without a parent made a span")
	}
	span.End()

	never := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.NeverSample()))
	unsampledCtx, unsampled := never.Tracer("test").Start(context.Background(), "root")
	if _, child := StartChild(unsampledCtx, tracer, "child"); child.IsRecording() {
		t.Error("StartChild under an unsampled parent made a span")
	}
	unsampled.End()

	parentCtx, parent := tracer.Start(context.Background(), "root")
	_, child := StartChild(parentCtx, tracer, "child")
	End(child, errors.New("disk full"), "")
	parent.End()
	spans := rec.Ended()
	if len(spans) != 2 || spans[0].Name() != "child" || spans[0].Parent().SpanID() != parent.SpanContext().SpanID() {
		t.Fatalf("spans = %v", spans)
	}
	if len(spans[0].Events()) != 1 {
		t.Errorf("an internal error recorded %d events, want the exception", len(spans[0].Events()))
	}
}

func TestEndExpectedError(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	defer tp.Shutdown(context.Background())
	_, span := tp.Tracer("test").Start(context.Background(), "x")
	End(span, errors.New("no such artifact"), "not_found")
	s := rec.Ended()[0]
	if len(s.Events()) != 0 {
		t.Error("a caller's error recorded an exception")
	}
	for _, kv := range s.Attributes() {
		if kv.Key == "error.type" && kv.Value.AsString() != "not_found" {
			t.Errorf("error.type = %s", kv.Value.AsString())
		}
	}
}

func TestOneOf(t *testing.T) {
	if OneOf("api", "api", "mcp") != "api" || OneOf("evil", "api", "mcp") != Other || OneOf("", "api") != Other {
		t.Error("OneOf does not bound its input")
	}
}

func TestRedactQuery(t *testing.T) {
	for in, want := range map[string]string{
		"token=s3cret":         "token=REDACTED",
		"a=1&b=&c":             "a=REDACTED&b=REDACTED&c",
		"code=x%20y&state=abc": "code=REDACTED&state=REDACTED",
	} {
		if got := RedactQuery(in); got != want {
			t.Errorf("RedactQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCached(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	loads := 0
	fail := false
	c := NewCached(30*time.Second, time.Second, func() time.Time { return now }, func(ctx context.Context) (int, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("load runs without a deadline")
		}
		if fail {
			return 0, errors.New("database locked")
		}
		loads++
		return loads, nil
	})
	get := func() int {
		t.Helper()
		v, err := c.Get(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if first, second := get(), get(); first != 1 || second != 1 {
		t.Fatal("Cached loaded twice within its ttl")
	}
	now = now.Add(30 * time.Second)
	fail = true
	if _, err := c.Get(context.Background()); err == nil {
		t.Fatal("a failed load returned no error")
	}
	fail = false
	if get() != 2 {
		t.Error("Cached did not load again after a failure")
	}
}

func TestMinLevel(t *testing.T) {
	h := MinLevel(slog.DiscardHandler, slog.LevelWarn)
	if h.Enabled(context.Background(), slog.LevelInfo) || !MinLevel(slog.NewTextHandler(nil, &slog.HandlerOptions{Level: slog.LevelDebug}), slog.LevelWarn).Enabled(context.Background(), slog.LevelError) {
		t.Error("MinLevel does not filter by level")
	}
}
