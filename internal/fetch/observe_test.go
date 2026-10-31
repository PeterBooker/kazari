package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/PeterBooker/kazari/internal/instrument"
)

func observed(t *testing.T) (*Observer, *tracetest.SpanRecorder, *sdkmetric.ManualReader, *sdktrace.TracerProvider) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()); _ = mp.Shutdown(context.Background()) })
	return NewObserver(instrument.Providers{TracerProvider: tp, MeterProvider: mp}), rec, reader, tp
}

func durations(t *testing.T, r *sdkmetric.ManualReader) map[string]uint64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]uint64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "kazari.fetch.duration" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Histogram[float64]).DataPoints {
				purpose, _ := dp.Attributes.Value(purposeKey)
				errType, _ := dp.Attributes.Value("error.type")
				out[purpose.AsString()+"/"+errType.AsString()] += dp.Count
			}
		}
	}
	return out
}

func TestFetchSpans(t *testing.T) {
	var sawTraceparent bool
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		sawTraceparent = sawTraceparent || r.Header.Get("Traceparent") != ""
		http.Redirect(w, r, "/final?sig=abc123", http.StatusFound)
	})
	mux.HandleFunc("/final", func(w http.ResponseWriter, r *http.Request) {
		sawTraceparent = sawTraceparent || r.Header.Get("Traceparent") != ""
		_, _ = w.Write([]byte("hello"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	obs, rec, reader, tp := observed(t)
	c := New(Options{Transport: http.DefaultTransport, Dir: t.TempDir(), Observer: obs})
	ctx, parent := tp.Tracer("test").Start(WithPurpose(context.Background(), PurposeImport), "artifact.import")
	if _, _, err := c.Get(ctx, srv.URL+"/start?token=s3cret", 100); err != nil {
		t.Fatal(err)
	}
	parent.End()

	var gets []sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		if s.Name() == "GET" {
			gets = append(gets, s)
		}
	}
	if len(gets) != 2 {
		t.Fatalf("client spans = %d, want one per attempt", len(gets))
	}
	first, second := attrs(gets[0]), attrs(gets[1])
	if first["url.full"] != srv.URL+"/start?token=REDACTED" || first["http.response.status_code"] != "302" {
		t.Errorf("first attempt = %v", first)
	}
	if second["http.request.resend_count"] != "1" || !strings.HasSuffix(second["url.full"], "/final?sig=REDACTED") {
		t.Errorf("redirect hop = %v", second)
	}
	if first["kazari.fetch.purpose"] != "import" || first["server.address"] != "127.0.0.1" {
		t.Errorf("attributes = %v", first)
	}
	if sawTraceparent {
		t.Error("the fetch sent a traceparent")
	}
	if got := durations(t, reader); got["import/"] != 1 || len(got) != 1 {
		t.Errorf("durations = %v, want one import fetch for the whole redirect chain", got)
	}
}

func TestFetchErrorTypes(t *testing.T) {
	missing := httptest.NewServer(http.NotFoundHandler())
	defer missing.Close()
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 100)))
	}))
	defer big.Close()

	obs, _, reader, _ := observed(t)
	loopback := New(Options{Transport: http.DefaultTransport, Dir: t.TempDir(), Observer: obs})
	public := New(Options{Dir: t.TempDir(), Observer: obs})
	ctx := WithPurpose(context.Background(), PurposeFile)
	_, _, _ = loopback.Get(ctx, missing.URL, 10)
	_, _, _ = loopback.Get(ctx, big.URL, 10)
	_, _, _ = public.Get(ctx, big.URL, 1000)
	_, _, _ = loopback.Get(ctx, "ftp://example.com/", 10)

	want := map[string]uint64{"file/404": 1, "file/too_large": 1, "file/refused": 1}
	got := durations(t, reader)
	if len(got) != len(want) {
		t.Errorf("durations = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %d, want %d (all %v)", k, got[k], v, got)
		}
	}
}

func TestRedactURL(t *testing.T) {
	u := mustParse(t, "https://ann:pw@cdn.example.com/a.png?X-Amz-Signature=abc&v=2#frag")
	if got := redactURL(u); got != "https://REDACTED:REDACTED@cdn.example.com/a.png?X-Amz-Signature=REDACTED&v=REDACTED" {
		t.Errorf("redactURL = %q", got)
	}
}

func attrs(s sdktrace.ReadOnlySpan) map[string]string {
	out := map[string]string{}
	for _, kv := range s.Attributes() {
		out[string(kv.Key)] = kv.Value.String()
	}
	return out
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
