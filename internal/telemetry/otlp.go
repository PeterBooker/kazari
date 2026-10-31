package telemetry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding/gzip"

	"github.com/PeterBooker/kazari/internal/certs"
)

// protocolJSON is OTLP/HTTP with JSON bodies, which only
// OTEL_EXPORTER_OTLP_PROTOCOL selects; Kazari's own key offers protobuf.
const protocolJSON = "http/json"

// defaultTimeout is the exporters' own default for one export attempt.
const defaultTimeout = 10 * time.Second

// Retries stay within the 30-second export timeout the processors and the
// reader default to.
const (
	retryInitial = 5 * time.Second
	retryMax     = 15 * time.Second
	retryElapsed = 30 * time.Second
)

// Signals, as the OTLP exporter variables name them.
const (
	signalTraces  = "TRACES"
	signalMetrics = "METRICS"
	signalLogs    = "LOGS"
)

// endpoint is where one signal exports and how. Kazari builds the HTTP
// client or gRPC connection itself, to record the collector's answer for
// error.type, to refuse redirects and to reload a rotated client
// certificate. That bypasses the exporters' own reading of the transport
// variables, so this resolves them the same way: Kazari's key when set,
// else OTEL_EXPORTER_OTLP_<SIGNAL>_*, else OTEL_EXPORTER_OTLP_*, else the
// exporters' default.
type endpoint struct {
	url      *url.URL
	protocol string
	// headers is nil when Kazari sets none, leaving the exporter to read
	// OTEL_EXPORTER_OTLP_HEADERS.
	headers map[string]string
	gzip    bool
	timeout time.Duration
	tls     *tls.Config // nil for plaintext
	server  []attribute.KeyValue
	// noRetry fails at once, for kazari check, which should not sit through
	// thirty seconds of backoff to report a refusal.
	noRetry bool

	http *http.Client
	conn *grpc.ClientConn
}

// otlpEnv returns the signal's variable, else the generic one, with the
// name it came from.
func otlpEnv(signal, key string) (value, name string) {
	for _, name := range []string{"OTEL_EXPORTER_OTLP_" + signal + "_" + key, "OTEL_EXPORTER_OTLP_" + key} {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v, name
		}
	}
	return "", ""
}

// otlpEndpoint returns the URL a signal exports to, or nil when neither
// Kazari nor the environment names one. For OTLP/HTTP a base endpoint gets
// the signal's path appended; a signal's own variable is used as it is.
func otlpEndpoint(o OTLP, signal, path string) (*url.URL, string, error) {
	raw, source, base := o.Endpoint, "telemetry.otlp.endpoint", true
	if raw == "" {
		if raw, source = otlpEnv(signal, "ENDPOINT"); raw == "" {
			return nil, "", nil
		}
		base = source == "OTEL_EXPORTER_OTLP_ENDPOINT"
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, "", fmt.Errorf("telemetry: %s must be an http or https URL", source)
	}
	if base {
		u.Path = strings.TrimSuffix(u.Path, "/") + path
	} else if u.Path == "" {
		u.Path = "/"
	}
	u.RawPath = ""
	return u, source, nil
}

// newEndpoint returns nil without an endpoint.
func newEndpoint(o OTLP, signal, path string, log *slog.Logger) (*endpoint, error) {
	u, _, err := otlpEndpoint(o, signal, path)
	if u == nil || err != nil {
		return nil, err
	}
	e := &endpoint{url: u, protocol: o.Protocol, timeout: o.Timeout}
	if e.protocol == "" {
		if e.protocol, _ = otlpEnv(signal, "PROTOCOL"); e.protocol == "" {
			e.protocol = ProtocolHTTP
		}
	}
	if e.protocol != ProtocolHTTP && e.protocol != ProtocolGRPC && e.protocol != protocolJSON {
		return nil, fmt.Errorf("telemetry: unknown OTLP protocol %q", e.protocol)
	}
	compression := o.Compression
	if compression == "" {
		compression, _ = otlpEnv(signal, "COMPRESSION")
	}
	switch compression {
	case "gzip":
		e.gzip = true
	case "", "none":
	default:
		return nil, fmt.Errorf("telemetry: unknown OTLP compression %q", compression)
	}
	if e.timeout == 0 {
		e.timeout = defaultTimeout
		if v, name := otlpEnv(signal, "TIMEOUT"); v != "" {
			ms, err := strconv.Atoi(v)
			if err != nil || ms <= 0 {
				return nil, fmt.Errorf("telemetry: %s must be a positive number of milliseconds", name)
			}
			e.timeout = time.Duration(ms) * time.Millisecond
		}
	}
	if len(o.Headers) > 0 {
		e.headers = maps.Clone(o.Headers)
	}
	if u.Scheme == "https" {
		if e.tls, err = tlsConfig(o, signal, log); err != nil {
			return nil, err
		}
	}
	host, port := u.Hostname(), u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	portNum, _ := strconv.Atoi(port)
	e.server = []attribute.KeyValue{semconv.ServerAddress(host), semconv.ServerPort(portNum)}

	if e.protocol == ProtocolGRPC {
		creds := insecure.NewCredentials()
		if e.tls != nil {
			creds = credentials.NewTLS(e.tls)
		}
		opts := []grpc.DialOption{grpc.WithTransportCredentials(creds), grpc.WithChainUnaryInterceptor(recordCode)}
		if e.gzip {
			opts = append(opts, grpc.WithDefaultCallOptions(grpc.UseCompressor(gzip.Name)))
		}
		if e.conn, err = grpc.NewClient(net.JoinHostPort(host, port), opts...); err != nil {
			return nil, fmt.Errorf("telemetry: %w", err)
		}
		return e, nil
	}
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.TLSClientConfig = e.tls
	e.http = &http.Client{
		Transport: recordStatus{next: base},
		Timeout:   e.timeout,
		// A redirect would resend the headers, often a credential, to
		// wherever the collector points.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return e, nil
}

func tlsConfig(o OTLP, signal string, log *slog.Logger) (*tls.Config, error) {
	c := &tls.Config{MinVersion: tls.VersionTLS12}
	caFile := o.CAFile
	if caFile == "" {
		caFile, _ = otlpEnv(signal, "CERTIFICATE")
	}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("telemetry: %w", err)
		}
		c.RootCAs = x509.NewCertPool()
		if !c.RootCAs.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("telemetry: %s: no PEM certificate found", caFile)
		}
	}
	certFile, keyFile := o.CertFile, o.KeyFile
	if certFile == "" && keyFile == "" {
		certFile, _ = otlpEnv(signal, "CLIENT_CERTIFICATE")
		keyFile, _ = otlpEnv(signal, "CLIENT_KEY")
	}
	switch {
	case certFile == "" && keyFile == "":
	case certFile == "" || keyFile == "":
		return nil, errors.New("telemetry: a client certificate needs both its certificate and its key")
	default:
		pair, err := certs.NewFiles(certFile, keyFile, certs.FilesOptions{Logger: log})
		if err != nil {
			return nil, fmt.Errorf("telemetry: client certificate: %w", err)
		}
		c.GetClientCertificate = pair.GetClientCertificate
	}
	return c, nil
}

func (e *endpoint) close() error {
	if e.conn != nil {
		return e.conn.Close()
	}
	e.http.CloseIdleConnections()
	return nil
}

func (e *endpoint) grpc() bool { return e.conn != nil }

// componentType names the exporter in the otel.sdk.exporter.* metrics.
func (e *endpoint) componentType(httpType, grpcType attribute.KeyValue) attribute.KeyValue {
	if e.grpc() {
		return grpcType
	}
	return httpType
}

func (e *endpoint) metricExporter(ctx context.Context) (sdkmetric.Exporter, error) {
	var (
		exp sdkmetric.Exporter
		err error
	)
	if e.grpc() {
		opts := []otlpmetricgrpc.Option{
			otlpmetricgrpc.WithGRPCConn(e.conn),
			otlpmetricgrpc.WithTimeout(e.timeout),
			otlpmetricgrpc.WithRetry(otlpmetricgrpc.RetryConfig{
				Enabled: !e.noRetry, InitialInterval: retryInitial, MaxInterval: retryMax, MaxElapsedTime: retryElapsed,
			}),
		}
		if e.headers != nil {
			opts = append(opts, otlpmetricgrpc.WithHeaders(e.headers))
		}
		exp, err = otlpmetricgrpc.New(ctx, opts...)
	} else {
		compression := otlpmetrichttp.NoCompression
		if e.gzip {
			compression = otlpmetrichttp.GzipCompression
		}
		opts := []otlpmetrichttp.Option{
			otlpmetrichttp.WithEndpointURL(e.url.String()),
			otlpmetrichttp.WithHTTPClient(e.http),
			// The client carries the TLS settings; this only stands in for
			// any the exporter read from the environment, which it would
			// refuse beside an http endpoint.
			otlpmetrichttp.WithTLSClientConfig(e.tls),
			otlpmetrichttp.WithCompression(compression),
			otlpmetrichttp.WithTimeout(e.timeout),
			otlpmetrichttp.WithRetry(otlpmetrichttp.RetryConfig{
				Enabled: !e.noRetry, InitialInterval: retryInitial, MaxInterval: retryMax, MaxElapsedTime: retryElapsed,
			}),
		}
		if e.headers != nil {
			opts = append(opts, otlpmetrichttp.WithHeaders(e.headers))
		}
		exp, err = otlpmetrichttp.New(ctx, opts...)
	}
	if err != nil {
		return nil, fmt.Errorf("telemetry: metric exporter: %w", err)
	}
	return exp, nil
}

// Warnings are settings that work but are probably not what was meant;
// startup logs them and kazari check prints them.
func Warnings(o Options) []string {
	var out []string
	warnedPlaintext := false
	for _, s := range []struct {
		on           bool
		signal, path string
		key          string
	}{
		{o.Traces.OTLP, signalTraces, "/v1/traces", "telemetry.traces.otlp"},
		{o.Metrics.OTLP, signalMetrics, "/v1/metrics", "telemetry.metrics.otlp"},
		{o.Logs.OTLP, signalLogs, "/v1/logs", "telemetry.logs.otlp"},
	} {
		if !s.on {
			continue
		}
		u, source, err := otlpEndpoint(o.OTLP, s.signal, s.path)
		switch {
		case err != nil:
			out = append(out, err.Error())
		case u == nil && s.signal != signalTraces:
			out = append(out, s.key+" is true but no endpoint is set, in telemetry.otlp.endpoint or OTEL_EXPORTER_OTLP_ENDPOINT")
		case u != nil && u.Scheme == "http" && !nearby(u.Hostname()) && !warnedPlaintext:
			warnedPlaintext = true
			out = append(out, source+" sends headers and telemetry to "+u.Hostname()+" unencrypted; use https")
		}
	}
	return out
}

// nearby reports a host that plain HTTP reaches without crossing the
// internet: loopback, a private address, or a single-label name such as a
// Compose service.
func nearby(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate()
	}
	return !strings.Contains(host, ".") || host == "localhost" || strings.HasSuffix(host, ".localhost")
}
