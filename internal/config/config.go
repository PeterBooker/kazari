// Package config loads, validates and describes Kazari's configuration: one
// YAML file whose ${NAME} references are filled from the environment, an
// optional .env file, and KAZARI_* environment overrides. Nothing else in
// the code reads files or the environment for settings; internal/app passes
// the loaded Config to every constructor.
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

// DefaultCSP is the Content-Security-Policy sent with every artifact unless
// serve.csp or a permitted per-artifact override replaces it.
const DefaultCSP = "default-src 'self'; " +
	"script-src 'self' 'unsafe-inline' https://cdnjs.cloudflare.com https://cdn.jsdelivr.net https://unpkg.com; " +
	"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; " +
	"font-src 'self' https://fonts.gstatic.com; " +
	"img-src 'self' data: https:; " +
	"connect-src 'self'; " +
	"frame-ancestors 'none'"

// defaultReservedSlugs are the hosts no artifact may claim.
var defaultReservedSlugs = []string{
	"api", "mcp", "admin", "www", "mail", "ns1", "ns2", "autoconfig", "autodiscover", "localhost",
}

// Config is the effective configuration. Field tags give the YAML key; the
// environment variable for a key is KAZARI_ followed by the key's full path,
// upper-cased and joined with underscores (see LoadWith).
type Config struct {
	// BaseDomain is the domain every artifact is served under, as
	// <slug>.<base_domain>. Lower-case, no scheme, port or trailing dot.
	BaseDomain string `yaml:"base_domain"`
	// DataDir holds kazari.db, blobs/, uploads/ and certmagic/.
	DataDir string `yaml:"data_dir"`
	// ExternalScheme is the scheme used when building public URLs:
	// "https" (default) or "http" for a plain-HTTP development setup.
	ExternalScheme string `yaml:"external_scheme"`
	// ExternalPort is appended to public URLs when non-zero and not the
	// scheme's default port, e.g. 8080 for http://demo.localhost:8080/.
	ExternalPort int `yaml:"external_port"`

	Listen    Listen    `yaml:"listen"`
	TLS       TLS       `yaml:"tls"`
	Limits    Limits    `yaml:"limits"`
	Lifecycle Lifecycle `yaml:"lifecycle"`
	Serve     Serve     `yaml:"serve"`
	Auth      Auth      `yaml:"auth"`
	Admin     Admin     `yaml:"admin"`
	Log       Log       `yaml:"log"`
}

// Listen holds the public listener addresses.
type Listen struct {
	// HTTPS is the TLS listener address, e.g. ":443".
	HTTPS string `yaml:"https"`
	// HTTP is the plain listener: a redirect to HTTPS normally, the only
	// listener in proxy mode. Empty disables it outside proxy mode.
	HTTP string `yaml:"http"`
	// ProxyMode serves plain HTTP on HTTP and trusts X-Forwarded-* headers
	// from a TLS-terminating reverse proxy.
	ProxyMode bool `yaml:"proxy_mode"`
}

// TLS selects where certificates come from.
type TLS struct {
	// Mode is acme, files or self-signed.
	Mode string `yaml:"mode"`
	// Email is the ACME account contact.
	Email string `yaml:"email"`
	// CA is letsencrypt, letsencrypt-staging or an https ACME directory URL.
	CA string `yaml:"ca"`
	// DNSProvider is the libdns provider for DNS-01: cloudflare, route53 or hetzner.
	DNSProvider string `yaml:"dns_provider"`
	// CertFile and KeyFile are PEM files for mode files.
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	DNS      DNS    `yaml:"dns"`
}

// DNS holds the DNS provider credentials.
type DNS struct {
	// APIToken is the provider's API token (unused for route53, which reads
	// the standard AWS credential chain).
	APIToken string `yaml:"api_token" secret:"true"`
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

// Serve controls headers on served artifacts and which slugs are reserved.
type Serve struct {
	// CSP is the instance default Content-Security-Policy; empty sends none.
	CSP string `yaml:"csp"`
	// AllowCSPOverride lets kazari.csp metadata replace CSP per artifact.
	AllowCSPOverride bool `yaml:"allow_csp_override"`
	// ReservedSlugs may not be used as artifact slugs.
	ReservedSlugs []string `yaml:"reserved_slugs"`
}

// Auth holds the write policy, the admin token and the optional identity provider.
type Auth struct {
	// WritePolicy is shared (any writer may update any artifact) or owner.
	WritePolicy string `yaml:"write_policy"`
	// AdminToken is accepted as a bearer token on the admin host.
	AdminToken string `yaml:"admin_token" secret:"true"`
	// OIDC is nil when the auth.oidc mapping is absent, which falls back
	// to API-key sign-in on the OAuth consent page.
	OIDC *OIDC `yaml:"oidc"`
}

// OIDC configures the upstream OpenID Connect provider users sign in with.
type OIDC struct {
	// Issuer is the provider's https issuer URL, used for discovery.
	Issuer       string `yaml:"issuer"`
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret" secret:"true"`
	// AllowedDomains are the verified email domains that may sign in.
	AllowedDomains []string `yaml:"allowed_domains"`
	// AutoProvision creates a member on first sign-in from an allowed
	// domain. Defaults to true when the mapping is present.
	AutoProvision bool `yaml:"auto_provision"`
	// AdminEmails are provisioned with the admin role.
	AdminEmails []string `yaml:"admin_emails"`
}

// applyDefaults fills the defaults that apply once the auth.oidc mapping exists.
func (o *OIDC) applyDefaults() {
	o.AutoProvision = true
}

// Admin configures the admin host.
type Admin struct {
	// Listen is an optional listener for the admin host only, e.g.
	// "127.0.0.1:9443". When it is set, the public listener no longer
	// serves the admin host.
	Listen string `yaml:"listen"`
}

// Log configures the slog logger.
type Log struct {
	// Level is debug, info, warn or error.
	Level string `yaml:"level"`
	// Format is text or json.
	Format string `yaml:"format"`
}

// Default returns the configuration used before the file and environment
// are applied. BaseDomain and DataDir are empty and must be set.
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
			CSP:              DefaultCSP,
			AllowCSPOverride: true,
			ReservedSlugs:    slices.Clone(defaultReservedSlugs),
		},
		Auth: Auth{
			WritePolicy: "shared",
		},
		Log: Log{
			Level:  "info",
			Format: "text",
		},
	}
}

// DefaultEnvFile is the dotenv file Load reads when no env file is named
// and the file exists in the working directory.
const DefaultEnvFile = ".env"

// Load reads the configuration from path (skipped when empty), with ${NAME}
// references and KAZARI_* overrides resolved from the process environment
// and a dotenv file, then validates it.
//
// The dotenv file is envFile when it is not empty, and then it must exist;
// otherwise it is ./.env (DefaultEnvFile) if that file exists. Variables in
// the process environment always win over the dotenv file (see EnvLookup).
func Load(path, envFile string) (Config, error) {
	dotenv, err := loadDotenv(envFile)
	if err != nil {
		return Config{}, err
	}
	return LoadWith(path, EnvLookup(dotenv))
}

// loadDotenv reads envFile, or DefaultEnvFile when envFile is empty and
// DefaultEnvFile exists.
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

// LoadWith is Load with the variable lookup supplied by the caller instead
// of the process environment and dotenv file.
//
// The order is: Default, then the YAML file at path if path is not empty,
// then overrides, then Validate. A missing file is an error satisfying
// fs.ErrNotExist. ${NAME} and ${NAME:-default} references in the file's
// scalar values are replaced through lookup (see expandNode); decoding is
// strict, so an unknown key is an error naming it.
//
// Every leaf key has an override named KAZARI_ plus its YAML path
// upper-cased with "." replaced by "_", for example KAZARI_LISTEN_PROXY_MODE
// for listen.proxy_mode and KAZARI_TLS_DNS_API_TOKEN for tls.dns.api_token.
// Overrides are read through lookup too. Lists are comma-separated. Setting
// any KAZARI_AUTH_OIDC_* variable enables the auth.oidc mapping.
func LoadWith(path string, lookup func(key string) (string, bool)) (Config, error) {
	cfg := Default()
	if path != "" {
		if err := decodeFile(path, &cfg, lookup); err != nil {
			return Config{}, err
		}
	}
	if err := applyEnv(&cfg, lookup); err != nil {
		return Config{}, fmt.Errorf("config: environment: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("config: invalid:\n%w", err)
	}
	return cfg, nil
}

// decodeFile decodes the YAML file at path over cfg: it parses the file,
// checks its keys, expands ${NAME} references in scalar values (see
// expandNode), re-encodes the result and decodes that strictly, rejecting
// unknown keys.
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
	// Decode into a pre-filled OIDC so its defaults survive a mapping that
	// omits them, then drop it again if the mapping was absent or null.
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

// yamlLine matches the "line N: " prefix of a yaml.TypeError entry.
var yamlLine = regexp.MustCompile(`^line \d+: `)

// withoutLines drops the line numbers from a decoding error: they refer to
// the re-encoded document, not to the file.
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

// Host returns sub.base_domain, or base_domain itself when sub is empty.
func (c Config) Host(sub string) string {
	if sub == "" {
		return c.BaseDomain
	}
	return sub + "." + c.BaseDomain
}

// ExternalURL returns the public root URL of a host, such as
// https://demo.art.example.com/ or, with external_scheme = "http" and
// external_port = 8080, http://demo.localhost:8080/.
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
func (c Config) SlogLevel() slog.Level {
	switch c.Log.Level {
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
// YAML file, with secrets (tls.dns.api_token, auth.admin_token,
// auth.oidc.client_secret) replaced by "***" when set. Sizes and durations
// are rendered as strings ("50MiB", "336h"); a missing auth.oidc is nil.
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
		if secret && v.String() != "" {
			return "***"
		}
		return v.String()
	default:
		return v.Interface()
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
