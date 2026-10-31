package telemetry

import (
	"context"
	"crypto/tls"
	"crypto/x509"
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

// Retries stay within exportTimeout, the processors' and the reader's
// export timeout.
const (
	retryInitial = 5 * time.Second
	retryMax     = 15 * time.Second
	retryElapsed = 30 * time.Second
)

// transport is the connection every signal's exporter shares: one HTTP
// client or one gRPC connection, built by Kazari so that no exporter falls
// back to an OTEL_EXPORTER_OTLP_* variable for its endpoint, TLS or
// compression.
type transport struct {
	endpoint *url.URL
	headers  map[string]string
	gzip     bool
	timeout  time.Duration
	tls      *tls.Config // nil for plaintext
	server   []attribute.KeyValue

	http *http.Client
	conn *grpc.ClientConn
}

func newTransport(o OTLP, log *slog.Logger) (*transport, error) {
	u, err := url.Parse(o.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("telemetry: endpoint: %w", err)
	}
	t := &transport{
		endpoint: u,
		headers:  maps.Clone(o.Headers),
		gzip:     o.Compression == "gzip",
		timeout:  o.Timeout,
	}
	if t.headers == nil {
		t.headers = map[string]string{}
	}
	if u.Scheme == "https" {
		if t.tls, err = tlsConfig(o, log); err != nil {
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
	t.server = []attribute.KeyValue{semconv.ServerAddress(host), semconv.ServerPort(portNum)}

	if o.Protocol == ProtocolGRPC {
		creds := insecure.NewCredentials()
		if t.tls != nil {
			creds = credentials.NewTLS(t.tls)
		}
		opts := []grpc.DialOption{grpc.WithTransportCredentials(creds), grpc.WithChainUnaryInterceptor(recordCode)}
		if t.gzip {
			opts = append(opts, grpc.WithDefaultCallOptions(grpc.UseCompressor(gzip.Name)))
		}
		if t.conn, err = grpc.NewClient(net.JoinHostPort(host, port), opts...); err != nil {
			return nil, fmt.Errorf("telemetry: %w", err)
		}
		return t, nil
	}
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.TLSClientConfig = t.tls
	t.http = &http.Client{
		Transport: recordStatus{next: base},
		Timeout:   o.Timeout,
		// A redirect would resend the headers, often a credential, to
		// wherever the collector points.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return t, nil
}

func tlsConfig(o OTLP, log *slog.Logger) (*tls.Config, error) {
	c := &tls.Config{MinVersion: tls.VersionTLS12}
	if o.CAFile != "" {
		pem, err := os.ReadFile(o.CAFile)
		if err != nil {
			return nil, fmt.Errorf("telemetry: %w", err)
		}
		c.RootCAs = x509.NewCertPool()
		if !c.RootCAs.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("telemetry: %s: no PEM certificate found", o.CAFile)
		}
	}
	if o.CertFile != "" {
		pair, err := certs.NewFiles(o.CertFile, o.KeyFile, certs.FilesOptions{Logger: log})
		if err != nil {
			return nil, fmt.Errorf("telemetry: client certificate: %w", err)
		}
		c.GetClientCertificate = pair.GetClientCertificate
	}
	return c, nil
}

func (t *transport) close() error {
	if t.conn != nil {
		return t.conn.Close()
	}
	t.http.CloseIdleConnections()
	return nil
}

func (t *transport) grpc() bool { return t.conn != nil }

// signalURL applies the OTLP/HTTP base-endpoint rule: the signal's path is
// appended to the endpoint's, so vendors that serve OTLP under a prefix work.
func (t *transport) signalURL(signalPath string) string {
	u := *t.endpoint
	u.Path = strings.TrimSuffix(u.Path, "/") + signalPath
	u.RawPath = ""
	return u.String()
}

// componentType names the exporter in the otel.sdk.exporter.* metrics.
func (t *transport) componentType(httpType, grpcType attribute.KeyValue) attribute.KeyValue {
	if t.grpc() {
		return grpcType
	}
	return httpType
}

func (t *transport) metricExporter(ctx context.Context) (sdkmetric.Exporter, error) {
	var (
		exp sdkmetric.Exporter
		err error
	)
	if t.grpc() {
		exp, err = otlpmetricgrpc.New(ctx,
			otlpmetricgrpc.WithGRPCConn(t.conn),
			otlpmetricgrpc.WithHeaders(t.headers),
			otlpmetricgrpc.WithTimeout(t.timeout),
			otlpmetricgrpc.WithRetry(otlpmetricgrpc.RetryConfig{
				Enabled: true, InitialInterval: retryInitial, MaxInterval: retryMax, MaxElapsedTime: retryElapsed,
			}),
			otlpmetricgrpc.WithTemporalitySelector(sdkmetric.DefaultTemporalitySelector),
			otlpmetricgrpc.WithAggregationSelector(sdkmetric.DefaultAggregationSelector),
		)
	} else {
		exp, err = otlpmetrichttp.New(ctx,
			otlpmetrichttp.WithEndpointURL(t.signalURL("/v1/metrics")),
			otlpmetrichttp.WithHTTPClient(t.http),
			// The client carries the TLS settings; this only replaces any
			// the environment set, which the exporter would refuse beside
			// an http endpoint.
			otlpmetrichttp.WithTLSClientConfig(t.tls),
			otlpmetrichttp.WithHeaders(t.headers),
			otlpmetrichttp.WithCompression(t.httpMetricCompression()),
			otlpmetrichttp.WithTimeout(t.timeout),
			otlpmetrichttp.WithRetry(otlpmetrichttp.RetryConfig{
				Enabled: true, InitialInterval: retryInitial, MaxInterval: retryMax, MaxElapsedTime: retryElapsed,
			}),
			otlpmetrichttp.WithTemporalitySelector(sdkmetric.DefaultTemporalitySelector),
			otlpmetrichttp.WithAggregationSelector(sdkmetric.DefaultAggregationSelector),
		)
	}
	if err != nil {
		return nil, fmt.Errorf("telemetry: metric exporter: %w", err)
	}
	return exp, nil
}

func (t *transport) httpMetricCompression() otlpmetrichttp.Compression {
	if t.gzip {
		return otlpmetrichttp.GzipCompression
	}
	return otlpmetrichttp.NoCompression
}
