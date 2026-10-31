package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func ptr[T any](v T) *T { return &v }

func TestTelemetryDefaults(t *testing.T) {
	got := Default().Telemetry
	want := Telemetry{
		OTLP:    OTLP{Headers: Headers{}},
		Traces:  Traces{OTLP: true},
		Metrics: Metrics{Prometheus: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Default().Telemetry = %+v, want %+v", got, want)
	}
}

func TestTelemetryYAML(t *testing.T) {
	path := writeYAML(t, `base_domain: art.example.com
data_dir: /d
tls:
  mode: self-signed
telemetry:
  environment: production
  otlp:
    endpoint: https://otlp.example.com/otlp
    protocol: http/protobuf
    headers:
      authorization: "Bearer ${OTLP_TOKEN}"
      x-scope: kazari
    compression: none
    timeout: 5s
  traces:
    sample_ratio: 0.5
    site_sample_ratio: 0
  metrics:
    token: ${METRICS_TOKEN}
    otlp: true
    interval: 30s
  logs:
    otlp: true
    level: warn
`)
	c, err := LoadWith(path, env(map[string]string{"OTLP_TOKEN": "t0k", "METRICS_TOKEN": testAdminToken}))
	if err != nil {
		t.Fatalf("LoadWith: %v", err)
	}
	want := Telemetry{
		Environment: "production",
		OTLP: OTLP{
			Endpoint:    "https://otlp.example.com/otlp",
			Protocol:    "http/protobuf",
			Headers:     Headers{"authorization": "Bearer t0k", "x-scope": "kazari"},
			Compression: "none",
			Timeout:     Duration(5 * time.Second),
		},
		Traces:  Traces{OTLP: true, SampleRatio: ptr(0.5), SiteSampleRatio: ptr(0.0)},
		Metrics: Metrics{Prometheus: true, Token: testAdminToken, OTLP: true, Interval: Duration(30 * time.Second)},
		Logs:    LogExport{OTLP: true, Level: "warn"},
	}
	if !reflect.DeepEqual(c.Telemetry, want) {
		t.Errorf("Telemetry = %+v, want %+v", c.Telemetry, want)
	}
}

func TestTelemetryEnv(t *testing.T) {
	headersFile := filepath.Join(t.TempDir(), "headers")
	if err := os.WriteFile(headersFile, []byte("authorization=Bearer%20from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		vars map[string]string
		want Headers
	}{
		{"pairs", map[string]string{"KAZARI_TELEMETRY_OTLP_HEADERS": "authorization=Bearer%20abc, x-team = ops ,api-key=a+b/c=="},
			Headers{"authorization": "Bearer abc", "x-team": "ops", "api-key": "a+b/c=="}},
		{"empty", map[string]string{"KAZARI_TELEMETRY_OTLP_HEADERS": ""}, Headers{}},
		{"file", map[string]string{"KAZARI_TELEMETRY_OTLP_HEADERS_FILE": headersFile}, Headers{"authorization": "Bearer from-file"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vars := map[string]string{
				"KAZARI_BASE_DOMAIN":                        "art.example.com",
				"KAZARI_DATA_DIR":                           "/d",
				"KAZARI_TLS_MODE":                           "self-signed",
				"KAZARI_TELEMETRY_TRACES_SITE_SAMPLE_RATIO": "0.25",
				"KAZARI_TELEMETRY_METRICS_TOKEN":            testAdminToken,
			}
			for k, v := range tt.vars {
				vars[k] = v
			}
			c, err := LoadWith("", env(vars))
			if err != nil {
				t.Fatalf("LoadWith: %v", err)
			}
			if !reflect.DeepEqual(c.Telemetry.OTLP.Headers, tt.want) {
				t.Errorf("Headers = %#v, want %#v", c.Telemetry.OTLP.Headers, tt.want)
			}
			if r := c.Telemetry.Traces.SiteSampleRatio; r == nil || *r != 0.25 {
				t.Errorf("SiteSampleRatio = %v, want 0.25", r)
			}
			if c.Telemetry.Traces.SampleRatio != nil {
				t.Errorf("SampleRatio = %v, want unset", *c.Telemetry.Traces.SampleRatio)
			}
			if c.Telemetry.Metrics.Token != testAdminToken {
				t.Errorf("Token = %q", c.Telemetry.Metrics.Token)
			}
		})
	}
}

func TestTelemetryEnvBadValues(t *testing.T) {
	for key, value := range map[string]string{
		"KAZARI_TELEMETRY_OTLP_HEADERS":        "no-equals-sign",
		"KAZARI_TELEMETRY_TRACES_SAMPLE_RATIO": "half",
		"KAZARI_TELEMETRY_METRICS_INTERVAL":    "often",
	} {
		t.Run(key, func(t *testing.T) {
			_, err := LoadWith("", env(map[string]string{key: value}))
			if err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("err = %v, want one naming %s", err, key)
			}
		})
	}
	_, err := LoadWith("", env(map[string]string{"KAZARI_TELEMETRY_OTLP_HEADERS": "authorization=%zz"}))
	if err == nil || strings.Contains(err.Error(), "%zz") {
		t.Errorf("bad escape: err = %v, want one that does not echo the value", err)
	}
}

func TestHeadersKeysNotUnknown(t *testing.T) {
	path := writeYAML(t, "base_domain: a.test\ndata_dir: /d\ntls:\n  mode: self-signed\ntelemetry:\n  otlp:\n    headers:\n      anything-at-all: x\n")
	c, err := LoadWith(path, env(nil))
	if err != nil {
		t.Fatalf("LoadWith: %v", err)
	}
	if c.Telemetry.OTLP.Headers["anything-at-all"] != "x" {
		t.Errorf("Headers = %v", c.Telemetry.OTLP.Headers)
	}
}

func TestHeadersRedacted(t *testing.T) {
	c := valid()
	c.Telemetry.OTLP.Headers = Headers{"authorization": "Bearer hush-123", "x-team": "ops-hush"}
	c.Telemetry.Metrics.Token = testAdminToken
	m := c.Redacted()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"hush", testAdminToken} {
		if strings.Contains(string(data), secret) {
			t.Errorf("redacted output contains %q: %s", secret, data)
		}
	}
	tel := m["telemetry"].(map[string]any)
	headers := tel["otlp"].(map[string]any)["headers"]
	if want := map[string]any{"authorization": "***", "x-team": "***"}; !reflect.DeepEqual(headers, want) {
		t.Errorf("headers = %#v, want %#v", headers, want)
	}
	if got := tel["metrics"].(map[string]any)["token"]; got != "***" {
		t.Errorf("token = %#v, want ***", got)
	}
	if got := Default().Redacted()["telemetry"].(map[string]any)["otlp"].(map[string]any)["headers"]; !reflect.DeepEqual(got, map[string]any{}) {
		t.Errorf("empty headers = %#v, want an empty map", got)
	}

	if got := c.Telemetry.OTLP.Headers.String(); got != "authorization=***,x-team=***" {
		t.Errorf("String() = %q", got)
	}
}

type secretStringer string

func (s secretStringer) String() string { return string(s) }

func TestRedactSecretBeforeStringer(t *testing.T) {
	type section struct {
		Key secretStringer `yaml:"key" secret:"true"`
	}
	got := redactStruct(reflect.ValueOf(section{Key: "plain-value"}))
	if got["key"] != "***" {
		t.Errorf("key = %#v, want ***", got["key"])
	}
}

func TestValidateTelemetry(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := writeKeyPair(t, dir)
	junk := filepath.Join(dir, "junk.pem")
	if err := os.WriteFile(junk, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*Telemetry)
		want   string
	}{
		{"protocol", func(t *Telemetry) { t.OTLP.Protocol = "http/json" }, "telemetry.otlp.protocol: must be one of http/protobuf, grpc"},
		{"compression", func(t *Telemetry) { t.OTLP.Compression = "zstd" }, "telemetry.otlp.compression: must be one of gzip, none"},
		{"relative endpoint", func(t *Telemetry) { t.OTLP.Endpoint = "otlp.example.com:4318" }, "telemetry.otlp.endpoint: must be an absolute http or https URL"},
		{"endpoint scheme", func(t *Telemetry) { t.OTLP.Endpoint = "grpc://otlp.example.com" }, "telemetry.otlp.endpoint: must be an absolute http or https URL"},
		{"endpoint userinfo", func(t *Telemetry) { t.OTLP.Endpoint = "https://user:pass@otlp.example.com" }, "telemetry.otlp.endpoint: must not contain credentials"},
		{"endpoint query", func(t *Telemetry) { t.OTLP.Endpoint = "https://otlp.example.com/?key=x" }, "telemetry.otlp.endpoint: must not have a query or fragment"},
		{"endpoint fragment", func(t *Telemetry) { t.OTLP.Endpoint = "https://otlp.example.com/#x" }, "telemetry.otlp.endpoint: must not have a query or fragment"},
		{"grpc path", func(t *Telemetry) {
			t.OTLP.Protocol = "grpc"
			t.OTLP.Endpoint = "https://otlp.example.com:4317/otlp"
		}, "telemetry.otlp.endpoint: must not have a path with protocol grpc"},
		{"header name", func(t *Telemetry) { t.OTLP.Headers = Headers{"bad name": "x"} }, "telemetry.otlp.headers:"},
		{"header value", func(t *Telemetry) { t.OTLP.Headers = Headers{"x-a": "line\r\nInjected: 1"} }, "telemetry.otlp.headers:"},
		{"timeout", func(t *Telemetry) { t.OTLP.Timeout = -1 }, "telemetry.otlp.timeout: must not be negative"},
		{"ca missing", func(t *Telemetry) { t.OTLP.CAFile = filepath.Join(dir, "none.pem") }, "telemetry.otlp.ca_file:"},
		{"ca junk", func(t *Telemetry) { t.OTLP.CAFile = junk }, "telemetry.otlp.ca_file: no PEM certificate found"},
		{"cert alone", func(t *Telemetry) { t.OTLP.CertFile = certFile }, "telemetry.otlp.cert_file: cert_file and key_file must be set together"},
		{"key alone", func(t *Telemetry) { t.OTLP.KeyFile = keyFile }, "telemetry.otlp.cert_file: cert_file and key_file must be set together"},
		{"cert junk", func(t *Telemetry) { t.OTLP.CertFile, t.OTLP.KeyFile = junk, keyFile }, "telemetry.otlp.cert_file:"},
		{"sample ratio", func(t *Telemetry) { t.Traces.SampleRatio = ptr(1.5) }, "telemetry.traces.sample_ratio: must be between 0 and 1"},
		{"site sample ratio", func(t *Telemetry) { t.Traces.SiteSampleRatio = ptr(-0.1) }, "telemetry.traces.site_sample_ratio: must be between 0 and 1"},
		{"metrics token", func(t *Telemetry) { t.Metrics.Token = "short" }, "telemetry.metrics.token: must be at least 32 characters"},
		{"metrics interval", func(t *Telemetry) { t.Metrics.Interval = Duration(time.Second) }, "telemetry.metrics.interval: must be 0 or at least 5s"},
		{"logs level", func(t *Telemetry) { t.Logs.Level = "trace" }, "telemetry.logs.level: must be one of debug, info, warn, error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := valid()
			tt.mutate(&c.Telemetry)
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tt.want)
			}
		})
	}

	ok := map[string]func(*Config){
		"http endpoint with path": func(c *Config) { c.Telemetry.OTLP.Endpoint = "http://collector:4318/otlp/" },
		"grpc endpoint": func(c *Config) {
			c.Telemetry.OTLP.Protocol = "grpc"
			c.Telemetry.OTLP.Endpoint = "https://otlp.example.com:4317/"
		},
		"signal without endpoint": func(c *Config) { c.Telemetry.Metrics.OTLP = true; c.Telemetry.Logs.OTLP = true },
		"mutual tls": func(c *Config) {
			c.Telemetry.OTLP.CAFile, c.Telemetry.OTLP.CertFile, c.Telemetry.OTLP.KeyFile = certFile, certFile, keyFile
		},
		"short token on localhost": func(c *Config) { c.BaseDomain = "localhost"; c.Telemetry.Metrics.Token = "dev" },
		"logs level":               func(c *Config) { c.Telemetry.Logs.Level = "debug" },
		"ratio bounds": func(c *Config) {
			c.Telemetry.Traces.SampleRatio, c.Telemetry.Traces.SiteSampleRatio = ptr(0.0), ptr(1.0)
		},
		"explicit settings": func(c *Config) {
			c.Telemetry.OTLP.Protocol, c.Telemetry.OTLP.Compression = "grpc", "none"
			c.Telemetry.OTLP.Timeout, c.Telemetry.Metrics.Interval = Duration(time.Second), Duration(5*time.Second)
		},
	}
	for name, mutate := range ok {
		t.Run(name, func(t *testing.T) {
			c := valid()
			mutate(&c)
			if err := c.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

// writeKeyPair writes a self-signed certificate and its key as PEM files.
func writeKeyPair(t *testing.T, dir string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "kazari test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
		KeyUsage:     x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}
