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

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/blob"
	"github.com/PeterBooker/kazari/internal/certs"
	"github.com/PeterBooker/kazari/internal/config"
	"github.com/PeterBooker/kazari/internal/gate"
	"github.com/PeterBooker/kazari/internal/httpapi"
	"github.com/PeterBooker/kazari/internal/mcpserver"
	"github.com/PeterBooker/kazari/internal/metrics"
	"github.com/PeterBooker/kazari/internal/oauth"
	"github.com/PeterBooker/kazari/internal/oidc"
	"github.com/PeterBooker/kazari/internal/present"
	"github.com/PeterBooker/kazari/internal/ratelimit"
	"github.com/PeterBooker/kazari/internal/scheduler"
	"github.com/PeterBooker/kazari/internal/site"
	"github.com/PeterBooker/kazari/internal/sqlite"
	"github.com/PeterBooker/kazari/internal/upload"
	"github.com/PeterBooker/kazari/internal/user"
)

const (
	uploadsExpireEvery = 10 * time.Minute
	oauthCleanupEvery  = time.Hour
	staleTempAge       = time.Hour // older temp files are crash leftovers
)

// Options are the process-level inputs that do not belong in the config.
type Options struct {
	Now    func() time.Time // nil means time.Now
	Logger *slog.Logger     // nil means slog.Default()
}

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

	db, err := sqlite.Open(ctx, cfg.DBPath())
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	a := &App{cfg: cfg, log: log, db: db, started: make(chan struct{})}
	if err := a.build(ctx, now); err != nil {
		db.Close()
		return nil, err
	}
	return a, nil
}

func (a *App) build(ctx context.Context, now func() time.Time) error {
	cfg, log := a.cfg, a.log

	blobs, err := blob.NewFS(cfg.BlobDir())
	if err != nil {
		return fmt.Errorf("app: %w", err)
	}
	auditStore := sqlite.NewAuditStore(a.db)

	arts := artifact.New(sqlite.NewArtifactStore(a.db), blobs, auditStore, artifact.Options{
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
		Now:               now,
		Logger:            log,
	})

	uploads := upload.New(sqlite.NewUploadStore(a.db), upload.Options{
		Dir:             cfg.UploadDir(),
		MaxChunkBytes:   int64(cfg.Limits.MaxChunkBytes),
		MaxTotalBytes:   int64(cfg.Limits.MaxVersionBytes),
		MaxFiles:        cfg.Limits.MaxFiles,
		TTL:             cfg.Limits.UploadTTL.Std(),
		MaxOpenPerOwner: cfg.Limits.MaxOpenUploads,
		Now:             now,
	})

	userOpts := user.Options{DefaultQuota: int64(cfg.Limits.DefaultUserQuota), Now: now}
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

	mcpURL := cfg.ExternalURL("mcp")
	var idp oauth.IdentityProvider
	if o := cfg.Auth.OIDC; o != nil {
		p, err := oidc.New(ctx, oidc.Config{
			Issuer:       o.Issuer,
			ClientID:     o.ClientID,
			ClientSecret: o.ClientSecret,
			RedirectURL:  mcpURL + strings.TrimPrefix(oauth.PathCallback, "/"),
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
	})
	authn := auth.Chain(oauthSvc, users)
	links := present.Links{URL: cfg.ExternalURL}
	m := metrics.New()
	// One write budget per credential across the user API and MCP.
	writes := ratelimit.New(cfg.Limits.WriteRatePerMinute, now)

	a.sched = scheduler.New(scheduler.Options{Logger: log, Now: now},
		job(m, "sweep", cfg.Lifecycle.SweepInterval.Std(), func(ctx context.Context) error {
			rep, err := arts.Sweep(ctx)
			if rep != (artifact.SweepReport{}) {
				log.InfoContext(ctx, "sweep", "soft_deleted", rep.SoftDeleted, "purged", rep.Purged, "freed", rep.Freed)
			}
			return err
		}),
		job(m, "gc", cfg.Lifecycle.GCInterval.Std(), func(ctx context.Context) error {
			n, err := arts.CollectGarbage(ctx)
			if n > 0 {
				log.InfoContext(ctx, "gc", "blobs_deleted", n)
			}
			cutoff := now().Add(-staleTempAge)
			puts, perr := blobs.RemoveStaleTemp(cutoff)
			spills, serr := httpapi.RemoveStaleSpills(tempDir(cfg), cutoff)
			if puts+spills > 0 {
				log.InfoContext(ctx, "gc", "temp_files_removed", puts+spills)
			}
			return errors.Join(err, perr, serr)
		}),
		job(m, "uploads-expire", uploadsExpireEvery, func(ctx context.Context) error {
			n, err := uploads.ExpireStale(ctx)
			if n > 0 {
				log.InfoContext(ctx, "uploads expired", "count", n)
			}
			return err
		}),
		job(m, "oauth-cleanup", oauthCleanupEvery, func(ctx context.Context) error {
			n, err := oauthSvc.Cleanup(ctx)
			if n > 0 {
				log.DebugContext(ctx, "oauth cleanup", "rows", n)
			}
			return err
		}),
	)

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
		APIBaseURL:          cfg.ExternalURL("api") + "v1",
		MCPURL:              mcpURL,
		DefaultVisibility:   cfg.Serve.DefaultVisibility,
		MaxShareLinkTTL:     cfg.Serve.ShareLinkMaxTTL.Std(),
		Now:                 now,
	}
	apiH := httpapi.NewUserAPI(httpapi.UserDeps{
		Artifacts: arts, Uploads: uploads, Users: users, Auth: authn, Links: links, Logger: log, Writes: writes,
	}, httpapi.UserOptions{
		MaxBodyBytes:  int64(cfg.Limits.MaxVersionBytes) + 1<<20,
		MaxChunkBytes: int64(cfg.Limits.MaxChunkBytes),
		MaxTotalBytes: int64(cfg.Limits.MaxVersionBytes),
		Now:           now,
		TempDir:       tempDir(cfg),
		Skill:         mcpserver.Skill(links, mcpOpts),
	})
	adminDeps := httpapi.AdminDeps{
		Artifacts: arts,
		Users:     users,
		Auth:      authn,
		Audit:     auditStore,
		Jobs:      a.sched,
		Grants:    grantStore{oauthSvc},
		Settings:  cfg.Redacted,
		Metrics:   m.Handler(),
		Links:     links,
		Logger:    log,
	}
	adminH := httpapi.NewAdminAPI(adminDeps, httpapi.AdminOptions{AdminToken: cfg.Auth.AdminToken, Now: now})
	if cfg.AdminSocketPath() != "" {
		a.adminLocal = a.surface(m, "admin", true, httpapi.NewAdminAPI(adminDeps, httpapi.AdminOptions{Local: true, Now: now}))
	}
	mcpH := mcpserver.NewHandler(mcpserver.Deps{
		Artifacts: arts, Uploads: uploads, Auth: authn, Links: links, Logger: log, Writes: writes,
	}, mcpOpts)
	siteH := site.NewHandler(arts, blobs, site.Options{
		BaseDomain:       cfg.BaseDomain,
		DefaultCSP:       cfg.Serve.CSP,
		AllowCSPOverride: cfg.Serve.AllowCSPOverride,
		ImmutableMaxAge:  cfg.Lifecycle.Tombstone.Std(),
		Logger:           log,
		Gate: gate.New(oauthSvc, arts, gate.Options{
			SignInURL:       mcpURL + strings.TrimPrefix(oauth.PathSignIn, "/"),
			InsecureCookies: cfg.ExternalScheme == "http",
			Now:             now,
			Logger:          log,
		}),
	})
	apexH, err := apexHandler(cfg)
	if err != nil {
		return err
	}

	a.admin = a.surface(m, "admin", true, adminH)
	publicAdmin := a.admin
	if cfg.Admin.Listen != "" {
		publicAdmin = a.surface(m, "admin", true, http.NotFoundHandler())
	}
	// One per-IP budget for OAuth and MCP, applied before authentication
	// like the user API's.
	mcpLimit := ratelimit.Middleware(ratelimit.New(ratelimit.DefaultIPPerMinute, now), ratelimit.IPKey, nil)
	a.router = a.routes(hosts{
		api:   a.surface(m, "api", true, apiH),
		mcp:   a.surface(m, "mcp", true, mcpLimit(mcpMux(oauthSvc.Handler(), mcpH))),
		admin: publicAdmin,
		apex:  a.surface(m, "apex", true, apexH),
		site:  a.surface(m, "site", false, siteH),
	})

	a.mode = "proxy"
	if !cfg.Listen.ProxyMode {
		a.mode = cfg.TLS.Mode
		if a.certs, err = certSource(cfg, log); err != nil {
			return err
		}
	}
	return nil
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

func job(m *metrics.Metrics, name string, every time.Duration, fn func(context.Context) error) scheduler.Job {
	return scheduler.Job{Name: name, Every: every, Run: func(ctx context.Context) error {
		return m.Job(name, func() error { return fn(ctx) })()
	}}
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

// Close closes the database; it is idempotent and Run calls it on exit.
func (a *App) Close() error {
	a.closeOnce.Do(func() {
		if err := a.db.Close(); err != nil {
			a.closeErr = fmt.Errorf("app: close database: %w", err)
		}
	})
	return a.closeErr
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
