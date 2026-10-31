package middleware

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/PeterBooker/kazari/internal/instrument"
)

func newTestObserver(t *testing.T, proxyMode bool) (*Observer, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	return NewObserver(instrument.Providers{MeterProvider: mp}, proxyMode), reader
}

type point struct {
	attrs map[string]string
	count uint64
	sum   float64
	value int64
}

// collect returns the data points of the named metric with their
// attributes as strings.
func collect(t *testing.T, r *sdkmetric.ManualReader, name string) []point {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	attrs := func(s attribute.Set) map[string]string {
		m := map[string]string{}
		for _, kv := range s.ToSlice() {
			m[string(kv.Key)] = kv.Value.String()
		}
		return m
	}
	var out []point
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			switch d := m.Data.(type) {
			case metricdata.Histogram[float64]:
				for _, dp := range d.DataPoints {
					out = append(out, point{attrs: attrs(dp.Attributes), count: dp.Count, sum: dp.Sum})
				}
			case metricdata.Histogram[int64]:
				for _, dp := range d.DataPoints {
					out = append(out, point{attrs: attrs(dp.Attributes), count: dp.Count, sum: float64(dp.Sum)})
				}
			case metricdata.Sum[int64]:
				for _, dp := range d.DataPoints {
					out = append(out, point{attrs: attrs(dp.Attributes), value: dp.Value})
				}
			default:
				t.Fatalf("%s: unexpected data %T", name, m.Data)
			}
		}
	}
	return out
}

func TestObserveRecords(t *testing.T) {
	obs, reader := newTestObserver(t, false)
	h := obs.Observe("api")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("hello"))
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/artifacts", nil))

	got := collect(t, reader, "http.server.request.duration")
	if len(got) != 1 || got[0].count != 1 {
		t.Fatalf("duration points = %+v", got)
	}
	want := map[string]string{
		"http.request.method":       "POST",
		"http.response.status_code": "201",
		"url.scheme":                "http",
		"network.protocol.version":  "1.1",
		"kazari.surface":            "api",
	}
	if !equalMaps(got[0].attrs, want) {
		t.Errorf("attributes = %v, want %v", got[0].attrs, want)
	}
	size := collect(t, reader, "http.server.response.body.size")
	if len(size) != 1 || size[0].sum != 5 {
		t.Errorf("body size points = %+v, want one summing to 5", size)
	}
	active := collect(t, reader, "http.server.active_requests")
	if len(active) != 1 || active[0].value != 0 {
		t.Errorf("active requests = %+v, want one at 0", active)
	}
}

func TestObserveRoute(t *testing.T) {
	inner := http.NewServeMux()
	inner.HandleFunc("GET /v1/artifacts/{slug}", func(http.ResponseWriter, *http.Request) {})
	outer := http.NewServeMux()
	outer.Handle("/v1/", Routed(inner))
	outer.HandleFunc("GET /health", func(http.ResponseWriter, *http.Request) {})

	tests := []struct {
		path, route string
	}{
		{"/v1/artifacts/q3-report", "/v1/artifacts/{slug}"},
		{"/health", "/health"},
		{"/v1/nothing", ""},
		{"/elsewhere", ""},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			obs, reader := newTestObserver(t, false)
			obs.Observe("admin")(Routed(outer)).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, tt.path, nil))
			got := collect(t, reader, "http.server.request.duration")
			if len(got) != 1 {
				t.Fatalf("points = %+v", got)
			}
			route, ok := got[0].attrs["http.route"]
			if tt.route == "" && ok {
				t.Errorf("http.route = %q, want none", route)
			}
			if tt.route != "" && route != tt.route {
				t.Errorf("http.route = %q, want %q", route, tt.route)
			}
		})
	}
}

func TestRouteOf(t *testing.T) {
	for in, want := range map[string]string{
		"GET /v1/artifacts/{slug}":            "/v1/artifacts/{slug}",
		"POST\t/oauth/token":                  "/oauth/token",
		"/{path...}":                          "/{path...}",
		"api.example.com/v1/":                 "/v1/",
		"GET api.example.com/v1/users/{user}": "/v1/users/{user}",
		"":                                    "",
	} {
		if got := routeOf(in); got != want {
			t.Errorf("routeOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestObserveMethodBounded(t *testing.T) {
	obs, reader := newTestObserver(t, false)
	h := obs.Observe("site")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	for i := range 100 {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Method = "M" + strings.Repeat("X", i)
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodDelete, "/", nil))

	counts := map[string]uint64{}
	for _, p := range collect(t, reader, "http.server.request.duration") {
		counts[p.attrs["http.request.method"]] += p.count
	}
	if len(counts) != 2 || counts["_OTHER"] != 100 || counts["DELETE"] != 1 {
		t.Errorf("counts by method = %v, want _OTHER 100 and DELETE 1", counts)
	}
	if n := len(collect(t, reader, "http.server.active_requests")); n != 2 {
		t.Errorf("active request series = %d, want 2", n)
	}
}

func TestObservePanicIs500(t *testing.T) {
	obs, reader := newTestObserver(t, false)
	l, _ := newJSONLogger()
	h := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }),
		obs.Observe("api"), Logger(l), Recover(l))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	got := collect(t, reader, "http.server.request.duration")
	if len(got) != 1 || got[0].attrs["http.response.status_code"] != "500" || got[0].attrs["error.type"] != "500" {
		t.Errorf("points = %+v, want one 500 with error.type", got)
	}
	if active := collect(t, reader, "http.server.active_requests"); len(active) != 1 || active[0].value != 0 {
		t.Errorf("active requests = %+v, want 0", active)
	}
}

func TestObserveAbortStillCounted(t *testing.T) {
	obs, reader := newTestObserver(t, false)
	h := obs.Observe("site")(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }))
	func() {
		defer func() {
			if v := recover(); v != http.ErrAbortHandler {
				t.Errorf("recovered %v, want http.ErrAbortHandler", v)
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}()
	if active := collect(t, reader, "http.server.active_requests"); len(active) != 1 || active[0].value != 0 {
		t.Errorf("active requests = %+v, want 0", active)
	}
}

func TestObserveScheme(t *testing.T) {
	tests := []struct {
		name      string
		proxyMode bool
		tls       bool
		forwarded string
		want      string
	}{
		{"plain", false, false, "", "http"},
		{"tls", false, true, "", "https"},
		{"forwarded ignored without proxy mode", false, false, "https", "http"},
		{"proxy https", true, false, "https", "https"},
		{"proxy http", true, false, "http", "http"},
		{"proxy junk", true, false, "gopher", "http"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obs, reader := newTestObserver(t, tt.proxyMode)
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.tls {
				r.TLS = &tls.ConnectionState{}
			}
			if tt.forwarded != "" {
				r.Header.Set("X-Forwarded-Proto", tt.forwarded)
			}
			obs.Observe("site")(http.NotFoundHandler()).ServeHTTP(httptest.NewRecorder(), r)
			if got := collect(t, reader, "http.server.request.duration")[0].attrs["url.scheme"]; got != tt.want {
				t.Errorf("url.scheme = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestObserveWrapsOnce(t *testing.T) {
	obs, _ := newTestObserver(t, false)
	l, _ := newJSONLogger()
	var inner http.ResponseWriter
	var observed *responseWriter
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner = w
		observed = &stateFrom(r.Context()).responseWriter
	}), obs.Observe("api"), Logger(l), Recover(l))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if inner != http.ResponseWriter(observed) {
		t.Errorf("handler got %T %p, want Observe's writer %p", inner, inner, observed)
	}
}

func TestSurfaceFrom(t *testing.T) {
	obs, _ := newTestObserver(t, false)
	var got string
	obs.Observe("mcp")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = SurfaceFrom(r.Context())
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if got != "mcp" {
		t.Errorf("SurfaceFrom = %q, want mcp", got)
	}
	if s := SurfaceFrom(context.Background()); s != "" {
		t.Errorf("SurfaceFrom(empty) = %q", s)
	}
	SetRoute(context.Background(), "/ignored") // no state: must not panic
}

func equalMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
