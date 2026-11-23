// Package app wires every Kazari component from a config.Config and runs the
// listeners, the scheduler and the certificate source.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/metric"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/PeterBooker/kazari/internal/account"
	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/buildinfo"
	"github.com/PeterBooker/kazari/internal/certs"
	"github.com/PeterBooker/kazari/internal/config"
	"github.com/PeterBooker/kazari/internal/dashboard"
	"github.com/PeterBooker/kazari/internal/dirlock"
	"github.com/PeterBooker/kazari/internal/gate"
	"github.com/PeterBooker/kazari/internal/httpapi"
	"github.com/PeterBooker/kazari/internal/instrument"
	"github.com/PeterBooker/kazari/internal/memlimit"
	"github.com/PeterBooker/kazari/internal/middleware"
	"github.com/PeterBooker/kazari/internal/oauth"
	"github.com/PeterBooker/kazari/internal/oidc"
	"github.com/PeterBooker/kazari/internal/preflight"
	"github.com/PeterBooker/kazari/internal/scheduler"
	"github.com/PeterBooker/kazari/internal/signin"
	"github.com/PeterBooker/kazari/internal/site"
	"github.com/PeterBooker/kazari/internal/sqlite"
	"github.com/PeterBooker/kazari/internal/telemetry"
	"github.com/PeterBooker/kazari/internal/user"
)

const (
	uploadsExpireEvery = 10 * time.Minute
	oauthCleanupEvery  = time.Hour
	// A minute of views is what a crash loses; the write it costs is one
	// short transaction whatever the traffic.
	viewsFlushEvery    = time.Minute
	viewsPruneEvery    = 24 * time.Hour
	updateCheckEvery   = 24 * time.Hour
	updateCheckTimeout = 15 * time.Second
	// oidcStartTimeout bounds discovery at start; an unreachable provider
	// is retried every oidcRetryEvery while the server runs.
	oidcStartTimeout = 10 * time.Second
	oidcRetryEvery   = 30 * time.Second
	staleTempAge     = time.Hour // older temp files are crash leftovers
	// telemetryFlushTimeout bounds the last export at shutdown, so a dead
	// collector cannot hold the process.
	telemetryFlushTimeout = 5 * time.Second
)

// Options are the process-level inputs that do not belong in the config.
type Options struct {
	Now    func() time.Time // nil means time.Now
	Logger *slog.Logger     // nil means slog.Default()

	// Stderr receives what must reach the operator and nobody else: the
	// first-run setup link, which the logger could export. Nil means
	// os.Stderr.
	Stderr io.Writer
	// UpdateURL replaces the address the update check asks, and Version the
	// running version it compares, for tests.
	UpdateURL, Version string
	// Argon2 replaces the parameters of password hashes, for tests: the
	// real ones cost 64 MiB a hash.
	Argon2 user.Argon2
	// Origin says where the configuration came from, for the start log and
	// the admin API's settings.
	Origin config.Origin
	// OIDCClient replaces the HTTP client of the identity provider, for
	// tests.
	OIDCClient *http.Client

	// Memory replaces where the memory the process may use is read, which
	// a percentage of serve.compression_cache is taken of, for tests.
	Memory memlimit.Options

	// SpanExporter, MetricReader and LogExporter replace their signal's
	// OTLP exporter whatever the endpoint, for tests.
	SpanExporter sdktrace.SpanExporter
	MetricReader sdkmetric.Reader
	LogExporter  sdklog.Exporter
}

const scope = "github.com/PeterBooker/kazari/internal/app"

// Addrs are the bound listener addresses, empty for one not configured.
type Addrs struct {
	HTTPS  string
	HTTP   string
	Admin  string
	Socket string // the admin Unix socket's path
}

// App is the assembled server.
type App struct {
	cfg  config.Config
	log  *slog.Logger
	mode string

	db         *sqlite.DB
	tel        *telemetry.Telemetry
	tracer     trace.Tracer
	obs        *middleware.Observer
	sched      *scheduler.Scheduler
	users      *user.Service
	views      recorder // nil with analytics off
	ready      *preflight.Checker
	stderr     io.Writer
	certs      certs.Source // nil in proxy mode
	router     http.Handler
	admin      http.Handler // on cfg.Admin.Listen when set, else on the router
	adminLocal http.Handler // on the admin socket, without credentials
	idp        *oidc.Lazy   // nil without auth.oidc
	origin     config.Origin
	lock       *dirlock.Lock
	started    chan struct{}

	mu    sync.Mutex
	addrs Addrs

	closeOnce sync.Once
	closeErr  error
}

// New validates cfg, creates the data directories, takes the lock on the
// data directory, opens the database and tries OIDC discovery, which
// retries in the background while the provider is unreachable; nothing
// listens until Run.
func New(ctx context.Context, cfg config.Config, opts Options) (*App, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("app: invalid config:\n%w", err)
	}
	cfg.Serve.ReservedSlugs = reservedSlugs(cfg.Serve.ReservedSlugs)
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}

	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return nil, fmt.Errorf("app: create data directory: %w", err)
	}
	// Before anything else touches the directory: two servers on one
	// database would each run the jobs, and the second's exit would take
	// the first's admin socket with it.
	lock, err := dirlock.Acquire(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("app: %w; stop it first, or give this one another data_dir", err)
	}
	for _, dir := range []string{cfg.BlobDir(), cfg.UploadDir(), cfg.CertStorageDir(), tempDir(cfg)} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			lock.Release()
			return nil, fmt.Errorf("app: create data directory: %w", err)
		}
	}

	tel, err := telemetry.New(ctx, telemetryOptions(cfg, opts, log, now))
	if err != nil {
		lock.Release()
		return nil, fmt.Errorf("app: %w", err)
	}
	log = tel.Logger(log)
	a := &App{cfg: cfg, log: log, tel: tel, tracer: tel.Providers().Tracer(scope), started: make(chan struct{}), stderr: opts.Stderr,
		origin: opts.Origin, lock: lock}
	if a.stderr == nil {
		a.stderr = os.Stderr
	}
	if err := a.start(ctx, now, opts); err != nil {
		if a.db == nil {
			a.flushTelemetry()
			a.lock.Release()
		} else {
			a.Close()
		}
		return nil, err
	}
	return a, nil
}

// start is traced as kazari.start, to show why a start was slow.
func (a *App) start(ctx context.Context, now func() time.Time, opts Options) (err error) {
	ctx, span := a.tracer.Start(ctx, "kazari.start")
	defer func() { instrument.End(span, err, "") }()
	migrateCtx, migrate := instrument.StartChild(ctx, a.tracer, "sqlite.migrate")
	a.db, err = sqlite.Open(migrateCtx, a.cfg.DBPath(), sqlite.Options{Telemetry: a.tel.Providers()})
	instrument.End(migrate, err, "")
	if err != nil {
		return fmt.Errorf("app: %w", err)
	}
	return a.build(ctx, now, opts)
}

// TelemetryOptions maps the telemetry section onto the SDK's options.
func TelemetryOptions(cfg config.Config) telemetry.Options {
	t := cfg.Telemetry
	o := telemetry.Options{
		ServiceName:    t.ServiceName,
		ServiceVersion: buildinfo.Get().Version,
		Environment:    t.Environment,
		OTLP: telemetry.OTLP{
			Endpoint:    t.OTLP.Endpoint,
			Protocol:    t.OTLP.Protocol,
			Headers:     t.OTLP.Headers,
			Compression: t.OTLP.Compression,
			Timeout:     t.OTLP.Timeout.Std(),
			CAFile:      t.OTLP.CAFile,
			CertFile:    t.OTLP.CertFile,
			KeyFile:     t.OTLP.KeyFile,
		},
		Traces: telemetry.Traces{
			OTLP:            t.Traces.OTLP,
			SampleRatio:     t.Traces.SampleRatio,
			SiteSampleRatio: t.Traces.SiteSampleRatio,
		},
		Metrics: telemetry.Metrics{
			Prometheus: t.Metrics.Prometheus,
			OTLP:       t.Metrics.OTLP,
			Interval:   t.Metrics.Interval.Std(),
		},
		Logs: telemetry.Logs{OTLP: t.Logs.OTLP, Level: cfg.SlogLevel()},
	}
	if t.Logs.Level != "" {
		o.Logs.Level = config.ParseLevel(t.Logs.Level)
	}
	return o
}

func telemetryOptions(cfg config.Config, opts Options, log *slog.Logger, now func() time.Time) telemetry.Options {
	o := TelemetryOptions(cfg)
	o.Logger, o.Now = log, now
	o.SpanExporter, o.MetricReader, o.LogExporter = opts.SpanExporter, opts.MetricReader, opts.LogExporter
	return o
}

// observeExpiry reports when the served certificate expires, for an alert
// well before a renewal that keeps failing runs out.
func observeExpiry(m metric.Meter, src certs.Source) {
	gauge := instrument.Handle(m.Float64ObservableGauge("kazari.certificate.expiry.timestamp",
		metric.WithUnit("s"), metric.WithDescription("When the served certificate expires, in seconds since the Unix epoch.")))
	_ = instrument.Handle(m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		if t := src.NotAfter(); !t.IsZero() {
			o.ObserveFloat64(gauge, float64(t.UnixNano())/1e9)
		}
		return nil
	}, gauge))
}

// tlsMode is tls.mode, or "proxy" when a proxy in front holds the
// certificate.
func tlsMode(cfg config.Config) string {
	if cfg.Listen.ProxyMode {
		return "proxy"
	}
	return cfg.TLS.Mode
}

// certError is the error of the last failed certificate order, nil when
// none failed or the source orders none.
func (a *App) certError() error {
	if s, ok := a.certs.(interface{ LastError() error }); ok {
		return s.LastError()
	}
	return nil
}

// certNotAfter is when the served certificate expires, or the zero time
// when Kazari serves none.
func (a *App) certNotAfter() time.Time {
	if a.certs == nil {
		return time.Time{}
	}
	return a.certs.NotAfter()
}

// adminMux puts the dashboard at the root of the admin host, beside the
// admin API. The session cookie of the one never reaches the other: the API
// reads bearer credentials alone.
func adminMux(api, dash http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/v1/", api)
	mux.Handle("/health", api)
	mux.Handle("/metrics", api)
	mux.Handle("/", dash)
	return mux
}

// plainHTTP reports whether the address is plain http, which the
// configuration accepts on this machine alone; cookies there cannot carry
// the Secure attribute or the __Host- prefix.
func plainHTTP(address string) bool {
	u, err := url.Parse(address)
	return err == nil && u.Scheme == "http"
}

// controlCallback returns where a browser sign-in ends on a control host:
// the dashboard at admin.public_url, and the account pages on the apex. Both come
// from the configuration, never from a request.
func controlCallback(u urls) func(host string) (string, bool) {
	admin := u.dashboard
	if admin != "" {
		admin += dashboard.CallbackPath
	}
	apex := u.apex + strings.TrimPrefix(account.CallbackPath, "/")
	return func(host string) (string, bool) {
		switch host {
		case signin.HostAdmin:
			return admin, admin != ""
		case signin.HostAccount:
			return apex, true
		}
		return "", false
	}
}

// reservedSlugs adds the labels of the hosts Kazari answers on to the
// configured list: a list that replaces the default must not make them
// creatable, nor serve their version hosts as artifacts.
func reservedSlugs(configured []string) []string {
	out := slices.Clone(configured)
	for _, l := range config.HostLabels() {
		if !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	return out
}

// dashboardURL is the dashboard's address, or "" when it is not served.
func dashboardURL(cfg config.Config) string {
	if !cfg.DashboardEnabled() {
		return ""
	}
	return cfg.AdminURL()
}

// viewCallback returns the gate's callback on an artifact host, where a
// browser sign-in for it ends. Only a canonical artifact label (<slug> or
// <slug>--v<N>, never a reserved slug) has one.
func viewCallback(cfg config.Config) func(host string) (string, bool) {
	return func(host string) (string, bool) {
		slug, version, ok := site.ParseHost(cfg.Host(host), cfg.BaseDomain)
		if !ok || slices.Contains(cfg.Serve.ReservedSlugs, slug) {
			return "", false
		}
		label := slug
		if version != 0 {
			label += artifact.VersionSeparator + strconv.Itoa(version)
		}
		if label != host {
			return "", false
		}
		return cfg.ExternalURL(host) + strings.TrimPrefix(gate.CallbackPath, "/"), true
	}
}

func certSource(cfg config.Config, log *slog.Logger, now func() time.Time) (certs.Source, error) {
	switch cfg.TLS.Mode {
	case "self-signed":
		s, err := certs.NewSelfSigned([]string{cfg.BaseDomain}, certs.SelfSignedOptions{Now: now})
		if err != nil {
			return nil, fmt.Errorf("app: %w", err)
		}
		return s, nil
	case "files":
		s, err := certs.NewFiles(cfg.TLS.CertFile, cfg.TLS.KeyFile, certs.FilesOptions{Logger: log})
		if err != nil {
			return nil, fmt.Errorf("app: %w", err)
		}
		return s, nil
	case "acme":
		dns, err := certs.NewDNSProvider(DNSConfig(cfg))
		if err != nil {
			return nil, fmt.Errorf("app: %w", err)
		}
		s, err := certs.NewACME(certs.ACMEConfig{
			Email:       cfg.TLS.Email,
			CA:          cfg.TLS.CA,
			Domains:     []string{cfg.BaseDomain, "*." + cfg.BaseDomain},
			StorageDir:  cfg.CertStorageDir(),
			Logger:      log,
			DNSProvider: cfg.TLS.DNSProvider,
		}, dns)
		if err != nil {
			return nil, fmt.Errorf("app: %w", err)
		}
		return s, nil
	default:
		return nil, fmt.Errorf("app: unknown tls.mode %q", cfg.TLS.Mode)
	}
}

func tempDir(cfg config.Config) string { return filepath.Join(cfg.DataDir, "tmp") }

// DNSConfig is the DNS-01 provider cfg names, with its credentials.
func DNSConfig(cfg config.Config) certs.DNSConfig {
	return certs.DNSConfig{
		Provider: cfg.TLS.DNSProvider,
		APIToken: cfg.TLS.DNS.APIToken,
		Endpoint: cfg.TLS.DNS.Endpoint,
		User:     cfg.TLS.DNS.User,
		ClientIP: cfg.TLS.DNS.ClientIP,
	}
}

// Handler returns the host router that the public listener serves.
func (a *App) Handler() http.Handler { return a.router }

// Scheduler returns the job scheduler, for triggering jobs in tests.
func (a *App) Scheduler() *scheduler.Scheduler { return a.sched }

// Started is closed once every listener of Run is bound.
func (a *App) Started() <-chan struct{} { return a.started }

// Addrs returns the bound addresses, complete once Started is closed.
func (a *App) Addrs() Addrs {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.addrs
}

// Close stops the certificate source, flushes telemetry and closes the
// database; it is idempotent and Run calls it on exit.
func (a *App) Close() error {
	a.closeOnce.Do(func() {
		if s, ok := a.certs.(interface{ Stop() }); ok {
			s.Stop()
		}
		a.flushTelemetry()
		if err := a.db.Close(); err != nil {
			a.closeErr = fmt.Errorf("app: close database: %w", err)
		}
		a.lock.Release()
	})
	return a.closeErr
}

// flushTelemetry logs a failed flush instead of returning it: losing the last
// export is no reason for a non-zero exit.
func (a *App) flushTelemetry() {
	ctx, cancel := context.WithTimeout(context.Background(), telemetryFlushTimeout)
	defer cancel()
	if err := a.tel.Shutdown(ctx); err != nil {
		a.log.Warn("telemetry: final flush failed", "err", err)
	}
}

// grantsAndSessions are a person's connected applications, which the
// authorization server keeps, and browser sessions, which the sign-in host
// keeps, as the account pages and the dashboard show them together.
type grantsAndSessions struct {
	grants   *oauth.Service
	sessions *signin.Service
}

func (g grantsAndSessions) ListGrants(ctx context.Context, handle string) ([]oauth.Token, error) {
	return g.grants.ListGrants(ctx, handle)
}

func (g grantsAndSessions) RevokeGrant(ctx context.Context, id int64) error {
	return g.grants.RevokeGrant(ctx, id)
}

func (g grantsAndSessions) GetClient(ctx context.Context, id string) (*oauth.Client, error) {
	return g.grants.GetClient(ctx, id)
}

func (g grantsAndSessions) EndSessions(ctx context.Context, handle string, keep int64) error {
	return g.sessions.EndSessions(ctx, handle, keep)
}

func (g grantsAndSessions) CountSessions(ctx context.Context, handle string) (int, error) {
	return g.sessions.CountSessions(ctx, handle)
}

// grantStore adapts oauth.Service to httpapi.GrantStore.
type grantStore struct{ svc *oauth.Service }

func (g grantStore) ListGrants(ctx context.Context, handle string) ([]api.OAuthGrant, error) {
	toks, err := g.svc.ListGrants(ctx, handle)
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	out := make([]api.OAuthGrant, 0, len(toks))
	for _, t := range toks {
		name, ok := names[t.ClientID]
		if !ok {
			if c, err := g.svc.GetClient(ctx, t.ClientID); err == nil {
				name = c.Name
			}
			names[t.ClientID] = name
		}
		out = append(out, api.OAuthGrant{
			ID:        t.FamilyID,
			ClientID:  t.ClientID,
			Client:    name,
			User:      t.UserHandle,
			Scopes:    t.Scopes,
			KeyPrefix: t.KeyPrefix,
			CreatedAt: t.CreatedAt,
			ExpiresAt: t.ExpiresAt,
			RevokedAt: t.RevokedAt,
		})
	}
	return out, nil
}

func (g grantStore) RevokeGrant(ctx context.Context, id int64) error {
	err := g.svc.RevokeGrant(ctx, id)
	if errors.Is(err, oauth.ErrNotFound) {
		return fmt.Errorf("%w: %d", httpapi.ErrGrantNotFound, id)
	}
	return err
}
