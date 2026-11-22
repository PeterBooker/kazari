package httpapi

import (
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/fetch"
	"github.com/PeterBooker/kazari/internal/instrument"
	"github.com/PeterBooker/kazari/internal/middleware"
	"github.com/PeterBooker/kazari/internal/present"
	"github.com/PeterBooker/kazari/internal/ratelimit"
	"github.com/PeterBooker/kazari/internal/upload"
	"github.com/PeterBooker/kazari/internal/user"
)

const DefaultIPRatePerMinute = ratelimit.DefaultIPPerMinute

// UserDeps are the collaborators of the user API.
type UserDeps struct {
	Artifacts *artifact.Service
	Uploads   *upload.Service
	Users     *user.Service
	Auth      auth.Authenticator // API keys and OAuth tokens, already chained
	Links     present.Links
	Logger    *slog.Logger
	// Writes is shared with the MCP server so a key has one write budget on
	// both; nil disables it.
	Writes *ratelimit.Limiter
	// Fetch downloads files named by url; nil refuses them.
	Fetch *fetch.Client
}

// UserOptions tunes the user API. Zero fields take the defaults noted.
type UserOptions struct {
	// MaxBodyBytes caps JSON bodies with inline files, the multipart stream
	// and a bundle's uncompressed size. Default: the default version size
	// plus 1 MiB.
	MaxBodyBytes int64
	// MaxChunkBytes caps one upload chunk. Default: the upload service's.
	MaxChunkBytes int64
	// MaxFileBytes caps a file named by url. Default: artifact.DefaultMaxFileBytes.
	MaxFileBytes int64
	// MaxTotalBytes is only echoed in POST /v1/uploads. Default:
	// upload.DefaultMaxTotalBytes.
	MaxTotalBytes int64
	// IPRatePerMinute applies per client IP, before authentication. 0 means
	// DefaultIPRatePerMinute; negative disables it.
	IPRatePerMinute int
	// TempDir holds spilled multipart bundles and archives. Default: os.TempDir().
	TempDir string
	// APIBaseURL is the REST root an upload's archive URL is built on, e.g.
	// https://api.<base>/v1.
	APIBaseURL string
	// Now clocks the rate limiters. Default: time.Now.
	Now func() time.Time
	// Skill is the SKILL.md served at GET /v1/skill; empty answers 404.
	Skill     string
	Telemetry instrument.Providers
}

func (o UserOptions) withDefaults(d UserDeps) UserOptions {
	if o.MaxBodyBytes <= 0 {
		o.MaxBodyBytes = artifact.DefaultMaxVersionBytes + 1<<20
	}
	if o.MaxChunkBytes <= 0 {
		o.MaxChunkBytes = upload.DefaultMaxChunkBytes
		if d.Uploads != nil {
			o.MaxChunkBytes = d.Uploads.MaxChunkBytes()
		}
	}
	if o.MaxTotalBytes <= 0 {
		o.MaxTotalBytes = upload.DefaultMaxTotalBytes
	}
	if o.MaxFileBytes <= 0 {
		o.MaxFileBytes = artifact.DefaultMaxFileBytes
	}
	if o.IPRatePerMinute == 0 {
		o.IPRatePerMinute = DefaultIPRatePerMinute
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

type userAPI struct {
	base
	d UserDeps
	o UserOptions
}

// NewUserAPI returns the /v1 handler of the api host. Every route requires a
// bearer credential and, except /me, artifacts:read, artifacts:write or
// artifacts:delete.
func NewUserAPI(d UserDeps, o UserOptions) http.Handler {
	o = o.withDefaults(d)
	h := &userAPI{base: newBase(d.Logger, d.Links, o.Telemetry), d: d, o: o}

	mux := http.NewServeMux()
	for _, rt := range h.routes() {
		mux.Handle(rt.pattern, h.guard(rt.scope, rt.write, rt.fn))
	}

	var protected http.Handler = jsonFallback(mux, &h.base)
	protected = auth.RequireBearer(d.Auth, auth.Options{Telemetry: o.Telemetry})(protected)
	// Only a PUT of an archive passes without a bearer; a pattern with a
	// method matches that method alone, so any other request for the path
	// falls through to the protected handler and gets its 401.
	open := http.NewServeMux()
	receive := h.serve(h.receiveArchive)
	open.HandleFunc("PUT /v1/uploads/{id}/archive", func(w http.ResponseWriter, r *http.Request) {
		// jsonFallback reports the route of every request past the bearer
		// check; this one never reaches it.
		middleware.SetRoute(r.Context(), r.Pattern)
		receive.ServeHTTP(w, r)
	})
	open.Handle("/", protected)
	return h.limit(ratelimit.New(o.IPRatePerMinute, ratelimit.Options{Now: o.Now, Scope: ratelimit.ScopeIP, Telemetry: o.Telemetry}), ratelimit.IPKey)(open)
}

// route is one route behind the bearer check: the scope it needs ("" for
// any credential) and whether it counts against the write rate.
type route struct {
	pattern string
	scope   string
	write   bool
	fn      apiFunc
}

func (h *userAPI) routes() []route {
	read := func(pattern string, fn apiFunc) route { return route{pattern, auth.ScopeRead, false, fn} }
	write := func(pattern string, fn apiFunc) route { return route{pattern, auth.ScopeWrite, true, fn} }
	share := func(pattern string, fn apiFunc) route { return route{pattern, auth.ScopeShare, true, fn} }
	return []route{
		write("POST /v1/artifacts", h.createArtifact),
		read("GET /v1/artifacts", h.listArtifacts),
		read("GET /v1/artifacts/{slug}", h.getArtifact),
		write("PUT /v1/artifacts/{slug}", h.updateArtifact),
		// A change of visibility alone takes artifacts:share and no
		// artifacts:write, as set_visibility does; update decides.
		{"PATCH /v1/artifacts/{slug}", "", true, h.patchArtifact},
		{"DELETE /v1/artifacts/{slug}", auth.ScopeDelete, true, h.deleteArtifact},
		write("POST /v1/artifacts/{slug}/edit", h.editFile),
		write("POST /v1/artifacts/{slug}/publish", h.publish),
		write("POST /v1/artifacts/{slug}/stale", h.markStale),
		write("DELETE /v1/artifacts/{slug}/stale", h.unmarkStale),
		read("GET /v1/artifacts/{slug}/files/{path...}", h.readFile),
		share("POST /v1/artifacts/{slug}/share-links", h.createShareLink),
		read("GET /v1/artifacts/{slug}/share-links", h.listShareLinks),
		share("DELETE /v1/artifacts/{slug}/share-links/{prefix}", h.revokeShareLink),
		write("POST /v1/uploads", h.beginUpload),
		write("PATCH /v1/uploads/{id}/files/{path...}", h.uploadChunk),
		write("DELETE /v1/uploads/{id}", h.abortUpload),
		read("GET /v1/skill", h.skill),
		{"GET /v1/me", "", false, h.me},
		{"GET /v1/me/keys", "", false, h.myKeys},
		{"POST /v1/me/keys", "", true, h.issueMyKey},
		{"DELETE /v1/me/keys/{prefix}", "", true, h.revokeMyKey},
		{"POST /v1/me/keys/{prefix}/rotate", "", true, h.rotateMyKey},
	}
}

func (h *userAPI) guard(scope string, write bool, fn apiFunc) http.Handler {
	next := h.serve(fn)
	if write {
		next = h.limit(h.d.Writes, keyOf)(next)
	}
	if scope != "" {
		next = auth.RequireScope(scope)(next)
	}
	return next
}

func keyOf(r *http.Request) string {
	p, _ := auth.FromContext(r.Context())
	return ratelimit.CredentialKey(p)
}

func (b *base) limit(l *ratelimit.Limiter, key func(*http.Request) string) func(http.Handler) http.Handler {
	return ratelimit.Middleware(l, key, func(w http.ResponseWriter, r *http.Request, d time.Duration) {
		b.writeError(w, r, &statusError{status: http.StatusTooManyRequests,
			code: api.CodeRateLimited, msg: "too many requests", retryAfter: d})
	})
}

// mediaType treats an absent Content-Type as JSON.
func mediaType(r *http.Request) string {
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		return "application/json"
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return ""
	}
	return mt
}

func unsupported() error {
	return &statusError{status: http.StatusUnsupportedMediaType, code: api.CodeInvalidArgument,
		msg: "Content-Type must be application/json or multipart/form-data"}
}

func (h *userAPI) createArtifact(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	switch mediaType(r) {
	case "application/json":
		return h.createJSON(w, r, p)
	case "multipart/form-data":
		return h.createMultipart(w, r, p)
	}
	return unsupported()
}

func (h *userAPI) createJSON(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	var req api.CreateArtifactRequest
	if err := decodeJSON(w, r, h.o.MaxBodyBytes, &req, false); err != nil {
		return err
	}
	if err := present.RequireShare(p, req.Visibility); err != nil {
		return err
	}
	files, closeDownloads, err := h.inlineFiles(r.Context(), req.Files, false)
	if err != nil {
		return err
	}
	defer closeDownloads()
	if req.SourceURL != "" {
		if req.UploadID != "" {
			return invalid("source_url cannot be combined with upload_id")
		}
		a, v, rep, err := h.d.Artifacts.Import(r.Context(), present.Actor(p), artifact.ImportRequest{
			URL:         req.SourceURL,
			Slug:        req.Slug,
			Name:        req.Name,
			Description: req.Description,
			Visibility:  artifact.Visibility(req.Visibility),
			Metadata:    req.Metadata,
			Label:       req.Label,
			KeepScripts: req.KeepScripts,
			Files:       files,
		})
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusCreated, h.links.Imported(a, v, rep))
		return nil
	}
	files, done, err := h.withUpload(r.Context(), p.Handle, req.UploadID, files)
	if err != nil {
		return err
	}
	defer done()
	return h.create(w, r, p, artifact.CreateRequest{
		Slug:        req.Slug,
		Name:        req.Name,
		Description: req.Description,
		Visibility:  artifact.Visibility(req.Visibility),
		Metadata:    req.Metadata,
		Label:       req.Label,
		Files:       files,
	})
}

func (h *userAPI) createMultipart(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	r.Body = http.MaxBytesReader(w, r.Body, h.o.MaxBodyBytes)
	mr, err := r.MultipartReader()
	if err != nil {
		return invalid("malformed multipart body: %v", err)
	}
	var m api.CreateManifest
	if err := readManifest(mr, &m); err != nil {
		return err
	}
	if err := present.RequireShare(p, m.Visibility); err != nil {
		return err
	}
	b, err := h.nextBundle(mr)
	if err != nil {
		return err
	}
	if b == nil {
		return invalid("a multipart create needs a bundle part")
	}
	defer h.closeBundle(r.Context(), b)
	return h.create(w, r, p, artifact.CreateRequest{
		Slug:        m.Slug,
		Name:        m.Name,
		Description: m.Description,
		Visibility:  artifact.Visibility(m.Visibility),
		Metadata:    m.Metadata,
		Label:       m.Label,
		Files:       stripTopDir(b.files),
	})
}

func (h *userAPI) create(w http.ResponseWriter, r *http.Request, p auth.Principal, req artifact.CreateRequest) error {
	a, v, err := h.d.Artifacts.Create(r.Context(), present.Actor(p), req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, h.links.Published(a, v))
	return nil
}

func (h *userAPI) listArtifacts(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	q, err := listQuery(r)
	if err != nil {
		return err
	}
	page, err := h.d.Artifacts.List(r.Context(), present.Actor(p), q)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, h.links.Artifacts(page))
	return nil
}

func (h *userAPI) getArtifact(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	withFiles, err := queryBool(r, "files")
	if err != nil {
		return err
	}
	a, versions, err := h.d.Artifacts.Get(r.Context(), present.Actor(p), r.PathValue("slug"))
	if err != nil {
		return err
	}
	next := 0
	if withFiles {
		before, err := queryInt(r, "files_before")
		if err != nil {
			return err
		}
		if next, err = h.d.Artifacts.ListFiles(r.Context(), versions, before); err != nil {
			return err
		}
	}
	d := h.links.Detail(a, versions)
	d.NextFilesBefore = next
	w.Header().Set("ETag", strconv.Quote(strconv.Itoa(a.LatestVersion)))
	writeJSON(w, http.StatusOK, d)
	return nil
}

func (h *userAPI) updateArtifact(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	var (
		req    api.UpdateArtifactRequest
		bundle *bundle
	)
	create, err := queryBool(r, "create")
	if err != nil {
		return err
	}
	switch mediaType(r) {
	case "application/json":
		if err := decodeJSON(w, r, h.o.MaxBodyBytes, &req, false); err != nil {
			return err
		}
	case "multipart/form-data":
		r.Body = http.MaxBytesReader(w, r.Body, h.o.MaxBodyBytes)
		mr, err := r.MultipartReader()
		if err != nil {
			return invalid("malformed multipart body: %v", err)
		}
		if err := readManifest(mr, &req); err != nil {
			return err
		}
		if bundle, err = h.nextBundle(mr); err != nil {
			return err
		}
		defer h.closeBundle(r.Context(), bundle)
	default:
		return unsupported()
	}
	files, closeDownloads, err := h.inlineFiles(r.Context(), req.Files, true)
	if err != nil {
		return err
	}
	defer closeDownloads()
	if bundle != nil {
		bf := bundle.files
		if req.Mirror {
			bf = stripTopDir(bf)
		}
		files = artifact.MergeFiles(files, bf)
	}
	files, done, err := h.withUpload(r.Context(), p.Handle, req.UploadID, files)
	if err != nil {
		return err
	}
	defer done()
	if create {
		created, err := h.createAbsent(w, r, p, req, files)
		if created || err != nil {
			return err
		}
	}
	if req.Mirror {
		if files, err = h.mirror(r, p, &req, files); err != nil {
			return err
		}
	}
	return h.update(w, r, p, req, files)
}

// createAbsent is PUT ?create=true on a slug with no artifact the caller can
// see. It reports false, with nothing written, when the artifact exists, so
// the PUT updates it instead.
func (h *userAPI) createAbsent(w http.ResponseWriter, r *http.Request, p auth.Principal, req api.UpdateArtifactRequest, files []artifact.FileInput) (bool, error) {
	slug := r.PathValue("slug")
	expected, err := expectedVersion(r, req.ExpectedVersion)
	if err != nil || expected != 0 || req.BaseVersion != 0 || req.Restore {
		return false, err
	}
	_, _, err = h.d.Artifacts.Get(r.Context(), present.Actor(p), slug)
	switch {
	case err == nil:
		return false, nil
	case !errors.Is(err, artifact.ErrNotFound) && !errors.Is(err, artifact.ErrGone):
		return false, err
	}
	if !p.HasScope(auth.ScopeWrite) {
		return false, artifact.Errorf(artifact.ErrForbidden, "scope %q required", auth.ScopeWrite)
	}
	cr := artifact.CreateRequest{Slug: slug, Label: req.Label, Files: files}
	if req.Name != nil {
		cr.Name = *req.Name
	}
	if req.Description != nil {
		cr.Description = *req.Description
	}
	if req.Visibility != nil {
		if err := present.RequireShare(p, *req.Visibility); err != nil {
			return false, err
		}
		cr.Visibility = artifact.Visibility(*req.Visibility)
	}
	for k, v := range req.Metadata {
		if v == nil {
			continue
		}
		if cr.Metadata == nil {
			cr.Metadata = map[string]string{}
		}
		cr.Metadata[k] = *v
	}
	err = h.create(w, r, p, cr)
	if errors.Is(err, artifact.ErrSlugTaken) {
		if _, _, gerr := h.d.Artifacts.Get(r.Context(), present.Actor(p), slug); gerr == nil {
			// Created by a concurrent request since the check above.
			return false, nil
		}
	}
	return true, err
}

// mirror adds a delete for every file of the base version the request does
// not carry. It also expects the latest version it read, so a version added
// meanwhile fails with version_conflict instead of keeping files this
// request was meant to remove.
func (h *userAPI) mirror(r *http.Request, p auth.Principal, req *api.UpdateArtifactRequest, files []artifact.FileInput) ([]artifact.FileInput, error) {
	a, versions, err := h.d.Artifacts.Get(r.Context(), present.Actor(p), r.PathValue("slug"))
	if err != nil {
		return nil, err
	}
	expected, err := expectedVersion(r, req.ExpectedVersion)
	if err != nil {
		return nil, err
	}
	if expected == 0 {
		req.ExpectedVersion = a.LatestVersion
	}
	base := req.BaseVersion
	if base == 0 {
		base = a.LatestVersion
	}
	if _, err := h.d.Artifacts.ListFiles(r.Context(), versions, base+1); err != nil {
		return nil, err
	}
	keep := make(map[string]bool, len(files))
	for _, f := range files {
		if !f.Delete {
			keep[f.Path] = true
		}
	}
	for _, v := range versions {
		if v.Number != base {
			continue
		}
		for _, f := range v.Files {
			if !keep[f.Path] {
				files = append(files, artifact.FileInput{Path: f.Path, Delete: true})
				keep[f.Path] = true
			}
		}
	}
	return files, nil
}

func (h *userAPI) patchArtifact(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	var req api.UpdateArtifactRequest
	if err := decodeJSON(w, r, jsonLimit, &req, false); err != nil {
		return err
	}
	if len(req.Files) > 0 || req.UploadID != "" || req.Mirror {
		return invalid("PATCH changes fields only; use PUT to change files")
	}
	return h.update(w, r, p, req, nil)
}

// update applies visibility through SetVisibility, so "unpublished" works and
// the others re-publish, and last, so a restore comes first.
func (h *userAPI) update(w http.ResponseWriter, r *http.Request, p auth.Principal, req api.UpdateArtifactRequest, files []artifact.FileInput) error {
	slug := r.PathValue("slug")
	expected, err := expectedVersion(r, req.ExpectedVersion)
	if err != nil {
		return err
	}
	if req.Visibility != nil {
		if !p.HasScope(auth.ScopeShare) {
			return artifact.Errorf(artifact.ErrForbidden, "scope %q required to change visibility", auth.ScopeShare)
		}
		switch *req.Visibility {
		case string(artifact.Public), string(artifact.Unlisted), string(artifact.Members), artifact.Unpublished:
		default:
			return invalid("visibility must be public, unlisted, members or unpublished")
		}
	}
	actor := present.Actor(p)
	visibilityOnly := req.Visibility != nil && len(files) == 0 && req.Name == nil && req.Description == nil &&
		len(req.Metadata) == 0 && !req.Restore && expected == 0
	if !visibilityOnly && !p.HasScope(auth.ScopeWrite) {
		return artifact.Errorf(artifact.ErrForbidden, "scope %q required", auth.ScopeWrite)
	}

	var (
		a *artifact.Artifact
		v *artifact.Version
	)
	if !visibilityOnly {
		a, v, err = h.d.Artifacts.Update(r.Context(), actor, artifact.UpdateRequest{
			Slug:            slug,
			Name:            req.Name,
			Description:     req.Description,
			Metadata:        req.Metadata,
			Label:           req.Label,
			ExpectedVersion: expected,
			BaseVersion:     req.BaseVersion,
			KeepStale:       req.KeepStale,
			Restore:         req.Restore,
			Files:           files,
		})
		if err != nil {
			return err
		}
	}
	if req.Visibility != nil {
		if a, err = h.d.Artifacts.SetVisibility(r.Context(), actor, slug, *req.Visibility); err != nil {
			return err
		}
	}
	if v != nil {
		res := h.links.Published(a, v)
		if req.Visibility != nil {
			res.Artifact = h.links.Artifact(a)
		}
		writeJSON(w, http.StatusOK, res)
		return nil
	}
	writeJSON(w, http.StatusOK, h.links.Result(a, 0))
	return nil
}

// expectedVersion is the body's expected_version, else If-Match: "<n>"
// (quoted or bare). "*" and an absent header mean no check.
func expectedVersion(r *http.Request, fromBody int) (int, error) {
	if fromBody != 0 {
		return fromBody, nil
	}
	v := strings.TrimSpace(r.Header.Get("If-Match"))
	if v == "" || v == "*" {
		return 0, nil
	}
	v = strings.TrimPrefix(v, "W/")
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		v = v[1 : len(v)-1]
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, invalid("If-Match must be a version number")
	}
	return n, nil
}

func (h *userAPI) editFile(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	var req api.EditRequest
	if err := decodeJSON(w, r, h.o.MaxBodyBytes, &req, false); err != nil {
		return err
	}
	expected, err := expectedVersion(r, req.ExpectedVersion)
	if err != nil {
		return err
	}
	edits := make([]artifact.Edit, len(req.Edits))
	for i, e := range req.Edits {
		edits[i] = artifact.Edit(e)
	}
	a, v, outs, err := h.d.Artifacts.Edit(r.Context(), present.Actor(p), artifact.EditRequest{
		Slug:            r.PathValue("slug"),
		Path:            req.Path,
		Edits:           edits,
		Label:           req.Label,
		ExpectedVersion: expected,
		BaseVersion:     req.BaseVersion,
		KeepStale:       req.KeepStale,
	})
	if err != nil {
		return err
	}
	clean, _ := artifact.ValidatePath(req.Path) // valid: Edit accepted it
	writeJSON(w, http.StatusOK, h.links.Edit(a, v, clean, outs))
	return nil
}

func (h *userAPI) skill(w http.ResponseWriter, _ *http.Request, _ auth.Principal) error {
	if h.o.Skill == "" {
		return artifact.Errorf(artifact.ErrNotFound, "this instance serves no skill")
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "text/markdown; charset=utf-8")
	hdr.Set("Cache-Control", "private, no-cache")
	hdr.Set("X-Content-Type-Options", "nosniff")
	_, _ = io.WriteString(w, h.o.Skill)
	return nil
}

func (h *userAPI) createShareLink(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	var req api.ShareLinkRequest
	if err := decodeJSON(w, r, jsonLimit, &req, true); err != nil {
		return err
	}
	ttl, err := present.ParseDuration("expires_in", req.ExpiresIn)
	if err != nil {
		return err
	}
	actor, slug := present.Actor(p), r.PathValue("slug")
	l, token, err := h.d.Artifacts.CreateShareLink(r.Context(), actor, slug, req.Label, ttl)
	if err != nil {
		return err
	}
	a, _, err := h.d.Artifacts.Get(r.Context(), actor, slug)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, h.links.CreatedShareLink(a, l, token, h.o.Now()))
	return nil
}

func (h *userAPI) listShareLinks(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	ls, err := h.d.Artifacts.ListShareLinks(r.Context(), present.Actor(p), r.PathValue("slug"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, present.ShareLinks(ls, h.o.Now()))
	return nil
}

func (h *userAPI) revokeShareLink(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	err := h.d.Artifacts.RevokeShareLink(r.Context(), present.Actor(p), r.PathValue("slug"), r.PathValue("prefix"))
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *userAPI) deleteArtifact(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	hard, err := queryBool(r, "hard")
	if err != nil {
		return err
	}
	if err := h.d.Artifacts.Delete(r.Context(), present.Actor(p), r.PathValue("slug"), hard); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *userAPI) publish(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	var req api.PublishRequest
	if err := decodeJSON(w, r, jsonLimit, &req, false); err != nil {
		return err
	}
	if req.Version < 1 {
		return invalid("version must be a positive version number")
	}
	a, err := h.d.Artifacts.Publish(r.Context(), present.Actor(p), r.PathValue("slug"), req.Version)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, h.links.Result(a, req.Version))
	return nil
}

func (h *userAPI) markStale(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	var req api.StaleRequest
	if err := decodeJSON(w, r, jsonLimit, &req, true); err != nil {
		return err
	}
	var grace time.Duration
	if req.Grace != "" {
		d, err := present.ParseDuration("grace", req.Grace)
		if err != nil || d <= 0 {
			return invalid("grace must be a positive duration such as 72h, 3d or 2w")
		}
		grace = d
	}
	a, err := h.d.Artifacts.MarkStale(r.Context(), present.Actor(p), r.PathValue("slug"), grace)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, h.links.Stale(a))
	return nil
}

func (h *userAPI) unmarkStale(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	a, err := h.d.Artifacts.UnmarkStale(r.Context(), present.Actor(p), r.PathValue("slug"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, h.links.Result(a, 0))
	return nil
}

func (h *userAPI) readFile(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	version, err := queryInt(r, "version")
	if err != nil {
		return err
	}
	anchors, err := queryBool(r, "anchors")
	if err != nil {
		return err
	}
	f, rc, err := h.d.Artifacts.OpenFile(r.Context(), present.Actor(p), r.PathValue("slug"), r.PathValue("path"), version)
	if err != nil {
		return err
	}
	defer rc.Close()
	if anchors {
		return writeAnchored(w, f, rc)
	}
	ct := f.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	name := path.Base(f.Path)
	disposition := mime.FormatMediaType("inline", map[string]string{"filename": name})
	if disposition == "" {
		disposition = "inline"
	}
	hdr := w.Header()
	hdr.Set("Content-Type", ct)
	hdr.Set("ETag", `"`+f.Hash+`"`)
	setUntrustedHeaders(hdr)
	hdr.Set("Content-Disposition", disposition)
	http.ServeContent(w, r, name, time.Time{}, rc)
	return nil
}

// writeAnchored streams because the anchored text can be several times the
// size of the file, and from the blob because a client that stops reading
// keeps the handler running: the file must not be in memory for that long.
func writeAnchored(w http.ResponseWriter, f artifact.File, rc io.ReadSeeker) error {
	text, err := artifact.AnchorableText(rc)
	if err != nil {
		return err
	}
	if !text {
		return invalid("%s is not UTF-8 text, so it has no line anchors", f.Path)
	}
	if _, err := rc.Seek(0, io.SeekStart); err != nil {
		return err
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "text/plain; charset=utf-8")
	setUntrustedHeaders(hdr)
	// Any other failed write is the client going away.
	if err := artifact.StreamAnchored(w, rc); errors.Is(err, artifact.ErrFileRead) {
		// The status has gone out; ending normally would pass off part of the
		// file as all of it.
		panic(http.ErrAbortHandler)
	}
	return nil
}

func setUntrustedHeaders(hdr http.Header) {
	hdr.Set("X-Content-Type-Options", "nosniff")
	// Untrusted artifact bytes must never run as a document on the api origin,
	// whatever their stored type.
	hdr.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	hdr.Set("Cache-Control", "private, no-cache")
}

func (h *userAPI) beginUpload(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	if h.d.Uploads == nil {
		return invalid("chunked uploads are not enabled")
	}
	var req api.BeginUploadRequest
	if err := decodeJSON(w, r, jsonLimit, &req, true); err != nil {
		return err
	}
	if req.ArchiveSHA256 == "" && req.ArchiveBytes == 0 {
		u, err := h.d.Uploads.Begin(r.Context(), p.Handle)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusCreated, present.Upload(u, h.o.MaxChunkBytes, h.o.MaxTotalBytes))
		return nil
	}
	u, token, err := h.d.Uploads.BeginArchive(r.Context(), p.Handle, req.ArchiveSHA256, req.ArchiveBytes)
	if err != nil {
		return err
	}
	out := present.Upload(u, h.o.MaxChunkBytes, h.o.MaxTotalBytes)
	out.ArchiveURL = present.ArchiveURL(h.o.APIBaseURL, u.ID, token)
	writeJSON(w, http.StatusCreated, out)
	return nil
}

func (h *userAPI) uploadChunk(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	if h.d.Uploads == nil {
		return invalid("chunked uploads are not enabled")
	}
	s := r.URL.Query().Get("offset")
	if s == "" {
		return invalid("offset is required")
	}
	offset, err := strconv.ParseInt(s, 10, 64)
	if err != nil || offset < 0 {
		return invalid("offset must be a non-negative integer")
	}
	ct := r.Header.Get("Content-Type")
	if mt, _, err := mime.ParseMediaType(ct); err == nil && mt == "application/octet-stream" {
		ct = "" // unspecified: derive from the extension at publish time
	}
	// One byte over the cap lets the service answer too_large, not truncate.
	body := http.MaxBytesReader(w, r.Body, h.o.MaxChunkBytes+1)
	f, err := h.d.Uploads.AppendChunk(r.Context(), p.Handle, r.PathValue("id"), r.PathValue("path"), offset, body, ct)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, present.Chunk(f))
	return nil
}

func (h *userAPI) abortUpload(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	if h.d.Uploads == nil {
		return invalid("chunked uploads are not enabled")
	}
	if err := h.d.Uploads.Abort(r.Context(), p.Handle, r.PathValue("id")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *userAPI) me(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	used, err := h.d.Artifacts.Usage(r.Context(), p.Handle)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, present.Me(p, used))
	return nil
}

func (h *userAPI) myKeys(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	out := []api.APIKey{}
	if p.Source == auth.SourceAPIKey && h.d.Users != nil {
		keys, err := h.d.Users.ListKeys(r.Context(), p.Handle)
		if err != nil {
			return err
		}
		for i := range keys {
			out = append(out, present.Key(&keys[i]))
		}
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// keyHolder refuses anything but an API key on the routes that change a
// person's keys. A key made or renewed with an OAuth token would outlive
// the connection it came from, and any connected agent could mint one.
func (h *userAPI) keyHolder(p auth.Principal) error {
	if p.Source != auth.SourceAPIKey || h.d.Users == nil {
		return artifact.Errorf(artifact.ErrForbidden,
			"an API key is required: an OAuth connection cannot issue, rotate or revoke keys")
	}
	return nil
}

func (h *userAPI) issueMyKey(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	if err := h.keyHolder(p); err != nil {
		return err
	}
	var req api.IssueKeyRequest
	if err := decodeJSON(w, r, jsonLimit, &req, true); err != nil {
		return err
	}
	expiresIn, err := present.ParseDuration("expires_in", req.ExpiresIn)
	if err != nil {
		return err
	}
	k, secret, err := h.d.Users.IssueKey(r.Context(), user.IssueKeyRequest{
		Handle: p.Handle, Label: req.Label, Scopes: req.Scopes, Slugs: req.Slugs, ExpiresIn: expiresIn, With: p.KeyID,
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, api.IssuedKey{Key: secret, APIKey: present.Key(k)})
	return nil
}

func (h *userAPI) revokeMyKey(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	if err := h.keyHolder(p); err != nil {
		return err
	}
	ref := user.KeyRef{Handle: p.Handle, Prefix: r.PathValue("prefix"), With: p.KeyID}
	if err := h.d.Users.RevokeKey(r.Context(), ref); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *userAPI) rotateMyKey(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	if err := h.keyHolder(p); err != nil {
		return err
	}
	return rotateKey(w, r, h.d.Users, user.KeyRef{Handle: p.Handle, Prefix: r.PathValue("prefix"), With: p.KeyID})
}

// rotateKey serves both rotate routes: the person's own and the admin's.
func rotateKey(w http.ResponseWriter, r *http.Request, users *user.Service, ref user.KeyRef) error {
	var req api.RotateKeyRequest
	if err := decodeJSON(w, r, jsonLimit, &req, true); err != nil {
		return err
	}
	overlap, err := present.ParseOverlap("overlap", req.Overlap, user.DefaultRotateOverlap)
	if err != nil {
		return err
	}
	k, secret, old, err := users.RotateKey(r.Context(), ref, overlap)
	if err != nil {
		return err
	}
	replaced := present.Key(old)
	writeJSON(w, http.StatusCreated, api.IssuedKey{Key: secret, APIKey: present.Key(k), Replaces: &replaced})
	return nil
}
