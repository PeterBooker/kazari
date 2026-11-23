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
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/backup"
	"github.com/PeterBooker/kazari/internal/blob"
	"github.com/PeterBooker/kazari/internal/buildinfo"
	"github.com/PeterBooker/kazari/internal/certs"
	"github.com/PeterBooker/kazari/internal/config"
	"github.com/PeterBooker/kazari/internal/dashboard"
	"github.com/PeterBooker/kazari/internal/dirlock"
	"github.com/PeterBooker/kazari/internal/fetch"
	"github.com/PeterBooker/kazari/internal/gate"
	"github.com/PeterBooker/kazari/internal/httpapi"
	"github.com/PeterBooker/kazari/internal/instrument"
	"github.com/PeterBooker/kazari/internal/mcpserver"
	"github.com/PeterBooker/kazari/internal/middleware"
	"github.com/PeterBooker/kazari/internal/oauth"
	"github.com/PeterBooker/kazari/internal/oidc"
	"github.com/PeterBooker/kazari/internal/passkey"
	"github.com/PeterBooker/kazari/internal/preflight"
	"github.com/PeterBooker/kazari/internal/present"
	"github.com/PeterBooker/kazari/internal/ratelimit"
	"github.com/PeterBooker/kazari/internal/scheduler"
	"github.com/PeterBooker/kazari/internal/site"
	"github.com/PeterBooker/kazari/internal/snapshot"
	"github.com/PeterBooker/kazari/internal/sqlite"
	"github.com/PeterBooker/kazari/internal/telemetry"
	"github.com/PeterBooker/kazari/internal/update"
	"github.com/PeterBooker/kazari/internal/upload"
	"github.com/PeterBooker/kazari/internal/user"
	"github.com/PeterBooker/kazari/internal/views"
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
	views      *views.Recorder // nil with analytics off
	ready      *preflight.Checker
	stderr     io.Writer
	certs      certs.Source // nil in proxy mode
	router     http.Handler
	admin      http.Handler // on cfg.Admin.Listen when set, else on the router
	adminLocal http.Handler // on the admin socket, without credentials
	oidcOn     bool
	idp        *oidc.Lazy // nil without auth.oidc
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

func (a *App) build(ctx context.Context, now func() time.Time, opts Options) error {
	tel := a.tel.Providers()
	fetchObs := fetch.NewObserver(tel)
	cfg, log := a.cfg, a.log

	blobs, err := blob.NewFS(cfg.BlobDir(), blob.Options{Telemetry: tel})
	if err != nil {
		return fmt.Errorf("app: %w", err)
	}
	auditStore := sqlite.NewAuditStore(a.db)

	userOpts := user.Options{DefaultQuota: int64(cfg.Limits.DefaultUserQuota), MaxKeyTTL: cfg.Auth.Tokens.MaxKeyTTL.Std(),
		Now: now, Telemetry: tel,
		LocalAccounts:     cfg.LocalAccounts(),
		MinPasswordLength: cfg.Auth.Local.MinPasswordLength,
		InviteTTL:         cfg.Auth.Local.InviteTTL.Std(),
		ResetTTL:          cfg.Auth.Local.ResetTTL.Std(),
		Argon2:            opts.Argon2,
	}
	if o := cfg.Auth.OIDC; o != nil {
		userOpts.AllowedDomains = o.AllowedDomains
		userOpts.AutoProvision = o.AutoProvision
		userOpts.AdminEmails = o.AdminEmails
	}
	// oauthSvc and arts are set below, before any request.
	var (
		oauthSvc *oauth.Service
		arts     *artifact.Service
	)
	userOpts.OnDisable = func(ctx context.Context, handle string) error {
		actor := artifact.Actor{Handle: "admin"}
		if p, ok := auth.FromContext(ctx); ok && p.Handle != "" {
			actor = present.Actor(p)
		}
		return errors.Join(oauthSvc.RevokeAllForUser(ctx, handle), arts.RevokeUserShareLinks(ctx, actor, handle))
	}
	userOpts.EndSessions = func(ctx context.Context, handle string, keep int64) error {
		return oauthSvc.EndSessions(ctx, handle, keep)
	}
	userOpts.RevokeGrants = func(ctx context.Context, handle string) error { return oauthSvc.RevokeAllForUser(ctx, handle) }
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
		CSP:               cfg.Serve.CSP,
		AllowCSPOverride:  cfg.Serve.AllowCSPOverride,
		Quotas:            users,
		Passwords:         users,
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
	arts = artifact.New(sqlite.NewArtifactStore(a.db), blobs, auditStore, artOpts)

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
		p, err := oidc.NewLazy(oidc.Config{
			Issuer:       o.Issuer,
			ClientID:     o.ClientID,
			ClientSecret: o.ClientSecret,
			RedirectURL:  mcpURL + strings.TrimPrefix(oauth.PathCallback, "/"),
			HTTPClient:   opts.OIDCClient,
			Telemetry:    tel,
			Fetch:        fetchObs,
		})
		if err != nil {
			return fmt.Errorf("app: %w", err)
		}
		// The pages and the API serve without the provider; only sign-in
		// waits for it.
		dctx, cancel := context.WithTimeout(ctx, oidcStartTimeout)
		err = p.Discover(dctx)
		cancel()
		if err != nil {
			log.WarnContext(ctx, "identity provider unreachable: sign-in answers that it is temporarily unavailable until it is reached",
				"issuer", o.Issuer, "err", err, "retry", oidcRetryEvery.String(), "next", "kazari check tests auth.oidc.issuer")
		}
		idp = p
		a.idp = p
		a.oidcOn = true
	}
	var (
		passwords  oauth.Passwords
		recoverURL string
		passkeys   *passkey.Service
	)
	if cfg.LocalAccounts() {
		passwords = users
		recoverURL = cfg.ExternalURL("") + "account/recover"
	}
	if cfg.LocalAccounts() && passkey.Supported(cfg.BaseDomain) {
		// The apex and the sign-in host share the base domain, so a passkey
		// made on the account page signs in on the sign-in page.
		passkeys, err = passkey.New(users, passkey.Options{
			RPID:    cfg.BaseDomain,
			Origins: []string{strings.TrimSuffix(cfg.ExternalURL(""), "/"), strings.TrimSuffix(mcpURL, "/")},
			Now:     now,
		})
		if err != nil {
			return fmt.Errorf("app: %w", err)
		}
	}
	oauthOpts := oauth.Options{
		Passwords:          passwords,
		RecoverURL:         recoverURL,
		ControlCallback:    controlCallback(cfg),
		Issuer:             strings.TrimSuffix(mcpURL, "/"),
		Resource:           mcpURL,
		AccessTTL:          cfg.Auth.Tokens.AccessTTL.Std(),
		RefreshTTL:         cfg.Auth.Tokens.RefreshTTL.Std(),
		RefreshReuseWindow: cfg.Auth.Tokens.RefreshReuseWindow.Std(),
		Now:                now,
		Logger:             log,
		InsecureCookies:    cfg.ExternalScheme == "http",
		ViewCallback:       viewCallback(cfg),
		Fetch:              fetchObs,
		Telemetry:          tel,
	}
	// Set only when there is one: a nil *passkey.Service in the interface
	// would not be a nil interface.
	if passkeys != nil {
		oauthOpts.Passkeys = passkeys
	}
	oauthSvc = oauth.New(sqlite.NewOAuthStore(a.db), users, users, idp, auditStore, oauthOpts)
	authn := auth.Chain(oauthSvc, users)
	links := present.Links{URL: cfg.ExternalURL}
	a.obs = middleware.NewObserver(tel, cfg.Listen.ProxyMode)
	// One write budget per credential across the user API and MCP.
	writes := ratelimit.New(cfg.Limits.WriteRatePerMinute, ratelimit.Options{Now: now, Scope: ratelimit.ScopeCredential, Telemetry: tel})

	backups := backup.New(a.db, backup.Options{
		DBPath: cfg.DBPath(), BlobDir: cfg.BlobDir(), CertDir: cfg.CertStorageDir(), TempDir: tempDir(cfg),
		Now: now, Logger: log, Audit: auditStore, Telemetry: tel,
	})
	var viewsSvc *views.Service
	if cfg.Analytics.Enabled {
		viewStore := sqlite.NewViewStore(a.db)
		viewsSvc = views.New(viewStore, views.Options{Retention: cfg.Analytics.Retention.Std(), Now: now})
		a.views = views.NewRecorder(viewStore, views.RecorderOptions{Readers: cfg.Analytics.Readers, Now: now, Telemetry: tel})
	}
	jobs := []scheduler.Job{
		{Name: "sweep", Every: cfg.Lifecycle.SweepInterval.Std(), Run: func(ctx context.Context) error {
			rep, err := arts.Sweep(ctx)
			if rep != (artifact.SweepReport{}) {
				log.InfoContext(ctx, "sweep", "soft_deleted", rep.SoftDeleted, "purged", rep.Purged, "freed", rep.Freed,
					"versions_deleted", rep.VersionsDeleted)
			}
			return err
		}},
		{Name: "gc", Every: cfg.Lifecycle.GCInterval.Std(), Run: func(ctx context.Context) error {
			// A backup copies the database, then the blobs it refers to: a
			// blob collected in between would be missing from the archive.
			release, ok := backups.HoldGC()
			if !ok {
				log.InfoContext(ctx, "gc: skipped, a backup is running")
				return nil
			}
			defer release()
			n, err := arts.CollectGarbage(ctx)
			if n > 0 {
				log.InfoContext(ctx, "gc", "blobs_deleted", n)
			}
			cutoff := now().Add(-staleTempAge)
			puts, perr := blobs.RemoveStaleTemp(cutoff)
			spills, serr := httpapi.RemoveStaleSpills(tempDir(cfg), cutoff)
			downloads, derr := fetch.RemoveStale(tempDir(cfg), cutoff)
			copies, berr := backup.RemoveStale(tempDir(cfg), cutoff)
			if puts+spills+downloads+copies > 0 {
				log.InfoContext(ctx, "gc", "temp_files_removed", puts+spills+downloads+copies)
			}
			return errors.Join(err, perr, serr, derr, berr)
		}},
		{Name: "uploads-expire", Every: uploadsExpireEvery, Run: func(ctx context.Context) error {
			n, err := uploads.ExpireStale(ctx)
			if n > 0 {
				log.InfoContext(ctx, "uploads expired", "count", n)
			}
			return err
		}},
		{Name: "oauth-cleanup", Every: oauthCleanupEvery, Run: func(ctx context.Context) error {
			n, err := oauthSvc.Cleanup(ctx)
			if n > 0 {
				log.DebugContext(ctx, "oauth cleanup", "rows", n)
			}
			return err
		}},
	}
	if a.idp != nil {
		jobs = append(jobs, scheduler.Job{Name: "oidc-discovery", Every: oidcRetryEvery, Run: func(ctx context.Context) error {
			if a.idp.Ready() == nil {
				return nil
			}
			if err := a.idp.Discover(ctx); err != nil {
				return err
			}
			log.InfoContext(ctx, "identity provider reached: sign-in is available", "issuer", a.idp.Issuer())
			return nil
		}})
	}
	if a.views != nil {
		jobs = append(jobs,
			scheduler.Job{Name: "views-flush", Every: viewsFlushEvery, Run: a.views.Flush},
			scheduler.Job{Name: "views-prune", Every: viewsPruneEvery, Run: func(ctx context.Context) error {
				n, err := viewsSvc.Prune(ctx)
				if n > 0 {
					log.InfoContext(ctx, "views pruned", "rows", n)
				}
				return err
			}})
	}
	// The one call Kazari makes on its own account: a failure is the
	// scheduler's to log, and never anything an admin is asked to act on.
	var updates *update.Checker
	if cfg.Admin.UpdateCheck {
		version := opts.Version
		if version == "" {
			version = buildinfo.Get().Version
		}
		// Traced like every other request Kazari makes, and held to public
		// addresses like them, except in a test, which stands in for the
		// releases API on this machine.
		var transport http.RoundTripper = fetch.Transport()
		if opts.UpdateURL != "" {
			transport = http.DefaultTransport
		}
		updates = update.New(update.Options{Current: version, URL: opts.UpdateURL, Now: now, Telemetry: tel,
			HTTPClient: &http.Client{Timeout: updateCheckTimeout, Transport: fetchObs.Transport(transport)}})
		if updates.Enabled() {
			jobs = append(jobs, scheduler.Job{Name: "update-check", Every: updateCheckEvery, Run: func(ctx context.Context) error {
				if err := updates.Check(fetch.WithPurpose(ctx, fetch.PurposeUpdate)); err != nil {
					return err
				}
				if r := updates.Result(); r.Newer {
					log.InfoContext(ctx, "a newer release is available", "latest", r.Latest, "running", version)
				}
				return nil
			}})
		}
	}
	a.sched = scheduler.New(scheduler.Options{Logger: log, Now: now, Telemetry: tel}, jobs...)
	ready := preflight.NewChecker(cfg, preflight.ReadyOptions{
		HTTPClient: opts.OIDCClient,
		Now:        now, CertNotAfter: a.certNotAfter, CertError: a.certError, Started: now(), Providers: tel,
	}, 0)
	a.ready = ready

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
		AllowCSPOverride:    cfg.Serve.AllowCSPOverride,
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
		MaxBodyBytes:    int64(cfg.Limits.MaxVersionBytes) + 1<<20,
		MaxVersionBytes: int64(cfg.Limits.MaxVersionBytes),
		MaxChunkBytes:   int64(cfg.Limits.MaxChunkBytes),
		MaxFileBytes:    int64(cfg.Limits.MaxFileBytes),
		MaxTotalBytes:   int64(cfg.Limits.MaxVersionBytes),
		Now:             now,
		TempDir:         tempDir(cfg),
		APIBaseURL:      apiBase,
		Skill:           mcpserver.Skill(links, mcpOpts),
		Telemetry:       tel,
	})
	adminDeps := httpapi.AdminDeps{
		Artifacts: arts,
		Users:     users,
		Auth:      authn,
		Audit:     auditStore,
		Jobs:      a.sched,
		Grants:    grantStore{oauthSvc},
		Sessions:  oauthSvc,
		Backup:    backups,
		Ready:     ready,
		Settings:  cfg.Redacted,
		Sources:   a.origin.Sources,
		Metrics:   a.tel.MetricsHandler(),
		Views:     viewsSvc,
		Links:     links,
		Logger:    log,
	}
	// Set only when there is one: a nil *update.Checker in the interface
	// would not be a nil interface.
	if updates != nil {
		adminDeps.Updates = updates
	}
	if cfg.AdminSocketPath() != "" {
		a.adminLocal = a.surface(instrument.SurfaceAdmin, true, httpapi.NewAdminAPI(adminDeps,
			httpapi.AdminOptions{Local: true, Now: now, Telemetry: tel, BackupName: cfg.BaseDomain}))
	}
	adminOpts := httpapi.AdminOptions{AdminToken: cfg.Auth.AdminToken, Now: now, Telemetry: tel, BackupName: cfg.BaseDomain}
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
	siteOpts := site.Options{
		BaseDomain:       cfg.BaseDomain,
		ReservedSlugs:    cfg.Serve.ReservedSlugs,
		DefaultCSP:       cfg.Serve.CSP,
		AllowCSPOverride: cfg.Serve.AllowCSPOverride,
		ImmutableMaxAge:  cfg.Lifecycle.Tombstone.Std(),
		Logger:           log,
		Gate: gate.New(oauthSvc, arts, gate.Options{
			SignInURL:       mcpURL + strings.TrimPrefix(oauth.PathSignIn, "/"),
			SignOutURL:      mcpURL + strings.TrimPrefix(oauth.PathSignOut, "/"),
			InsecureCookies: cfg.ExternalScheme == "http",
			Now:             now,
			Logger:          log,
			Telemetry:       tel,
		}),
	}
	if a.views != nil {
		siteOpts.Views = a.views
	}
	siteH := site.NewHandler(arts, blobs, siteOpts)
	accountDeps := account.Deps{
		Sessions: oauthSvc, Users: users, Grants: oauthSvc, Artifacts: arts, Audit: auditStore, Links: links, Logger: log,
	}
	if passkeys != nil {
		accountDeps.Passkeys = passkeys
	}
	accountH := account.New(accountDeps, account.Options{
		URL:             strings.TrimSuffix(cfg.ExternalURL(""), "/"),
		SignInURL:       mcpURL + strings.TrimPrefix(oauth.PathSignIn, "/"),
		AdminURL:        dashboardURL(cfg),
		MCPURL:          mcpURL,
		APIURL:          apiBase,
		LocalAccounts:   cfg.LocalAccounts(),
		MaxKeyTTL:       cfg.Auth.Tokens.MaxKeyTTL.Std(),
		MaxShareLinkTTL: cfg.Serve.ShareLinkMaxTTL.Std(),
		InsecureCookies: cfg.ExternalScheme == "http",
		Now:             now,
		Telemetry:       tel,
	})
	// The link pages take a token from anyone who has the address, so they
	// get the per-IP budget every other surface has before authentication.
	accountIPs := ratelimit.New(ratelimit.DefaultIPPerMinute, ratelimit.Options{Now: now, Scope: ratelimit.ScopeIP, Telemetry: tel})
	accountLimit := ratelimit.Middleware(accountIPs, ratelimit.IPKey, nil)
	front := account.NewFront(account.FrontOptions{
		URL: strings.TrimSuffix(cfg.ExternalURL(""), "/"), MCPURL: mcpURL, APIURL: apiBase, AdminURL: dashboardURL(cfg),
	})
	apexH := apexHandler(front.Home)

	adminHost := adminH
	if cfg.DashboardEnabled() {
		deps := dashboard.Deps{
			Sessions: oauthSvc, Users: users, Artifacts: arts, Grants: oauthSvc, Audit: auditStore, Recorder: auditStore,
			Jobs: a.sched, Ready: ready, Backup: backups, Settings: cfg.Redacted, Views: viewsSvc, Links: links, Logger: log,
			Flags: sqlite.NewFlagStore(a.db),
		}
		// Assigned only when there is one: a nil *update.Checker in the
		// interface would not be a nil interface.
		if updates != nil {
			deps.Updates = updates
		}
		adminHost = adminMux(adminH, dashboard.New(deps, dashboard.Options{
			URL:             cfg.AdminURL(),
			SignInURL:       mcpURL + strings.TrimPrefix(oauth.PathSignIn, "/"),
			AccountURL:      cfg.ExternalURL("") + "account/",
			MCPURL:          mcpURL,
			APIURL:          apiBase,
			BaseDomain:      cfg.BaseDomain,
			LocalAccounts:   cfg.LocalAccounts(),
			InsecureCookies: plainHTTP(cfg.AdminURL()),
			Now:             now,
			Started:         now(),
			Build:           buildinfo.Get(),
			DataDir:         cfg.DataDir,
			TLSMode:         tlsMode(cfg),
			CertNotAfter:    a.certNotAfter,
			Retention:       cfg.Lifecycle.Retention.Std(),
			Tombstone:       cfg.Lifecycle.Tombstone.Std(),
			MaxKeyTTL:       cfg.Auth.Tokens.MaxKeyTTL.Std(),
			MaxShareLinkTTL: cfg.Serve.ShareLinkMaxTTL.Std(),
			DefaultQuota:    int64(cfg.Limits.DefaultUserQuota),
			ViewReaders:     cfg.Analytics.Readers,
			Telemetry:       tel,
		}))
	}
	a.admin = a.surface(instrument.SurfaceAdmin, true, adminHost)
	publicAdmin := a.admin
	if cfg.Admin.Listen != "" {
		publicAdmin = a.surface(instrument.SurfaceAdmin, true, http.NotFoundHandler())
	}
	// One per-IP budget for OAuth and MCP, applied before authentication
	// like the user API's.
	mcpIPs := ratelimit.New(ratelimit.DefaultIPPerMinute, ratelimit.Options{Now: now, Scope: ratelimit.ScopeIP, Telemetry: tel})
	mcpLimit := ratelimit.Middleware(mcpIPs, ratelimit.IPKey, nil)
	a.router = a.routes(hosts{
		api:     a.surface(instrument.SurfaceAPI, true, apiHost(front.API, apiH)),
		mcp:     a.surface(instrument.SurfaceMCP, true, mcpLimit(frontPage(front.Connector, mcpMux(oauthSvc.Handler(), mcpH)))),
		admin:   publicAdmin,
		apex:    a.surface(instrument.SurfaceApex, true, apexH),
		account: a.surface(instrument.SurfaceAccount, true, accountLimit(accountH)),
		site:    a.surface(instrument.SurfaceSite, false, siteH),
	})

	a.mode = tlsMode(cfg)
	if !cfg.Listen.ProxyMode {
		_, span := instrument.StartChild(ctx, a.tracer, "certs.init")
		a.certs, err = certSource(cfg, log, now)
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
func controlCallback(cfg config.Config) func(host string) (string, bool) {
	admin := dashboardURL(cfg)
	if admin != "" {
		admin += dashboard.CallbackPath
	}
	apex := cfg.ExternalURL("") + strings.TrimPrefix(account.CallbackPath, "/")
	return func(host string) (string, bool) {
		switch host {
		case oauth.HostAdmin:
			return admin, admin != ""
		case oauth.HostAccount:
			return apex, true
		}
		return "", false
	}
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
