package metrics

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestHTTPAndJob(t *testing.T) {
	m := New()
	h := m.HTTP("api")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, "x")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d", rec.Code)
	}
	if err := m.Job("sweep", func() error { return errors.New("x") })(); err == nil {
		t.Fatal("job error swallowed")
	}

	out := httptest.NewRecorder()
	m.Handler().ServeHTTP(out, httptest.NewRequest("GET", "/metrics", nil))
	body := out.Body.String()
	for _, want := range []string{
		`kazari_http_requests_total{method="GET",status="418",surface="api"} 1`,
		`kazari_job_runs_total{job="sweep",result="error"} 1`,
		`kazari_http_requests_in_flight{surface="api"} 0`,
		"go_goroutines",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q", want)
		}
	}
}

func TestHTTPMethodLabelBounded(t *testing.T) {
	m := New()
	h := m.HTTP("site")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	for i := range 100 {
		r := httptest.NewRequest("GET", "/", nil)
		r.Method = "M" + strings.Repeat("X", i)
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("DELETE", "/", nil))

	out := httptest.NewRecorder()
	m.Handler().ServeHTTP(out, httptest.NewRequest("GET", "/metrics", nil))
	var series []string
	for line := range strings.SplitSeq(out.Body.String(), "\n") {
		if strings.HasPrefix(line, "kazari_http_requests_total{") {
			series = append(series, line)
		}
	}
	want := []string{
		`kazari_http_requests_total{method="DELETE",status="405",surface="site"} 1`,
		`kazari_http_requests_total{method="other",status="405",surface="site"} 100`,
	}
	if !slices.Equal(series, want) {
		t.Errorf("series = %q, want %q", series, want)
	}
}
