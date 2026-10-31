// Package instrument hands packages their OpenTelemetry meter and tracer
// through Options, falling back to no-ops, and holds the helpers and
// attribute keys Kazari's instrumentation shares. It depends on the
// OpenTelemetry API and semantic conventions only, never on the SDK.
package instrument

import (
	"slices"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
)

// Providers are the meter and tracer providers a package's Options carry.
// The zero value records nothing.
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
	Surface     = attribute.Key("kazari.surface")
	JobName     = attribute.Key("kazari.job.name")
	SurfaceAPI  = "api"
	SurfaceMCP  = "mcp"
	SurfaceSite = "site"
)
