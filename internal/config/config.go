// Package config loads, validates and describes Agalma's configuration: one
// TOML file plus AGALMA_* environment overrides. Nothing else in the code
// reads files or the environment for settings; internal/app passes the
// loaded Config to every constructor.
package config

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
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

// Config is the effective configuration. Field tags give the TOML key; the
// environment variable for a key is AGALMA_ followed by the key's full path,
// upper-cased and joined with underscores (see LoadWith).
type Config struct {
	// BaseDomain is the domain every artifact is served under, as
	// <slug>.<base_domain>. Lower-case, no scheme, port or trailing dot.
	BaseDomain string `toml:"base_domain"`
	// DataDir holds agalma.db, blobs/, uploads/ and certmagic/.
	DataDir string `toml:"data_dir"`
	// ExternalScheme is the scheme used when building public URLs:
	// "https" (default) or "http" for a plain-HTTP development setup.
	ExternalScheme string `toml:"external_scheme"`
	// ExternalPort is appended to public URLs when non-zero and not the
	// scheme's default port, e.g. 8080 for http://demo.localhost:8080/.
	ExternalPort int `toml:"external_port"`

	Listen    Listen    `toml:"listen"`
	TLS       TLS       `toml:"tls"`
	Limits    Limits    `toml:"limits"`
	Lifecycle Lifecycle `toml:"lifecycle"`
	Serve     Serve     `toml:"serve"`
	Auth      Auth      `toml:"auth"`
	Admin     Admin     `toml:"admin"`
	Log       Log       `toml:"log"`
}

// Listen holds the public listener addresses.
type Listen struct {
	// HTTPS is the TLS listener address, e.g. ":443".
	HTTPS string `toml:"https"`
	// HTTP is the plain listener: a redirect to HTTPS normally, the only
	// listener in proxy mode. Empty disables it outside proxy mode.
	HTTP string `toml:"http"`
	// ProxyMode serves plain HTTP on HTTP and trusts X-Forwarded-* headers
	// from a TLS-terminating reverse proxy.
	ProxyMode bool `toml:"proxy_mode"`
}

// TLS selects where certificates come from.
type TLS struct {
	// Mode is acme, files or self-signed.
	Mode string `toml:"mode"`
	// Email is the ACME account contact.
	Email string `toml:"email"`
	// CA is letsencrypt, letsencrypt-staging or an https ACME directory URL.
	CA string `toml:"ca"`
	// DNSProvider is the libdns provider for DNS-01: cloudflare, route53 or hetzner.
	DNSProvider string `toml:"dns_provider"`
	// CertFile and KeyFile are PEM files for mode files.
	CertFile string `toml:"cert_file"`
	KeyFile  string `toml:"key_file"`
	DNS      DNS    `toml:"dns"`
}

// DNS holds the DNS provider credentials.
type DNS struct {
	// APIToken is the provider's API token (unused for route53, which reads
	// the standard AWS credential chain).
	APIToken string `toml:"api_token" secret:"true"`
}

// Limits bounds uploads, metadata and write rates.
type Limits struct {
	MaxVersionBytes     Bytes    `toml:"max_version_bytes"`
	MaxFileBytes        Bytes    `toml:"max_file_bytes"`
	MaxFiles            int      `toml:"max_files"`
	MetadataMaxPairs    int      `toml:"metadata_max_pairs"`
	WriteRatePerMinute  int      `toml:"write_rate_per_minute"`
	DefaultUserQuota    Bytes    `toml:"default_user_quota"`
	InlineGuidanceBytes Bytes    `toml:"inline_guidance_bytes"`
	MaxChunkBytes       Bytes    `toml:"max_chunk_bytes"`
	UploadTTL           Duration `toml:"upload_ttl"`
	MaxOpenUploads      int      `toml:"max_open_uploads"`
}

// Lifecycle sets the stale, retention and garbage-collection timings.
type Lifecycle struct {
	DefaultGrace  Duration `toml:"default_grace"`
	Retention     Duration `toml:"retention"`
	Tombstone     Duration `toml:"tombstone"`
	SweepInterval Duration `toml:"sweep_interval"`
	GCInterval    Duration `toml:"gc_interval"`
}

// Serve controls headers on served artifacts and which slugs are reserved.
type Serve struct {
	// CSP is the instance default Content-Security-Policy; empty sends none.
	CSP string `toml:"csp"`
	// AllowCSPOverride lets agalma.csp metadata replace CSP per artifact.
	AllowCSPOverride bool `toml:"allow_csp_override"`
	// ReservedSlugs may not be used as artifact slugs.
	ReservedSlugs []string `toml:"reserved_slugs"`
}

// Auth holds the write policy, the admin token and the optional identity provider.
type Auth struct {
	// WritePolicy is shared (any writer may update any artifact) or owner.
	WritePolicy string `toml:"write_policy"`
	// AdminToken is accepted as a bearer token on the admin host.
	AdminToken string `toml:"admin_token" secret:"true"`
	// OIDC is nil when the [auth.oidc] table is absent, which falls back
	// to API-key sign-in on the OAuth consent page.
	OIDC *OIDC `toml:"oidc"`
}

// OIDC configures the upstream OpenID Connect provider users sign in with.
type OIDC struct {
	// Issuer is the provider's https issuer URL, used for discovery.
	Issuer       string `toml:"issuer"`
	ClientID     string `toml:"client_id"`
	ClientSecret string `toml:"client_secret" secret:"true"`
	// AllowedDomains are the verified email domains that may sign in.
	AllowedDomains []string `toml:"allowed_domains"`
	// AutoProvision creates a member on first sign-in from an allowed
	// domain. Defaults to true when the table is present.
	AutoProvision bool `toml:"auto_provision"`
	// AdminEmails are provisioned with the admin role.
	AdminEmails []string `toml:"admin_emails"`
}

// applyDefaults fills the defaults that apply once the [auth.oidc] table exists.
func (o *OIDC) applyDefaults() {
	o.AutoProvision = true
}

// Admin configures the admin host.
type Admin struct {
	// Listen is an optional second listener for the admin host only,
	// e.g. "127.0.0.1:9443". Empty serves admin on the public listener.
	Listen string `toml:"listen"`
}

// Log configures the slog logger.
type Log struct {
	// Level is debug, info, warn or error.
	Level string `toml:"level"`
	// Format is text or json.
	Format string `toml:"format"`
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

// Load reads the configuration from path (skipped when empty), applies
// environment overrides from the process environment and validates it.
func Load(path string) (Config, error) {
	return LoadWith(path, os.LookupEnv)
}

// LoadWith is Load with the environment lookup supplied by the caller.
//
// The order is: Default, then the TOML file at path if path is not empty
// (a missing file or an unknown key is an error), then overrides, then
// Validate. Every leaf key has an override named AGALMA_ plus its TOML path
// upper-cased with "." replaced by "_", for example AGALMA_LISTEN_PROXY_MODE
// for listen.proxy_mode and AGALMA_TLS_DNS_API_TOKEN for tls.dns.api_token.
// Lists are comma-separated. Setting any AGALMA_AUTH_OIDC_* variable
// enables the [auth.oidc] table.
func LoadWith(path string, lookup func(key string) (string, bool)) (Config, error) {
	cfg := Default()
	if path != "" {
		if err := decodeFile(path, &cfg); err != nil {
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

// decodeFile decodes the TOML file at path over cfg, rejecting unknown keys.
func decodeFile(path string, cfg *Config) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	// Decode into a pre-filled OIDC so its defaults survive a table that
	// omits them, then drop it again if the table was absent.
	oidc := &OIDC{}
	oidc.applyDefaults()
	cfg.Auth.OIDC = oidc
	md, err := toml.Decode(string(data), cfg)
	if err != nil {
		return fmt.Errorf("config: %s: %w", path, err)
	}
	if !md.IsDefined("auth", "oidc") {
		cfg.Auth.OIDC = nil
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return fmt.Errorf("config: %s: unknown keys: %s", path, strings.Join(keys, ", "))
	}
	return nil
}

// DBPath is the SQLite database file.
func (c Config) DBPath() string { return filepath.Join(c.DataDir, "agalma.db") }

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
// TOML file, with secrets (tls.dns.api_token, auth.admin_token,
// auth.oidc.client_secret) replaced by "***" when set. Sizes and durations
// are rendered as strings ("50MiB", "336h"); a missing [auth.oidc] is nil.
func (c Config) Redacted() map[string]any {
	return redactStruct(reflect.ValueOf(c))
}

func redactStruct(v reflect.Value) map[string]any {
	t := v.Type()
	m := make(map[string]any, t.NumField())
	for i := range t.NumField() {
		f := t.Field(i)
		name := tomlName(f)
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

// tomlName returns the TOML key of a struct field, or "" to skip it.
func tomlName(f reflect.StructField) string {
	if !f.IsExported() {
		return ""
	}
	name, _, _ := strings.Cut(f.Tag.Get("toml"), ",")
	if name == "-" || name == "" {
		return ""
	}
	return name
}
