package telemetry

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

var testNow = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

func newTelemetry(t *testing.T, o Options) *Telemetry {
	t.Helper()
	if o.ServiceName == "" {
		o.ServiceName = "kazari"
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Metrics.Interval == 0 {
		o.Metrics.Interval = time.Hour
	}
	if o.OTLP.Timeout == 0 {
		o.OTLP.Timeout = 5 * time.Second
	}
	tel, err := New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
	return tel
}

func record(tel *Telemetry) {
	c, _ := tel.Providers().Meter("test").Int64Counter("test.requests")
	c.Add(context.Background(), 1)
}

func TestOff(t *testing.T) {
	tel := newTelemetry(t, Options{})
	if tel.MetricsHandler() != nil {
		t.Error("MetricsHandler is set with Prometheus off")
	}
	if _, ok := tel.Providers().MeterProvider.(metricnoop.MeterProvider); !ok {
		t.Errorf("MeterProvider = %T, want the no-op", tel.Providers().MeterProvider)
	}
	if tel.transport != nil {
		t.Error("transport built without an endpoint")
	}
}

func TestPrometheusOnly(t *testing.T) {
	tel := newTelemetry(t, Options{ServiceVersion: "1.2.3", Metrics: Metrics{Prometheus: true}})
	record(tel)
	rec := httptest.NewRecorder()
	tel.MetricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		"test_requests_total 1",
		`target_info{`,
		`service_version="1.2.3"`,
		"go_goroutine_count",
		"process_cpu_seconds_total",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape lacks %s", want)
		}
	}
	if strings.Contains(body, "otel_scope_") {
		t.Error("scrape has otel_scope_ labels")
	}
	if tel.transport != nil {
		t.Error("transport built without an endpoint")
	}
}

func TestOTLPHTTP(t *testing.T) {
	recv := &httpReceiver{}
	srv := httptest.NewServer(recv)
	defer srv.Close()
	tel := newTelemetry(t, Options{
		OTLP: OTLP{
			Endpoint:    srv.URL + "/otlp/",
			Protocol:    ProtocolHTTP,
			Headers:     map[string]string{"Authorization": "Bearer t0k"},
			Compression: "gzip",
		},
		Metrics: Metrics{OTLP: true},
	})
	record(tel)
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := recv.requests()
	if len(got) != 1 {
		t.Fatalf("exports = %d, want 1", len(got))
	}
	r := got[0]
	if r.path != "/otlp/v1/metrics" {
		t.Errorf("path = %q, want /otlp/v1/metrics", r.path)
	}
	if r.header.Get("Authorization") != "Bearer t0k" || r.encoding != "gzip" {
		t.Errorf("Authorization %q, Content-Encoding %q", r.header.Get("Authorization"), r.encoding)
	}
	service, names := metricNames(decodeMetrics(t, r.body))
	if service != "kazari" || !names["test.requests"] || !names["go.goroutine.count"] || !names["go.schedule.duration"] {
		t.Errorf("service %q, metrics %v", service, names)
	}
}

func TestOTLPGRPC(t *testing.T) {
	for _, compression := range []string{"gzip", "none"} {
		t.Run(compression, func(t *testing.T) {
			recv := &grpcReceiver{}
			addr := startGRPC(t, recv)
			tel := newTelemetry(t, Options{
				OTLP:    OTLP{Endpoint: "http://" + addr, Protocol: ProtocolGRPC, Headers: map[string]string{"x-team": "ops"}, Compression: compression},
				Metrics: Metrics{OTLP: true},
			})
			record(tel)
			if err := tel.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			recv.mu.Lock()
			defer recv.mu.Unlock()
			if len(recv.metrics) != 1 {
				t.Fatalf("exports = %d, want 1", len(recv.metrics))
			}
			if got := recv.md[0].Get("x-team"); len(got) != 1 || got[0] != "ops" {
				t.Errorf("x-team metadata = %q", got)
			}
			if _, names := metricNames(recv.metrics[0]); !names["test.requests"] {
				t.Errorf("metrics = %v", names)
			}
		})
	}
}

func TestOTLPGRPCTLS(t *testing.T) {
	pki := newPKI(t)
	serverCert := pki.issue(t, "collector", 0)
	recv := &grpcReceiver{}
	addr := startGRPC(t, recv, grpc.Creds(credentials.NewServerTLSFromCert(&serverCert)))
	tel := newTelemetry(t, Options{
		OTLP:    OTLP{Endpoint: "https://" + addr, Protocol: ProtocolGRPC, Compression: "gzip", CAFile: pki.caFile},
		Metrics: Metrics{OTLP: true},
	})
	record(tel)
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	recv.mu.Lock()
	defer recv.mu.Unlock()
	if len(recv.metrics) != 1 {
		t.Fatalf("exports = %d, want 1", len(recv.metrics))
	}
}

func TestOTLPMutualTLSRotation(t *testing.T) {
	pki := newPKI(t)
	recv := &httpReceiver{}
	srv := startTLS(t, pki, recv, true)
	base := time.Now().Add(-time.Hour)
	certFile, keyFile := pki.writeClient(t, "kazari-1", base)

	tr, err := newTransport(OTLP{
		Endpoint: srv.URL, Protocol: ProtocolHTTP, Timeout: 5 * time.Second,
		CAFile: pki.caFile, CertFile: certFile, KeyFile: keyFile,
	}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer tr.close()
	// A new connection per export, so each one shakes hands.
	tr.http.Transport.(recordStatus).next.(*http.Transport).DisableKeepAlives = true
	exp, err := tr.metricExporter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	export := func() {
		t.Helper()
		if err := exp.Export(context.Background(), sampleMetrics()); err != nil {
			t.Fatalf("Export: %v", err)
		}
	}
	export()
	pki.writeClient(t, "kazari-2", base.Add(time.Minute))
	export()

	got := recv.requests()
	if len(got) != 2 || got[0].clientCN != "kazari-1" || got[1].clientCN != "kazari-2" {
		t.Errorf("client certificates = %v, want kazari-1 then kazari-2", clientCNs(got))
	}
}

func TestOTLPUntrustedServer(t *testing.T) {
	recv := &httpReceiver{}
	srv := httptest.NewUnstartedServer(recv)
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	defer srv.Close()
	tr, err := newTransport(OTLP{Endpoint: srv.URL, Protocol: ProtocolHTTP, Timeout: 5 * time.Second}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer tr.close()
	exp, err := tr.metricExporter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := exp.Export(context.Background(), sampleMetrics()); err == nil {
		t.Error("export to a server outside the system roots succeeded")
	}
}

func TestOTLPNoRedirects(t *testing.T) {
	var elsewhere bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { elsewhere = true }))
	defer target.Close()
	srv := httptest.NewServer(http.RedirectHandler(target.URL+"/v1/metrics", http.StatusTemporaryRedirect))
	defer srv.Close()
	tr, err := newTransport(OTLP{Endpoint: srv.URL, Protocol: ProtocolHTTP, Timeout: 5 * time.Second,
		Headers: map[string]string{"Authorization": "Bearer t0k"}}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer tr.close()
	exp, _ := tr.metricExporter(context.Background())
	_ = exp.Export(context.Background(), sampleMetrics())
	if elsewhere {
		t.Error("the exporter followed a redirect with its headers")
	}
}

func TestSignalURL(t *testing.T) {
	for endpoint, want := range map[string]string{
		"http://collector:4318":          "http://collector:4318/v1/metrics",
		"http://collector:4318/":         "http://collector:4318/v1/metrics",
		"https://otlp.example.com/otlp":  "https://otlp.example.com/otlp/v1/metrics",
		"https://otlp.example.com/otlp/": "https://otlp.example.com/otlp/v1/metrics",
	} {
		tr, err := newTransport(OTLP{Endpoint: endpoint, Protocol: ProtocolHTTP}, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		if got := tr.signalURL("/v1/metrics"); got != want {
			t.Errorf("signalURL(%s) = %s, want %s", endpoint, got, want)
		}
	}
}

func TestExportCounts(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		errType string
	}{
		{"ok", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }, ""},
		{"unauthorized", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) }, "401"},
		{"partial success", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/x-protobuf")
			// ExportMetricsServiceResponse{partial_success: {rejected_data_points: 1}}
			_, _ = w.Write([]byte{0x0a, 0x02, 0x08, 0x01})
		}, "rejected"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()
			var logs bytes.Buffer
			log := newLimitedLog(slog.New(slog.NewTextHandler(&logs, nil)), func() time.Time { return testNow })
			tr, err := newTransport(OTLP{Endpoint: srv.URL, Protocol: ProtocolHTTP, Timeout: 5 * time.Second}, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			defer tr.close()
			inner, err := tr.metricExporter(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			reader := sdkmetric.NewManualReader()
			mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
			defer mp.Shutdown(context.Background())
			counter, _ := mp.Meter("test").Int64Counter("exported")
			exp := &metricExporter{Exporter: inner}
			exp.stats.Store(newExportStats("metrics", func(ctx context.Context, n int64, set attribute.Set) {
				counter.Add(ctx, n, metric.WithAttributeSet(set))
			}, tr, attribute.String("otel.component.type", "otlp_http_metric_exporter"), log))

			err = exp.Export(context.Background(), sampleMetrics())
			if (err != nil) != (tt.errType != "") {
				t.Fatalf("Export = %v", err)
			}
			var logged *exportError
			if err != nil && !errors.As(err, &logged) {
				t.Errorf("Export error %T is not marked as logged", err)
			}
			attrs := counted(t, reader)
			if attrs["error.type"] != tt.errType {
				t.Errorf("error.type = %q, want %q", attrs["error.type"], tt.errType)
			}
			if attrs["otel.component.name"] != "otlp_http_metric_exporter/0" || attrs["server.address"] != "127.0.0.1" || attrs["server.port"] == "" {
				t.Errorf("attributes = %v", attrs)
			}
			if (tt.errType != "") != strings.Contains(logs.String(), "telemetry: export failed") {
				t.Errorf("log = %q", logs.String())
			}
		})
	}
}

// counted returns the attributes of the one data point of "exported", which
// must count 2 (sampleMetrics' points).
func counted(t *testing.T, reader *sdkmetric.ManualReader) map[string]string {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "exported" {
				continue
			}
			pts := m.Data.(metricdata.Sum[int64]).DataPoints
			if len(pts) != 1 || pts[0].Value != 2 {
				t.Fatalf("exported points = %+v, want one of 2", pts)
			}
			out := map[string]string{}
			for _, kv := range pts[0].Attributes.ToSlice() {
				out[string(kv.Key)] = kv.Value.String()
			}
			return out
		}
	}
	t.Fatal("no exported metric")
	return nil
}

func sampleMetrics() *metricdata.ResourceMetrics {
	return &metricdata.ResourceMetrics{ScopeMetrics: []metricdata.ScopeMetrics{{
		Metrics: []metricdata.Metrics{
			{Name: "a", Data: metricdata.Sum[int64]{Temporality: metricdata.CumulativeTemporality, IsMonotonic: true,
				DataPoints: []metricdata.DataPoint[int64]{{Value: 1, StartTime: testNow, Time: testNow}}}},
			{Name: "b", Data: metricdata.Gauge[float64]{DataPoints: []metricdata.DataPoint[float64]{{Value: 1, Time: testNow}}}},
		},
	}}}
}

func TestErrorType(t *testing.T) {
	timeout := &timeoutErr{}
	tests := []struct {
		name string
		o    outcome
		err  error
		want string
	}{
		{"http status", outcome{answered: true, status: 503}, errors.New("x"), "503"},
		{"grpc code", outcome{answered: true, code: 7}, errors.New("x"), "PERMISSION_DENIED"},
		{"grpc deadline", outcome{answered: true, code: 4}, errors.New("x"), "timeout"},
		{"grpc unknown code", outcome{answered: true, code: 99}, errors.New("x"), "_OTHER"},
		{"context deadline", outcome{}, context.DeadlineExceeded, "timeout"},
		{"net timeout", outcome{}, timeout, "timeout"},
		{"partial success", outcome{answered: true, status: 200}, errors.New("x"), "rejected"},
		{"refused", outcome{}, errors.New("connection refused"), "_OTHER"},
	}
	for _, tt := range tests {
		if got := tt.o.errorType(tt.err); got != tt.want {
			t.Errorf("%s: errorType = %q, want %q", tt.name, got, tt.want)
		}
	}
}

type timeoutErr struct{}

func (*timeoutErr) Error() string   { return "i/o timeout" }
func (*timeoutErr) Timeout() bool   { return true }
func (*timeoutErr) Temporary() bool { return false }

func TestLimitedLog(t *testing.T) {
	var buf bytes.Buffer
	now := testNow
	l := newLimitedLog(slog.New(slog.NewTextHandler(&buf, nil)), func() time.Time { return now })
	l.warn("traces", "down")
	l.warn("traces", "down")
	l.warn("metrics", "down")
	now = now.Add(59 * time.Second)
	l.warn("traces", "down")
	now = now.Add(time.Second)
	l.warn("traces", "down")
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %q, want 3", lines)
	}
	if !strings.Contains(lines[2], "suppressed=2") {
		t.Errorf("third line %q does not count the 2 held back", lines[2])
	}
}

func TestErrorHandlerFollowsLatest(t *testing.T) {
	var first, second bytes.Buffer
	installErrorHandler(newLimitedLog(slog.New(slog.NewTextHandler(&first, nil)), time.Now))
	installErrorHandler(newLimitedLog(slog.New(slog.NewTextHandler(&second, nil)), time.Now))
	otel.Handle(errors.New("sdk trouble"))
	otel.Handle(&exportError{err: errors.New("already logged")})
	if first.Len() != 0 || !strings.Contains(second.String(), "sdk trouble") || strings.Contains(second.String(), "already logged") {
		t.Errorf("first %q, second %q", first.String(), second.String())
	}
}

func TestInstanceID(t *testing.T) {
	dns := [16]byte{0x6b, 0xa7, 0xb8, 0x10, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}
	if got := uuid5(dns, "www.example.com"); got != "2ed6657d-e927-568b-95e1-2665a8aea6a2" {
		t.Errorf("uuid5 = %s, want the RFC 9562 example", got)
	}
	if a, b := instanceID("web-1"), instanceID("web-1"); a != b || a == instanceID("web-2") {
		t.Error("instanceID is not a stable function of the host name")
	}
}

func TestNewFailsCleanly(t *testing.T) {
	_, err := New(context.Background(), Options{
		OTLP:    OTLP{Endpoint: "https://127.0.0.1:1", Protocol: ProtocolHTTP, CAFile: "/nonexistent/ca.pem"},
		Metrics: Metrics{OTLP: true},
		Logger:  slog.New(slog.DiscardHandler),
	})
	if err == nil {
		t.Fatal("New succeeded with a missing CA file")
	}
}

func clientCNs(rs []received) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.clientCN)
	}
	return out
}
