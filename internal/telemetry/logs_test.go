package telemetry

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	collogpb "go.opentelemetry.io/proto/otlp/collector/logs/v1"

	"github.com/PeterBooker/kazari/internal/middleware"
)

func jsonLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestLoggerUnwrappedWhenOff(t *testing.T) {
	var buf bytes.Buffer
	l := jsonLogger(&buf)
	if got := newTelemetry(t, Options{Metrics: Metrics{Prometheus: true}}).Logger(l); got != l {
		t.Error("Logger wrapped the logger with tracing and log export off")
	}
}

func TestLoggerCorrelates(t *testing.T) {
	var buf bytes.Buffer
	tel := newTelemetry(t, Options{SpanExporter: newSink(), Traces: Traces{SampleRatio: ratio(1)}})
	l := tel.Logger(jsonLogger(&buf)).With("component", "test")
	ctx, span := tel.Providers().Tracer("test").Start(context.Background(), "GET /")
	l.InfoContext(ctx, "inside")
	span.End()
	unsampled, never := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.NeverSample())).Tracer("test").Start(context.Background(), "x")
	l.InfoContext(unsampled, "unsampled")
	never.End()
	l.Info("outside")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %q", lines)
	}
	var in map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &in); err != nil {
		t.Fatal(err)
	}
	sc := span.SpanContext()
	if in["trace_id"] != sc.TraceID().String() || in["span_id"] != sc.SpanID().String() || in["component"] != "test" {
		t.Errorf("record = %v, want the span's ids", in)
	}
	for _, l := range lines[1:] {
		if strings.Contains(l, "trace_id") {
			t.Errorf("record without a sampled span has a trace id: %s", l)
		}
	}
}

func TestLogExport(t *testing.T) {
	recv := &httpReceiver{}
	srv := httptest.NewServer(recv)
	defer srv.Close()
	var buf bytes.Buffer
	tel := newTelemetry(t, Options{
		OTLP:   OTLP{Endpoint: srv.URL, Protocol: ProtocolHTTP},
		Traces: Traces{OTLP: true, SampleRatio: ratio(1)},
		Logs:   Logs{OTLP: true, Level: slog.LevelInfo},
	})
	l := tel.Logger(jsonLogger(&buf)).With("handle", "peter")
	ctx, span := tel.Providers().Tracer("test").Start(context.Background(), "GET /")
	l.InfoContext(ctx, "request", "ip", "198.51.100.7", "status", 200)
	l.InfoContext(ctx, "oauth: identity not admitted", "subject", "abc", "email", "ann@example.com", "family", 7)
	l.DebugContext(ctx, "below the export level")
	span.End()
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	for _, secret := range []string{"198.51.100.7", "ann@example.com", "peter"} {
		if !strings.Contains(buf.String(), secret) {
			t.Errorf("stderr lacks %q, which only the export drops", secret)
		}
	}
	r, ok := byPath(recv.requests(), "/v1/logs")
	if !ok {
		t.Fatalf("no log export: %v", paths(recv.requests()))
	}
	records := logRecords(decodeLogs(t, r.body))
	if len(records) != 2 {
		t.Fatalf("exported records = %v, want the two at info", records)
	}
	for _, rec := range records {
		for _, key := range identityKeys {
			if _, ok := rec.attrs[key]; ok {
				t.Errorf("record %q exports %s", rec.body, key)
			}
		}
		if rec.traceID != span.SpanContext().TraceID().String() {
			t.Errorf("record %q has trace id %q", rec.body, rec.traceID)
		}
	}
	if records[0].attrs["status"] != "200" {
		t.Errorf("record attributes = %v", records[0].attrs)
	}
}

// An exported record must not carry an identity inside the value of another
// key: stderr keeps what the export hides.
func TestLogExportHidesIdentityInValues(t *testing.T) {
	const (
		handle = "alice-handle"
		prefix = "ab12cd34"
		email  = "alice@example.com"
	)
	recv := &httpReceiver{}
	srv := httptest.NewServer(recv)
	defer srv.Close()
	var buf bytes.Buffer
	tel := newTelemetry(t, Options{
		OTLP: OTLP{Endpoint: srv.URL, Protocol: ProtocolHTTP},
		Logs: Logs{OTLP: true, Level: slog.LevelInfo},
	})
	l := tel.Logger(jsonLogger(&buf))
	admin := middleware.Logger(l)(http.NotFoundHandler())
	for _, p := range []string{
		"/v1/users/" + handle + "/keys/" + prefix,
		"/v1/grants/4242",
		"/v1/artifacts/q3-report/transfer",
	} {
		admin.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodDelete, p, nil))
	}
	cause := errors.New("database is locked")
	l.Error("httpapi: request failed", "path", "/v1/users/"+handle, "err",
		fmt.Errorf("sqlite: revoke key %q of %q: %w", prefix, handle, cause))
	l.Info("oauth: identity not admitted", "err", fmt.Errorf("provision %q: %w", email, cause))
	l.Warn("oauth: refresh token replay, revoking family", "client_id", "cli", "user", handle)
	l.With("path", "/v1/users/"+handle, "err", fmt.Errorf("get %q: %w", handle, cause), "user", handle).Info("preset")
	l.Error("job failed", "err", fmt.Errorf("sqlite: list users: %w", cause))
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	for _, kept := range []string{handle, prefix, email, "4242"} {
		if !strings.Contains(buf.String(), kept) {
			t.Errorf("stderr lacks %q, which only the export hides", kept)
		}
	}
	r, ok := byPath(recv.requests(), "/v1/logs")
	if !ok {
		t.Fatalf("no log export: %v", paths(recv.requests()))
	}
	req := decodeLogs(t, r.body)
	for _, rec := range logRecords(req) {
		for _, secret := range []string{handle, prefix, email, "4242"} {
			for k, v := range rec.attrs {
				if strings.Contains(v, secret) {
					t.Errorf("record %q exports %q in %s=%q", rec.body, secret, k, v)
				}
			}
		}
	}
	records := logRecords(req)
	for _, want := range []struct {
		i          int
		key, value string
	}{
		{0, "path", "/v1/users/REDACTED/keys/REDACTED"},
		{1, "path", "/v1/grants/REDACTED"},
		{2, "path", "/v1/artifacts/q3-report/transfer"},
		{3, "exception.message", `sqlite: revoke key "REDACTED" of "REDACTED": database is locked`},
		{7, "exception.message", "sqlite: list users: database is locked"},
	} {
		if len(records) <= want.i {
			t.Fatalf("exported records = %v", records)
		}
		if got := records[want.i].attrs[want.key]; got != want.value {
			t.Errorf("record %d %s = %q, want %q", want.i, want.key, got, want.value)
		}
	}
}

func TestLogExportGRPC(t *testing.T) {
	recv := &grpcReceiver{}
	addr := startGRPC(t, recv)
	tel := newTelemetry(t, Options{OTLP: OTLP{Endpoint: "http://" + addr, Protocol: ProtocolGRPC}, Logs: Logs{OTLP: true}})
	tel.Logger(slog.New(slog.DiscardHandler)).Info("hello")
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	recv.mu.Lock()
	defer recv.mu.Unlock()
	if len(recv.logs) != 1 || len(logRecords(recv.logs[0])) != 1 {
		t.Errorf("gRPC log exports = %v", recv.logs)
	}
}

type logRecord struct {
	body    string
	traceID string
	attrs   map[string]string
}

func logRecords(req *collogpb.ExportLogsServiceRequest) []logRecord {
	var out []logRecord
	for _, rl := range req.ResourceLogs {
		for _, sl := range rl.ScopeLogs {
			for _, lr := range sl.LogRecords {
				r := logRecord{body: lr.GetBody().GetStringValue(), traceID: hex.EncodeToString(lr.TraceId), attrs: map[string]string{}}
				for _, kv := range lr.Attributes {
					v := kv.GetValue().GetStringValue()
					if v == "" {
						v = strings.TrimPrefix(kv.GetValue().String(), "int_value:")
					}
					r.attrs[kv.Key] = v
				}
				out = append(out, r)
			}
		}
	}
	return out
}

func TestNewLogsExports(t *testing.T) {
	var buf bytes.Buffer
	newTelemetry(t, Options{
		OTLP:    OTLP{Endpoint: "http://localhost:4318"},
		Traces:  Traces{OTLP: true},
		Metrics: Metrics{Prometheus: true},
		Logs:    Logs{OTLP: true},
		Logger:  jsonLogger(&buf),
	})
	var got struct {
		Msg     string
		Traces  map[string]string
		Metrics map[string]string
		Logs    map[string]string
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("want one JSON line: %v\n%s", err, buf.String())
	}
	want := map[string]string{"endpoint": "http://localhost:4318/v1/traces", "protocol": ProtocolHTTP}
	if got.Msg != "telemetry: exporting over OTLP" || !maps.Equal(got.Traces, want) ||
		got.Logs["endpoint"] != "http://localhost:4318/v1/logs" || got.Metrics != nil {
		t.Errorf("start line = %s", buf.String())
	}

	buf.Reset()
	newTelemetry(t, Options{Metrics: Metrics{Prometheus: true}, Logger: jsonLogger(&buf)})
	if buf.Len() != 0 {
		t.Errorf("logged with nothing exported over OTLP: %s", buf.String())
	}
}
