package metrics

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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
		`agalma_http_requests_total{method="GET",status="418",surface="api"} 1`,
		`agalma_job_runs_total{job="sweep",result="error"} 1`,
		`agalma_http_requests_in_flight{surface="api"} 0`,
		"go_goroutines",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q", want)
		}
	}
}
