// Package config loads and validates Kazari's configuration from a YAML file
// with ${NAME} references, an optional .env file and KAZARI_* overrides. No
// other package reads settings from files or the environment.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultCSP is the default serve.csp.
const DefaultCSP = "default-src 'self'; " +
	"script-src 'self' 'unsafe-inline' https://cdnjs.cloudflare.com https://cdn.jsdelivr.net https://unpkg.com; " +
	"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; " +
	"font-src 'self' https://fonts.gstatic.com; " +
	"img-src 'self' data: https:; " +
	"connect-src 'self'; " +
	"frame-ancestors 'none'"

var defaultReservedSlugs = []string{
	"api", "mcp", "admin", "www", "mail", "ns1", "ns2", "autoconfig", "autodiscover", "localhost",
}

// Config is the effective configuration. The yaml tags also name the KAZARI_*
// overrides, and fields tagged secret:"true" are redacted.
type Config struct {
	// Lower-case, without scheme, port or trailing dot.
	BaseDomain     string `yaml:"base_domain"`
	DataDir        string `yaml:"data_dir"`
	ExternalScheme string `yaml:"external_scheme"`
	// 0 or the scheme's default port is left out of public URLs.
	ExternalPort int `yaml:"external_port"`

	Listen    Listen    `yaml:"listen"`
	TLS       TLS       `yaml:"tls"`
	Limits    Limits    `yaml:"limits"`
	Lifecycle Lifecycle `yaml:"lifecycle"`
	Serve     Serve     `yaml:"serve"`
	Fetch     Fetch     `yaml:"fetch"`
	Auth      Auth      `yaml:"auth"`
	Admin     Admin     `yaml:"admin"`
	Log       Log       `yaml:"log"`
	Telemetry Telemetry `yaml:"telemetry"`
}

// Listen holds the public listener addresses.
type Listen struct {
	HTTPS string `yaml:"https"`
	// Redirects to HTTPS, or is the only listener in proxy mode; empty
	// disables the redirect.
	HTTP string `yaml:"http"`
	// Plain HTTP only, trusting X-Forwarded-* from a TLS-terminating proxy.
	ProxyMode bool `yaml:"proxy_mode"`
}

// TLS selects where certificates come from.
type TLS struct {
	Mode        string `yaml:"mode"`
	Email       string `yaml:"email"`
	CA          string `yaml:"ca"`
	DNSProvider string `yaml:"dns_provider"`
	CertFile    string `yaml:"cert_file"`
	KeyFile     string `yaml:"key_file"`
	DNS         DNS    `yaml:"dns"`
}

// DNS holds the DNS provider credentials. Which keys a provider reads is
// documented with certs.NewDNSProvider.
type DNS struct {
	// Unused for route53, which reads the AWS credential chain.
	APIToken string `yaml:"api_token" secret:"true"`
	// ovh: the API endpoint, ovh-eu by default.
	Endpoint string `yaml:"endpoint"`
	// namecheap: the API user.
	User string `yaml:"user"`
	// namecheap: the allow-listed client IP; discovered when empty.
	ClientIP string `yaml:"client_ip"`
}

// Limits bounds uploads, metadata and write rates.
type Limits struct {
	MaxVersionBytes     Bytes    `yaml:"max_version_bytes"`
	MaxFileBytes        Bytes    `yaml:"max_file_bytes"`
	MaxFiles            int      `yaml:"max_files"`
	MetadataMaxPairs    int      `yaml:"metadata_max_pairs"`
	WriteRatePerMinute  int      `yaml:"write_rate_per_minute"`
	DefaultUserQuota    Bytes    `yaml:"default_user_quota"`
	InlineGuidanceBytes Bytes    `yaml:"inline_guidance_bytes"`
	MaxChunkBytes       Bytes    `yaml:"max_chunk_bytes"`
	UploadTTL           Duration `yaml:"upload_ttl"`
	MaxOpenUploads      int      `yaml:"max_open_uploads"`
}

// Lifecycle sets the stale, retention and garbage-collection timings.
type Lifecycle struct {
	DefaultGrace  Duration `yaml:"default_grace"`
	Retention     Duration `yaml:"retention"`
	Tombstone     Duration `yaml:"tombstone"`
	SweepInterval Duration `yaml:"sweep_interval"`
	GCInterval    Duration `yaml:"gc_interval"`
}

// Serve controls headers on served artifacts, which slugs are reserved and
// how artifacts are shared.
type Serve struct {
	CSP               string   `yaml:"csp"` // empty sends none
	AllowCSPOverride  bool     `yaml:"allow_csp_override"`
	ReservedSlugs     []string `yaml:"reserved_slugs"`
	DefaultVisibility string   `yaml:"default_visibility"`
	ShareLinkMaxTTL   Duration `yaml:"share_link_max_ttl"`
}

// Fetch bounds the URLs the server fetches for members: files named by url
// and pages imported with import_page.
type Fetch struct {
	Enabled bool `yaml:"enabled"`
	// Timeout bounds one request, Budget every request of one import.
	Timeout Duration `yaml:"timeout"`
	Budget  Duration `yaml:"budget"`
}

// Auth holds the write policy, the admin token and the optional identity provider.
type Auth struct {
	WritePolicy string `yaml:"write_policy"`
	AdminToken  string `yaml:"admin_token" secret:"true"`
	// Nil when auth.oidc is absent: sign-in falls back to API keys.
	OIDC *OIDC `yaml:"oidc"`
}

// OIDC configures the upstream OpenID Connect provider users sign in with.
type OIDC struct {
	Issuer         string   `yaml:"issuer"`
	ClientID       string   `yaml:"client_id"`
	ClientSecret   string   `yaml:"client_secret" secret:"true"`
	AllowedDomains []string `yaml:"allowed_domains"`
	AutoProvision  bool     `yaml:"auto_provision"` // true by default once auth.oidc is present
	AdminEmails    []string `yaml:"admin_emails"`
}

// applyDefaults sets the defaults of a present auth.oidc mapping.
func (o *OIDC) applyDefaults() {
	o.AutoProvision = true
}

// Admin configures the admin host.
type Admin struct {
	// When set, the admin host is served only on this listener.
	Listen string `yaml:"listen"`
	// Unix socket that serves the admin API without a token, relative to
	// data_dir unless absolute; empty disables it.
	Socket string `yaml:"socket"`
}

// DefaultAdminSocket is admin.socket's default, under data_dir.
const DefaultAdminSocket = "admin.sock"

// Log configures the slog logger.
type Log struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// Default returns the built-in defaults; BaseDomain and DataDir are left
// empty and must be set.
func Default() Config {
	return Config{
		ExternalScheme: "https",
		Listen: Listen{
			HTTPS: ":443",
			HTTP:  ":80",
		},
		TLS: TLS{
			Mode:        "acme",
			CA:          "letsencrypt",
			DNSProvider: "cloudflare",
		},
		Limits: Limits{
			MaxVersionBytes:     50 * MiB,
			MaxFileBytes:        25 * MiB,
			MaxFiles:            2000,
			MetadataMaxPairs:    64,
			WriteRatePerMinute:  60,
			DefaultUserQuota:    2 * GiB,
			InlineGuidanceBytes: 2 * MiB,
			MaxChunkBytes:       4 * MiB,
			UploadTTL:           Duration(1 * hour),
			MaxOpenUploads:      5,
		},
		Lifecycle: Lifecycle{
			DefaultGrace:  Duration(14 * day),
			Retention:     Duration(30 * day),
			Tombstone:     Duration(90 * day),
			SweepInterval: Duration(15 * minute),
			GCInterval:    Duration(1 * hour),
		},
		Serve: Serve{
			CSP:               DefaultCSP,
			AllowCSPOverride:  true,
			ReservedSlugs:     slices.Clone(defaultReservedSlugs),
			DefaultVisibility: "public",
			ShareLinkMaxTTL:   Duration(90 * day),
		},
		Fetch: Fetch{
			Enabled: true,
			Timeout: Duration(15 * second),
			Budget:  Duration(1 * minute),
		},
		Auth: Auth{
			WritePolicy: "shared",
		},
		Admin: Admin{
			Socket: DefaultAdminSocket,
		},
		Log: Log{
			Level:  "info",
			Format: "text",
		},
		Telemetry: defaultTelemetry(),
	}
}

// Local returns the preset of kazari serve --local: plain HTTP on
// 127.0.0.1:8080 under the base domain localhost, which browsers resolve to
// the local machine, with the data in ./.data. No DNS, certificate or token
// is needed; the admin API is reached over the socket.
func Local() Config {
	c := Default()
	c.BaseDomain = "localhost"
	c.DataDir = "./.data"
	c.ExternalScheme = "http"
	c.ExternalPort = 8080
	c.Listen = Listen{HTTP: "127.0.0.1:8080", ProxyMode: true}
	c.TLS.Mode = "self-signed"
	c.Log.Level = "debug"
	return c
}

// DefaultEnvFile is the dotenv file Load reads by default.
const DefaultEnvFile = ".env"

// Load is LoadWith over the process environment and a dotenv file: envFile,
// which must exist, or else DefaultEnvFile if it exists. The process
// environment wins over the dotenv file.
func Load(path, envFile string) (Config, error) {
	dotenv, err := loadDotenv(envFile)
	if err != nil {
		return Config{}, err
	}
	return LoadWith(path, EnvLookup(dotenv))
}

func loadDotenv(envFile string) (map[string]string, error) {
	if envFile != "" {
		return LoadEnvFile(envFile)
	}
	m, err := LoadEnvFile(DefaultEnvFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return m, err
}

// LoadLocal is Load for the Local preset: no file, the dotenv file as Load
// reads it, and KAZARI_* overrides on top of the preset.
func LoadLocal(envFile string) (Config, error) {
	dotenv, err := loadDotenv(envFile)
	if err != nil {
		return Config{}, err
	}
	return LoadWithBase(Local(), "", EnvLookup(dotenv))
}

// LoadWith applies, in order, Default, the YAML file at path (skipped when
// empty; a missing file satisfies fs.ErrNotExist; unknown keys are errors),
// the KAZARI_<PATH> overrides (lists comma-separated; any KAZARI_AUTH_OIDC_*
// enables auth.oidc; a secret also reads KAZARI_<PATH>_FILE), then Validate.
// lookup resolves both ${NAME} references and overrides.
func LoadWith(path string, lookup func(key string) (string, bool)) (Config, error) {
	return LoadWithBase(Default(), path, lookup)
}

// LoadWithBase is LoadWith starting from base instead of Default.
func LoadWithBase(base Config, path string, lookup func(key string) (string, bool)) (Config, error) {
	cfg, err := load(base, path, lookup)
	if err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("config: invalid:\n%w", err)
	}
	return cfg, nil
}

// Peek is LoadWith without Validate: enough for a client on the server's
// host to learn where the server keeps its data and its admin socket. A
// ${NAME} reference to a variable the client does not have, such as a
// secret only the service reads, expands to "" instead of failing.
func Peek(path string, lookup func(key string) (string, bool)) (Config, error) {
	cfg := Default()
	if path != "" {
		lenient := func(key string) (string, bool) {
			if v, ok := lookup(key); ok {
				return v, true
			}
			return "", true
		}
		if err := decodeFile(path, &cfg, lenient); err != nil {
			return Config{}, err
		}
	}
	if err := applyEnv(&cfg, lookup); err != nil {
		return Config{}, fmt.Errorf("config: environment: %w", err)
	}
	return cfg, nil
}

func load(base Config, path string, lookup func(key string) (string, bool)) (Config, error) {
	cfg := base
	if path != "" {
		if err := decodeFile(path, &cfg, lookup); err != nil {
			return Config{}, err
		}
	}
	if err := applyEnv(&cfg, lookup); err != nil {
		return Config{}, fmt.Errorf("config: environment: %w", err)
	}
	return cfg, nil
}

func decodeFile(path string, cfg *Config, lookup func(string) (string, bool)) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("config: %s: %w", path, err)
	}
	if doc.Kind == 0 {
		return nil // empty, or nothing but comments
	}
	if err := checkKeys(&doc, path); err != nil {
		return err
	}
	if err := expandNode(&doc, path, lookup); err != nil {
		return err
	}
	expanded, err := yaml.Marshal(&doc)
	if err != nil {
		return fmt.Errorf("config: %s: %w", path, err)
	}
	// Pre-filled so its defaults survive a mapping that omits them; dropped
	// below if the mapping is absent or null.
	oidc := &OIDC{}
	oidc.applyDefaults()
	cfg.Auth.OIDC = oidc
	dec := yaml.NewDecoder(bytes.NewReader(expanded))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("config: %s: %w", path, withoutLines(err))
	}
	var probe struct {
		Auth struct {
			OIDC *yaml.Node `yaml:"oidc"`
		} `yaml:"auth"`
	}
	if err := doc.Decode(&probe); err != nil {
		return fmt.Errorf("config: %s: %w", path, err)
	}
	if probe.Auth.OIDC == nil {
		cfg.Auth.OIDC = nil
	}
	return nil
}

var yamlLine = regexp.MustCompile(`^line \d+: `)

// withoutLines drops line numbers from a decoding error: they refer to the
// re-encoded document, not to the file.
func withoutLines(err error) error {
	var te *yaml.TypeError
	if !errors.As(err, &te) {
		return err
	}
	out := &yaml.TypeError{Errors: make([]string, len(te.Errors))}
	for i, e := range te.Errors {
		out.Errors[i] = yamlLine.ReplaceAllString(e, "")
	}
	return out
}

// DBPath is the SQLite database file.
func (c Config) DBPath() string { return filepath.Join(c.DataDir, "kazari.db") }

// BlobDir is the content-addressed blob store root.
func (c Config) BlobDir() string { return filepath.Join(c.DataDir, "blobs") }

// UploadDir is where chunked uploads are staged.
func (c Config) UploadDir() string { return filepath.Join(c.DataDir, "uploads") }

// CertStorageDir is CertMagic's storage root.
func (c Config) CertStorageDir() string { return filepath.Join(c.DataDir, "certmagic") }

// AdminSocketPath is the admin Unix socket, resolved against data_dir, or
// "" when admin.socket is empty.
func (c Config) AdminSocketPath() string {
	if c.Admin.Socket == "" || filepath.IsAbs(c.Admin.Socket) {
		return c.Admin.Socket
	}
	return filepath.Join(c.DataDir, c.Admin.Socket)
}

// Host returns sub.base_domain, or base_domain itself when sub is empty.
func (c Config) Host(sub string) string {
	if sub == "" {
		return c.BaseDomain
	}
	return sub + "." + c.BaseDomain
}

// ExternalURL returns the public root URL of host sub, ending in "/", such as
// http://demo.localhost:8080/.
func (c Config) ExternalURL(sub string) string {
	scheme := c.ExternalScheme
	if scheme == "" {
		scheme = "https"
	}
	host := c.Host(sub)
	port := c.ExternalPort
	if port != 0 && !(scheme == "https" && port == 443) && !(scheme == "http" && port == 80) {
		host += ":" + strconv.Itoa(port)
	}
	return scheme + "://" + host + "/"
}

// SlogLevel returns log.level as a slog.Level (info when unrecognised).
func (c Config) SlogLevel() slog.Level { return ParseLevel(c.Log.Level) }

// ParseLevel returns a log.level value as a slog.Level (info when
// unrecognised).
func ParseLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Logger returns a logger writing to w in log.format at log.level.
func (c Config) Logger(w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: c.SlogLevel()}
	if c.Log.Format == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// Redacted returns the configuration as a JSON-friendly map keyed like the
// YAML file, with set secrets as "***", sizes and durations as strings
// ("50MiB", "336h") and a missing auth.oidc as nil.
func (c Config) Redacted() map[string]any {
	return redactStruct(reflect.ValueOf(c))
}

func redactStruct(v reflect.Value) map[string]any {
	t := v.Type()
	m := make(map[string]any, t.NumField())
	for i := range t.NumField() {
		f := t.Field(i)
		name := keyName(f)
		if name == "" {
			continue
		}
		m[name] = redactValue(v.Field(i), f.Tag.Get("secret") == "true")
	}
	return m
}

func redactValue(v reflect.Value, secret bool) any {
	// Before the Stringer check, so a secret type with a String method never
	// prints its value.
	if secret {
		return redactSecret(v)
	}
	if s, ok := v.Interface().(fmt.Stringer); ok {
		return s.String()
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return nil
		}
		return redactValue(v.Elem(), secret)
	case reflect.Struct:
		return redactStruct(v)
	case reflect.Slice:
		out := make([]any, v.Len())
		for i := range v.Len() {
			out[i] = redactValue(v.Index(i), secret)
		}
		return out
	case reflect.String:
		return v.String()
	default:
		return v.Interface()
	}
}

// redactSecret keeps the keys of a map, such as header names, and whether a
// value is set.
func redactSecret(v reflect.Value) any {
	switch {
	case v.Kind() == reflect.Map:
		out := make(map[string]any, v.Len())
		for _, k := range v.MapKeys() {
			out[fmt.Sprint(k.Interface())] = "***"
		}
		return out
	case v.IsZero():
		return ""
	default:
		return "***"
	}
}

// keyName returns the YAML key of a struct field, or "" to skip it.
func keyName(f reflect.StructField) string {
	if !f.IsExported() {
		return ""
	}
	name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
	if name == "-" || name == "" {
		return ""
	}
	return name
}
