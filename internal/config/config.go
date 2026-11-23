// Package config loads and validates Kazari's configuration from a YAML file
// with ${NAME} references, an optional .env file and KAZARI_* overrides. No
// other package reads settings from files or the environment.
package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net"
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
	"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com https://cdnjs.cloudflare.com https://cdn.jsdelivr.net https://unpkg.com; " +
	"font-src 'self' https://fonts.gstatic.com https://cdnjs.cloudflare.com https://cdn.jsdelivr.net https://unpkg.com; " +
	"img-src 'self' data: https:; " +
	"connect-src 'self'; " +
	"frame-ancestors 'none'"

// The labels of the hosts Kazari answers on under base_domain, besides the
// apex.
const (
	APIHost   = "api"
	MCPHost   = "mcp"
	AdminHost = "admin"
)

// HostLabels returns APIHost, MCPHost and AdminHost.
func HostLabels() []string { return []string{APIHost, MCPHost, AdminHost} }

var defaultReservedSlugs = []string{
	APIHost, MCPHost, AdminHost, "www", "mail", "ns1", "ns2", "autoconfig", "autodiscover", "localhost",
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
	Analytics Analytics `yaml:"analytics"`
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
	// In proxy mode, the one header the proxy sets to the client's address:
	// X-Real-IP or X-Forwarded-For (its last entry).
	ClientIPHeader string `yaml:"client_ip_header"`
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
	// Timeout bounds one request, Budget one import: its requests and parsing its page.
	Timeout Duration `yaml:"timeout"`
	Budget  Duration `yaml:"budget"`
}

// Auth holds the write policy, the admin token, the lifetimes of
// credentials, the built-in accounts and the optional identity provider.
type Auth struct {
	WritePolicy string    `yaml:"write_policy"`
	AdminToken  string    `yaml:"admin_token" secret:"true"`
	Tokens      Tokens    `yaml:"tokens"`
	Local       LocalAuth `yaml:"local"`
	// Nil when auth.oidc is absent: people sign in with a built-in account,
	// or with an API key when auth.local is off.
	OIDC *OIDC `yaml:"oidc"`
}

// LocalAuth configures the accounts Kazari keeps itself: a password per
// person, set through a link an admin hands over.
type LocalAuth struct {
	// Nil follows auth.oidc: on without a provider, off with one.
	Enabled           *bool    `yaml:"enabled"`
	MinPasswordLength int      `yaml:"min_password_length"`
	InviteTTL         Duration `yaml:"invite_ttl"`
	ResetTTL          Duration `yaml:"reset_ttl"`
}

// Bounds of auth.local.min_password_length. The upper one is also the
// longest password accepted.
const (
	MinPasswordLength = 8
	MaxPasswordLength = 256
)

// Tokens sets how long OAuth tokens and API keys live.
type Tokens struct {
	AccessTTL  Duration `yaml:"access_ttl"`
	RefreshTTL Duration `yaml:"refresh_ttl"`
	// How long after its rotation a refresh token is accepted once more;
	// 0 never accepts one again.
	RefreshReuseWindow Duration `yaml:"refresh_reuse_window"`
	// The longest an API key may live; 0 lets a key be issued without an
	// expiry.
	MaxKeyTTL Duration `yaml:"max_key_ttl"`
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
	// Serve the dashboard at the root of the admin host.
	Dashboard bool `yaml:"dashboard"`
	// The address a browser reaches the admin host at. Empty means
	// https://admin.<base_domain>, or none at all with admin.listen set.
	PublicURL string `yaml:"public_url"`
	// Ask GitHub once a day for the newest release.
	UpdateCheck bool `yaml:"update_check"`
}

// Analytics configures the counting of page views.
type Analytics struct {
	// Count page views of artifacts.
	Enabled bool `yaml:"enabled"`
	// Record which members opened a members page.
	Readers bool `yaml:"readers"`
	// How long daily counts are kept.
	Retention Duration `yaml:"retention"`
}

// MinAnalyticsRetention is the shortest analytics.retention: one day.
const MinAnalyticsRetention = Duration(day)

// DefaultAdminSocket is admin.socket's default, under data_dir.
const DefaultAdminSocket = "admin.sock"

// Log configures the slog logger.
type Log struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// DefaultDataDir is data_dir's default: the systemd unit's StateDirectory
// and the data volume of the image.
const DefaultDataDir = "/var/lib/kazari"

// Default returns the built-in defaults; BaseDomain is left empty and must
// be set.
func Default() Config {
	return Config{
		DataDir:        DefaultDataDir,
		ExternalScheme: "https",
		Listen: Listen{
			HTTPS:          ":443",
			HTTP:           ":80",
			ClientIPHeader: "X-Real-IP",
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
			Tokens: Tokens{
				AccessTTL:          Duration(1 * hour),
				RefreshTTL:         Duration(90 * day),
				RefreshReuseWindow: Duration(30 * second),
			},
			Local: LocalAuth{
				MinPasswordLength: 15,
				InviteTTL:         Duration(7 * day),
				ResetTTL:          Duration(1 * day),
			},
		},
		Admin: Admin{
			Socket:      DefaultAdminSocket,
			Dashboard:   true,
			UpdateCheck: true,
		},
		Analytics: Analytics{
			Enabled:   true,
			Readers:   true,
			Retention: Duration(366 * day),
		},
		Log: Log{
			Level:  "info",
			Format: "text",
		},
		Telemetry: defaultTelemetry(),
	}
}

// Local returns the preset of kazari serve --local: plain HTTP on
// 127.0.0.1:8080 under the base domain kazari.localhost, which browsers
// resolve to the local machine, with the data in ./.data. No DNS,
// certificate or token is needed; the admin API is reached over the socket.
func Local() Config {
	c := Default()
	// Not bare localhost: browsers make a passkey only for a registrable
	// domain that the apex and the sign-in host share.
	c.BaseDomain = "kazari.localhost"
	c.DataDir = "./.data"
	c.ExternalScheme = "http"
	c.ExternalPort = 8080
	c.Listen = Listen{HTTP: "127.0.0.1:8080", ProxyMode: true, ClientIPHeader: c.Listen.ClientIPHeader}
	c.TLS.Mode = "self-signed"
	c.Log.Level = "debug"
	return c
}

// DefaultEnvFile is the dotenv file Load reads when no other is found.
const DefaultEnvFile = ".env"

// EnvFileName is the dotenv file Load reads beside a configuration file:
// /etc/kazari/kazari.env, which the systemd unit names as its
// EnvironmentFile.
const EnvFileName = "kazari.env"

// Load is LoadWith over the process environment and a dotenv file: envFile,
// which must exist; else EnvFileName beside the file at path, if it exists;
// else DefaultEnvFile, if it exists. The process environment wins over the
// dotenv file. Beside a config file, a DefaultEnvFile nobody named feeds
// ${NAME} references only: a KAZARI_* override from it could send a secret
// in the file to another destination. The Origin is filled as far as
// loading got, also on error.
func Load(path, envFile string) (Config, Origin, error) {
	origin := Origin{File: path}
	dot, err := findDotenv(path, envFile, &origin)
	if err != nil {
		return Config{}, origin, err
	}
	used := map[string]bool{}
	refs := func(key string) (string, bool) {
		used[key] = true
		return dot.lookup(key)
	}
	overrides := dot.lookup
	if path != "" && !dot.named {
		overrides = os.LookupEnv
	}
	cfg, err := load(Default(), path, refs, overrides, dot.where, &origin)
	if path != "" && !dot.named {
		for _, k := range slices.Sorted(maps.Keys(dot.vars)) {
			if _, set := os.LookupEnv(k); strings.HasPrefix(k, envPrefix+"_") && !used[k] && !set {
				origin.Ignored = append(origin.Ignored, k)
			}
		}
	}
	if err != nil {
		return Config{}, origin, err
	}
	cfg, err = validated(cfg, origin)
	return cfg, origin, err
}

// dotenv is the dotenv file Load reads, if any.
type dotenv struct {
	file  string
	named bool // named by the caller or found beside the config file
	vars  map[string]string
}

func (d dotenv) lookup(key string) (string, bool) {
	if v, ok := os.LookupEnv(key); ok {
		return v, true
	}
	v, ok := d.vars[key]
	return v, ok
}

// where names the dotenv file key comes from, "" for the process
// environment.
func (d dotenv) where(key string) string {
	if _, ok := os.LookupEnv(key); ok {
		return ""
	}
	if _, ok := d.vars[key]; ok {
		return d.file
	}
	return ""
}

func findDotenv(path, envFile string, origin *Origin) (dotenv, error) {
	if envFile != "" {
		vars, err := LoadEnvFile(envFile)
		if err != nil {
			return dotenv{}, err
		}
		origin.EnvFiles = append(origin.EnvFiles, envFile)
		return dotenv{file: envFile, named: true, vars: vars}, nil
	}
	if path != "" {
		beside := filepath.Join(filepath.Dir(path), EnvFileName)
		vars, err := LoadEnvFile(beside)
		switch {
		case err == nil:
			origin.EnvFiles = append(origin.EnvFiles, beside)
			return dotenv{file: beside, named: true, vars: vars}, nil
		case errors.Is(err, fs.ErrPermission):
			// The service itself, under systemd, has the file's variables
			// from EnvironmentFile and no permission to read it.
			origin.Unreadable = beside + ": permission denied"
		case !errors.Is(err, fs.ErrNotExist):
			return dotenv{}, err
		}
	}
	vars, err := LoadEnvFile(DefaultEnvFile)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return dotenv{}, nil
	case err != nil:
		return dotenv{}, err
	}
	label := DefaultEnvFile
	if path != "" {
		label += " (references only)"
	}
	origin.EnvFiles = append(origin.EnvFiles, label)
	return dotenv{file: DefaultEnvFile, named: path == "", vars: vars}, nil
}

// LoadLocal is Load for the Local preset: no file, the dotenv file as Load
// reads it, and KAZARI_* overrides on top of the preset. external_port
// follows the port of listen.http unless it is set too.
func LoadLocal(envFile string) (Config, Origin, error) {
	origin := Origin{Preset: "--local"}
	dot, err := findDotenv("", envFile, &origin)
	if err != nil {
		return Config{}, origin, err
	}
	return loadLocal(dot.lookup, dot.where, origin)
}

// LoadLocalWith is LoadLocal over lookup alone.
func LoadLocalWith(lookup func(key string) (string, bool)) (Config, error) {
	cfg, _, err := loadLocal(lookup, nil, Origin{Preset: "--local"})
	return cfg, err
}

func loadLocal(lookup func(string) (string, bool), where func(string) string, origin Origin) (Config, Origin, error) {
	cfg, err := load(Local(), "", lookup, lookup, where, &origin)
	if err != nil {
		return Config{}, origin, err
	}
	if _, set := origin.Keys["external_port"]; !set {
		if _, port, err := net.SplitHostPort(cfg.Listen.HTTP); err == nil {
			if n, err := strconv.Atoi(port); err == nil && n > 0 {
				cfg.ExternalPort = n
			}
		}
	}
	cfg, err = validated(cfg, origin)
	return cfg, origin, err
}

// LoadWith applies, in order, Default, the YAML file at path (skipped when
// empty; a missing file satisfies fs.ErrNotExist; unknown keys are errors),
// the KAZARI_<PATH> overrides (lists comma-separated; a non-empty
// KAZARI_AUTH_OIDC_* enables auth.oidc; a secret also reads
// KAZARI_<PATH>_FILE), then Validate. lookup resolves both ${NAME}
// references and overrides.
func LoadWith(path string, lookup func(key string) (string, bool)) (Config, error) {
	return LoadWithBase(Default(), path, lookup)
}

// LoadWithBase is LoadWith starting from base instead of Default.
func LoadWithBase(base Config, path string, lookup func(key string) (string, bool)) (Config, error) {
	origin := Origin{File: path}
	cfg, err := load(base, path, lookup, lookup, nil, &origin)
	if err != nil {
		return Config{}, err
	}
	return validated(cfg, origin)
}

func validated(cfg Config, origin Origin) (Config, error) {
	err := cfg.Validate()
	if err == nil {
		return cfg, nil
	}
	if cfg.Auth.OIDC != nil && strings.Contains(err.Error(), "auth.oidc.") {
		if _, ok := origin.Keys["auth.oidc"]; ok {
			err = errors.Join(err, fmt.Errorf("auth.oidc: on because of %s; remove that to sign in with built-in accounts",
				origin.Describe("auth.oidc")))
		}
	}
	return Config{}, fmt.Errorf("config: invalid:\n%w", err)
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
		if err := decodeFile(path, &cfg, lenient, map[string]int{}); err != nil {
			return Config{}, err
		}
	}
	if err := applyEnv(&cfg, lookup, map[string]string{}); err != nil {
		return Config{}, fmt.Errorf("config: environment: %w", err)
	}
	return cfg, nil
}

// load resolves ${NAME} references through refs and KAZARI_* overrides
// through overrides, and records in origin where each value came from;
// where names the dotenv file of a variable, and may be nil.
func load(base Config, path string, refs, overrides func(key string) (string, bool), where func(string) string, origin *Origin) (Config, error) {
	cfg := base
	lines := map[string]int{}
	if path != "" {
		if err := decodeFile(path, &cfg, refs, lines); err != nil {
			return Config{}, err
		}
	}
	envs := map[string]string{}
	if err := applyEnv(&cfg, overrides, envs); err != nil {
		return Config{}, fmt.Errorf("config: environment: %w", err)
	}
	origin.Keys = make(map[string]Source, len(lines)+len(envs))
	for k, line := range lines {
		origin.Keys[k] = Source{Line: line}
	}
	for k, name := range envs {
		s := origin.Keys[k]
		s.Env = name
		if where != nil {
			s.EnvFile = where(name)
		}
		origin.Keys[k] = s
	}
	return cfg, nil
}

// decodeFile decodes the file at path into cfg key by key, so that every
// error names its line and key and none stops the others, and records in
// lines the line of each key it set.
func decodeFile(path string, cfg *Config, lookup func(string) (string, bool), lines map[string]int) error {
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
	keysErr := checkKeys(&doc, path)
	expandErr := expandNode(&doc, path, lookup)
	d := decoder{path: path, lines: lines}
	d.decode(&doc, reflect.ValueOf(cfg).Elem(), "")
	return errors.Join(keysErr, expandErr, errors.Join(d.errs...))
}

type decoder struct {
	path  string
	lines map[string]int
	errs  []error
}

func (d *decoder) fail(n *yaml.Node, key, msg string) {
	d.errs = append(d.errs, fmt.Errorf("config: %s: line %d: %s: %s", d.path, n.Line, key, msg))
}

func (d *decoder) decode(n *yaml.Node, v reflect.Value, key string) {
	if n.Kind == yaml.DocumentNode {
		for _, c := range n.Content {
			d.decode(c, v, key)
		}
		return
	}
	if n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	t := v.Type()
	switch {
	case t.Kind() == reflect.Pointer && t.Elem().Kind() == reflect.Struct:
		// An optional mapping, such as auth.oidc: present unless null.
		if isNull(n) {
			v.SetZero()
			return
		}
		if v.IsNil() {
			p := reflect.New(t.Elem())
			if def, ok := p.Interface().(interface{ applyDefaults() }); ok {
				def.applyDefaults()
			}
			v.Set(p)
		}
		d.lines[key] = n.Line
		d.decode(n, v.Elem(), key)
	case t.Kind() == reflect.Struct && !isTextUnmarshaler(v):
		if isNull(n) {
			return
		}
		if n.Kind != yaml.MappingNode {
			what := key
			if what == "" {
				what = "the file"
			}
			d.fail(n, what, "want a mapping of keys, got "+describeNode(n))
			return
		}
		d.mapping(n, v, key, map[*yaml.Node]bool{})
	default:
		if err := n.Decode(v.Addr().Interface()); err != nil {
			d.fail(n, key, describeDecodeError(err, n, t))
			return
		}
		d.lines[key] = n.Line
	}
}

// mapping applies merged mappings first, so the mapping's own keys win.
// Unknown keys are left to checkKeys.
func (d *decoder) mapping(n *yaml.Node, v reflect.Value, key string, merged map[*yaml.Node]bool) {
	fields := map[string]int{}
	for i := range v.NumField() {
		if name := keyName(v.Type().Field(i)); name != "" {
			fields[name] = i
		}
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].ShortTag() != "!!merge" {
			continue
		}
		m := n.Content[i+1]
		targets := []*yaml.Node{m}
		if m.Kind == yaml.SequenceNode {
			targets = m.Content
		}
		for _, t := range targets {
			if t.Kind == yaml.AliasNode && t.Alias != nil {
				t = t.Alias
			}
			if t.Kind == yaml.MappingNode && !merged[t] {
				merged[t] = true
				d.mapping(t, v, key, merged)
			}
		}
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i]
		if k.ShortTag() == "!!merge" {
			continue
		}
		idx, ok := fields[k.Value]
		if !ok {
			continue
		}
		path := k.Value
		if key != "" {
			path = key + "." + k.Value
		}
		d.decode(n.Content[i+1], v.Field(idx), path)
		if _, set := d.lines[path]; set {
			d.lines[path] = k.Line // the key's, for a value on the lines below it
		}
	}
}

func isNull(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.ShortTag() == "!!null"
}

// describeDecodeError says what a key wants and what the file holds, in
// place of the decoder's "cannot unmarshal !!str `lots` into int".
func describeDecodeError(err error, n *yaml.Node, t reflect.Type) string {
	var te *yaml.TypeError
	if !errors.As(err, &te) {
		return err.Error()
	}
	if want := describeType(t); want != "" && !sameShape(n, t) {
		return "want " + want + ", got " + describeNode(n)
	}
	var msgs []string
	for _, e := range te.Errors {
		if m := yamlLine.ReplaceAllString(e, ""); !slices.Contains(msgs, m) {
			msgs = append(msgs, m)
		}
	}
	return strings.Join(msgs, "; ")
}

var yamlLine = regexp.MustCompile(`^line \d+: `)

// sameShape reports whether n is the kind of node t is decoded from, so
// that the error lies inside it.
func sameShape(n *yaml.Node, t reflect.Type) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Slice:
		return n.Kind == yaml.SequenceNode
	case reflect.Map:
		return n.Kind == yaml.MappingNode
	}
	return false
}

func describeType(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t {
	case reflect.TypeFor[Bytes]():
		return "a size such as 50MiB"
	case reflect.TypeFor[Duration]():
		return "a duration such as 90s, 15m or 14d"
	}
	switch t.Kind() {
	case reflect.String:
		return "a string"
	case reflect.Bool:
		return "true or false"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return "a whole number"
	case reflect.Float32, reflect.Float64:
		return "a number"
	case reflect.Slice:
		return "a list"
	case reflect.Map:
		return "a mapping"
	}
	return ""
}

func describeNode(n *yaml.Node) string {
	switch n.Kind {
	case yaml.SequenceNode:
		return "a list"
	case yaml.MappingNode:
		return "a mapping"
	}
	return strconv.Quote(n.Value)
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

// LocalAccounts reports whether people sign in with a password Kazari
// keeps: auth.local.enabled, which without a value is on exactly when no
// identity provider is configured.
func (c Config) LocalAccounts() bool {
	if c.Auth.Local.Enabled != nil {
		return *c.Auth.Local.Enabled
	}
	return c.Auth.OIDC == nil
}

// AdminURL is the address a browser reaches the admin host at, without a
// trailing slash: admin.public_url, else the public admin host. It is "" when
// admin.listen moves the host off the public listener and admin.public_url does not
// say where to, so nothing can link to it.
func (c Config) AdminURL() string {
	if c.Admin.PublicURL != "" {
		return strings.TrimSuffix(c.Admin.PublicURL, "/")
	}
	if c.Admin.Listen != "" {
		return ""
	}
	return strings.TrimSuffix(c.ExternalURL(AdminHost), "/")
}

// DashboardEnabled reports whether the dashboard is served: admin.dashboard
// is on, AdminURL names where a browser finds it, and that address is one
// CheckAdminURL accepts. The last matters for the default address under
// external_scheme http, which no validation of admin.public_url has seen.
func (c Config) DashboardEnabled() bool {
	return c.Admin.Dashboard && c.AdminURL() != "" && CheckAdminURL(c.AdminURL()) == nil
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

// Logger returns a logger writing to w in log.format at log.level. With
// journal, w is the journal stream systemd connected, and each line starts
// with its syslog priority.
func (c Config) Logger(w io.Writer, journal bool) *slog.Logger {
	opts := &slog.HandlerOptions{Level: c.SlogLevel()}
	handler := func(w io.Writer) slog.Handler {
		if c.Log.Format == "json" {
			return slog.NewJSONHandler(w, opts)
		}
		return slog.NewTextHandler(w, opts)
	}
	if journal {
		return slog.New(newPriorityHandler(w, handler))
	}
	return slog.New(handler(w))
}

// Redacted returns the configuration as a JSON-friendly map keyed like the
// YAML file, with set secrets as "***", sizes and durations as strings
// ("50MiB", "336h") and a missing auth.oidc as nil.
func (c Config) Redacted() map[string]any {
	m := redactStruct(reflect.ValueOf(c))
	// The effective value, where the key's own may be left to follow
	// auth.oidc.
	if auth, ok := m["auth"].(map[string]any); ok {
		if local, ok := auth["local"].(map[string]any); ok {
			local["enabled"] = c.LocalAccounts()
		}
	}
	return m
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
