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
	"github.com/PeterBooker/kazari/internal/present"
	"github.com/PeterBooker/kazari/internal/ratelimit"
	"github.com/PeterBooker/kazari/internal/scheduler"
	"github.com/PeterBooker/kazari/internal/user"
)

const codeJobRunning = "job_running"

// GrantStore lists and revokes OAuth grants. RevokeGrant returns an error
// matching ErrGrantNotFound for an unknown id.
type GrantStore interface {
	ListGrants(ctx context.Context, handle string) ([]api.OAuthGrant, error)
	RevokeGrant(ctx context.Context, id int64) error
}

// AdminDeps are the collaborators of the admin API. Jobs, Grants, Audit,
// Settings and Metrics may be nil; their routes are then absent.
type AdminDeps struct {
	Artifacts *artifact.Service
	Users     *user.Service
	Auth      auth.Authenticator // resolves admin-role API keys
	Audit     audit.Reader
	Jobs      *scheduler.Scheduler
	Grants    GrantStore
	Settings  func() map[string]any // effective config, secrets redacted
	Metrics   http.Handler          // mounted at /metrics, behind AdminOptions.MetricsToken
	Links     present.Links
	Logger    *slog.Logger
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
}

type adminAPI struct {
	base
	d AdminDeps
}

// NewAdminAPI returns the admin host handler: /v1 behind the admin token or an
// admin-role key (or open, with AdminOptions.Local), /health open and
// /metrics behind AdminOptions.MetricsToken.
func NewAdminAPI(d AdminDeps, o AdminOptions) http.Handler {
	h := &adminAPI{base: newBase(d.Logger, d.Links), d: d}
	if o.IPRatePerMinute == 0 {
		o.IPRatePerMinute = DefaultIPRatePerMinute
	}

	v1 := http.NewServeMux()
	handle := func(pattern string, fn apiFunc) { v1.Handle(pattern, h.serve(fn)) }
	handle("GET /v1/users", h.listUsers)
	handle("POST /v1/users", h.createUser)
	handle("GET /v1/users/{handle}", h.getUser)
	handle("PATCH /v1/users/{handle}", h.updateUser)
	handle("GET /v1/users/{handle}/keys", h.listKeys)
	handle("POST /v1/users/{handle}/keys", h.issueKey)
	handle("DELETE /v1/users/{handle}/keys/{prefix}", h.revokeKey)
	handle("POST /v1/users/{handle}/identities", h.linkIdentity)
	handle("POST /v1/users/{handle}/identities/unlink", h.unlinkIdentity)
	if d.Grants != nil {
		handle("GET /v1/users/{handle}/grants", h.listGrants)
		handle("DELETE /v1/grants/{id}", h.revokeGrant)
	}
	handle("GET /v1/artifacts", h.listArtifacts)
	handle("POST /v1/artifacts/{slug}/transfer", h.transfer)
	handle("POST /v1/artifacts/{slug}/hide", h.hide)
	if d.Settings != nil {
		handle("GET /v1/settings", h.settings)
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
		top.Handle("/v1/", auth.AsLocalAdmin()(jsonFallback(v1, &h.base)))
		return jsonFallback(top, &h.base)
	}
	guarded := auth.RequireAdmin(o.AdminToken, d.Auth)(jsonFallback(v1, &h.base))
	top.Handle("/v1/", h.limit(ratelimit.New(o.IPRatePerMinute, o.Now), ratelimit.IPKey)(guarded))
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
	users, err := h.d.Users.ListUsers(r.Context())
	if err != nil {
		return err
	}
	out := make([]api.User, 0, len(users))
	for i := range users {
		u := present.User(&users[i])
		if u.UsedBytes, err = h.d.Artifacts.Usage(r.Context(), u.Handle); err != nil {
			return err
		}
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
	out := present.User(u)
	for _, id := range ids {
		out.Identities = append(out.Identities, present.Identity(id))
	}
	if out.UsedBytes, err = h.d.Artifacts.Usage(r.Context(), handle); err != nil {
		return err
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
	var expiresIn time.Duration
	if req.ExpiresIn != "" {
		d, err := time.ParseDuration(req.ExpiresIn)
		if err != nil || d <= 0 {
			return invalid("expires_in must be a positive duration such as 720h")
		}
		expiresIn = d
	}
	k, secret, err := h.d.Users.IssueKey(r.Context(), r.PathValue("handle"), req.Label, req.Scopes, expiresIn)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, api.IssuedKey{Key: secret, APIKey: present.Key(k)})
	return nil
}

func (h *adminAPI) revokeKey(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	if err := h.d.Users.RevokeKey(r.Context(), r.PathValue("handle"), r.PathValue("prefix")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
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
	if err := h.d.Users.UnlinkIdentity(r.Context(), r.PathValue("handle"), req.Issuer, req.Subject); err != nil {
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
		return &statusError{status: http.StatusConflict, code: codeJobRunning, msg: "job " + strconv.Quote(name) + " is already running"}
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
	aq := audit.Query{Slug: q.Get("slug"), Actor: q.Get("actor"), Limit: limit}
	if s := q.Get("since"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return invalid("since must be an RFC 3339 time")
		}
		aq.Since = t
	}
	events, err := h.d.Audit.List(r.Context(), aq)
	if err != nil {
		return err
	}
	out := make([]api.AuditEvent, 0, len(events))
	for _, e := range events {
		out = append(out, present.AuditEvent(e))
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}
