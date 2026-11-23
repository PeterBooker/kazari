package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/audit"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/buildinfo"
	"github.com/PeterBooker/kazari/internal/instrument"
	"github.com/PeterBooker/kazari/internal/present"
	"github.com/PeterBooker/kazari/internal/ratelimit"
	"github.com/PeterBooker/kazari/internal/scheduler"
	"github.com/PeterBooker/kazari/internal/user"
	"github.com/PeterBooker/kazari/internal/views"
)

// GrantStore lists and revokes OAuth grants. RevokeGrant returns an error
// matching ErrGrantNotFound for an unknown id.
type GrantStore interface {
	ListGrants(ctx context.Context, handle string) ([]api.OAuthGrant, error)
	RevokeGrant(ctx context.Context, id int64) error
}

// AdminDeps are the collaborators of the admin API. Jobs, Grants, Audit,
// Sessions, Backup, Ready, Settings, Sources and Metrics may be nil; their routes are
// then absent. Views is nil with analytics off; its routes then say so.
type AdminDeps struct {
	Artifacts *artifact.Service
	Users     *user.Service
	Auth      auth.Authenticator // resolves admin-role API keys
	Audit     audit.Reader
	Jobs      *scheduler.Scheduler
	Grants    GrantStore
	Sessions  Sessions
	Backup    BackupWriter
	Ready     ReadyChecker
	Updates   Updates               // the update check's answer, on GET /v1/ready
	Settings  func() map[string]any // effective config, secrets redacted
	// Sources names where each setting not at its default came from.
	Sources func() map[string]string
	Metrics http.Handler // mounted at /metrics, behind AdminOptions.MetricsToken
	Views   *views.Service
	Links   present.Links
	Logger  *slog.Logger
}

// AdminOptions tunes the admin API.
type AdminOptions struct {
	// AdminToken is accepted as a bearer token; empty leaves only admin-role
	// keys.
	AdminToken string
	// IPRatePerMinute applies per client IP, before authentication, so the
	// admin token cannot be guessed at speed. 0 means DefaultIPRatePerMinute;
	// negative disables it.
	IPRatePerMinute int
	// Now clocks the rate limiter. Default: time.Now.
	Now func() time.Time
	// Local serves every request as the admin principal, with no credential
	// and no rate limit, for the admin Unix socket. AdminToken is ignored.
	Local bool
	// MetricsToken is the bearer token /metrics requires; empty leaves it
	// open.
	MetricsToken string
	// BackupName names the instance in the file name of a backup, as in
	// kazari-<name>-<time>.tar; the base domain.
	BackupName string
	Telemetry  instrument.Providers
}

type adminAPI struct {
	base
	d AdminDeps
	o AdminOptions
}

// NewAdminAPI returns the admin host handler: /v1 behind the admin token or an
// admin-role key (or open, with AdminOptions.Local), /health open and
// /metrics behind AdminOptions.MetricsToken.
func NewAdminAPI(d AdminDeps, o AdminOptions) http.Handler {
	if o.IPRatePerMinute == 0 {
		o.IPRatePerMinute = DefaultIPRatePerMinute
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	h := &adminAPI{base: newBase(d.Logger, d.Links, o.Telemetry), d: d, o: o}

	v1 := http.NewServeMux()
	handle := func(pattern string, fn apiFunc) { v1.Handle(pattern, h.serve(fn)) }
	handle("GET /v1/users", h.listUsers)
	handle("POST /v1/users", h.createUser)
	handle("GET /v1/users/{handle}", h.getUser)
	handle("PATCH /v1/users/{handle}", h.updateUser)
	handle("GET /v1/users/{handle}/keys", h.listKeys)
	handle("POST /v1/users/{handle}/keys", h.issueKey)
	handle("DELETE /v1/users/{handle}/keys/{prefix}", h.revokeKey)
	handle("POST /v1/users/{handle}/keys/{prefix}/rotate", h.rotateKey)
	handle("POST /v1/users/{handle}/identities", h.linkIdentity)
	handle("POST /v1/users/{handle}/identities/unlink", h.unlinkIdentity)
	handle("POST /v1/users/{handle}/transfer", h.transferAll)
	handle("POST /v1/users/{handle}/links", h.issueLink)
	handle("POST /v1/invites", h.invite)
	handle("GET /v1/invites", h.listLinks)
	handle("DELETE /v1/invites/{id}", h.revokeLink)
	handle("POST /v1/setup-link", h.setupLink)
	if d.Sessions != nil {
		handle("DELETE /v1/users/{handle}/sessions", h.endSessions)
	}
	if d.Backup != nil {
		handle("POST /v1/backup", h.backup)
	}
	if d.Ready != nil {
		handle("GET /v1/ready", h.ready)
	}
	if d.Grants != nil {
		handle("GET /v1/users/{handle}/grants", h.listGrants)
		handle("DELETE /v1/grants/{id}", h.revokeGrant)
	}
	handle("GET /v1/artifacts", h.listArtifacts)
	handle("POST /v1/artifacts/{slug}/transfer", h.transfer)
	handle("POST /v1/artifacts/{slug}/hide", h.hide)
	handle("GET /v1/views", h.viewSummary)
	handle("GET /v1/artifacts/{slug}/views", h.artifactViews)
	if d.Settings != nil {
		handle("GET /v1/settings", h.settings)
	}
	if d.Sources != nil {
		handle("GET /v1/settings/sources", h.settingSources)
	}
	if d.Jobs != nil {
		handle("GET /v1/jobs", h.listJobs)
		handle("POST /v1/jobs/{name}/run", h.runJob)
	}
	if d.Audit != nil {
		handle("GET /v1/audit", h.listAudit)
	}

	top := http.NewServeMux()
	top.HandleFunc("GET /health", Health)
	top.HandleFunc("GET /v1/health", Health) // the CLI addresses everything under /v1
	if d.Metrics != nil {
		metrics := d.Metrics
		if o.MetricsToken != "" {
			metrics = auth.RequireToken(o.MetricsToken)(metrics)
		}
		top.Handle("GET /metrics", metrics)
	}
	if o.Local {
		top.Handle("/v1/", auth.AsLocalAdmin(auth.Options{Telemetry: o.Telemetry})(jsonFallback(v1, &h.base)))
		return jsonFallback(top, &h.base)
	}
	guarded := auth.RequireAdmin(o.AdminToken, d.Auth, auth.Options{Telemetry: o.Telemetry})(jsonFallback(v1, &h.base))
	ipLimit := ratelimit.New(o.IPRatePerMinute, ratelimit.Options{Now: o.Now, Scope: ratelimit.ScopeIP, Telemetry: o.Telemetry})
	top.Handle("/v1/", h.limit(ipLimit, ratelimit.IPKey)(guarded))
	return jsonFallback(top, &h.base)
}

// Health answers liveness checks with the server version.
func Health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, api.Health{Status: "ok", Version: buildinfo.Get().Version})
}

// Liveness answers liveness checks on the public listener without the
// version, which only the admin host discloses.
func Liveness(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, api.Health{Status: "ok"})
}

func (h *adminAPI) listUsers(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	people, err := h.d.Users.People(r.Context(), h.d.Artifacts)
	if err != nil {
		return err
	}
	out := make([]api.User, 0, len(people))
	for i := range people {
		p := &people[i]
		u := present.WithAccount(present.User(&p.User), p.Account)
		u.UsedBytes = p.Usage.Bytes
		out = append(out, u)
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (h *adminAPI) createUser(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	var req api.CreateUserRequest
	if err := decodeJSON(w, r, jsonLimit, &req, false); err != nil {
		return err
	}
	u, err := h.d.Users.CreateUser(r.Context(), user.CreateUserRequest{
		Handle:      req.Handle,
		Email:       req.Email,
		DisplayName: req.DisplayName,
		Role:        user.Role(req.Role),
		QuotaBytes:  req.QuotaBytes,
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, present.User(u))
	return nil
}

func (h *adminAPI) getUser(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	handle := r.PathValue("handle")
	u, err := h.d.Users.GetUser(r.Context(), handle)
	if err != nil {
		return err
	}
	ids, err := h.d.Users.ListIdentities(r.Context(), handle)
	if err != nil {
		return err
	}
	account, err := h.d.Users.Account(r.Context(), handle)
	if err != nil {
		return err
	}
	out := present.WithAccount(present.User(u), account)
	for _, id := range ids {
		out.Identities = append(out.Identities, present.Identity(id))
	}
	if out.UsedBytes, err = h.d.Artifacts.Usage(r.Context(), handle); err != nil {
		return err
	}
	if h.d.Sessions != nil {
		n, err := h.d.Sessions.CountSessions(r.Context(), handle)
		if err != nil {
			return err
		}
		out.Sessions = &n
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (h *adminAPI) updateUser(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	var req api.UpdateUserRequest
	if err := decodeJSON(w, r, jsonLimit, &req, false); err != nil {
		return err
	}
	var role *user.Role
	if req.Role != nil {
		rl := user.Role(*req.Role)
		role = &rl
	}
	u, err := h.d.Users.UpdateUser(r.Context(), r.PathValue("handle"), user.UpdateUserRequest{
		Email:       req.Email,
		DisplayName: req.DisplayName,
		Role:        role,
		QuotaBytes:  req.QuotaBytes,
		Disabled:    req.Disabled,
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, present.User(u))
	return nil
}

func (h *adminAPI) listKeys(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	handle := r.PathValue("handle")
	if _, err := h.d.Users.GetUser(r.Context(), handle); err != nil {
		return err
	}
	keys, err := h.d.Users.ListKeys(r.Context(), handle)
	if err != nil {
		return err
	}
	out := make([]api.APIKey, 0, len(keys))
	for i := range keys {
		out = append(out, present.Key(&keys[i]))
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (h *adminAPI) issueKey(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	var req api.IssueKeyRequest
	if err := decodeJSON(w, r, jsonLimit, &req, true); err != nil {
		return err
	}
	expiresIn, err := present.ParseDuration("expires_in", req.ExpiresIn)
	if err != nil {
		return err
	}
	k, secret, err := h.d.Users.IssueKey(r.Context(), user.IssueKeyRequest{
		Handle: r.PathValue("handle"), Label: req.Label, Scopes: req.Scopes, Slugs: req.Slugs, ExpiresIn: expiresIn,
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, api.IssuedKey{Key: secret, APIKey: present.Key(k)})
	return nil
}

func (h *adminAPI) revokeKey(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	ref := user.KeyRef{Handle: r.PathValue("handle"), Prefix: r.PathValue("prefix")}
	if err := h.d.Users.RevokeKey(r.Context(), ref); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *adminAPI) rotateKey(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	return rotateKey(w, r, h.d.Users, user.KeyRef{Handle: r.PathValue("handle"), Prefix: r.PathValue("prefix")})
}

func (h *adminAPI) linkIdentity(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	var req api.LinkIdentityRequest
	if err := decodeJSON(w, r, jsonLimit, &req, false); err != nil {
		return err
	}
	if err := h.d.Users.LinkIdentity(r.Context(), r.PathValue("handle"), req.Issuer, req.Subject, req.Email); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *adminAPI) unlinkIdentity(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	var req api.LinkIdentityRequest
	if err := decodeJSON(w, r, jsonLimit, &req, false); err != nil {
		return err
	}
	handle := r.PathValue("handle")
	err := h.d.Users.UnlinkIdentity(r.Context(), handle, req.Issuer, req.Subject)
	if errors.Is(err, user.ErrNotFound) {
		// The store answers the same for an unknown user and for an identity
		// the user does not have; say which, so the caller fixes the right
		// thing.
		if _, uerr := h.d.Users.GetUser(r.Context(), handle); uerr == nil {
			return &statusError{status: http.StatusNotFound, code: api.CodeNotFound,
				msg: "no identity with that issuer and subject is linked to " + strconv.Quote(handle)}
		}
	}
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *adminAPI) listGrants(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	handle := r.PathValue("handle")
	if _, err := h.d.Users.GetUser(r.Context(), handle); err != nil {
		return err
	}
	grants, err := h.d.Grants.ListGrants(r.Context(), handle)
	if err != nil {
		return err
	}
	if grants == nil {
		grants = []api.OAuthGrant{}
	}
	writeJSON(w, http.StatusOK, grants)
	return nil
}

func (h *adminAPI) revokeGrant(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		return invalid("grant id must be a positive integer")
	}
	if err := h.d.Grants.RevokeGrant(r.Context(), id); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *adminAPI) listArtifacts(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	q, err := listQuery(r)
	if err != nil {
		return err
	}
	q.All = true
	q.Owner = r.URL.Query().Get("owner")
	page, err := h.d.Artifacts.List(r.Context(), present.Actor(p), q)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, h.links.Artifacts(page))
	return nil
}

func (h *adminAPI) transfer(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	var req api.TransferRequest
	if err := decodeJSON(w, r, jsonLimit, &req, false); err != nil {
		return err
	}
	a, err := h.d.Artifacts.Transfer(r.Context(), present.Actor(p), r.PathValue("slug"), req.Owner)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, h.links.Artifact(a))
	return nil
}

// transferAll hands every artifact of the user in the path to another owner.
func (h *adminAPI) transferAll(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	var req api.TransferRequest
	if err := decodeJSON(w, r, jsonLimit, &req, false); err != nil {
		return err
	}
	moved, err := h.d.Artifacts.TransferAll(r.Context(), present.Actor(p), r.PathValue("handle"), req.Owner)
	if err != nil {
		return err
	}
	if moved == nil {
		moved = []string{}
	}
	writeJSON(w, http.StatusOK, api.TransferredArtifacts{Transferred: moved})
	return nil
}

func (h *adminAPI) hide(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	a, err := h.d.Artifacts.SetVisibility(r.Context(), present.Actor(p), r.PathValue("slug"), artifact.Unpublished)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, h.links.Artifact(a))
	return nil
}

func (h *adminAPI) settings(w http.ResponseWriter, _ *http.Request, _ auth.Principal) error {
	s := h.d.Settings()
	if s == nil {
		s = map[string]any{}
	}
	writeJSON(w, http.StatusOK, s)
	return nil
}

func (h *adminAPI) settingSources(w http.ResponseWriter, _ *http.Request, _ auth.Principal) error {
	s := h.d.Sources()
	if s == nil {
		s = map[string]string{}
	}
	writeJSON(w, http.StatusOK, s)
	return nil
}

func (h *adminAPI) listJobs(w http.ResponseWriter, _ *http.Request, _ auth.Principal) error {
	status := h.d.Jobs.Status()
	out := make([]api.JobStatus, 0, len(status))
	for _, s := range status {
		out = append(out, present.Job(s))
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// runJob runs synchronously. A failing run is a 500; its error is in the job's
// last_error.
func (h *adminAPI) runJob(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	name := r.PathValue("name")
	err := h.d.Jobs.Trigger(r.Context(), name)
	switch {
	case errors.Is(err, scheduler.ErrUnknownJob):
		return &statusError{status: http.StatusNotFound, code: api.CodeNotFound, msg: "no job named " + strconv.Quote(name)}
	case errors.Is(err, scheduler.ErrRunning):
		return &statusError{status: http.StatusConflict, code: api.CodeJobRunning, msg: "job " + strconv.Quote(name) + " is already running"}
	case err != nil:
		return err
	}
	for _, s := range h.d.Jobs.Status() {
		if s.Name == name {
			writeJSON(w, http.StatusOK, present.Job(s))
			return nil
		}
	}
	writeJSON(w, http.StatusOK, api.JobStatus{Name: name})
	return nil
}

func (h *adminAPI) listAudit(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	q := r.URL.Query()
	limit, err := queryInt(r, "limit")
	if err != nil {
		return err
	}
	aq := audit.Query{Slug: q.Get("slug"), Actor: q.Get("actor"), Subject: q.Get("subject"), Actions: q["action"], Cursor: q.Get("cursor"), Limit: limit}
	for _, bound := range []struct {
		name string
		dst  *time.Time
	}{{"since", &aq.Since}, {"until", &aq.Until}} {
		s := q.Get(bound.name)
		if s == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return invalid("%s must be an RFC 3339 time", bound.name)
		}
		*bound.dst = t
	}
	page, err := h.d.Audit.List(r.Context(), aq)
	if err != nil {
		return err
	}
	out := api.AuditList{Events: make([]api.AuditEvent, 0, len(page.Events)), NextCursor: page.NextCursor}
	for _, e := range page.Events {
		out.Events = append(out.Events, present.AuditEvent(e))
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// DefaultViewDays is the range of a views report without ?days.
const DefaultViewDays = 30

// viewDays reads ?days, or DefaultViewDays; the service checks the range.
func (h *adminAPI) viewDays(r *http.Request) (int, error) {
	if h.d.Views == nil {
		return 0, &statusError{status: http.StatusNotFound, code: api.CodeNotFound,
			msg: "analytics is off on this instance; set analytics.enabled to count page views"}
	}
	if r.URL.Query().Get("days") == "" {
		return DefaultViewDays, nil
	}
	days, err := strconv.Atoi(r.URL.Query().Get("days"))
	if err != nil {
		return 0, invalid("days must be a whole number")
	}
	return days, nil
}

func (h *adminAPI) viewSummary(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	days, err := h.viewDays(r)
	if err != nil {
		return err
	}
	s, err := h.d.Views.Summary(r.Context(), days)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, present.ViewSummary(s))
	return nil
}

func (h *adminAPI) artifactViews(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	days, err := h.viewDays(r)
	if err != nil {
		return err
	}
	rep, err := h.d.Views.Report(r.Context(), r.PathValue("slug"), days)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, present.ArtifactViews(rep))
	return nil
}
