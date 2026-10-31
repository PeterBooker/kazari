package app_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/PeterBooker/kazari/internal/app"
)

// spanSink keeps what it exported after Shutdown, which the in-memory
// exporter of tracetest forgets; the app shuts telemetry down on exit.
type spanSink struct{ *tracetest.InMemoryExporter }

func (spanSink) Shutdown(context.Context) error { return nil }

// startTraced runs an app that exports spans to the returned sink, which is
// complete once stop returns.
func startTraced(t *testing.T) (a *app.App, sink spanSink, stop func()) {
	t.Helper()
	sink = spanSink{tracetest.NewInMemoryExporter()}
	clk := &clock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	ctx, cancel := context.WithCancel(context.Background())
	a, err := app.New(ctx, testConfig(t), app.Options{Now: clk.Now, Logger: testLogger(), SpanExporter: sink})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	select {
	case <-a.Started():
	case <-time.After(10 * time.Second):
		t.Fatal("not started")
	}
	return a, sink, func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	}
}

func spansByName(sink spanSink) map[string]tracetest.SpanStub {
	out := map[string]tracetest.SpanStub{}
	for _, s := range sink.GetSpans() {
		out[s.Name] = s
	}
	return out
}

func stubAttrs(s tracetest.SpanStub) map[string]string {
	out := map[string]string{}
	for _, kv := range s.Attributes {
		out[string(kv.Key)] = kv.Value.String()
	}
	return out
}

func TestStartAndRequestSpans(t *testing.T) {
	a, sink, stop := startTraced(t)
	hc := client(a.Addrs().HTTP)
	wantStatus(t, do(t, hc, http.MethodGet, "http://admin.localhost/v1/users", adminToken, ""), http.StatusOK, "admin users")
	wantStatus(t, get(t, hc, "http://admin.localhost/health"), http.StatusOK, "admin health")
	stop()

	spans := spansByName(sink)
	start, migrate := spans["kazari.start"], spans["sqlite.migrate"]
	if !start.SpanContext.IsValid() || migrate.Parent.SpanID() != start.SpanContext.SpanID() {
		t.Errorf("sqlite.migrate is not a child of kazari.start: %v", sink.GetSpans())
	}
	users, ok := spans["GET /v1/users"]
	if !ok {
		t.Fatalf("no server span for GET /v1/users: %v", names(sink))
	}
	attrs := stubAttrs(users)
	if attrs["kazari.surface"] != "admin" || attrs["kazari.credential.type"] != "admin_token" || attrs["http.route"] != "/v1/users" {
		t.Errorf("attributes = %v", attrs)
	}
	if _, ok := spans["GET /health"]; ok {
		t.Error("/health was traced")
	}
	if _, ok := spans["GET"]; ok {
		t.Error("an unrouted span was recorded")
	}
}

func names(sink spanSink) []string {
	var out []string
	for _, s := range sink.GetSpans() {
		out = append(out, s.Name)
	}
	return out
}
