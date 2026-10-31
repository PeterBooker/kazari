package telemetry

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	collogpb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
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
