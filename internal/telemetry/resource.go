package telemetry

import (
	"crypto/sha1"
	"encoding/hex"
	"os"
	"runtime"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
)

// instanceNamespace is the UUID namespace of service.instance.id, fixed so
// the id of a host stays the same across restarts and releases.
var instanceNamespace = [16]byte{0x6b, 0x61, 0x7a, 0x61, 0x72, 0x69, 0x4f, 0x8e, 0xa1, 0x5d, 0x2c, 0x7f, 0x9b, 0x30, 0xe4, 0x51}

// newResource never detects: resource.Default and the environment detector
// read OTEL_RESOURCE_ATTRIBUTES and OTEL_SERVICE_NAME.
func newResource(o Options) *resource.Resource {
	host := o.HostName
	if host == "" {
		host, _ = os.Hostname()
	}
	attrs := []attribute.KeyValue{
		semconv.ServiceName(o.ServiceName),
		semconv.ServiceVersion(o.ServiceVersion),
		semconv.ServiceInstanceID(instanceID(host)),
		semconv.HostName(host),
		semconv.ProcessRuntimeName("go"),
		semconv.ProcessRuntimeVersion(runtime.Version()),
		semconv.TelemetrySDKName("opentelemetry"),
		semconv.TelemetrySDKLanguageGo,
		semconv.TelemetrySDKVersion(sdk.Version()),
	}
	if o.Environment != "" {
		attrs = append(attrs, semconv.DeploymentEnvironmentName(o.Environment))
	}
	return resource.NewWithAttributes(semconv.SchemaURL, attrs...)
}

// instanceID is a name-based UUID, which the semantic conventions recommend
// when the instance has an inherent name.
func instanceID(host string) string { return uuid5(instanceNamespace, host) }

// uuid5 is a version 5 UUID (RFC 9562).
func uuid5(namespace [16]byte, name string) string {
	h := sha1.New()
	h.Write(namespace[:])
	h.Write([]byte(name))
	var u [16]byte
	copy(u[:], h.Sum(nil))
	u[6] = u[6]&0x0f | 0x50
	u[8] = u[8]&0x3f | 0x80
	var b [36]byte
	hex.Encode(b[0:8], u[0:4])
	b[8] = '-'
	hex.Encode(b[9:13], u[4:6])
	b[13] = '-'
	hex.Encode(b[14:18], u[6:8])
	b[18] = '-'
	hex.Encode(b[19:23], u[8:10])
	b[23] = '-'
	hex.Encode(b[24:], u[10:])
	return string(b[:])
}
