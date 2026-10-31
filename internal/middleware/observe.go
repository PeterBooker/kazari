package middleware

import (
	"context"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/semconv/v1.40.0/httpconv"
	"go.opentelemetry.io/otel/trace"

	"github.com/PeterBooker/kazari/internal/instrument"
)

const scope = "github.com/PeterBooker/kazari/internal/middleware"

// Observer records the HTTP server metrics and spans of every surface.
type Observer struct {
	duration  metric.Float64Histogram
	active    metric.Int64UpDownCounter
	size      metric.Int64Histogram
	tracer    trace.Tracer
	tracing   bool
	proxyMode bool
}

// ObserveOptions tune Observe for one surface.
type ObserveOptions struct {
	// ControlPlane honours an incoming traceparent and records the user
	// agent. Neither applies to artifact hosts: their visitors are anonymous,
	// and a parent they sent would decide sampling.
	ControlPlane bool
	// Skip lists paths that are counted but never traced, such as /metrics,
	// which a scraper requests every few seconds.
	Skip []string
	// NoTraces counts requests without tracing any.
	NoTraces bool
}

// traceContext reads W3C traceparent and tracestate only: baggage is
// ignored, and nothing is ever injected outbound.
var traceContext = propagation.TraceContext{}

const redacted = "REDACTED"

// NewObserver creates the instruments once; in proxy mode it takes the
// scheme from X-Forwarded-Proto.
func NewObserver(p instrument.Providers, proxyMode bool) *Observer {
	m := p.Meter(scope)
	return &Observer{
		tracer:  p.Tracer(scope),
		tracing: p.Tracing(),
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
// outside Logger and Recover, so the access log carries its trace id and the
// 500 Recover writes is what it records.
func (o *Observer) Observe(surface string, opts ObserveOptions) func(http.Handler) http.Handler {
	s := &surfaceObserver{
		Observer: o,
		opts:     opts,
		traced:   o.tracing && !opts.NoTraces,
		surface:  surface,
		startOpts: []trace.SpanStartOption{
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(instrument.Surface.String(surface)),
		},
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
	opts    ObserveOptions
	traced  bool
	surface string
	// startOpts carry only what the sampler reads, so an unsampled request
	// builds no attributes.
	startOpts []trace.SpanStartOption

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

	var span trace.Span
	if s.traced && !slices.Contains(s.opts.Skip, r.URL.Path) {
		ctx, span = s.startSpan(ctx, r, method, scheme)
	}
	st := &requestState{responseWriter: responseWriter{ResponseWriter: w}, surface: s.surface}
	r = r.WithContext(context.WithValue(ctx, stateKey{}, st))
	defer func() {
		status := st.Status()
		req := s.requestAttrs(requestKey{
			route:  st.route,
			method: method,
			scheme: scheme,
			proto:  protocolVersion(r),
			status: status,
		})
		// The span's context, so a sampled request becomes its bucket's
		// exemplar.
		s.duration.Record(ctx, time.Since(start).Seconds(), req.record...)
		s.size.Record(ctx, st.bytes, req.record...)
		s.active.Add(ctx, -1, active.add...)
		if span != nil {
			endSpan(span, st, method, status)
		}
	}()
	next.ServeHTTP(&st.responseWriter, r)
}

func (s *surfaceObserver) startSpan(ctx context.Context, r *http.Request, method, scheme string) (context.Context, trace.Span) {
	if s.opts.ControlPlane {
		ctx = traceContext.Extract(ctx, propagation.HeaderCarrier(r.Header))
	}
	ctx, span := s.tracer.Start(ctx, spanName(method, ""), s.startOpts...)
	if !span.IsRecording() {
		return ctx, span
	}
	path := r.URL.Path
	if s.surface == instrument.SurfaceAdmin {
		path = redactAdminPath(path)
	}
	attrs := make([]attribute.KeyValue, 0, 12)
	attrs = append(attrs,
		semconv.HTTPRequestMethodKey.String(method),
		semconv.URLPath(path),
		semconv.URLScheme(scheme),
		semconv.NetworkProtocolVersion(protocolVersion(r)),
		instrument.RequestID.String(RequestIDFrom(ctx)),
	)
	if method == instrument.Other {
		attrs = append(attrs, semconv.HTTPRequestMethodOriginal(r.Method))
	}
	if host, port := serverAddress(r.Host, scheme); host != "" {
		attrs = append(attrs, semconv.ServerAddress(host), semconv.ServerPort(port))
	}
	if r.URL.RawQuery != "" {
		attrs = append(attrs, semconv.URLQuery(redactQuery(r.URL.RawQuery)))
	}
	if ua := r.UserAgent(); s.opts.ControlPlane && ua != "" {
		attrs = append(attrs, semconv.UserAgentOriginal(truncate(ua, maxUserAgent)))
	}
	span.SetAttributes(attrs...)
	return ctx, span
}

// endSpan follows the HTTP conventions: only a 5xx is a server error. A JSON
// error's code gives error.type, else the status code does.
func endSpan(span trace.Span, st *requestState, method string, status int) {
	defer span.End()
	if !span.IsRecording() {
		return
	}
	span.SetName(spanName(method, st.route))
	span.SetAttributes(semconv.HTTPResponseStatusCode(status))
	if st.route != "" {
		span.SetAttributes(semconv.HTTPRoute(st.route))
	}
	if st.errorCode != "" {
		span.SetAttributes(instrument.ErrorCode.String(st.errorCode))
	}
	if status >= http.StatusInternalServerError {
		errType := st.errorCode
		if errType == "" {
			errType = strconv.Itoa(status)
		}
		span.SetAttributes(semconv.ErrorTypeKey.String(errType))
		span.SetStatus(codes.Error, "")
	}
}

// spanName is "{method} {route}", or the method alone without a route; an
// unknown method is HTTP.
func spanName(method, route string) string {
	if method == instrument.Other {
		method = "HTTP"
	}
	if route == "" {
		return method
	}
	return method + " " + route
}

func serverAddress(host, scheme string) (string, int) {
	name, port, err := net.SplitHostPort(host)
	if err != nil {
		name = host
	}
	if p, err := strconv.Atoi(port); err == nil && p > 0 && p <= 65535 {
		return name, p
	}
	if scheme == "https" {
		return name, 443
	}
	return name, 80
}

// redactQuery keeps the keys and hides every value: share tokens and OAuth
// codes travel in query strings.
func redactQuery(raw string) string {
	var b strings.Builder
	b.Grow(len(raw))
	for part := range strings.SplitSeq(raw, "&") {
		if b.Len() > 0 {
			b.WriteByte('&')
		}
		key, _, hasValue := strings.Cut(part, "=")
		b.WriteString(key)
		if hasValue {
			b.WriteString("=" + redacted)
		}
	}
	return b.String()
}

// redactAdminPath hides the handles and credential ids in admin paths
// (/v1/users/{handle}, /v1/users/{handle}/keys/{prefix}, /v1/grants/{id}):
// who did what stays in the audit log.
func redactAdminPath(p string) string {
	rest, ok := strings.CutPrefix(p, "/v1/")
	if !ok {
		return p
	}
	seg := strings.Split(rest, "/")
	switch {
	case len(seg) >= 2 && seg[0] == "users":
		seg[1] = redacted
		if len(seg) >= 4 && seg[2] == "keys" {
			seg[3] = redacted
		}
	case len(seg) >= 2 && seg[0] == "grants":
		seg[1] = redacted
	default:
		return p
	}
	return "/v1/" + strings.Join(seg, "/")
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
	surface   string
	route     string
	errorCode string
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

// SetErrorCode records the code of the JSON error a handler answered with,
// for kazari.error.code and a 5xx's error.type.
func SetErrorCode(ctx context.Context, code string) {
	if st := stateFrom(ctx); st != nil {
		st.errorCode = code
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
