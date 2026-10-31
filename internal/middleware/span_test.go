package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/PeterBooker/kazari/internal/instrument"
)

func newTracedObserver(t *testing.T) (*Observer, *tracetest.SpanRecorder) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	mp := sdkmetric.NewMeterProvider()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()); _ = mp.Shutdown(context.Background()) })
	return NewObserver(instrument.Providers{TracerProvider: tp, MeterProvider: mp}, false), rec
}

func spanAttrs(s sdktrace.ReadOnlySpan) map[string]string {
	m := map[string]string{}
	for _, kv := range s.Attributes() {
		m[string(kv.Key)] = kv.Value.String()
	}
	return m
}

func only(t *testing.T, rec *tracetest.SpanRecorder) sdktrace.ReadOnlySpan {
	t.Helper()
	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}
	return spans[0]
}

func TestServerSpan(t *testing.T) {
	obs, rec := newTracedObserver(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/artifacts/{slug}", func(w http.ResponseWriter, r *http.Request) {
		SetErrorCode(r.Context(), "not_found")
		w.WriteHeader(http.StatusNotFound)
	})
	h := Chain(Routed(mux), RequestID(), obs.Observe("api", ObserveOptions{ControlPlane: true}))
	req := httptest.NewRequest(http.MethodGet, "https://api.art.test/v1/artifacts/q3-report?token=s3cret&code=abc&flag", nil)
	req.Header.Set("User-Agent", "claude-code/2.0")
	req.Header.Set(RequestIDHeader, "req-12345678")
	req.RemoteAddr = "198.51.100.7:4242"
	h.ServeHTTP(httptest.NewRecorder(), req)

	s := only(t, rec)
	if s.Name() != "GET /v1/artifacts/{slug}" || s.SpanKind() != trace.SpanKindServer {
		t.Errorf("name %q kind %v", s.Name(), s.SpanKind())
	}
	want := map[string]string{
		"kazari.surface":            "api",
		"http.request.method":       "GET",
		"http.route":                "/v1/artifacts/{slug}",
		"http.response.status_code": "404",
		"url.path":                  "/v1/artifacts/q3-report",
		"url.query":                 "token=REDACTED&code=REDACTED&flag",
		"url.scheme":                "https",
		"server.address":            "api.art.test",
		"server.port":               "443",
		"network.protocol.version":  "1.1",
		"kazari.request.id":         "req-12345678",
		"user_agent.original":       "claude-code/2.0",
		"kazari.error.code":         "not_found",
	}
	got := spanAttrs(s)
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	for _, k := range []string{"client.address", "enduser.id", "error.type", "url.full"} {
		if _, ok := got[k]; ok {
			t.Errorf("span carries %s", k)
		}
	}
	if strings.Contains(strings.Join(attrValues(s), " "), "s3cret") || strings.Contains(strings.Join(attrValues(s), " "), "198.51.100.7") {
		t.Error("span carries a query value or the client address")
	}
	if s.Status().Code == codes.Error {
		t.Error("a 404 marked the span as an error")
	}
}

func attrValues(s sdktrace.ReadOnlySpan) []string {
	var out []string
	for _, kv := range s.Attributes() {
		out = append(out, kv.Value.String())
	}
	return out
}

func TestServerSpanErrors(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		errType string
	}{
		{"plain 502", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) }, "502"},
		{"json 500", func(w http.ResponseWriter, r *http.Request) {
			SetErrorCode(r.Context(), "internal")
			w.WriteHeader(http.StatusInternalServerError)
		}, "internal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obs, rec := newTracedObserver(t)
			obs.Observe("api", ObserveOptions{})(tt.handler).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil))
			s := only(t, rec)
			if s.Status().Code != codes.Error || spanAttrs(s)["error.type"] != tt.errType {
				t.Errorf("status %v, error.type %q; want error and %q", s.Status(), spanAttrs(s)["error.type"], tt.errType)
			}
			if s.Name() != "POST" {
				t.Errorf("name = %q, want POST without a route", s.Name())
			}
		})
	}
}

func TestServerSpanUnknownMethod(t *testing.T) {
	obs, rec := newTracedObserver(t)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Method = "BREW"
	obs.Observe("site", ObserveOptions{})(http.NotFoundHandler()).ServeHTTP(httptest.NewRecorder(), r)
	s := only(t, rec)
	got := spanAttrs(s)
	if s.Name() != "HTTP" || got["http.request.method"] != "_OTHER" || got["http.request.method_original"] != "BREW" {
		t.Errorf("name %q, attributes %v", s.Name(), got)
	}
}

func TestServerSpanPanic(t *testing.T) {
	obs, rec := newTracedObserver(t)
	l, _ := newJSONLogger()
	h := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }),
		obs.Observe("api", ObserveOptions{}), Logger(l), Recover(l))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	s := only(t, rec)
	if spanAttrs(s)["http.response.status_code"] != "500" || spanAttrs(s)["error.type"] != "internal" {
		t.Errorf("attributes = %v", spanAttrs(s))
	}
	if len(s.Events()) != 1 || s.Events()[0].Name != "exception" {
		t.Fatalf("events = %v, want one exception", s.Events())
	}
	msg := ""
	for _, kv := range s.Events()[0].Attributes {
		if kv.Key == "exception.message" {
			msg = kv.Value.AsString()
		}
	}
	if msg != "boom" {
		t.Errorf("exception.message = %q", msg)
	}
}

func TestServerSpanPropagation(t *testing.T) {
	const parent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	for _, tt := range []struct {
		name    string
		opts    ObserveOptions
		honours bool
	}{
		{"control plane", ObserveOptions{ControlPlane: true}, true},
		{"artifact host", ObserveOptions{}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			obs, rec := newTracedObserver(t)
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Header.Set("Traceparent", parent)
			r.Header.Set("User-Agent", "Mozilla/5.0")
			obs.Observe("x", tt.opts)(http.NotFoundHandler()).ServeHTTP(httptest.NewRecorder(), r)
			s := only(t, rec)
			if got := s.Parent().TraceID().String() == "4bf92f3577b34da6a3ce929d0e0e4736"; got != tt.honours {
				t.Errorf("parent %v, want honoured %v", s.Parent(), tt.honours)
			}
			if _, ok := spanAttrs(s)["user_agent.original"]; ok != tt.honours {
				t.Errorf("user agent recorded %v, want %v", ok, tt.honours)
			}
		})
	}
}

func TestServerSpanSkipsAndNoTraces(t *testing.T) {
	obs, rec := newTracedObserver(t)
	obs.Observe("admin", ObserveOptions{Skip: []string{"/metrics"}})(http.NotFoundHandler()).
		ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/metrics", nil))
	obs.Observe("redirect", ObserveOptions{NoTraces: true})(http.NotFoundHandler()).
		ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if n := len(rec.Ended()); n != 0 {
		t.Errorf("spans = %d, want none", n)
	}
}

func TestAdminPathRedacted(t *testing.T) {
	for in, want := range map[string]string{
		"/v1/users/peter":                  "/v1/users/REDACTED",
		"/v1/users/peter/keys":             "/v1/users/REDACTED/keys",
		"/v1/users/peter/keys/ab12cd34":    "/v1/users/REDACTED/keys/REDACTED",
		"/v1/users/peter/identities":       "/v1/users/REDACTED/identities",
		"/v1/grants/42":                    "/v1/grants/REDACTED",
		"/v1/users":                        "/v1/users",
		"/v1/artifacts/q3-report/transfer": "/v1/artifacts/q3-report/transfer",
		"/metrics":                         "/metrics",
	} {
		if got := redactAdminPath(in); got != want {
			t.Errorf("redactAdminPath(%q) = %q, want %q", in, got, want)
		}
	}
	obs, rec := newTracedObserver(t)
	obs.Observe(instrument.SurfaceAdmin, ObserveOptions{})(http.NotFoundHandler()).
		ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodDelete, "/v1/users/peter/keys/ab12cd34", nil))
	if got := spanAttrs(only(t, rec))["url.path"]; got != "/v1/users/REDACTED/keys/REDACTED" {
		t.Errorf("url.path = %q", got)
	}
}

func TestUnsampledRequestStillCounted(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec), sdktrace.WithSampler(sdktrace.NeverSample()))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer tp.Shutdown(context.Background())
	defer mp.Shutdown(context.Background())
	obs := NewObserver(instrument.Providers{TracerProvider: tp, MeterProvider: mp}, false)
	obs.Observe("site", ObserveOptions{})(http.NotFoundHandler()).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if len(rec.Ended()) != 0 {
		t.Error("an unsampled request was recorded")
	}
	if got := collect(t, reader, "http.server.request.duration"); len(got) != 1 {
		t.Errorf("duration points = %d, want 1", len(got))
	}
}
