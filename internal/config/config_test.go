package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func valid() Config {
	c := Default()
	c.BaseDomain = "art.example.com"
	c.DataDir = "/var/lib/kazari"
	c.Auth.AdminToken = "admin-secret-0123456789abcdef0123"
	c.TLS.Email = "ops@example.com"
	c.TLS.DNS.APIToken = "dns-secret"
	return c
}

// testAdminToken is long enough for auth.admin_token outside localhost.
const testAdminToken = "adm-0123456789abcdef0123456789ab"

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func writeYAML(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kazari.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDefaultValid(t *testing.T) {
	tests := map[string]func(*Config){
		"acme":        func(*Config) {},
		"proxy mode":  func(c *Config) { c.Listen.ProxyMode = true; c.TLS.Email = ""; c.TLS.DNS.APIToken = "" },
		"self-signed": func(c *Config) { c.TLS.Mode = "self-signed"; c.TLS.Email = "" },
		"route53":     func(c *Config) { c.TLS.DNSProvider = "route53"; c.TLS.DNS.APIToken = "" },
		"no admin token or http listener": func(c *Config) {
			c.Auth.AdminToken = ""
			c.Listen.HTTP = ""
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			c := valid()
			mutate(&c)
			if err := c.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

func TestDefaultNotShared(t *testing.T) {
	a := Default()
	a.Serve.ReservedSlugs[0] = "changed"
	if b := Default(); b.Serve.ReservedSlugs[0] != "api" {
		t.Fatalf("Default shares the reserved slug slice: got %q", b.Serve.ReservedSlugs[0])
	}
}

func TestExampleFile(t *testing.T) {
	const path = "../../deploy/kazari.yaml"
	got, err := LoadWith(path, env(map[string]string{"KAZARI_TLS_DNS_API_TOKEN": "tok", "KAZARI_AUTH_ADMIN_TOKEN": testAdminToken}))
	if err != nil {
		t.Fatalf("LoadWith: %v", err)
	}

	if got.BaseDomain != "art.example.com" {
		t.Errorf("BaseDomain = %q", got.BaseDomain)
	}
	if got.Limits.MaxVersionBytes != 50*MiB {
		t.Errorf("MaxVersionBytes = %v", got.Limits.MaxVersionBytes)
	}
	if got.Limits.MaxFiles != 2000 {
		t.Errorf("MaxFiles = %d", got.Limits.MaxFiles)
	}
	if got.Lifecycle.DefaultGrace.Std() != 14*24*time.Hour {
		t.Errorf("DefaultGrace = %v", got.Lifecycle.DefaultGrace)
	}
	if got.Serve.CSP != DefaultCSP {
		t.Errorf("CSP = %q", got.Serve.CSP)
	}
	if got.Auth.OIDC != nil {
		t.Errorf("OIDC = %+v, want nil", got.Auth.OIDC)
	}

	want := Default()
	want.BaseDomain = "art.example.com"
	want.DataDir = "/var/lib/kazari"
	want.TLS.Email = "ops@example.com"
	want.TLS.DNS.APIToken = "tok"
	want.Auth.AdminToken = testAdminToken
	if !reflect.DeepEqual(got, want) {
		t.Errorf("example differs from Default:\n got %+v\nwant %+v", got, want)
	}
}

func TestExampleFileUnsetSecret(t *testing.T) {
	_, err := LoadWith("../../deploy/kazari.yaml", env(map[string]string{"KAZARI_AUTH_ADMIN_TOKEN": testAdminToken}))
	want := "config: ../../deploy/kazari.yaml: ${KAZARI_TLS_DNS_API_TOKEN} is not set"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

func TestDevFile(t *testing.T) {
	got, err := LoadWith("../../deploy/dev.yaml", env(nil))
	if err != nil {
		t.Fatalf("LoadWith: %v", err)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"base_domain", got.BaseDomain, "localhost"},
		{"external_scheme", got.ExternalScheme, "http"},
		{"external_port", got.ExternalPort, 8080},
		{"listen.http", got.Listen.HTTP, "127.0.0.1:8080"},
		{"listen.proxy_mode", got.Listen.ProxyMode, true},
		{"tls.mode", got.TLS.Mode, "self-signed"},
		{"auth.admin_token", got.Auth.AdminToken, "dev-admin-token"},
		{"log.level", got.Log.Level, "debug"},
	}
	for _, ch := range checks {
		if !reflect.DeepEqual(ch.got, ch.want) {
			t.Errorf("%s = %#v, want %#v", ch.name, ch.got, ch.want)
		}
	}

	got, err = LoadWith("../../deploy/dev.yaml", env(map[string]string{"KAZARI_AUTH_ADMIN_TOKEN": "mine"}))
	if err != nil || got.Auth.AdminToken != "mine" {
		t.Errorf("admin token from env = %q, %v; want mine", got.Auth.AdminToken, err)
	}
}

func TestLoadNoFile(t *testing.T) {
	_, err := LoadWith("", env(nil))
	if err == nil || !strings.Contains(err.Error(), "base_domain: required") {
		t.Fatalf("err = %v, want base_domain: required", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := LoadWith(filepath.Join(t.TempDir(), "nope.yaml"), env(nil))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want not exist", err)
	}
}

func TestLoadUnknownKey(t *testing.T) {
	path := writeYAML(t, `
base_domain: art.example.com
colour: blue

listen:
  htps: ":443"
`)
	_, err := LoadWith(path, env(nil))
	if err == nil {
		t.Fatal("want error for unknown keys")
	}
	for _, k := range []string{path + ": line 3: unknown key colour", path + ": line 6: unknown key listen.htps"} {
		if !strings.Contains(err.Error(), k) {
			t.Errorf("error %q does not name %s", err, k)
		}
	}
}

func TestLoadUnknownKeyMerged(t *testing.T) {
	path := writeYAML(t, "listen: &l\n  https: \":443\"\nadmin:\n  <<: *l\n")
	_, err := LoadWith(path, env(nil))
	if want := path + ": line 2: unknown key admin.https"; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

func TestLoadTypeError(t *testing.T) {
	path := writeYAML(t, "\n\nbase_domain: [a, b]\n")
	_, err := LoadWith(path, env(nil))
	if err == nil || !strings.Contains(err.Error(), "cannot unmarshal !!seq into string") {
		t.Fatalf("err = %v, want a type error", err)
	}
	if strings.Contains(err.Error(), "line ") {
		t.Errorf("err = %v, want no line number", err)
	}
}

func TestLoadBadValue(t *testing.T) {
	path := writeYAML(t, "limits:\n  max_version_bytes: lots\n")
	if _, err := LoadWith(path, env(nil)); err == nil || !strings.Contains(err.Error(), "lots") {
		t.Fatalf("err = %v, want invalid size error", err)
	}
}

func TestLoadOIDCMapping(t *testing.T) {
	base := `
base_domain: art.example.com
data_dir: /data
tls:
  mode: self-signed
auth:
  oidc:
    issuer: https://accounts.google.com
    client_id: id
    client_secret: secret
`
	tests := []struct {
		name  string
		extra string
		want  bool
	}{
		{"default", "", true},
		{"explicit false", "    auto_provision: false\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := LoadWith(writeYAML(t, base+tt.extra), env(nil))
			if err != nil {
				t.Fatalf("LoadWith: %v", err)
			}
			if c.Auth.OIDC == nil {
				t.Fatal("OIDC is nil")
			}
			if c.Auth.OIDC.AutoProvision != tt.want {
				t.Errorf("AutoProvision = %v, want %v", c.Auth.OIDC.AutoProvision, tt.want)
			}
		})
	}
}

func TestLoadOIDCAbsent(t *testing.T) {
	tests := map[string]string{
		"no auth":       "",
		"auth only":     "auth:\n  write_policy: owner\n",
		"null oidc":     "auth:\n  oidc:\n",
		"empty mapping": "auth: {}\n",
	}
	for name, extra := range tests {
		t.Run(name, func(t *testing.T) {
			c, err := LoadWith(writeYAML(t, "base_domain: localhost\ndata_dir: /d\ntls:\n  mode: self-signed\n"+extra), env(nil))
			if err != nil {
				t.Fatalf("LoadWith: %v", err)
			}
			if c.Auth.OIDC != nil {
				t.Errorf("OIDC = %+v, want nil", c.Auth.OIDC)
			}
		})
	}
}

func TestLoadScalars(t *testing.T) {
	path := writeYAML(t, `
base_domain: localhost
data_dir: /d
tls:
  mode: self-signed
limits:
  max_chunk_bytes: 1048576
  max_file_bytes: "2 MiB"
  upload_ttl: 1d12h
lifecycle:
  default_grace: 14d
serve:
  reserved_slugs:
    - api
    - mcp
`)
	c, err := LoadWith(path, env(nil))
	if err != nil {
		t.Fatalf("LoadWith: %v", err)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"limits.max_chunk_bytes", c.Limits.MaxChunkBytes, MiB},
		{"limits.max_file_bytes", c.Limits.MaxFileBytes, 2 * MiB},
		{"limits.upload_ttl", c.Limits.UploadTTL, Duration(36 * time.Hour)},
		{"lifecycle.default_grace", c.Lifecycle.DefaultGrace, Duration(14 * 24 * time.Hour)},
		{"serve.reserved_slugs", c.Serve.ReservedSlugs, []string{"api", "mcp"}},
		{"limits.max_version_bytes default", c.Limits.MaxVersionBytes, 50 * MiB},
	}
	for _, ch := range checks {
		if !reflect.DeepEqual(ch.got, ch.want) {
			t.Errorf("%s = %#v, want %#v", ch.name, ch.got, ch.want)
		}
	}
}

func TestLoadEmptyFile(t *testing.T) {
	c, err := LoadWith(writeYAML(t, "# nothing but a comment\n"), env(map[string]string{
		"KAZARI_BASE_DOMAIN": "localhost", "KAZARI_DATA_DIR": "/d", "KAZARI_TLS_MODE": "self-signed",
	}))
	if err != nil {
		t.Fatalf("LoadWith: %v", err)
	}
	if c.BaseDomain != "localhost" || c.Limits.MaxFiles != 2000 {
		t.Errorf("got %+v", c)
	}
}

func TestLoadBadYAML(t *testing.T) {
	path := writeYAML(t, "base_domain: [unclosed\n")
	_, err := LoadWith(path, env(nil))
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("err = %v, want a YAML error naming %s", err, path)
	}
}

func TestEnvOverrides(t *testing.T) {
	vars := map[string]string{
		"KAZARI_BASE_DOMAIN":              "demo.test",
		"KAZARI_DATA_DIR":                 "/srv/kazari",
		"KAZARI_LISTEN_PROXY_MODE":        "true",
		"KAZARI_LISTEN_HTTP":              "127.0.0.1:8080",
		"KAZARI_LIMITS_MAX_FILES":         "10",
		"KAZARI_LIMITS_MAX_VERSION_BYTES": "1GiB",
		"KAZARI_LIFECYCLE_DEFAULT_GRACE":  "7d",
		"KAZARI_SERVE_RESERVED_SLUGS":     "api, mcp ,admin",
		"KAZARI_TLS_DNS_API_TOKEN":        "dns",
		"KAZARI_AUTH_ADMIN_TOKEN":         testAdminToken,
		"KAZARI_AUTH_OIDC_ISSUER":         "https://accounts.google.com",
		"KAZARI_AUTH_OIDC_CLIENT_ID":      "cid",
		"KAZARI_AUTH_OIDC_CLIENT_SECRET":  "csecret",
		"KAZARI_EXTERNAL_PORT":            "8080",
	}
	c, err := LoadWith("", env(vars))
	if err != nil {
		t.Fatalf("LoadWith: %v", err)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"base_domain", c.BaseDomain, "demo.test"},
		{"data_dir", c.DataDir, "/srv/kazari"},
		{"listen.proxy_mode", c.Listen.ProxyMode, true},
		{"listen.http", c.Listen.HTTP, "127.0.0.1:8080"},
		{"limits.max_files", c.Limits.MaxFiles, 10},
		{"limits.max_version_bytes", c.Limits.MaxVersionBytes, GiB},
		{"lifecycle.default_grace", c.Lifecycle.DefaultGrace, Duration(7 * 24 * time.Hour)},
		{"serve.reserved_slugs", c.Serve.ReservedSlugs, []string{"api", "mcp", "admin"}},
		{"tls.dns.api_token", c.TLS.DNS.APIToken, "dns"},
		{"auth.admin_token", c.Auth.AdminToken, testAdminToken},
		{"external_port", c.ExternalPort, 8080},
	}
	for _, ch := range checks {
		if !reflect.DeepEqual(ch.got, ch.want) {
			t.Errorf("%s = %#v, want %#v", ch.name, ch.got, ch.want)
		}
	}
	if c.Auth.OIDC == nil {
		t.Fatal("OIDC not materialised")
	}
	want := OIDC{Issuer: "https://accounts.google.com", ClientID: "cid", ClientSecret: "csecret", AutoProvision: true}
	if !reflect.DeepEqual(*c.Auth.OIDC, want) {
		t.Errorf("OIDC = %+v, want %+v", *c.Auth.OIDC, want)
	}
}

func TestEnvOverridesFile(t *testing.T) {
	path := writeYAML(t, "base_domain: from.file\ndata_dir: /d\ntls:\n  mode: self-signed\n")
	c, err := LoadWith(path, env(map[string]string{"KAZARI_BASE_DOMAIN": "from.env"}))
	if err != nil {
		t.Fatalf("LoadWith: %v", err)
	}
	if c.BaseDomain != "from.env" {
		t.Errorf("BaseDomain = %q, want from.env", c.BaseDomain)
	}
	if c.Auth.OIDC != nil {
		t.Errorf("OIDC = %+v, want nil without any KAZARI_AUTH_OIDC_* variable", c.Auth.OIDC)
	}
}

func TestEnvBadValues(t *testing.T) {
	tests := []struct{ key, value string }{
		{"KAZARI_LISTEN_PROXY_MODE", "maybe"},
		{"KAZARI_LIMITS_MAX_FILES", "many"},
		{"KAZARI_LIMITS_MAX_FILE_BYTES", "5 parsecs"},
		{"KAZARI_LIFECYCLE_RETENTION", "soon"},
		{"KAZARI_AUTH_OIDC_AUTO_PROVISION", "yes please"},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			_, err := LoadWith("", env(map[string]string{tt.key: tt.value}))
			if err == nil || !strings.Contains(err.Error(), tt.key) {
				t.Fatalf("err = %v, want one naming %s", err, tt.key)
			}
		})
	}
}

func TestEnvNamesUnique(t *testing.T) {
	seen := map[string]bool{}
	var walk func(reflect.Type, string)
	walk = func(rt reflect.Type, prefix string) {
		for i := range rt.NumField() {
			f := rt.Field(i)
			name := keyName(f)
			if name == "" {
				continue
			}
			key := prefix + "_" + strings.ToUpper(name)
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				walk(ft, key)
				continue
			}
			if seen[key] {
				t.Errorf("duplicate environment variable %s", key)
			}
			seen[key] = true
		}
	}
	walk(reflect.TypeFor[Config](), envPrefix)
	for _, k := range []string{"KAZARI_TLS_DNS_API_TOKEN", "KAZARI_TLS_DNS_PROVIDER", "KAZARI_AUTH_OIDC_CLIENT_SECRET"} {
		if !seen[k] {
			t.Errorf("missing %s", k)
		}
	}
}

func TestParseBytes(t *testing.T) {
	tests := []struct {
		in   string
		want Bytes
	}{
		{"0", 0},
		{"52428800", 50 * MiB},
		{"50MiB", 50 * MiB},
		{"2GiB", 2 * GiB},
		{"4 MiB", 4 * MiB},
		{"4mib", 4 * MiB},
		{"1KiB", 1024},
		{"1TiB", TiB},
		{"10B", 10},
		{"1KB", 1000},
		{"3mb", 3_000_000},
		{"1GB", 1_000_000_000},
		{"2TB", 2_000_000_000_000},
		{" 7 KiB ", 7 * KiB},
	}
	for _, tt := range tests {
		got, err := ParseBytes(tt.in)
		if err != nil || got != tt.want {
			t.Errorf("ParseBytes(%q) = %d, %v; want %d", tt.in, got, err, tt.want)
		}
	}

	for _, in := range []string{"", "MiB", "-1MiB", "1.5GiB", "10 PiB", "1 MiBs", "abc", "9999999999TiB", "99999999999999999999"} {
		if got, err := ParseBytes(in); err == nil {
			t.Errorf("ParseBytes(%q) = %d, want error", in, got)
		}
	}
}

func TestBytesString(t *testing.T) {
	tests := []struct {
		in   Bytes
		want string
	}{
		{0, "0B"},
		{1000, "1000B"},
		{1024, "1KiB"},
		{1536 * KiB, "1536KiB"},
		{50 * MiB, "50MiB"},
		{2 * GiB, "2GiB"},
		{3 * TiB, "3TiB"},
	}
	for _, tt := range tests {
		if got := tt.in.String(); got != tt.want {
			t.Errorf("Bytes(%d).String() = %q, want %q", int64(tt.in), got, tt.want)
		}
		var back Bytes
		if err := back.UnmarshalText([]byte(tt.want)); err != nil || back != tt.in {
			t.Errorf("round trip %q = %d, %v", tt.want, back, err)
		}
	}
}

func TestParseDuration(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"0", 0},
		{"15m", 15 * time.Minute},
		{"1h", time.Hour},
		{"336h", 14 * 24 * time.Hour},
		{"14d", 14 * 24 * time.Hour},
		{"1d12h", 36 * time.Hour},
		{"0d", 0},
		{"1h30m", 90 * time.Minute},
		{"500ms", 500 * time.Millisecond},
	}
	for _, tt := range tests {
		got, err := ParseDuration(tt.in)
		if err != nil || got.Std() != tt.want {
			t.Errorf("ParseDuration(%q) = %v, %v; want %v", tt.in, got, err, tt.want)
		}
	}

	for _, in := range []string{"", "d", "14", "-1d", "+1d", "1.5d", "1d-2h", "1dx", "soon", "999999999d"} {
		if got, err := ParseDuration(in); err == nil {
			t.Errorf("ParseDuration(%q) = %v, want error", in, got)
		}
	}
}

func TestDurationString(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{0, "0s"},
		{time.Hour, "1h"},
		{15 * time.Minute, "15m"},
		{90 * time.Minute, "1h30m"},
		{14 * 24 * time.Hour, "336h"},
		{time.Hour + 30*time.Second, "1h0m30s"},
		{1500 * time.Millisecond, "1.5s"},
	}
	for _, tt := range tests {
		if got := Duration(tt.in).String(); got != tt.want {
			t.Errorf("Duration(%v).String() = %q, want %q", tt.in, got, tt.want)
		}
		var back Duration
		if err := back.UnmarshalText([]byte(tt.want)); err != nil || back.Std() != tt.in {
			t.Errorf("round trip %q = %v, %v", tt.want, back, err)
		}
	}
}

func TestValidateErrors(t *testing.T) {
	oidc := func(c *Config) *OIDC {
		c.Auth.OIDC = &OIDC{Issuer: "https://accounts.google.com", ClientID: "id", ClientSecret: "s", AutoProvision: true}
		return c.Auth.OIDC
	}
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"no base domain", func(c *Config) { c.BaseDomain = "" }, "base_domain: required"},
		{"base domain scheme", func(c *Config) { c.BaseDomain = "https://art.example.com" }, "base_domain: must not include a scheme"},
		{"base domain port", func(c *Config) { c.BaseDomain = "localhost:8080" }, "base_domain: must not include a port"},
		{"base domain dot", func(c *Config) { c.BaseDomain = "art.example.com." }, "base_domain: must not end with a dot"},
		{"base domain case", func(c *Config) { c.BaseDomain = "Art.example.com" }, "base_domain: must be lower-case"},
		{"base domain chars", func(c *Config) { c.BaseDomain = "art..example.com" }, "base_domain: must be a host name"},
		{"no data dir", func(c *Config) { c.DataDir = "" }, "data_dir: required"},
		{"external scheme", func(c *Config) { c.ExternalScheme = "ftp" }, "external_scheme: must be one of http, https"},
		{"external port", func(c *Config) { c.ExternalPort = 70000 }, "external_port:"},
		{"no https listener", func(c *Config) { c.Listen.HTTPS = "" }, "listen.https: required"},
		{"proxy without http", func(c *Config) { c.Listen.ProxyMode = true; c.Listen.HTTP = "" }, "listen.http: required"},
		{"tls mode", func(c *Config) { c.TLS.Mode = "magic" }, "tls.mode: must be one of acme, files, self-signed"},
		{"acme email", func(c *Config) { c.TLS.Email = "" }, "tls.email: required"},
		{"acme ca", func(c *Config) { c.TLS.CA = "http://ca.example/dir" }, "tls.ca:"},
		{"dns provider", func(c *Config) { c.TLS.DNSProvider = "bind" }, "tls.dns_provider: must be one of"},
		{"dns token", func(c *Config) { c.TLS.DNS.APIToken = "" }, "tls.dns.api_token: required"},
		{"cert file", func(c *Config) { c.TLS.Mode = "files"; c.TLS.KeyFile = "k" }, "tls.cert_file: required"},
		{"key file", func(c *Config) { c.TLS.Mode = "files"; c.TLS.CertFile = "c" }, "tls.key_file: required"},
		{"limit zero", func(c *Config) { c.Limits.MaxFiles = 0 }, "limits.max_files: must be greater than 0"},
		{"limit negative", func(c *Config) { c.Limits.UploadTTL = -1 }, "limits.upload_ttl: must be greater than 0"},
		{"file over version", func(c *Config) { c.Limits.MaxFileBytes = 100 * MiB }, "limits.max_file_bytes: must not exceed"},
		{"chunk over version", func(c *Config) { c.Limits.MaxChunkBytes = 100 * MiB }, "limits.max_chunk_bytes: must not exceed"},
		{"lifecycle zero", func(c *Config) { c.Lifecycle.GCInterval = 0 }, "lifecycle.gc_interval: must be greater than 0"},
		{"reserved slug case", func(c *Config) { c.Serve.ReservedSlugs = []string{"API"} }, "serve.reserved_slugs:"},
		{"default visibility", func(c *Config) { c.Serve.DefaultVisibility = "private" }, "serve.default_visibility: must be one of public, unlisted, members"},
		{"share link lifetime", func(c *Config) { c.Serve.ShareLinkMaxTTL = 0 }, "serve.share_link_max_ttl: must be greater than 0"},
		{"write policy", func(c *Config) { c.Auth.WritePolicy = "anyone" }, "auth.write_policy: must be one of shared, owner"},
		{"oidc issuer", func(c *Config) { oidc(c).Issuer = "http://idp.example" }, "auth.oidc.issuer: must be an https URL"},
		{"oidc client id", func(c *Config) { oidc(c).ClientID = "" }, "auth.oidc.client_id: required"},
		{"oidc client secret", func(c *Config) { oidc(c).ClientSecret = "" }, "auth.oidc.client_secret: required"},
		{"log level", func(c *Config) { c.Log.Level = "loud" }, "log.level: must be one of debug, info, warn, error"},
		{"log format", func(c *Config) { c.Log.Format = "xml" }, "log.format: must be one of text, json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := valid()
			tt.mutate(&c)
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tt.want)
			}
		})
	}
}

func TestValidateCollectsAll(t *testing.T) {
	c := Default()
	c.Log.Level = "loud"
	err := c.Validate()
	if err == nil {
		t.Fatal("want error")
	}
	for _, key := range []string{"base_domain:", "data_dir:", "tls.email:", "tls.dns.api_token:", "log.level:"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error does not mention %s:\n%v", key, err)
		}
	}
}

func TestRedacted(t *testing.T) {
	c := valid()
	c.Auth.OIDC = &OIDC{Issuer: "https://accounts.google.com", ClientID: "id", ClientSecret: "csecret", AllowedDomains: []string{"example.com"}}
	m := c.Redacted()

	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("json: %v", err)
	}
	for _, secret := range []string{"admin-secret", "dns-secret", "csecret"} {
		if bytes.Contains(data, []byte(secret)) {
			t.Errorf("redacted output contains %q: %s", secret, data)
		}
	}

	section := func(m map[string]any, key string) map[string]any {
		t.Helper()
		s, ok := m[key].(map[string]any)
		if !ok {
			t.Fatalf("%s is %T, want map", key, m[key])
		}
		return s
	}
	auth := section(m, "auth")
	tls := section(m, "tls")
	checks := []struct {
		name      string
		got, want any
	}{
		{"auth.admin_token", auth["admin_token"], "***"},
		{"tls.dns.api_token", section(tls, "dns")["api_token"], "***"},
		{"auth.oidc.client_secret", section(auth, "oidc")["client_secret"], "***"},
		{"auth.oidc.client_id", section(auth, "oidc")["client_id"], "id"},
		{"base_domain", m["base_domain"], "art.example.com"},
		{"limits.max_version_bytes", section(m, "limits")["max_version_bytes"], "50MiB"},
		{"lifecycle.default_grace", section(m, "lifecycle")["default_grace"], "336h"},
		{"limits.max_files", section(m, "limits")["max_files"], 2000},
		{"listen.proxy_mode", section(m, "listen")["proxy_mode"], false},
	}
	for _, ch := range checks {
		if !reflect.DeepEqual(ch.got, ch.want) {
			t.Errorf("%s = %#v, want %#v", ch.name, ch.got, ch.want)
		}
	}

	d := Default()
	m = d.Redacted()
	if got := section(m, "auth")["admin_token"]; got != "" {
		t.Errorf("empty admin_token = %#v, want empty", got)
	}
	if got, ok := section(m, "auth")["oidc"]; !ok || got != nil {
		t.Errorf("oidc = %#v, want nil", got)
	}
}

func TestPathsAndURLs(t *testing.T) {
	c := valid()
	c.DataDir = "/data"
	checks := []struct{ name, got, want string }{
		{"DBPath", c.DBPath(), "/data/kazari.db"},
		{"BlobDir", c.BlobDir(), "/data/blobs"},
		{"UploadDir", c.UploadDir(), "/data/uploads"},
		{"CertStorageDir", c.CertStorageDir(), "/data/certmagic"},
		{"Host", c.Host("demo"), "demo.art.example.com"},
		{"Host empty", c.Host(""), "art.example.com"},
		{"ExternalURL", c.ExternalURL("mcp"), "https://mcp.art.example.com/"},
	}
	dev := c
	dev.BaseDomain = "localhost"
	dev.ExternalScheme = "http"
	dev.ExternalPort = 8080
	checks = append(checks, struct{ name, got, want string }{"ExternalURL dev", dev.ExternalURL("demo"), "http://demo.localhost:8080/"})
	std := c
	std.ExternalPort = 443
	checks = append(checks, struct{ name, got, want string }{"ExternalURL default port", std.ExternalURL("demo"), "https://demo.art.example.com/"})
	for _, ch := range checks {
		if ch.got != ch.want {
			t.Errorf("%s = %q, want %q", ch.name, ch.got, ch.want)
		}
	}
}

func TestLogger(t *testing.T) {
	tests := []struct {
		level, format string
		want          slog.Level
	}{
		{"debug", "text", slog.LevelDebug},
		{"info", "json", slog.LevelInfo},
		{"warn", "text", slog.LevelWarn},
		{"error", "json", slog.LevelError},
	}
	for _, tt := range tests {
		c := valid()
		c.Log.Level, c.Log.Format = tt.level, tt.format
		if got := c.SlogLevel(); got != tt.want {
			t.Errorf("SlogLevel(%s) = %v, want %v", tt.level, got, tt.want)
		}
		var buf bytes.Buffer
		l := c.Logger(&buf)
		l.Debug("hidden-unless-debug")
		l.Error("shown")
		out := buf.String()
		if strings.Contains(out, "hidden-unless-debug") != (tt.level == "debug") {
			t.Errorf("level %s: debug line visibility wrong: %q", tt.level, out)
		}
		if strings.HasPrefix(out, "{") != (tt.format == "json") {
			t.Errorf("format %s: output %q", tt.format, out)
		}
	}
}

func TestAdminTokenLength(t *testing.T) {
	tests := []struct {
		base, token string
		ok          bool
	}{
		{"art.example.com", "change-me", false},
		{"art.example.com", testAdminToken[:MinAdminTokenLen-1], false},
		{"art.example.com", testAdminToken, true},
		{"art.example.com", "", true},
		{"localhost", "dev-admin-token", true},
		{"dev.localhost", "dev-admin-token", true},
		{"localhost.example.com", "dev-admin-token", false},
	}
	for _, tt := range tests {
		c := valid()
		c.BaseDomain, c.Auth.AdminToken = tt.base, tt.token
		err := c.Validate()
		if tt.ok != (err == nil) {
			t.Errorf("base %s, token %q: Validate = %v, want ok %v", tt.base, tt.token, err, tt.ok)
		}
		if err != nil && !strings.Contains(err.Error(), "auth.admin_token: must be at least 32 characters") {
			t.Errorf("base %s, token %q: error %q does not name auth.admin_token", tt.base, tt.token, err)
		}
	}
}
