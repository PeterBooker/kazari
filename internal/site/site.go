// Package site serves published artifacts at <slug>.<base> and pinned
// versions at <slug>--v<N>.<base>. A gated artifact is served only once its
// Gate allows the request, and the Gate answers paths under ReservedPrefix.
// The host router keeps control-plane hosts away from it.
package site

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/blob"
	"github.com/PeterBooker/kazari/internal/csp"
	"github.com/PeterBooker/kazari/internal/gate"
	"github.com/PeterBooker/kazari/internal/middleware"
	"github.com/PeterBooker/kazari/internal/theme"
	"github.com/PeterBooker/kazari/internal/views"
)

// Resolver looks up what to serve for a slug. ResolvePublished and
// ResolveVersion return artifact.ErrNotFound or artifact.ErrVersionNotFound.
type Resolver interface {
	ResolvePublished(ctx context.Context, slug string) (*artifact.Published, error)
	ResolveVersion(ctx context.Context, slug string, n int) (*artifact.Published, error)
	// File returns the version's file at path, and false when it has none.
	File(ctx context.Context, versionID int64, path string) (artifact.File, bool, error)
	// WorkerControlled reports whether whoever uploaded the version's file
	// at path may still change the artifact.
	WorkerControlled(ctx context.Context, versionID int64, path string) (bool, error)
}

// Gate guards artifacts with visibility members. gate.Gate implements it.
type Gate interface {
	// Allow reports whether r may be served p on host, the request host's
	// label (<slug> or <slug>--v<N>), and who was let in. On false it has
	// answered the request.
	Allow(w http.ResponseWriter, r *http.Request, p *artifact.Published, host string) (views.Access, bool)
	// Admits reports whether r carries access to p on host, and answers nothing.
	Admits(r *http.Request, p *artifact.Published, host string) (bool, error)
	ServeReserved(w http.ResponseWriter, r *http.Request, p *artifact.Published, host string)
}

// Views counts page views. views.Recorder implements it.
type Views interface {
	// Record is given every request answered with an HTML file, and counts
	// those that are page views.
	Record(r *http.Request, h views.Hit)
}

// ReservedPrefix is left to the Gate on every artifact host; no artifact file
// can live under it (artifact.ReservedDir).
const ReservedPrefix = "/" + artifact.ReservedDir + "/"

// fileRoute is http.route for every file of an artifact.
const fileRoute = "/{path...}"

// The artifact a request is for, on its server span; version 0 is the
// published host.
const (
	slugKey    = attribute.Key("kazari.artifact.slug")
	versionKey = attribute.Key("kazari.artifact.version")
)

// Options configures the handler. Only BaseDomain is required.
type Options struct {
	BaseDomain string
	// ReservedSlugs are never served, whatever the store holds: an
	// artifact created before its slug was reserved stops serving, and the
	// handler does not rely on the host router alone to keep control-plane
	// labels away from it.
	ReservedSlugs []string
	// DefaultCSP is sent with every response; empty sends none unless the
	// artifact has its own.
	DefaultCSP string
	// AllowCSPOverride lets an artifact's kazari.csp metadata add sources
	// to DefaultCSP for its own files.
	AllowCSPOverride bool
	NotFoundPage     []byte       // nil uses a built-in page
	GonePage         []byte       // 410 for purged artifacts; nil uses a built-in page
	Logger           *slog.Logger // nil uses slog.Default()
	// ImmutableMaxAge caps caching of pinned versions; zero means
	// MaxImmutableAge. Keep it within the tombstone period, or a freed
	// slug's <slug>--v1 may be answered from a cache of the old owner's.
	ImmutableMaxAge time.Duration
	// Gate guards gated artifacts and answers ReservedPrefix. Without one,
	// gated artifacts and reserved paths are not found: nothing gated is
	// ever served unchecked.
	Gate Gate
	// Views counts page views; nil counts none.
	Views Views
}

// MaxImmutableAge is the default and the cap of ImmutableMaxAge.
const MaxImmutableAge = 365 * 24 * time.Hour

//go:embed pages/error.html
var errorPageHTML string

// The instance's own error pages, in the theme's inline form: an artifact
// host serves no file of Kazari's. A share link that opens nothing gets
// its own words, since whoever holds one was sent it by someone.
var (
	defaultNotFoundPage = errorPage("Not found", "Nothing <b>here</b>",
		"There is no page at this address. It may have moved, been taken offline or never existed; check the address with whoever gave it to you.")
	defaultGonePage = errorPage("Gone", "Removed <b>for good</b>",
		"The page that was at this address has been removed, and nothing brings it back.")
	shareNotFoundPage = errorPage("Link not working", "This link <b>opens nothing</b>",
		"The page it was for is offline for now, or no longer there. Ask the person who sent you the link for a new one.")
)

func errorPage(title string, heading template.HTML, message string) []byte {
	t := template.Must(theme.Templates().New("error").Parse(errorPageHTML))
	var b bytes.Buffer
	if err := t.Execute(&b, struct {
		Title   string
		Heading template.HTML
		Message string
	}{title, heading, message}); err != nil {
		panic(err)
	}
	return b.Bytes()
}

// A gated artifact is cacheGated on every host, so no shared cache keeps it
// and a sign-out or revoked share link applies even to a version the browser
// has seen (the strong ETag keeps that a 304).
const (
	cacheLatest = "no-cache"
	cacheGated  = "private, no-cache"
)

const indexFile = "index.html"

// evictionScript replaces a service worker and then removes itself: the one
// answer a browser acts on, since a failed update check leaves the worker in
// place. It has no fetch handler, so it sees nothing on the way out.
const evictionScript = `self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", (e) => e.waitUntil(self.registration.unregister()));
`

type handler struct {
	resolver  Resolver
	blobs     blob.Store
	opts      Options
	log       *slog.Logger
	immutable string
	// shareNotFound is the 404 page of the share path: an operator's own
	// page when set, else one that speaks of the link.
	shareNotFound []byte
}

// NewHandler returns the artifact-serving handler.
func NewHandler(r Resolver, blobs blob.Store, opts Options) http.Handler {
	if opts.NotFoundPage == nil {
		opts.NotFoundPage = defaultNotFoundPage
	}
	if opts.GonePage == nil {
		opts.GonePage = defaultGonePage
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	age := opts.ImmutableMaxAge
	if age <= 0 || age > MaxImmutableAge {
		age = MaxImmutableAge
	}
	immutable := "public, max-age=" + strconv.FormatInt(int64(age/time.Second), 10) + ", immutable"
	shareNotFound := opts.NotFoundPage
	if bytes.Equal(shareNotFound, defaultNotFoundPage) {
		shareNotFound = shareNotFoundPage
	}
	return &handler{resolver: r, blobs: blobs, opts: opts, log: log, immutable: immutable, shareNotFound: shareNotFound}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength != 0 {
		// GET and HEAD carry no body. Closing stops net/http waiting, with
		// no deadline, to discard a body the client announces but never sends.
		w.Header().Set("Connection", "close")
	}
	// The reserved paths take a form post (sign-out); the gate checks
	// their methods itself.
	reserved := strings.HasPrefix(r.URL.Path, ReservedPrefix)
	if !reserved && r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		h.writeError(w, r, http.StatusMethodNotAllowed)
		return
	}

	slug, version, ok := ParseHost(r.Host, h.opts.BaseDomain)
	if !ok || slices.Contains(h.opts.ReservedSlugs, slug) {
		h.miss(w, r, http.StatusNotFound)
		return
	}
	if span := trace.SpanFromContext(r.Context()); span.IsRecording() {
		span.SetAttributes(slugKey.String(slug), versionKey.Int(version))
	}

	p, err := h.resolve(r.Context(), slug, version)
	if err != nil {
		if errors.Is(err, artifact.ErrNotFound) || errors.Is(err, artifact.ErrVersionNotFound) {
			h.miss(w, r, http.StatusNotFound)
			return
		}
		h.internalError(w, r, fmt.Errorf("resolve %s: %w", slug, err))
		return
	}
	if !p.Servable() {
		// An offline artifact is not found on every host, whatever became
		// of the version, and a members page answers before its gate, so
		// it does not say which of its versions were deleted.
		if p.PurgedAt != nil || (p.VersionDeleted && p.UnpublishedAt == nil && p.DeletedAt == nil && !p.Visibility.Gated()) {
			h.miss(w, r, http.StatusGone)
			return
		}
		h.miss(w, r, http.StatusNotFound)
		return
	}

	if p.Gated() {
		// A visitor without access ends on the sign-in page, whose policy
		// severs a cross-site opener's window handle, so every answer on
		// this host needs the same policy or the handle tells that opener
		// who has access. Set here it covers the gate's answers and error
		// responses, which get no artifact and so no setCommonHeaders.
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		// Sibling artifact hosts are same-site, so a viewer's Lax cookie
		// rides along when another artifact's page requests this host.
		// same-origin stops the browser handing it the response. Every
		// answer from here on, the gate's and any error, has to carry it:
		// the page can see which ones the browser refuses, and any
		// difference would tell it whether the visitor has access.
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	}

	access := views.Access{Via: views.ViaPublic}
	host := slug
	if version != 0 {
		host += artifact.VersionSeparator + strconv.Itoa(version)
	}
	if h.opts.Gate == nil {
		if reserved || p.Gated() {
			h.writeError(w, r, http.StatusNotFound)
			return
		}
	} else {
		if reserved {
			h.opts.Gate.ServeReserved(w, r, p, host)
			return
		}
		// Ahead of Allow, whose 401 a browser would keep the worker through.
		// Every path gets this answer, so it does not show which files exist.
		if isWorkerScript(r) && p.Gated() {
			admitted, err := h.opts.Gate.Admits(r, p, host)
			if err != nil {
				h.internalError(w, r, fmt.Errorf("check access to %s: %w", host, err))
				return
			}
			if !admitted {
				h.evictWorker(w, r, false)
				return
			}
		}
		// Public pages pass the gate too, which counts them.
		var ok bool
		if access, ok = h.opts.Gate.Allow(w, r, p, host); !ok {
			return
		}
	}

	middleware.SetRoute(r.Context(), fileRoute)
	h.serveVersion(w, r, p, access, version != 0)
}

func (h *handler) resolve(ctx context.Context, slug string, version int) (*artifact.Published, error) {
	if version == 0 {
		return h.resolver.ResolvePublished(ctx, slug)
	}
	return h.resolver.ResolveVersion(ctx, slug, version)
}

func (h *handler) serveVersion(w http.ResponseWriter, r *http.Request, p *artifact.Published, access views.Access, pinned bool) {
	clean, dir, ok := cleanPath(r.URL.Path)
	if !ok {
		h.writeError(w, r, http.StatusNotFound)
		return
	}
	name := clean
	if dir {
		name = path.Join(clean, indexFile)
	}

	f, found, err := h.file(r, p, name)
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	if isWorkerScript(r) {
		// Ahead of the redirect, the SPA route and 404.html below, none of
		// which is a script.
		if !found || !javaScript(f.ContentType) {
			h.evictWorker(w, r, false)
			return
		}
		controlled, err := h.resolver.WorkerControlled(r.Context(), p.VersionID, f.Path)
		if err != nil {
			h.internalError(w, r, fmt.Errorf("check worker %s/%s: %w", p.Slug, f.Path, err))
			return
		}
		if !controlled {
			h.evictWorker(w, r, false)
			return
		}
	}
	if found {
		h.serveFile(w, r, p, access, pinned, f)
		return
	}

	// Redirect so relative links in the directory's index.html resolve
	// against it.
	if !dir {
		_, found, err := h.file(r, p, clean+"/"+indexFile)
		if err != nil {
			h.internalError(w, r, err)
			return
		}
		if found {
			h.redirectDir(w, r, p, pinned, clean)
			return
		}
	}

	// SPA routes have no extension; assets that miss still 404.
	if p.SPA && path.Ext(clean) == "" {
		f, found, err := h.file(r, p, indexFile)
		if err != nil {
			h.internalError(w, r, err)
			return
		}
		if found {
			h.serveFile(w, r, p, access, pinned, f)
			return
		}
	}

	f, found, err = h.file(r, p, "404.html")
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	if found {
		h.serveNotFoundFile(w, r, p, f)
		return
	}
	h.writeError(w, r, http.StatusNotFound)
}

func (h *handler) file(r *http.Request, p *artifact.Published, name string) (artifact.File, bool, error) {
	f, found, err := h.resolver.File(r.Context(), p.VersionID, name)
	if err != nil {
		return artifact.File{}, false, fmt.Errorf("load %s/%s of version %d: %w", p.Slug, name, p.VersionNumber, err)
	}
	return f, found, nil
}

// cleanPath returns the path without its leading slash ("" for the root)
// and whether it names a directory. ok is false for "..", control
// characters and backslashes, which no stored path contains and browsers
// read as "/" in a Location header.
func cleanPath(p string) (clean string, dir bool, ok bool) {
	for i := 0; i < len(p); i++ {
		if p[i] < 0x20 || p[i] == 0x7f || p[i] == '\\' {
			return "", false, false
		}
	}
	for seg := range strings.SplitSeq(p, "/") {
		if seg == ".." {
			return "", false, false
		}
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	dir = strings.HasSuffix(p, "/")
	clean = strings.TrimPrefix(path.Clean(p), "/")
	return clean, dir || clean == "", true
}

func (h *handler) redirectDir(w http.ResponseWriter, r *http.Request, p *artifact.Published, pinned bool, clean string) {
	h.setCommonHeaders(w, p, pinned)
	loc := (&url.URL{Path: "/" + clean + "/", RawQuery: r.URL.RawQuery}).String()
	w.Header().Set("Location", loc)
	w.WriteHeader(http.StatusMovedPermanently)
}

func (h *handler) serveFile(w http.ResponseWriter, r *http.Request, p *artifact.Published, access views.Access, pinned bool, f artifact.File) {
	rc, err := h.blobs.Open(r.Context(), f.Hash)
	if err != nil {
		h.internalError(w, r, fmt.Errorf("open blob %s for %s/%s: %w", f.Hash, p.Slug, f.Path, err))
		return
	}
	defer rc.Close()

	h.setCommonHeaders(w, p, pinned)
	hdr := w.Header()
	hdr.Set("Content-Type", contentType(f.ContentType))
	hdr.Set("ETag", `"`+f.Hash+`"`)
	if !p.Indexable {
		hdr.Set("X-Robots-Tag", "noindex")
	}
	// Ahead of ServeContent, so a 304 counts as the view it is; a 412 counts
	// too, which is rare and not worth wrapping the writer to see.
	if h.opts.Views != nil && html(f.ContentType) {
		h.opts.Views.Record(r, views.Hit{ArtifactID: p.ArtifactID, Path: f.Path, Access: access,
			IP: middleware.ClientIP(r), UA: r.Header.Get("User-Agent")})
	}
	if compressible(f.ContentType, f.Size) {
		// On identity answers too, so a shared cache of a pinned version
		// keeps the two representations apart.
		hdr.Add("Vary", "Accept-Encoding")
		if wantsGzip(r, gzipETag(f.Hash)) {
			h.serveGzip(w, r, p, f, rc)
			return
		}
	}
	http.ServeContent(w, r, f.Path, time.Time{}, rc)
}

// serveGzip answers what http.ServeContent would for a request without
// Range, If-Range or If-Match, with the gzip representation and its own ETag.
func (h *handler) serveGzip(w http.ResponseWriter, r *http.Request, p *artifact.Published, f artifact.File, rc io.Reader) {
	hdr := w.Header()
	etag := gzipETag(f.Hash)
	hdr.Set("ETag", etag)
	if noneMatch(r.Header.Values("If-None-Match"), etag) {
		hdr.Del("Content-Type")
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if err := writeGzip(w, r, http.StatusOK, rc); err != nil {
		h.abortGzip(r, p, f.Path, err)
	}
}

// abortGzip ends a gzip response whose status has gone out, which
// writeGzip explains.
func (h *handler) abortGzip(r *http.Request, p *artifact.Published, path string, err error) {
	if !errors.Is(err, context.Canceled) {
		h.log.WarnContext(r.Context(), "site: send gzip", "slug", p.Slug, "path", path, "err", err)
	}
	panic(http.ErrAbortHandler)
}

// html reports whether ct is an HTML page, the one kind of file a view is.
func html(ct string) bool {
	base, _, _ := strings.Cut(ct, ";")
	return strings.EqualFold(strings.TrimSpace(base), "text/html")
}

// serveNotFoundFile avoids ServeContent, which always answers 200 or 206.
func (h *handler) serveNotFoundFile(w http.ResponseWriter, r *http.Request, p *artifact.Published, f artifact.File) {
	rc, err := h.blobs.Open(r.Context(), f.Hash)
	if err != nil {
		h.internalError(w, r, fmt.Errorf("open blob %s for %s/%s: %w", f.Hash, p.Slug, f.Path, err))
		return
	}
	defer rc.Close()

	h.setErrorHeaders(w, p)
	w.Header().Set("Content-Type", contentType(f.ContentType))
	if compressible(f.ContentType, f.Size) {
		w.Header().Add("Vary", "Accept-Encoding")
		if acceptsGzip(r.Header.Values("Accept-Encoding")) {
			if err := writeGzip(w, r, http.StatusNotFound, rc); err != nil {
				h.abortGzip(r, p, f.Path, err)
			}
			return
		}
	}
	w.Header().Set("Content-Length", strconv.FormatInt(f.Size, 10))
	w.WriteHeader(http.StatusNotFound)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := io.Copy(w, rc); err != nil && !errors.Is(err, context.Canceled) {
		h.log.WarnContext(r.Context(), "site: copy 404.html", "slug", p.Slug, "err", err)
	}
}

// isWorkerScript reports whether a browser is fetching r's path as a service
// worker, to install it or to check it for an update. That request skips any
// worker the browser holds, so it is the one a worker cannot answer itself.
func isWorkerScript(r *http.Request) bool {
	return r.Header.Get("Service-Worker") == "script"
}

// javaScript reports whether a browser accepts ct for a service worker; it
// keeps the worker it has when the update check gets anything else.
func javaScript(ct string) bool {
	base, _, _ := strings.Cut(strings.ToLower(ct), ";")
	switch strings.TrimSpace(base) {
	case "text/javascript", "application/javascript", "application/x-javascript",
		"text/ecmascript", "application/ecmascript":
		return true
	}
	return false
}

// miss answers a request for an artifact or version that is not served. A
// worker's update check gets the eviction script; only a purged artifact,
// which no page of the origin will come back to, loses the origin's storage.
func (h *handler) miss(w http.ResponseWriter, r *http.Request, status int) {
	if isWorkerScript(r) {
		h.evictWorker(w, r, status == http.StatusGone)
		return
	}
	h.writeError(w, r, status)
}

// evictWorker answers a worker-script request with evictionScript. clear also
// wipes the origin's storage. Live pages never ask for that: a first
// registration looks the same as an update check, so a page that registers a
// missing script would lose its visitors' data on every load.
func (h *handler) evictWorker(w http.ResponseWriter, r *http.Request, clear bool) {
	h.setErrorHeaders(w, nil)
	hdr := w.Header()
	hdr.Set("Content-Type", "text/javascript; charset=utf-8")
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("Content-Length", strconv.Itoa(len(evictionScript)))
	if clear {
		hdr.Set("Clear-Site-Data", `"storage"`)
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, evictionScript)
	}
}

// writeError always uses DefaultCSP: the page is the instance's, not the
// artifact's.
func (h *handler) writeError(w http.ResponseWriter, r *http.Request, status int) {
	var body []byte
	switch {
	case status == http.StatusNotFound && r.URL.Path == gate.SharePath:
		body = h.shareNotFound
	case status == http.StatusNotFound:
		body = h.opts.NotFoundPage
	case status == http.StatusGone:
		body = h.opts.GonePage
	}
	h.setErrorHeaders(w, nil)
	hdr := w.Header()
	if body != nil {
		hdr.Set("Content-Type", "text/html; charset=utf-8")
	} else {
		body = []byte(http.StatusText(status) + "\n")
		hdr.Set("Content-Type", "text/plain; charset=utf-8")
	}
	hdr.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

func (h *handler) internalError(w http.ResponseWriter, r *http.Request, err error) {
	if !errors.Is(err, context.Canceled) {
		h.log.ErrorContext(r.Context(), "site: serve", "host", r.Host, "path", r.URL.Path, "err", err)
	}
	h.writeError(w, r, http.StatusInternalServerError)
}

func (h *handler) setCommonHeaders(w http.ResponseWriter, p *artifact.Published, pinned bool) {
	hdr := w.Header()
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	if csp := h.csp(p); csp != "" {
		hdr.Set("Content-Security-Policy", csp)
	}
	gated := p != nil && p.Gated()
	switch {
	case gated:
		hdr.Set("Cache-Control", cacheGated)
	case pinned:
		hdr.Set("Cache-Control", h.immutable)
	default:
		hdr.Set("Cache-Control", cacheLatest)
	}
}

func (h *handler) setErrorHeaders(w http.ResponseWriter, p *artifact.Published) {
	h.setCommonHeaders(w, p, false)
	w.Header().Set("X-Robots-Tag", "noindex")
}

func (h *handler) csp(p *artifact.Published) string {
	if p != nil && p.CSP != "" && h.opts.AllowCSPOverride {
		return csp.Merge(h.opts.DefaultCSP, p.CSP)
	}
	return h.opts.DefaultCSP
}

func contentType(ct string) string {
	if ct == "" {
		return "application/octet-stream"
	}
	lower := strings.ToLower(ct)
	if strings.Contains(lower, "charset=") {
		return ct
	}
	base, _, _ := strings.Cut(lower, ";")
	base = strings.TrimSpace(base)
	if strings.HasPrefix(base, "text/") || base == "application/javascript" || base == "application/json" {
		return ct + "; charset=utf-8"
	}
	return ct
}
