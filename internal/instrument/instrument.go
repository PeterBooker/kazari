// Package instrument hands packages their OpenTelemetry meter and tracer
// through Options, falling back to no-ops, and holds the helpers and
// attribute keys Kazari's instrumentation shares. It depends on the
// OpenTelemetry API and semantic conventions only, never on the SDK.
package instrument

import (
	"context"
	"slices"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// Providers are the meter and tracer providers a package's Options carry.
// A nil provider records nothing: the zero value is telemetry off.
type Providers struct {
	TracerProvider trace.TracerProvider
	MeterProvider  metric.MeterProvider
}

// Meter returns the meter for the instrumentation scope name, the import
// path of the package that records.
func (p Providers) Meter(name string) metric.Meter {
	if p.MeterProvider == nil {
		return metricnoop.Meter{}
	}
	return p.MeterProvider.Meter(name)
}

// Tracer returns the tracer for the instrumentation scope name.
func (p Providers) Tracer(name string) trace.Tracer {
	if p.TracerProvider == nil {
		return tracenoop.Tracer{}
	}
	return p.TracerProvider.Tracer(name)
}

// Tracing reports whether spans are recorded at all, so a caller can skip
// the work of building their attributes.
func (p Providers) Tracing() bool { return p.TracerProvider != nil }

// StartChild starts a span only under a recording parent; otherwise it
// returns the parent unchanged, without running the sampler or allocating.
// Everything below a root span starts this way, so an unsampled request
// costs one sampling decision however deep it goes.
func StartChild(ctx context.Context, t trace.Tracer, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	parent := trace.SpanFromContext(ctx)
	if !parent.IsRecording() {
		return ctx, parent
	}
	return t.Start(ctx, name, opts...)
}

// End ends span, marking it failed when err is set, with error.type set to
// code, Kazari's error code. Only internal errors become exception events:
// the others are messages meant for the caller, already counted by code.
func End(span trace.Span, err error, code string) {
	if err != nil && span.IsRecording() {
		if code == "" {
			code = CodeInternal
		}
		span.SetAttributes(semconv.ErrorTypeKey.String(code))
		span.SetStatus(codes.Error, code)
		if code == CodeInternal {
			span.RecordError(err)
		}
	}
	span.End()
}

// CodeInternal is the error code of every failure that is not the caller's.
const CodeInternal = "internal"

// Other is the value a bounded attribute takes when its input is outside the
// known set, as for http.request.method and error.type.
const Other = "_OTHER"

// OneOf returns v if it is one of allowed, else Other, so a client-chosen
// value never creates a new series.
func OneOf(v string, allowed ...string) string {
	if slices.Contains(allowed, v) {
		return v
	}
	return Other
}

// Handle passes on an instrument whose registration failed: the API returns
// a working no-op with the error, which goes to the SDK's error handler.
func Handle[T any](inst T, err error) T {
	if err != nil {
		otel.Handle(err)
	}
	return inst
}

// Attribute keys of Kazari's own telemetry.
const (
	Surface        = attribute.Key("kazari.surface")
	RequestID      = attribute.Key("kazari.request.id")
	ErrorCode      = attribute.Key("kazari.error.code")
	CredentialType = attribute.Key("kazari.credential.type")
	OAuthClientID  = attribute.Key("kazari.oauth.client.id")
	AuthResult     = attribute.Key("kazari.auth.result")
	RateLimitScope = attribute.Key("kazari.rate_limit.scope")
	JobName        = attribute.Key("kazari.job.name")
)

// Surfaces, the values of kazari.surface.
const (
	SurfaceSite     = "site"
	SurfaceAPI      = "api"
	SurfaceMCP      = "mcp"
	SurfaceAdmin    = "admin"
	SurfaceApex     = "apex"
	SurfaceRedirect = "redirect"
)
