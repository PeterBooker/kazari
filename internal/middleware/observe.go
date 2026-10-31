package middleware

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/semconv/v1.40.0/httpconv"

	"github.com/PeterBooker/kazari/internal/instrument"
)

const scope = "github.com/PeterBooker/kazari/internal/middleware"

// Observer records the HTTP server metrics of every surface.
type Observer struct {
	duration  metric.Float64Histogram
	active    metric.Int64UpDownCounter
	size      metric.Int64Histogram
	proxyMode bool
}

// NewObserver creates the instruments once; in proxy mode it takes the
// scheme from X-Forwarded-Proto.
func NewObserver(p instrument.Providers, proxyMode bool) *Observer {
	m := p.Meter(scope)
	return &Observer{
		duration: instrument.Handle(httpconv.NewServerRequestDuration(m,
			metric.WithExplicitBucketBoundaries(0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10))).Inst(),
		active: instrument.Handle(httpconv.NewServerActiveRequests(m)).Inst(),
		size: instrument.Handle(httpconv.NewServerResponseBodySize(m,
			metric.WithExplicitBucketBoundaries(sizeBuckets...))).Inst(),
		proxyMode: proxyMode,
	}
}

var sizeBuckets = []float64{256, 1 << 10, 4 << 10, 16 << 10, 64 << 10, 256 << 10, 1 << 20, 4 << 20, 16 << 20, 64 << 20}

// Observe returns the middleware that records requests on surface. It goes
// outside Logger and Recover, so the 500 Recover writes is what it records.
func (o *Observer) Observe(surface string) func(http.Handler) http.Handler {
	s := &surfaceObserver{
		Observer: o,
		surface:  surface,
		requests: map[requestKey]attrs{},
		actives:  map[activeKey]attrs{},
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.serve(next, w, r) })
	}
}

type requestKey struct {
	route, method, scheme, proto string
	status                       int
}

type activeKey struct{ method, scheme string }

// surfaceObserver caches attribute sets, so a request builds none: every
// part of a key comes from a bounded set.
type surfaceObserver struct {
	*Observer
	surface string

	mu       sync.RWMutex
	requests map[requestKey]attrs
	actives  map[activeKey]attrs
}

// attrs holds one attribute set as the option slices the instruments take.
// Passing a slice the cache owns, instead of building the option on each
// call, records without allocating.
type attrs struct {
	record []metric.RecordOption
	add    []metric.AddOption
}

func newAttrs(kv ...attribute.KeyValue) attrs {
	opt := metric.WithAttributeSet(attribute.NewSet(kv...))
	return attrs{record: []metric.RecordOption{opt}, add: []metric.AddOption{opt}}
}

func (s *surfaceObserver) serve(next http.Handler, w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	ctx := r.Context()
	method := knownMethod(r.Method)
	scheme := s.scheme(r)
	active := s.activeAttrs(activeKey{method, scheme})
	s.active.Add(ctx, 1, active.add...)

	st := &requestState{responseWriter: responseWriter{ResponseWriter: w}, surface: s.surface}
	r = r.WithContext(context.WithValue(ctx, stateKey{}, st))
	defer func() {
		req := s.requestAttrs(requestKey{
			route:  st.route,
			method: method,
			scheme: scheme,
			proto:  protocolVersion(r),
			status: st.Status(),
		})
		s.duration.Record(ctx, time.Since(start).Seconds(), req.record...)
		s.size.Record(ctx, st.bytes, req.record...)
		s.active.Add(ctx, -1, active.add...)
	}()
	next.ServeHTTP(&st.responseWriter, r)
}

func (s *surfaceObserver) scheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if s.proxyMode && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		return "https"
	}
	return "http"
}

func (s *surfaceObserver) activeAttrs(k activeKey) attrs {
	s.mu.RLock()
	a, ok := s.actives[k]
	s.mu.RUnlock()
	if ok {
		return a
	}
	a = newAttrs(
		semconv.HTTPRequestMethodKey.String(k.method),
		semconv.URLScheme(k.scheme),
		instrument.Surface.String(s.surface),
	)
	s.mu.Lock()
	s.actives[k] = a
	s.mu.Unlock()
	return a
}

func (s *surfaceObserver) requestAttrs(k requestKey) attrs {
	s.mu.RLock()
	a, ok := s.requests[k]
	s.mu.RUnlock()
	if ok {
		return a
	}
	kv := []attribute.KeyValue{
		semconv.HTTPRequestMethodKey.String(k.method),
		semconv.URLScheme(k.scheme),
		semconv.HTTPResponseStatusCode(k.status),
		semconv.NetworkProtocolVersion(k.proto),
		instrument.Surface.String(s.surface),
	}
	if k.route != "" {
		kv = append(kv, semconv.HTTPRoute(k.route))
	}
	if k.status >= http.StatusInternalServerError {
		kv = append(kv, semconv.ErrorTypeKey.String(strconv.Itoa(k.status)))
	}
	a = newAttrs(kv...)
	s.mu.Lock()
	s.requests[k] = a
	s.mu.Unlock()
	return a
}

// knownMethod bounds http.request.method: net/http accepts any token as a
// method, and each new one would be a series that is never freed.
func knownMethod(m string) string {
	switch m {
	case http.MethodConnect, http.MethodDelete, http.MethodGet, http.MethodHead, http.MethodOptions,
		http.MethodPatch, http.MethodPost, http.MethodPut, "QUERY", http.MethodTrace:
		return m
	default:
		return instrument.Other
	}
}

func protocolVersion(r *http.Request) string {
	switch {
	case r.ProtoMajor == 1 && r.ProtoMinor == 1:
		return "1.1"
	case r.ProtoMajor == 1 && r.ProtoMinor == 0:
		return "1.0"
	case r.ProtoMajor == 2:
		return "2"
	case r.ProtoMajor == 3:
		return "3"
	default:
		return instrument.Other
	}
}

type stateKey struct{}

// requestState is the per-request state Observe shares with the handlers
// inside it. The mux sets r.Pattern on a copy of the request that no outer
// middleware sees, so the innermost router reports its match here.
type requestState struct {
	responseWriter
	surface string
	route   string
}

func stateFrom(ctx context.Context) *requestState {
	st, _ := ctx.Value(stateKey{}).(*requestState)
	return st
}

// SetRoute records the mux pattern that matched, without its method; the
// last call wins, so a nested mux overrides the one around it. An empty
// pattern means no match.
func SetRoute(ctx context.Context, pattern string) {
	if st := stateFrom(ctx); st != nil {
		st.route = routeOf(pattern)
	}
}

// Routed reports mux's match to SetRoute before serving.
func Routed(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pattern := mux.Handler(r)
		SetRoute(r.Context(), pattern)
		mux.ServeHTTP(w, r)
	})
}

// SurfaceFrom returns the surface Observe serves the request on, or "".
func SurfaceFrom(ctx context.Context) string {
	if st := stateFrom(ctx); st != nil {
		return st.surface
	}
	return ""
}

// routeOf strips the method and host from a ServeMux pattern
// ("GET api.example.com/v1/artifacts/{slug}" is /v1/artifacts/{slug}).
func routeOf(pattern string) string {
	if i := strings.IndexAny(pattern, " \t"); i >= 0 {
		pattern = strings.TrimLeft(pattern[i:], " \t")
	}
	if i := strings.IndexByte(pattern, '/'); i > 0 {
		pattern = pattern[i:]
	}
	return pattern
}
