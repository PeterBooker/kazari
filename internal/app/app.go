// Package app wires every Kazari component from a config.Config and runs the
// listeners, the scheduler and the certificate source.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/blob"
	"github.com/PeterBooker/kazari/internal/buildinfo"
	"github.com/PeterBooker/kazari/internal/certs"
	"github.com/PeterBooker/kazari/internal/config"
	"github.com/PeterBooker/kazari/internal/fetch"
	"github.com/PeterBooker/kazari/internal/gate"
	"github.com/PeterBooker/kazari/internal/httpapi"
	"github.com/PeterBooker/kazari/internal/instrument"
	"github.com/PeterBooker/kazari/internal/mcpserver"
	"github.com/PeterBooker/kazari/internal/middleware"
	"github.com/PeterBooker/kazari/internal/oauth"
	"github.com/PeterBooker/kazari/internal/oidc"
	"github.com/PeterBooker/kazari/internal/present"
	"github.com/PeterBooker/kazari/internal/ratelimit"
	"github.com/PeterBooker/kazari/internal/scheduler"
	"github.com/PeterBooker/kazari/internal/site"
	"github.com/PeterBooker/kazari/internal/snapshot"
	"github.com/PeterBooker/kazari/internal/sqlite"
	"github.com/PeterBooker/kazari/internal/telemetry"
	"github.com/PeterBooker/kazari/internal/upload"
	"github.com/PeterBooker/kazari/internal/user"
)

const (
	uploadsExpireEvery = 10 * time.Minute
	oauthCleanupEvery  = time.Hour
	staleTempAge       = time.Hour // older temp files are crash leftovers
	// telemetryFlushTimeout bounds the last export at shutdown, so a dead
	// collector cannot hold the process.
	telemetryFlushTimeout = 5 * time.Second
)

// Options are the process-level inputs that do not belong in the config.
type Options struct {
	Now    func() time.Time // nil means time.Now
	Logger *slog.Logger     // nil means slog.Default()

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
	certs      certs.Source // nil in proxy mode
	router     http.Handler
	admin      http.Handler // on cfg.Admin.Listen when set, else on the router
	adminLocal http.Handler // on the admin socket, without credentials
	oidcOn     bool
	started    chan struct{}

	mu    sync.Mutex
	addrs Addrs

	closeOnce sync.Once
	closeErr  error
}

// New validates cfg, creates the data directories, opens the database and
// runs OIDC discovery; nothing listens until Run.
func New(ctx context.Context, cfg config.Config, opts Options) (*App, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("app: invalid config:\n%w", err)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}

	for _, dir := range []string{cfg.DataDir, cfg.BlobDir(), cfg.UploadDir(), cfg.CertStorageDir(), tempDir(cfg)} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("app: create data directory: %w", err)
		}
	}

	tel, err := telemetry.New(ctx, telemetryOptions(cfg, opts, log, now))
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	log = tel.Logger(log)
	a := &App{cfg: cfg, log: log, tel: tel, tracer: tel.Providers().Tracer(scope), started: make(chan struct{})}
	if err := a.start(ctx, now); err != nil {
		if a.db == nil {
			a.flushTelemetry()
		} else {
			a.Close()
		}
		return nil, err
	}
	return a, nil
}

// start is traced as kazari.start, to show why a start was slow.
func (a *App) start(ctx context.Context, now func() time.Time) (err error) {
	ctx, span := a.tracer.Start(ctx, "kazari.start")
	defer func() { instrument.End(span, err, "") }()
	migrateCtx, migrate := instrument.StartChild(ctx, a.tracer, "sqlite.migrate")
	a.db, err = sqlite.Open(migrateCtx, a.cfg.DBPath(), sqlite.Options{Telemetry: a.tel.Providers()})
	instrument.End(migrate, err, "")
	if err != nil {
		return fmt.Errorf("app: %w", err)
	}
	return a.build(ctx, now)
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

func (a *App) build(ctx context.Context, now func() time.Time) error {
	tel := a.tel.Providers()
	fetchObs := fetch.NewObserver(tel)
	cfg, log := a.cfg, a.log

	blobs, err := blob.NewFS(cfg.BlobDir(), blob.Options{Telemetry: tel})
	if err != nil {
		return fmt.Errorf("app: %w", err)
	}
	auditStore := sqlite.NewAuditStore(a.db)

	userOpts := user.Options{DefaultQuota: int64(cfg.Limits.DefaultUserQuota), Now: now, Telemetry: tel}
	if o := cfg.Auth.OIDC; o != nil {
		userOpts.AllowedDomains = o.AllowedDomains
		userOpts.AutoProvision = o.AutoProvision
		userOpts.AdminEmails = o.AdminEmails
	}
	// oauthSvc is set below, before any request.
	var oauthSvc *oauth.Service
	userOpts.OnDisable = func(ctx context.Context, handle string) error {
		return oauthSvc.RevokeAllForUser(ctx, handle)
	}
	users := user.New(sqlite.NewUserStore(a.db), auditStore, userOpts)
	a.users = users

	var fetcher *fetch.Client
	artOpts := artifact.Options{
		MaxVersionBytes:   int64(cfg.Limits.MaxVersionBytes),
		MaxFileBytes:      int64(cfg.Limits.MaxFileBytes),
		MaxFiles:          cfg.Limits.MaxFiles,
		MaxMetadataPairs:  cfg.Limits.MetadataMaxPairs,
		ReservedSlugs:     cfg.Serve.ReservedSlugs,
		DefaultGrace:      cfg.Lifecycle.DefaultGrace.Std(),
		Retention:         cfg.Lifecycle.Retention.Std(),
		Tombstone:         cfg.Lifecycle.Tombstone.Std(),
		WritePolicy:       cfg.Auth.WritePolicy,
		DefaultVisibility: artifact.Visibility(cfg.Serve.DefaultVisibility),
		MaxShareLinkTTL:   cfg.Serve.ShareLinkMaxTTL.Std(),
		Quotas:            users,
		Now:               now,
		Logger:            log,
		Telemetry:         tel,
	}
	if cfg.Fetch.Enabled {
		fetcher = fetch.New(fetch.Options{
			Timeout:   cfg.Fetch.Timeout.Std(),
			Budget:    cfg.Fetch.Budget.Std(),
			Dir:       tempDir(cfg),
			UserAgent: "Kazari/" + buildinfo.Get().Version + " (+https://kazari.io)",
			Observer:  fetchObs,
		})
		artOpts.Pages = snapshot.New(fetcher, snapshot.Options{
			CSP:           cfg.Serve.CSP,
			MaxFileBytes:  int64(cfg.Limits.MaxFileBytes),
			MaxTotalBytes: int64(cfg.Limits.MaxVersionBytes),
			MaxFiles:      cfg.Limits.MaxFiles,
		})
	}
	arts := artifact.New(sqlite.NewArtifactStore(a.db), blobs, auditStore, artOpts)

	uploads := upload.New(sqlite.NewUploadStore(a.db), upload.Options{
		Telemetry:       tel,
		Dir:             cfg.UploadDir(),
		MaxChunkBytes:   int64(cfg.Limits.MaxChunkBytes),
		MaxTotalBytes:   int64(cfg.Limits.MaxVersionBytes),
		MaxFiles:        cfg.Limits.MaxFiles,
		TTL:             cfg.Limits.UploadTTL.Std(),
		MaxOpenPerOwner: cfg.Limits.MaxOpenUploads,
		Now:             now,
	})

	mcpURL := cfg.ExternalURL("mcp")
	var idp oauth.IdentityProvider
	if o := cfg.Auth.OIDC; o != nil {
		p, err := oidc.New(ctx, oidc.Config{
			Issuer:       o.Issuer,
			ClientID:     o.ClientID,
			ClientSecret: o.ClientSecret,
			RedirectURL:  mcpURL + strings.TrimPrefix(oauth.PathCallback, "/"),
			Telemetry:    tel,
			Fetch:        fetchObs,
		})
		if err != nil {
			return fmt.Errorf("app: %w", err)
		}
		idp = p
		a.oidcOn = true
	}
	oauthSvc = oauth.New(sqlite.NewOAuthStore(a.db), users, users, idp, auditStore, oauth.Options{
		Issuer:          strings.TrimSuffix(mcpURL, "/"),
		Resource:        mcpURL,
		Now:             now,
		Logger:          log,
		InsecureCookies: cfg.ExternalScheme == "http",
		ViewCallback:    viewCallback(cfg),
		Fetch:           fetchObs,
		Telemetry:       tel,
	})
	authn := auth.Chain(oauthSvc, users)
	links := present.Links{URL: cfg.ExternalURL}
	a.obs = middleware.NewObserver(tel, cfg.Listen.ProxyMode)
	// One write budget per credential across the user API and MCP.
	writes := ratelimit.New(cfg.Limits.WriteRatePerMinute, ratelimit.Options{Now: now, Scope: ratelimit.ScopeCredential, Telemetry: tel})

	a.sched = scheduler.New(scheduler.Options{Logger: log, Now: now, Telemetry: tel},
		scheduler.Job{Name: "sweep", Every: cfg.Lifecycle.SweepInterval.Std(), Run: func(ctx context.Context) error {
			rep, err := arts.Sweep(ctx)
			if rep != (artifact.SweepReport{}) {
				log.InfoContext(ctx, "sweep", "soft_deleted", rep.SoftDeleted, "purged", rep.Purged, "freed", rep.Freed)
			}
			return err
		}},
		scheduler.Job{Name: "gc", Every: cfg.Lifecycle.GCInterval.Std(), Run: func(ctx context.Context) error {
			n, err := arts.CollectGarbage(ctx)
			if n > 0 {
				log.InfoContext(ctx, "gc", "blobs_deleted", n)
			}
			cutoff := now().Add(-staleTempAge)
			puts, perr := blobs.RemoveStaleTemp(cutoff)
			spills, serr := httpapi.RemoveStaleSpills(tempDir(cfg), cutoff)
			downloads, derr := fetch.RemoveStale(tempDir(cfg), cutoff)
			if puts+spills+downloads > 0 {
				log.InfoContext(ctx, "gc", "temp_files_removed", puts+spills+downloads)
			}
			return errors.Join(err, perr, serr, derr)
		}},
		scheduler.Job{Name: "uploads-expire", Every: uploadsExpireEvery, Run: func(ctx context.Context) error {
			n, err := uploads.ExpireStale(ctx)
			if n > 0 {
				log.InfoContext(ctx, "uploads expired", "count", n)
			}
			return err
		}},
		scheduler.Job{Name: "oauth-cleanup", Every: oauthCleanupEvery, Run: func(ctx context.Context) error {
			n, err := oauthSvc.Cleanup(ctx)
			if n > 0 {
				log.DebugContext(ctx, "oauth cleanup", "rows", n)
			}
			return err
		}},
	)

	apiBase := cfg.ExternalURL("api") + "v1"
	mcpOpts := mcpserver.Options{
		BaseDomain:          cfg.BaseDomain,
		ResourceMetadataURL: mcpURL + strings.TrimPrefix(oauth.PathResourceMeta, "/"),
		AllowedOrigins:      []string{strings.TrimSuffix(mcpURL, "/")},
		InlineGuidanceBytes: int64(cfg.Limits.InlineGuidanceBytes),
		MaxVersionBytes:     int64(cfg.Limits.MaxVersionBytes),
		MaxFileBytes:        int64(cfg.Limits.MaxFileBytes),
		MaxFiles:            cfg.Limits.MaxFiles,
		MaxChunkBytes:       int64(cfg.Limits.MaxChunkBytes),
		MaxTotalBytes:       int64(cfg.Limits.MaxVersionBytes),
		CSP:                 cfg.Serve.CSP,
		ReservedSlugs:       cfg.Serve.ReservedSlugs,
		APIBaseURL:          apiBase,
		MCPURL:              mcpURL,
		DefaultVisibility:   cfg.Serve.DefaultVisibility,
		MaxShareLinkTTL:     cfg.Serve.ShareLinkMaxTTL.Std(),
		Fetch:               cfg.Fetch.Enabled,
		Now:                 now,
		Telemetry:           tel,
	}
	apiH := httpapi.NewUserAPI(httpapi.UserDeps{
		Artifacts: arts, Uploads: uploads, Users: users, Auth: authn, Links: links, Logger: log, Writes: writes, Fetch: fetcher,
	}, httpapi.UserOptions{
		MaxBodyBytes:  int64(cfg.Limits.MaxVersionBytes) + 1<<20,
		MaxChunkBytes: int64(cfg.Limits.MaxChunkBytes),
		MaxFileBytes:  int64(cfg.Limits.MaxFileBytes),
		MaxTotalBytes: int64(cfg.Limits.MaxVersionBytes),
		Now:           now,
		TempDir:       tempDir(cfg),
		APIBaseURL:    apiBase,
		Skill:         mcpserver.Skill(links, mcpOpts),
		Telemetry:     tel,
	})
	adminDeps := httpapi.AdminDeps{
		Artifacts: arts,
		Users:     users,
		Auth:      authn,
		Audit:     auditStore,
		Jobs:      a.sched,
		Grants:    grantStore{oauthSvc},
		Settings:  cfg.Redacted,
		Metrics:   a.tel.MetricsHandler(),
		Links:     links,
		Logger:    log,
	}
	if cfg.AdminSocketPath() != "" {
		a.adminLocal = a.surface(instrument.SurfaceAdmin, true, httpapi.NewAdminAPI(adminDeps, httpapi.AdminOptions{Local: true, Now: now, Telemetry: tel}))
	}
	adminOpts := httpapi.AdminOptions{AdminToken: cfg.Auth.AdminToken, Now: now, Telemetry: tel}
	if cfg.Admin.Listen == "" && !config.IsLocalhost(cfg.BaseDomain) {
		// On the public listener the gauges and exemplar trace ids are for
		// the operator's scraper alone.
		adminOpts.MetricsToken = cfg.Telemetry.Metrics.Token
		if adminOpts.MetricsToken == "" {
			adminDeps.Metrics = nil
		}
	}
	adminH := httpapi.NewAdminAPI(adminDeps, adminOpts)
	mcpH := mcpserver.NewHandler(mcpserver.Deps{
		Artifacts: arts, Uploads: uploads, Auth: authn, Links: links, Logger: log, Writes: writes, Fetch: fetcher,
	}, mcpOpts)
	siteH := site.NewHandler(arts, blobs, site.Options{
		BaseDomain:       cfg.BaseDomain,
		ReservedSlugs:    cfg.Serve.ReservedSlugs,
		DefaultCSP:       cfg.Serve.CSP,
		AllowCSPOverride: cfg.Serve.AllowCSPOverride,
		ImmutableMaxAge:  cfg.Lifecycle.Tombstone.Std(),
		Logger:           log,
		Gate: gate.New(oauthSvc, arts, gate.Options{
			SignInURL:       mcpURL + strings.TrimPrefix(oauth.PathSignIn, "/"),
			InsecureCookies: cfg.ExternalScheme == "http",
			Now:             now,
			Logger:          log,
			Telemetry:       tel,
		}),
	})
	apexH, err := apexHandler(cfg)
	if err != nil {
		return err
	}

	a.admin = a.surface(instrument.SurfaceAdmin, true, adminH)
	publicAdmin := a.admin
	if cfg.Admin.Listen != "" {
		publicAdmin = a.surface(instrument.SurfaceAdmin, true, http.NotFoundHandler())
	}
	// One per-IP budget for OAuth and MCP, applied before authentication
	// like the user API's.
	mcpIPs := ratelimit.New(ratelimit.DefaultIPPerMinute, ratelimit.Options{Now: now, Scope: ratelimit.ScopeIP, Telemetry: tel})
	mcpLimit := ratelimit.Middleware(mcpIPs, ratelimit.IPKey, nil)
	a.router = a.routes(hosts{
		api:   a.surface(instrument.SurfaceAPI, true, apiH),
		mcp:   a.surface(instrument.SurfaceMCP, true, mcpLimit(mcpMux(oauthSvc.Handler(), mcpH))),
		admin: publicAdmin,
		apex:  a.surface(instrument.SurfaceApex, true, apexH),
		site:  a.surface(instrument.SurfaceSite, false, siteH),
	})

	a.mode = "proxy"
	if !cfg.Listen.ProxyMode {
		a.mode = cfg.TLS.Mode
		_, span := instrument.StartChild(ctx, a.tracer, "certs.init")
		a.certs, err = certSource(cfg, log)
		instrument.End(span, err, "")
		if err != nil {
			return err
		}
		observeExpiry(tel.Meter(scope), a.certs)
	}
	return nil
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

func certSource(cfg config.Config, log *slog.Logger) (certs.Source, error) {
	switch cfg.TLS.Mode {
	case "self-signed":
		s, err := certs.NewSelfSigned(cfg.BaseDomain)
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
			Email:      cfg.TLS.Email,
			CA:         cfg.TLS.CA,
			Domains:    []string{cfg.BaseDomain, "*." + cfg.BaseDomain},
			StorageDir: cfg.CertStorageDir(),
			Logger:     log,
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

// Close flushes telemetry and closes the database; it is idempotent and Run
// calls it on exit.
func (a *App) Close() error {
	a.closeOnce.Do(func() {
		a.flushTelemetry()
		if err := a.db.Close(); err != nil {
			a.closeErr = fmt.Errorf("app: close database: %w", err)
		}
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
			ID:        t.ID,
			ClientID:  t.ClientID,
			Client:    name,
			User:      t.UserHandle,
			Scopes:    t.Scopes,
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
