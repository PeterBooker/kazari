package httpapi

import (
	"bytes"
	"io"
	"log/slog"
	"maps"
	"mime"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"kazari/internal/api"
	"kazari/internal/artifact"
	"kazari/internal/auth"
	"kazari/internal/present"
	"kazari/internal/ratelimit"
	"kazari/internal/upload"
	"kazari/internal/user"
)

// DefaultIPRatePerMinute is the per-IP request rate of the user API when
// UserOptions.IPRatePerMinute is zero.
const DefaultIPRatePerMinute = ratelimit.DefaultIPPerMinute

// UserDeps are the collaborators of the user API.
type UserDeps struct {
	Artifacts *artifact.Service
	Uploads   *upload.Service
	Users     *user.Service
	Auth      auth.Authenticator // API keys and OAuth tokens, already chained
	Links     present.Links
	Logger    *slog.Logger
	// Writes limits write requests per credential. It is shared with the
	// MCP server so that a key has one budget on both; nil disables it.
	Writes *ratelimit.Limiter
}

// UserOptions tunes the user API. Zero fields take the defaults noted.
type UserOptions struct {
	// MaxBodyBytes caps JSON bodies with inline files, the multipart stream
	// and the uncompressed size of a bundle. Default: the artifact
	// service's default version size plus 1 MiB of headroom.
	MaxBodyBytes int64
	// MaxChunkBytes caps one raw-body upload chunk. Default: the upload
	// service's chunk limit.
	MaxChunkBytes int64
	// MaxTotalBytes is echoed in POST /v1/uploads. Default:
	// upload.DefaultMaxTotalBytes.
	MaxTotalBytes int64
	// IPRatePerMinute limits requests per client IP before authentication.
	// 0 means DefaultIPRatePerMinute; a negative value disables it.
	IPRatePerMinute int
	// TempDir is where multipart bundles are spilled while they are
	// published. Default: os.TempDir().
	TempDir string
	// Now is the clock of the rate limiters. Default: time.Now.
	Now func() time.Time
	// Skill is the instance's Agent Skill (a SKILL.md) served at
	// GET /v1/skill; empty answers 404.
	Skill string
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

// NewUserAPI returns the handler for /v1/... on the api host. Every route
// requires a bearer credential; reads need artifacts:read, writes
// artifacts:write and deletes artifacts:delete.
func NewUserAPI(d UserDeps, o UserOptions) http.Handler {
	o = o.withDefaults(d)
	h := &userAPI{base: newBase(d.Logger, d.Links), d: d, o: o}

	read := func(fn apiFunc) http.Handler { return h.guard(auth.ScopeRead, false, fn) }
	write := func(fn apiFunc) http.Handler { return h.guard(auth.ScopeWrite, true, fn) }
	del := func(fn apiFunc) http.Handler { return h.guard(auth.ScopeDelete, true, fn) }

	mux := http.NewServeMux()
	mux.Handle("POST /v1/artifacts", write(h.createArtifact))
	mux.Handle("GET /v1/artifacts", read(h.listArtifacts))
	mux.Handle("GET /v1/artifacts/{slug}", read(h.getArtifact))
	mux.Handle("PUT /v1/artifacts/{slug}", write(h.updateArtifact))
	mux.Handle("PATCH /v1/artifacts/{slug}", write(h.patchArtifact))
	mux.Handle("DELETE /v1/artifacts/{slug}", del(h.deleteArtifact))
	mux.Handle("POST /v1/artifacts/{slug}/edit", write(h.editFile))
	mux.Handle("POST /v1/artifacts/{slug}/publish", write(h.publish))
	mux.Handle("POST /v1/artifacts/{slug}/stale", write(h.markStale))
	mux.Handle("DELETE /v1/artifacts/{slug}/stale", write(h.unmarkStale))
	mux.Handle("GET /v1/artifacts/{slug}/files/{path...}", read(h.readFile))
	mux.Handle("POST /v1/uploads", write(h.beginUpload))
	mux.Handle("PATCH /v1/uploads/{id}/files/{path...}", write(h.uploadChunk))
	mux.Handle("DELETE /v1/uploads/{id}", write(h.abortUpload))
	mux.Handle("GET /v1/skill", read(h.skill))
	mux.Handle("GET /v1/me", h.guard("", false, h.me))
	mux.Handle("GET /v1/me/keys", h.guard("", false, h.myKeys))

	var handler http.Handler = jsonFallback(mux, &h.base)
	handler = auth.RequireBearer(d.Auth, auth.BearerOptions{})(handler)
	handler = h.limit(ratelimit.New(o.IPRatePerMinute, o.Now), ratelimit.IPKey)(handler)
	return handler
}

// guard wraps fn with the scope check and, for writes, the per-key limit.
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

// keyOf is the per-credential rate-limit bucket.
func keyOf(r *http.Request) string {
	p, _ := auth.FromContext(r.Context())
	return ratelimit.CredentialKey(p)
}

// limit rejects requests over l's rate with 429 and Retry-After.
func (b *base) limit(l *ratelimit.Limiter, key func(*http.Request) string) func(http.Handler) http.Handler {
	return ratelimit.Middleware(l, key, func(w http.ResponseWriter, r *http.Request, d time.Duration) {
		b.writeError(w, r, &statusError{status: http.StatusTooManyRequests,
			code: api.CodeRateLimited, msg: "too many requests", retryAfter: d})
	})
}

// mediaType is the request's media type; an absent Content-Type is JSON.
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
	files, err := inlineFiles(req.Files, false)
	if err != nil {
		return err
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
		Files:       b.files,
	})
}

func (h *userAPI) create(w http.ResponseWriter, r *http.Request, p auth.Principal, req artifact.CreateRequest) error {
	a, v, err := h.d.Artifacts.Create(r.Context(), present.Actor(p), req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, h.links.Result(a, v.Number))
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
	if withFiles {
		for i := range versions {
			m, err := h.d.Artifacts.Files(r.Context(), versions[i].ID)
			if err != nil {
				return err
			}
			versions[i].Files = sortedFiles(m)
		}
	}
	writeJSON(w, http.StatusOK, h.links.Detail(a, versions))
	return nil
}

func sortedFiles(m map[string]artifact.File) []artifact.File {
	out := make([]artifact.File, 0, len(m))
	for _, p := range slices.Sorted(maps.Keys(m)) {
		out = append(out, m[p])
	}
	return out
}

// updateArtifact is PUT: files and fields, from JSON or multipart.
func (h *userAPI) updateArtifact(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	var (
		req    api.UpdateArtifactRequest
		bundle *bundle
	)
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
	files, err := inlineFiles(req.Files, true)
	if err != nil {
		return err
	}
	if bundle != nil {
		files = mergeFiles(files, bundle.files)
	}
	files, done, err := h.withUpload(r.Context(), p.Handle, req.UploadID, files)
	if err != nil {
		return err
	}
	defer done()
	return h.update(w, r, p, req, files)
}

// patchArtifact is PATCH: fields only, including visibility "unpublished".
func (h *userAPI) patchArtifact(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	var req api.UpdateArtifactRequest
	if err := decodeJSON(w, r, jsonLimit, &req, false); err != nil {
		return err
	}
	if len(req.Files) > 0 || req.UploadID != "" {
		return invalid("PATCH changes fields only; use PUT to change files")
	}
	return h.update(w, r, p, req, nil)
}

// update applies an update request. Visibility goes through
// SetVisibility, so "unpublished" works and "public" or "unlisted" also
// re-publish an unpublished artifact; it is applied after the other
// changes, so a restore comes first.
func (h *userAPI) update(w http.ResponseWriter, r *http.Request, p auth.Principal, req api.UpdateArtifactRequest, files []artifact.FileInput) error {
	slug := r.PathValue("slug")
	expected, err := expectedVersion(r, req.ExpectedVersion)
	if err != nil {
		return err
	}
	if req.Visibility != nil {
		switch *req.Visibility {
		case string(artifact.Public), string(artifact.Unlisted), artifact.Unpublished:
		default:
			return invalid("visibility must be public, unlisted or unpublished")
		}
	}
	actor := present.Actor(p)
	visibilityOnly := req.Visibility != nil && len(files) == 0 && req.Name == nil && req.Description == nil &&
		len(req.Metadata) == 0 && !req.Restore && expected == 0

	var (
		a       *artifact.Artifact
		version int
	)
	if !visibilityOnly {
		var v *artifact.Version
		a, v, err = h.d.Artifacts.Update(r.Context(), actor, artifact.UpdateRequest{
			Slug:            slug,
			Name:            req.Name,
			Description:     req.Description,
			Metadata:        req.Metadata,
			Label:           req.Label,
			ExpectedVersion: expected,
			KeepStale:       req.KeepStale,
			Restore:         req.Restore,
			Files:           files,
		})
		if err != nil {
			return err
		}
		if v != nil {
			version = v.Number
		}
	}
	if req.Visibility != nil {
		if a, err = h.d.Artifacts.SetVisibility(r.Context(), actor, slug, *req.Visibility); err != nil {
			return err
		}
	}
	writeJSON(w, http.StatusOK, h.links.Result(a, version))
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

// editFile is POST /artifacts/{slug}/edit: edits to one text file of the
// latest version, published as a new version.
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
		KeepStale:       req.KeepStale,
	})
	if err != nil {
		return err
	}
	clean, _ := artifact.ValidatePath(req.Path) // valid: Edit accepted it
	writeJSON(w, http.StatusOK, h.links.Edit(a, v.Number, clean, outs))
	return nil
}

// skill is GET /skill: the instance's Agent Skill, as Markdown.
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
		d, err := time.ParseDuration(req.Grace)
		if err != nil || d <= 0 {
			return invalid("grace must be a positive duration such as 336h")
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

// writeAnchored writes a text file with every line prefixed by its anchor
// (artifact.AnchorLines), the form edit anchors refer to. The anchored text
// is streamed: it can be several times the size of the file.
func writeAnchored(w http.ResponseWriter, f artifact.File, rc io.Reader) error {
	data, err := io.ReadAll(rc) // stored files are at most limits.max_file_bytes
	if err != nil {
		return err
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return invalid("%s is not UTF-8 text, so it has no line anchors", f.Path)
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "text/plain; charset=utf-8")
	setUntrustedHeaders(hdr)
	_ = artifact.WriteAnchored(w, string(data)) // a failed write is the client going away
	return nil
}

// setUntrustedHeaders sets the headers of a response carrying artifact
// bytes on the api origin.
func setUntrustedHeaders(hdr http.Header) {
	hdr.Set("X-Content-Type-Options", "nosniff")
	// Artifact bytes are untrusted; on the api origin they must never run
	// as a document, whatever their stored type.
	hdr.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	hdr.Set("Cache-Control", "private, no-cache")
}

func (h *userAPI) beginUpload(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	if h.d.Uploads == nil {
		return invalid("chunked uploads are not enabled")
	}
	u, err := h.d.Uploads.Begin(r.Context(), p.Handle)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, present.Upload(u, h.o.MaxChunkBytes, h.o.MaxTotalBytes))
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
	// One byte over the chunk cap lets the service see the overflow and
	// answer too_large rather than truncating.
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

// me reports the caller and how much of its quota is in use.
func (h *userAPI) me(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	used, err := h.d.Artifacts.Usage(r.Context(), p.Handle)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, present.Me(p, used))
	return nil
}

// myKeys lists the caller's API keys; OAuth callers get an empty list.
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
