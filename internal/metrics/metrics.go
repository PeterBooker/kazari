// Package metrics exposes Prometheus metrics for the HTTP surfaces and the
// scheduler. One Metrics value owns its registry so tests never collide.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds the collectors.
type Metrics struct {
	reg      *prometheus.Registry
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	inflight *prometheus.GaugeVec
	jobRuns  *prometheus.CounterVec
	jobTime  *prometheus.HistogramVec
}

// New registers the collectors on a fresh registry, including the Go and
// process collectors.
func New() *Metrics {
	m := &Metrics{
		reg: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "agalma_http_requests_total",
			Help: "HTTP requests by surface, method and status.",
		}, []string{"surface", "method", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "agalma_http_request_duration_seconds",
			Help:    "HTTP request duration by surface.",
			Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}, []string{"surface"}),
		inflight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "agalma_http_requests_in_flight",
			Help: "HTTP requests currently being served by surface.",
		}, []string{"surface"}),
		jobRuns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "agalma_job_runs_total",
			Help: "Scheduler job runs by job and result.",
		}, []string{"job", "result"}),
		jobTime: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "agalma_job_duration_seconds",
			Help:    "Scheduler job duration by job.",
			Buckets: prometheus.ExponentialBuckets(0.01, 4, 8),
		}, []string{"job"}),
	}
	m.reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.requests, m.duration, m.inflight, m.jobRuns, m.jobTime,
	)
	return m
}

// Handler serves the registry in the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// Registry exposes the registry for additional collectors.
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }

// HTTP returns a middleware that records requests under the given surface
// name ("site", "api", "mcp", "admin").
func (m *Metrics) HTTP(surface string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			m.inflight.WithLabelValues(surface).Inc()
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			defer func() {
				m.inflight.WithLabelValues(surface).Dec()
				m.requests.WithLabelValues(surface, r.Method, strconv.Itoa(sw.status)).Inc()
				m.duration.WithLabelValues(surface).Observe(time.Since(start).Seconds())
			}()
			next.ServeHTTP(sw, r)
		})
	}
}

// Job wraps a scheduler job function to record runs and durations.
func (m *Metrics) Job(name string, run func() error) func() error {
	return func() error {
		start := time.Now()
		err := run()
		result := "ok"
		if err != nil {
			result = "error"
		}
		m.jobRuns.WithLabelValues(name, result).Inc()
		m.jobTime.WithLabelValues(name).Observe(time.Since(start).Seconds())
		return err
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status = code
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

// Flush supports streaming handlers.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
