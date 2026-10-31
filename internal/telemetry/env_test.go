package telemetry

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/PeterBooker/kazari/internal/instrument"
)

// export runs a root span and a counter through tel and shuts it down.
func export(t *testing.T, tel *Telemetry) {
	t.Helper()
	p := tel.Providers()
	_, span := p.Tracer("test").Start(context.Background(), "GET /v1/artifacts/{slug}",
		trace.WithAttributes(instrument.Surface.String(instrument.SurfaceAPI)))
	span.End()
	c, _ := p.Meter("test").Int64Counter("test.requests")
	c.Add(context.Background(), 1)
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func resourceOf(t *testing.T, r received) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, rm := range decodeMetrics(t, r.body).ResourceMetrics {
		for _, kv := range rm.GetResource().GetAttributes() {
			out[kv.Key] = kv.GetValue().GetStringValue()
		}
	}
	return out
}

func spanCount(t *testing.T, rs []received) int {
	t.Helper()
	n := 0
	for _, r := range rs {
		if strings.HasSuffix(r.path, "traces") {
			for _, s := range decodeTraces(t, r.body).ResourceSpans {
				for _, ss := range s.ScopeSpans {
					n += len(ss.Spans)
				}
			}
		}
	}
	return n
}

func byPath(rs []received, suffix string) (received, bool) {
	for _, r := range rs {
		if strings.HasSuffix(r.path, suffix) {
			return r, true
		}
	}
	return received{}, false
}

// otelEnv is an operator's environment: an endpoint, headers, compression,
// resource attributes and a sampler that drops every trace.
func otelEnv(t *testing.T, endpoint string) {
	t.Helper()
	for k, v := range map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT":    endpoint,
		"OTEL_EXPORTER_OTLP_HEADERS":     "x-from-env=1",
		"OTEL_EXPORTER_OTLP_COMPRESSION": "gzip",
		"OTEL_SERVICE_NAME":              "from-env",
		"OTEL_RESOURCE_ATTRIBUTES":       "deployment.environment.name=staging,k8s.pod.name=web-1",
		"OTEL_TRACES_SAMPLER":            "always_off",
	} {
		t.Setenv(k, v)
	}
}

func TestOTELVariablesApplyWhenUnset(t *testing.T) {
	recv := &httpReceiver{}
	srv := httptest.NewServer(recv)
	defer srv.Close()
	otelEnv(t, srv.URL+"/env")
	export(t, newTelemetry(t, Options{Traces: Traces{OTLP: true}, Metrics: Metrics{OTLP: true}}))

	got := recv.requests()
	m, ok := byPath(got, "/env/v1/metrics")
	if !ok {
		t.Fatalf("no metrics at the environment's endpoint: %v", paths(got))
	}
	if m.header.Get("X-From-Env") != "1" || m.encoding != "gzip" {
		t.Errorf("headers %v, encoding %q; want the environment's", m.header, m.encoding)
	}
	res := resourceOf(t, m)
	if res["service.name"] != "from-env" || res["deployment.environment.name"] != "staging" || res["k8s.pod.name"] != "web-1" {
		t.Errorf("resource = %v, want the environment's attributes", res)
	}
	if n := spanCount(t, got); n != 0 {
		t.Errorf("spans = %d, want none under OTEL_TRACES_SAMPLER=always_off", n)
	}
}

func TestKazariKeysWin(t *testing.T) {
	envRecv, kzRecv := &httpReceiver{}, &httpReceiver{}
	envSrv, kzSrv := httptest.NewServer(envRecv), httptest.NewServer(kzRecv)
	defer envSrv.Close()
	defer kzSrv.Close()
	otelEnv(t, envSrv.URL+"/env")
	export(t, newTelemetry(t, Options{
		ServiceName: "kazari-prod",
		Environment: "production",
		OTLP: OTLP{
			Endpoint: kzSrv.URL + "/kz", Protocol: ProtocolHTTP, Compression: "none",
			Headers: map[string]string{"x-kazari": "1"},
		},
		Traces:  Traces{OTLP: true, SampleRatio: ratio(1)},
		Metrics: Metrics{OTLP: true},
	}))

	if got := envRecv.requests(); len(got) != 0 {
		t.Errorf("the environment's endpoint got %v", paths(got))
	}
	got := kzRecv.requests()
	m, ok := byPath(got, "/kz/v1/metrics")
	if !ok {
		t.Fatalf("no metrics at Kazari's endpoint: %v", paths(got))
	}
	if m.header.Get("X-Kazari") != "1" || m.header.Get("X-From-Env") != "" || m.encoding != "" {
		t.Errorf("headers %v, encoding %q; want Kazari's", m.header, m.encoding)
	}
	res := resourceOf(t, m)
	if res["service.name"] != "kazari-prod" || res["deployment.environment.name"] != "production" {
		t.Errorf("resource = %v, want Kazari's keys", res)
	}
	if res["k8s.pod.name"] != "web-1" {
		t.Errorf("resource = %v, want the environment's attributes Kazari has no key for", res)
	}
	if n := spanCount(t, got); n != 1 {
		t.Errorf("spans = %d, want 1: Kazari's ratio wins over OTEL_TRACES_SAMPLER", n)
	}
}

func TestOTELSignalEndpointAndProtocol(t *testing.T) {
	recv := &grpcReceiver{}
	addr := startGRPC(t, recv)
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://"+addr)
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "grpc")
	export(t, newTelemetry(t, Options{Traces: Traces{OTLP: true}}))
	recv.mu.Lock()
	defer recv.mu.Unlock()
	if len(recv.traces) != 1 {
		t.Errorf("gRPC trace exports = %d, want 1", len(recv.traces))
	}
}

func TestDefaultServiceName(t *testing.T) {
	recv := &httpReceiver{}
	srv := httptest.NewServer(recv)
	defer srv.Close()
	export(t, newTelemetry(t, Options{OTLP: OTLP{Endpoint: srv.URL}, Metrics: Metrics{OTLP: true}}))
	m, ok := byPath(recv.requests(), "/v1/metrics")
	if !ok {
		t.Fatal("no metrics")
	}
	if got := resourceOf(t, m)["service.name"]; got != "kazari" {
		t.Errorf("service.name = %q, want kazari", got)
	}
}

func TestBadOTELEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "collector:4318")
	_, err := New(context.Background(), Options{Traces: Traces{OTLP: true}, Logger: slog.New(slog.DiscardHandler)})
	if err == nil || !strings.Contains(err.Error(), "OTEL_EXPORTER_OTLP_ENDPOINT must be an http or https URL") {
		t.Errorf("err = %v", err)
	}
}

func TestWarnings(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		o    Options
		want []string
	}{
		{"defaults", nil, Options{Traces: Traces{OTLP: true}}, nil},
		{"metrics without endpoint", nil, Options{Metrics: Metrics{OTLP: true}}, []string{"telemetry.metrics.otlp is true"}},
		{"logs without endpoint", nil, Options{Logs: Logs{OTLP: true}}, []string{"telemetry.logs.otlp is true"}},
		{"metrics with an environment endpoint", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318"},
			Options{Metrics: Metrics{OTLP: true}}, nil},
		{"public plaintext", nil, Options{OTLP: OTLP{Endpoint: "http://otlp.example.com:4318"}, Traces: Traces{OTLP: true}},
			[]string{"otlp.example.com unencrypted"}},
		{"public plaintext ipv6", nil, Options{OTLP: OTLP{Endpoint: "http://[2001:db8::1]:4318"}, Traces: Traces{OTLP: true}},
			[]string{"2001:db8::1 unencrypted"}},
		{"plaintext from the environment", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://otlp.example.com"},
			Options{Traces: Traces{OTLP: true}}, []string{"OTEL_EXPORTER_OTLP_ENDPOINT sends"}},
		{"https", nil, Options{OTLP: OTLP{Endpoint: "https://otlp.example.com"}, Traces: Traces{OTLP: true}}, nil},
		{"compose service", nil, Options{OTLP: OTLP{Endpoint: "http://otel-collector:4318"}, Traces: Traces{OTLP: true}}, nil},
		{"loopback", nil, Options{OTLP: OTLP{Endpoint: "http://127.0.0.1:4318"}, Traces: Traces{OTLP: true}}, nil},
		{"private", nil, Options{OTLP: OTLP{Endpoint: "http://10.1.2.3:4318"}, Traces: Traces{OTLP: true}}, nil},
		{"localhost", nil, Options{OTLP: OTLP{Endpoint: "http://localhost:4318"}, Traces: Traces{OTLP: true}}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			got := Warnings(tt.o)
			if len(got) != len(tt.want) {
				t.Fatalf("Warnings = %q, want %d", got, len(tt.want))
			}
			for i, w := range tt.want {
				if !strings.Contains(got[i], w) {
					t.Errorf("warning %q does not contain %q", got[i], w)
				}
			}
		})
	}
}

func paths(rs []received) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.path)
	}
	return out
}
