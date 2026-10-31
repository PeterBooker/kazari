package config

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"golang.org/x/net/http/httpguts"
)

// Telemetry configures OpenTelemetry: one OTLP destination shared by every
// signal, and a switch per signal.
type Telemetry struct {
	ServiceName string    `yaml:"service_name"`
	Environment string    `yaml:"environment"`
	OTLP        OTLP      `yaml:"otlp"`
	Traces      Traces    `yaml:"traces"`
	Metrics     Metrics   `yaml:"metrics"`
	Logs        LogExport `yaml:"logs"`
}

// OTLP is the collector or vendor every enabled signal exports to.
type OTLP struct {
	Endpoint    string   `yaml:"endpoint"`
	Protocol    string   `yaml:"protocol"`
	Headers     Headers  `yaml:"headers" secret:"true"`
	Compression string   `yaml:"compression"`
	Timeout     Duration `yaml:"timeout"`
	CAFile      string   `yaml:"ca_file"`
	CertFile    string   `yaml:"cert_file"`
	KeyFile     string   `yaml:"key_file"`
}

// OTLP protocols.
const (
	ProtocolHTTP = "http/protobuf"
	ProtocolGRPC = "grpc"
)

// Traces configures span export and sampling.
type Traces struct {
	OTLP            bool    `yaml:"otlp"`
	SampleRatio     float64 `yaml:"sample_ratio"`
	SiteSampleRatio float64 `yaml:"site_sample_ratio"`
}

// Metrics configures the Prometheus endpoint and the optional OTLP push.
type Metrics struct {
	Prometheus bool     `yaml:"prometheus"`
	Token      string   `yaml:"token" secret:"true"`
	OTLP       bool     `yaml:"otlp"`
	Interval   Duration `yaml:"interval"`
}

// LogExport configures the optional OTLP copy of the log.
type LogExport struct {
	OTLP  bool   `yaml:"otlp"`
	Level string `yaml:"level"`
}

// MinMetricsInterval is the shortest telemetry.metrics.interval accepted.
const MinMetricsInterval = 5 * time.Second

func defaultTelemetry() Telemetry {
	return Telemetry{
		ServiceName: "kazari",
		OTLP: OTLP{
			Protocol:    ProtocolHTTP,
			Headers:     Headers{},
			Compression: "gzip",
			Timeout:     Duration(10 * second),
		},
		Traces: Traces{
			OTLP:            true,
			SampleRatio:     1,
			SiteSampleRatio: 0.01,
		},
		Metrics: Metrics{
			Prometheus: true,
			Interval:   Duration(60 * second),
		},
	}
}

// Headers are HTTP headers sent with every export. In the environment they
// take the OTLP exporter form name=value,name=value with percent-encoded
// values.
type Headers map[string]string

// UnmarshalText implements encoding.TextUnmarshaler for the environment form.
func (h *Headers) UnmarshalText(text []byte) error {
	out := Headers{}
	for pair := range strings.SplitSeq(string(text), ",") {
		if strings.TrimSpace(pair) == "" {
			continue
		}
		// The errors leave out the text, which may be a credential.
		name, value, ok := strings.Cut(pair, "=")
		if !ok {
			return errors.New("invalid headers: want name=value pairs separated by commas")
		}
		name = strings.TrimSpace(name)
		v, err := url.PathUnescape(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("invalid header %s: the value is not percent-encoded", name)
		}
		out[name] = v
	}
	*h = out
	return nil
}

// String names the headers and hides their values, which are often
// credentials.
func (h Headers) String() string {
	names := slices.Sorted(maps.Keys(h))
	for i, n := range names {
		names[i] = n + "=***"
	}
	return strings.Join(names, ",")
}

func (v *validator) telemetry(c *Config) {
	t := c.Telemetry
	v.required("telemetry.service_name", t.ServiceName)
	v.oneOf("telemetry.otlp.protocol", t.OTLP.Protocol, ProtocolHTTP, ProtocolGRPC)
	v.oneOf("telemetry.otlp.compression", t.OTLP.Compression, "gzip", "none")
	if t.OTLP.Endpoint != "" {
		if err := checkEndpoint(t.OTLP.Endpoint, t.OTLP.Protocol); err != nil {
			v.add("telemetry.otlp.endpoint", err.Error())
		}
	}
	for name, value := range t.OTLP.Headers {
		if !httpguts.ValidHeaderFieldName(name) || !httpguts.ValidHeaderFieldValue(value) {
			v.add("telemetry.otlp.headers", fmt.Sprintf("%q is not a valid header name and value", name))
		}
	}
	if t.OTLP.Timeout <= 0 {
		v.add("telemetry.otlp.timeout", "must be greater than 0")
	}
	if err := checkTLSFiles(t.OTLP); err != nil {
		v.errs = append(v.errs, err)
	}
	v.ratio("telemetry.traces.sample_ratio", t.Traces.SampleRatio)
	v.ratio("telemetry.traces.site_sample_ratio", t.Traces.SiteSampleRatio)
	if tok := t.Metrics.Token; tok != "" && len(tok) < MinAdminTokenLen && !isLocalhost(c.BaseDomain) {
		v.add("telemetry.metrics.token", fmt.Sprintf("must be at least %d characters; generate one with openssl rand -hex 32", MinAdminTokenLen))
	}
	if t.Metrics.Interval.Std() < MinMetricsInterval {
		v.add("telemetry.metrics.interval", "must be at least "+MinMetricsInterval.String())
	}
	if t.Logs.Level != "" {
		v.oneOf("telemetry.logs.level", t.Logs.Level, logLevels...)
	}
}

func (v *validator) ratio(key string, r float64) {
	if !(r >= 0 && r <= 1) {
		v.add(key, "must be between 0 and 1")
	}
}

// checkEndpoint keeps credentials out of the URL, which kazari check, admin
// settings and exporter errors print.
func checkEndpoint(endpoint, protocol string) error {
	u, err := url.Parse(endpoint)
	switch {
	case err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https"):
		return errors.New("must be an absolute http or https URL")
	case u.User != nil:
		return errors.New("must not contain credentials; send them in telemetry.otlp.headers")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return errors.New("must not have a query or fragment")
	case protocol == ProtocolGRPC && strings.Trim(u.Path, "/") != "":
		return errors.New("must not have a path with protocol grpc")
	}
	return nil
}

func checkTLSFiles(o OTLP) error {
	var errs []error
	if o.CAFile != "" {
		pem, err := os.ReadFile(o.CAFile)
		if err != nil {
			errs = append(errs, fmt.Errorf("telemetry.otlp.ca_file: %w", err))
		} else if !x509.NewCertPool().AppendCertsFromPEM(pem) {
			errs = append(errs, errors.New("telemetry.otlp.ca_file: no PEM certificate found"))
		}
	}
	switch {
	case (o.CertFile == "") != (o.KeyFile == ""):
		errs = append(errs, errors.New("telemetry.otlp.cert_file: cert_file and key_file must be set together"))
	case o.CertFile != "":
		if _, err := tls.LoadX509KeyPair(o.CertFile, o.KeyFile); err != nil {
			errs = append(errs, fmt.Errorf("telemetry.otlp.cert_file: %w", err))
		}
	}
	return errors.Join(errs...)
}
