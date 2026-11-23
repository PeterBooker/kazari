package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/PeterBooker/kazari/internal/account"
	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/backup"
	"github.com/PeterBooker/kazari/internal/blob"
	"github.com/PeterBooker/kazari/internal/buildinfo"
	"github.com/PeterBooker/kazari/internal/config"
	"github.com/PeterBooker/kazari/internal/dashboard"
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
	"github.com/PeterBooker/kazari/internal/signin"
	"github.com/PeterBooker/kazari/internal/site"
	"github.com/PeterBooker/kazari/internal/snapshot"
	"github.com/PeterBooker/kazari/internal/sqlite"
	"github.com/PeterBooker/kazari/internal/update"
	"github.com/PeterBooker/kazari/internal/upload"
	"github.com/PeterBooker/kazari/internal/user"
	"github.com/PeterBooker/kazari/internal/views"
)

// parts are what build has made so far, handed from one step to the next.
type parts struct {
	now      func() time.Time
	tel      instrument.Providers
	fetchObs *fetch.Observer

	blobs    *blob.FS
	audit    *sqlite.AuditStore
	users    *user.Service
	fetcher  *fetch.Client // nil with fetch off
	arts     *artifact.Service
	shares   *artifact.ShareLinks
	uploads  *upload.Service
	backups  *backup.Service
	viewsSvc *views.Service // nil with analytics off

	idp      signin.IdentityProvider
	passkeys *passkey.Service
	signins  *signin.Service
	oauth    *oauth.Service
	authn    auth.Authenticator
	links    present.Links

	updates *update.Checker // nil with admin.update_check off
}

func (a *App) build(ctx context.Context, now func() time.Time, opts Options) error {
	cfg, log := a.cfg, a.log
	p, err := a.domain(now, opts)
	if err != nil {
		return err
	}
	if err := a.identity(ctx, p, opts); err != nil {
		return err
	}
	a.obs = middleware.NewObserver(p.tel, cfg.Listen.ProxyMode)
	p.updates = a.updateChecker(p, opts)
	a.sched = scheduler.New(scheduler.Options{Logger: log, Now: now, Telemetry: p.tel}, a.jobs(p, opts)...)
	a.ready = preflight.NewChecker(cfg, preflight.ReadyOptions{
		HTTPClient: opts.OIDCClient,
		Now:        now, CertNotAfter: a.certNotAfter, CertError: a.certError, Started: now(), Providers: p.tel,
	}, 0)
	a.router = a.routes(a.surfaces(p))

	a.mode = tlsMode(cfg)
	if !cfg.Listen.ProxyMode {
		_, span := instrument.StartChild(ctx, a.tracer, "certs.init")
		a.certs, err = certSource(cfg, log, now)
		instrument.End(span, err, "")
		if err != nil {
			return err
		}
		observeExpiry(p.tel.Meter(scope), a.certs)
	}
	return nil
}

// domain builds the services that hold the instance's data: blobs, users,
// artifacts, share links, uploads, backups and views.
func (a *App) domain(now func() time.Time, opts Options) (*parts, error) {
	cfg, log := a.cfg, a.log
	tel := a.tel.Providers()
	p := &parts{now: now, tel: tel, fetchObs: fetch.NewObserver(tel)}

	var err error
	p.blobs, err = blob.NewFS(cfg.BlobDir(), blob.Options{Telemetry: tel})
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	p.audit = sqlite.NewAuditStore(a.db)

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
	// p.shares is set below and p.oauth and p.signins by identity, all
	// before any request.
	userOpts.OnDisable = func(ctx context.Context, handle string) error {
		actor := artifact.Actor{Handle: "admin"}
		if pr, ok := auth.FromContext(ctx); ok && pr.Handle != "" {
			actor = present.Actor(pr)
		}
		return errors.Join(p.oauth.RevokeAllForUser(ctx, handle), p.shares.RevokeUserShareLinks(ctx, actor, handle))
	}
	userOpts.EndSessions = func(ctx context.Context, handle string, keep int64) error {
		return p.signins.EndSessions(ctx, handle, keep)
	}
	userOpts.RevokeGrants = func(ctx context.Context, handle string) error { return p.oauth.RevokeAllForUser(ctx, handle) }
	p.users = user.New(sqlite.NewUserStore(a.db), p.audit, userOpts)
	a.users = p.users

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
		Passwords:         p.users,
		Now:               now,
		Logger:            log,
		Telemetry:         tel,
	}
	if cfg.Fetch.Enabled {
		p.fetcher = fetch.New(fetch.Options{
			Timeout:   cfg.Fetch.Timeout.Std(),
			Budget:    cfg.Fetch.Budget.Std(),
			Dir:       tempDir(cfg),
			UserAgent: "Kazari/" + buildinfo.Get().Version + " (+https://kazari.io)",
			Observer:  p.fetchObs,
		})
		artOpts.Pages = snapshot.New(p.fetcher, snapshot.Options{
			CSP:           cfg.Serve.CSP,
			MaxFileBytes:  int64(cfg.Limits.MaxFileBytes),
			MaxTotalBytes: int64(cfg.Limits.MaxVersionBytes),
			MaxFiles:      cfg.Limits.MaxFiles,
		})
	}
	artStore := sqlite.NewArtifactStore(a.db)
	p.arts = artifact.New(artStore, artStore, p.blobs, p.users, p.audit, artOpts)
	p.shares = artifact.NewShareLinks(artStore, p.arts)

	p.uploads = upload.New(sqlite.NewUploadStore(a.db), upload.Options{
		Telemetry:       tel,
		Dir:             cfg.UploadDir(),
		MaxChunkBytes:   int64(cfg.Limits.MaxChunkBytes),
		MaxTotalBytes:   int64(cfg.Limits.MaxVersionBytes),
		MaxFiles:        cfg.Limits.MaxFiles,
		TTL:             cfg.Limits.UploadTTL.Std(),
		MaxOpenPerOwner: cfg.Limits.MaxOpenUploads,
		Now:             now,
	})

	p.backups = backup.New(a.db, backup.Options{
		DBPath: cfg.DBPath(), BlobDir: cfg.BlobDir(), CertDir: cfg.CertStorageDir(), TempDir: tempDir(cfg),
		Now: now, Logger: log, Audit: p.audit, Telemetry: tel,
	})
	if cfg.Analytics.Enabled {
		viewStore := sqlite.NewViewStore(a.db)
		p.viewsSvc = views.New(viewStore, views.Options{Retention: cfg.Analytics.Retention.Std(), Now: now})
		a.views = views.NewRecorder(viewStore, views.RecorderOptions{Readers: cfg.Analytics.Readers, Now: now, Telemetry: tel})
	}
	return p, nil
}

// identity builds who may sign in and how: the identity provider, passkeys,
// the sign-in host and the authorization server.
func (a *App) identity(ctx context.Context, p *parts, opts Options) error {
	cfg, log, now, tel := a.cfg, a.log, p.now, p.tel
	mcpURL := cfg.ExternalURL(config.MCPHost)
	if o := cfg.Auth.OIDC; o != nil {
		idp, err := oidc.NewLazy(oidc.Config{
			Issuer:       o.Issuer,
			ClientID:     o.ClientID,
			ClientSecret: o.ClientSecret,
			RedirectURL:  mcpURL + strings.TrimPrefix(oauth.PathCallback, "/"),
			HTTPClient:   opts.OIDCClient,
			Telemetry:    tel,
			Fetch:        p.fetchObs,
		})
		if err != nil {
			return fmt.Errorf("app: %w", err)
		}
		// The pages and the API serve without the provider; only sign-in
		// waits for it.
		dctx, cancel := context.WithTimeout(ctx, oidcStartTimeout)
		err = idp.Discover(dctx)
		cancel()
		if err != nil {
			log.WarnContext(ctx, "identity provider unreachable: sign-in answers that it is temporarily unavailable until it is reached",
				"issuer", o.Issuer, "err", err, "retry", oidcRetryEvery.String(), "next", "kazari check tests auth.oidc.issuer")
		}
		p.idp = idp
		a.idp = idp
		a.oidcOn = true
	}
	var (
		passwords  signin.Passwords
		recoverURL string
	)
	if cfg.LocalAccounts() {
		passwords = p.users
		recoverURL = cfg.ExternalURL("") + "account/recover"
	}
	if cfg.LocalAccounts() && passkey.Supported(cfg.BaseDomain) {
		// The apex and the sign-in host share the base domain, so a passkey
		// made on the account page signs in on the sign-in page.
		var err error
		p.passkeys, err = passkey.New(p.users, passkey.Options{
			RPID:    cfg.BaseDomain,
			Origins: []string{strings.TrimSuffix(cfg.ExternalURL(""), "/"), strings.TrimSuffix(mcpURL, "/")},
			Now:     now,
		})
		if err != nil {
			return fmt.Errorf("app: %w", err)
		}
	}
	signInDeps := signin.Deps{
		Users: p.users, Keys: p.users, Passwords: passwords, IdP: p.idp, Audit: p.audit,
		ViewCallback: viewCallback(cfg), ControlCallback: controlCallback(cfg),
	}
	// Set only when there is one: a nil *passkey.Service in the interface
	// would not be a nil interface.
	if p.passkeys != nil {
		signInDeps.Passkeys = p.passkeys
	}
	p.signins = signin.New(sqlite.NewSignInStore(a.db), signInDeps, signin.Options{
		Origin:          strings.TrimSuffix(mcpURL, "/"),
		RecoverURL:      recoverURL,
		InsecureCookies: cfg.ExternalScheme == "http",
		Now:             now,
		Logger:          log,
		Telemetry:       tel,
	})
	p.oauth = oauth.New(sqlite.NewOAuthStore(a.db), oauth.Deps{
		Users: p.users, Keys: p.users, IdP: p.idp, SignIn: p.signins, Audit: p.audit,
	}, oauth.Options{
		Issuer:             strings.TrimSuffix(mcpURL, "/"),
		Resource:           mcpURL,
		AccessTTL:          cfg.Auth.Tokens.AccessTTL.Std(),
		RefreshTTL:         cfg.Auth.Tokens.RefreshTTL.Std(),
		RefreshReuseWindow: cfg.Auth.Tokens.RefreshReuseWindow.Std(),
		Now:                now,
		Logger:             log,
		InsecureCookies:    cfg.ExternalScheme == "http",
		Fetch:              p.fetchObs,
		Telemetry:          tel,
	})
	p.authn = auth.Chain(p.oauth, p.users)
	p.links = present.Links{URL: cfg.ExternalURL}
	return nil
}

// updateChecker is nil with admin.update_check off. It is the one call
// Kazari makes on its own account: a failure is the scheduler's to log, and
// never anything an admin is asked to act on.
func (a *App) updateChecker(p *parts, opts Options) *update.Checker {
	if !a.cfg.Admin.UpdateCheck {
		return nil
	}
	// Traced like every other request Kazari makes, and held to public
	// addresses like them, except in a test, which stands in for the
	// releases API on this machine.
	var transport http.RoundTripper = fetch.Transport()
	if opts.UpdateURL != "" {
		transport = http.DefaultTransport
	}
	return update.New(update.Options{Current: runningVersion(opts), URL: opts.UpdateURL, Now: p.now, Telemetry: p.tel,
		HTTPClient: &http.Client{Timeout: updateCheckTimeout, Transport: p.fetchObs.Transport(transport)}})
}

// runningVersion is the version the update check compares.
func runningVersion(opts Options) string {
	if opts.Version != "" {
		return opts.Version
	}
	return buildinfo.Get().Version
}

// jobs are what the scheduler runs.
func (a *App) jobs(p *parts, opts Options) []scheduler.Job {
	cfg, log, now := a.cfg, a.log, p.now
	jobs := []scheduler.Job{
		{Name: "sweep", Every: cfg.Lifecycle.SweepInterval.Std(), Run: func(ctx context.Context) error {
			rep, err := p.arts.Sweep(ctx)
			if rep != (artifact.SweepReport{}) {
				log.InfoContext(ctx, "sweep", "soft_deleted", rep.SoftDeleted, "purged", rep.Purged, "freed", rep.Freed,
					"versions_deleted", rep.VersionsDeleted)
			}
			return err
		}},
		{Name: "gc", Every: cfg.Lifecycle.GCInterval.Std(), Run: func(ctx context.Context) error {
			// A backup copies the database, then the blobs it refers to: a
			// blob collected in between would be missing from the archive.
			release, ok := p.backups.HoldGC()
			if !ok {
				log.InfoContext(ctx, "gc: skipped, a backup is running")
				return nil
			}
			defer release()
			n, err := p.arts.CollectGarbage(ctx)
			if n > 0 {
				log.InfoContext(ctx, "gc", "blobs_deleted", n)
			}
			cutoff := now().Add(-staleTempAge)
			puts, perr := p.blobs.RemoveStaleTemp(cutoff)
			spills, serr := httpapi.RemoveStaleSpills(tempDir(cfg), cutoff)
			downloads, derr := fetch.RemoveStale(tempDir(cfg), cutoff)
			copies, berr := backup.RemoveStale(tempDir(cfg), cutoff)
			if puts+spills+downloads+copies > 0 {
				log.InfoContext(ctx, "gc", "temp_files_removed", puts+spills+downloads+copies)
			}
			return errors.Join(err, perr, serr, derr, berr)
		}},
		{Name: "uploads-expire", Every: uploadsExpireEvery, Run: func(ctx context.Context) error {
			n, err := p.uploads.ExpireStale(ctx)
			if n > 0 {
				log.InfoContext(ctx, "uploads expired", "count", n)
			}
			return err
		}},
		{Name: "oauth-cleanup", Every: oauthCleanupEvery, Run: func(ctx context.Context) error {
			n, err := p.oauth.Cleanup(ctx)
			m, serr := p.signins.Cleanup(ctx)
			n, err = n+m, errors.Join(err, serr)
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
				n, err := p.viewsSvc.Prune(ctx)
				if n > 0 {
					log.InfoContext(ctx, "views pruned", "rows", n)
				}
				return err
			}})
	}
	if updates := p.updates; updates != nil && updates.Enabled() {
		jobs = append(jobs, scheduler.Job{Name: "update-check", Every: updateCheckEvery, Run: func(ctx context.Context) error {
			if err := updates.Check(fetch.WithPurpose(ctx, fetch.PurposeUpdate)); err != nil {
				return err
			}
			if r := updates.Result(); r.Newer {
				log.InfoContext(ctx, "a newer release is available", "latest", r.Latest, "running", runningVersion(opts))
			}
			return nil
		}})
	}
	return jobs
}

// surfaces builds the handler of every host the router serves, and those of
// the admin listener and socket.
func (a *App) surfaces(p *parts) hosts {
	cfg, log, now, tel := a.cfg, a.log, p.now, p.tel
	mcpURL := cfg.ExternalURL(config.MCPHost)
	grants := grantsAndSessions{p.oauth, p.signins}
	// One write budget per credential across the user API and MCP.
	writes := ratelimit.New(cfg.Limits.WriteRatePerMinute, ratelimit.Options{Now: now, Scope: ratelimit.ScopeCredential, Telemetry: tel})

	apiBase := cfg.ExternalURL(config.APIHost) + "v1"
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
		Artifacts: p.arts, Uploads: p.uploads, Users: p.users, Auth: p.authn, Links: p.links, Logger: log, Writes: writes, Fetch: p.fetcher,
		ShareLinks: p.shares,
	}, httpapi.UserOptions{
		MaxBodyBytes:    int64(cfg.Limits.MaxVersionBytes) + 1<<20,
		MaxVersionBytes: int64(cfg.Limits.MaxVersionBytes),
		MaxChunkBytes:   int64(cfg.Limits.MaxChunkBytes),
		MaxFileBytes:    int64(cfg.Limits.MaxFileBytes),
		MaxTotalBytes:   int64(cfg.Limits.MaxVersionBytes),
		Now:             now,
		TempDir:         tempDir(cfg),
		APIBaseURL:      apiBase,
		Skill:           mcpserver.Skill(p.links, mcpOpts),
		Telemetry:       tel,
	})
	mcpH := mcpserver.NewHandler(mcpserver.Deps{
		Artifacts: p.arts, Uploads: p.uploads, Auth: p.authn, Links: p.links, Logger: log, Writes: writes, Fetch: p.fetcher,
		ShareLinks: p.shares,
	}, mcpOpts)
	siteOpts := site.Options{
		BaseDomain:       cfg.BaseDomain,
		ReservedSlugs:    cfg.Serve.ReservedSlugs,
		DefaultCSP:       cfg.Serve.CSP,
		AllowCSPOverride: cfg.Serve.AllowCSPOverride,
		ImmutableMaxAge:  cfg.Lifecycle.Tombstone.Std(),
		Logger:           log,
		Gate: gate.New(p.signins, p.shares, gate.Options{
			SignInURL:       mcpURL + strings.TrimPrefix(signin.PathSignIn, "/"),
			SignOutURL:      mcpURL + strings.TrimPrefix(signin.PathSignOut, "/"),
			InsecureCookies: cfg.ExternalScheme == "http",
			Now:             now,
			Logger:          log,
			Telemetry:       tel,
		}),
	}
	if a.views != nil {
		siteOpts.Views = a.views
	}
	siteH := site.NewHandler(p.arts, p.blobs, siteOpts)
	accountDeps := account.Deps{
		Sessions: p.signins, Users: p.users, Grants: grants, Artifacts: p.arts, Audit: p.audit, Links: p.links, Logger: log,
		ShareLinks: p.shares,
	}
	if p.passkeys != nil {
		accountDeps.Passkeys = p.passkeys
	}
	accountH := account.New(accountDeps, account.Options{
		URL:             strings.TrimSuffix(cfg.ExternalURL(""), "/"),
		SignInURL:       mcpURL + strings.TrimPrefix(signin.PathSignIn, "/"),
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

	a.admin = a.surface(instrument.SurfaceAdmin, true, a.adminHost(p, grants, apiBase))
	publicAdmin := a.admin
	if cfg.Admin.Listen != "" {
		publicAdmin = a.surface(instrument.SurfaceAdmin, true, http.NotFoundHandler())
	}
	// One per-IP budget for OAuth and MCP, applied before authentication
	// like the user API's.
	mcpIPs := ratelimit.New(ratelimit.DefaultIPPerMinute, ratelimit.Options{Now: now, Scope: ratelimit.ScopeIP, Telemetry: tel})
	mcpLimit := ratelimit.Middleware(mcpIPs, ratelimit.IPKey, nil)
	return hosts{
		api:     a.surface(instrument.SurfaceAPI, true, apiHost(front.API, apiH)),
		mcp:     a.surface(instrument.SurfaceMCP, true, mcpLimit(frontPage(front.Connector, mcpMux(p.oauth.Handler(), mcpH)))),
		admin:   publicAdmin,
		apex:    a.surface(instrument.SurfaceApex, true, apexHandler(front.Home)),
		account: a.surface(instrument.SurfaceAccount, true, accountLimit(accountH)),
		site:    a.surface(instrument.SurfaceSite, false, siteH),
	}
}

// adminHost is the admin API, beside the dashboard when that is served. It
// also builds the admin socket's handler.
func (a *App) adminHost(p *parts, grants grantsAndSessions, apiBase string) http.Handler {
	cfg, log, now, tel := a.cfg, a.log, p.now, p.tel
	mcpURL := cfg.ExternalURL(config.MCPHost)
	adminDeps := httpapi.AdminDeps{
		Artifacts: p.arts,
		Users:     p.users,
		Auth:      p.authn,
		Audit:     p.audit,
		Jobs:      a.sched,
		Grants:    grantStore{p.oauth},
		Sessions:  p.signins,
		Backup:    p.backups,
		Ready:     a.ready,
		Settings:  cfg.Redacted,
		Sources:   a.origin.Sources,
		Metrics:   a.tel.MetricsHandler(),
		Views:     p.viewsSvc,
		Links:     p.links,
		Logger:    log,
	}
	// Set only when there is one: a nil *update.Checker in the interface
	// would not be a nil interface.
	if p.updates != nil {
		adminDeps.Updates = p.updates
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
	if !cfg.DashboardEnabled() {
		return adminH
	}
	deps := dashboard.Deps{
		Sessions: p.signins, Users: p.users, Artifacts: p.arts, Grants: grants, Audit: p.audit, Recorder: p.audit,
		Jobs: a.sched, Ready: a.ready, Backup: p.backups, Settings: cfg.Redacted, Views: p.viewsSvc, Links: p.links, Logger: log,
		Flags: sqlite.NewFlagStore(a.db), ShareLinks: p.shares,
	}
	// Assigned only when there is one: a nil *update.Checker in the
	// interface would not be a nil interface.
	if p.updates != nil {
		deps.Updates = p.updates
	}
	return adminMux(adminH, dashboard.New(deps, dashboard.Options{
		URL:             cfg.AdminURL(),
		SignInURL:       mcpURL + strings.TrimPrefix(signin.PathSignIn, "/"),
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
