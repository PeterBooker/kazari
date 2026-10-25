// Package site serves published artifacts at <slug>.<base> and pinned
// versions at <slug>--v<N>.<base>. The host router keeps control-plane hosts
// away from it.
package site

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"kazari/internal/artifact"
	"kazari/internal/blob"
)

// Resolver looks up what to serve for a slug. ResolvePublished and
// ResolveVersion return artifact.ErrNotFound or artifact.ErrVersionNotFound.
type Resolver interface {
	ResolvePublished(ctx context.Context, slug string) (*artifact.Published, error)
	ResolveVersion(ctx context.Context, slug string, n int) (*artifact.Published, error)
	Files(ctx context.Context, versionID int64) (map[string]artifact.File, error)
}

// Options configures the handler. Only BaseDomain is required.
type Options struct {
	BaseDomain string
	// DefaultCSP is sent with every response; empty sends none unless the
	// artifact has its own.
	DefaultCSP string
	// AllowCSPOverride lets an artifact's kazari.csp metadata replace
	// DefaultCSP for its own files.
	AllowCSPOverride bool
	NotFoundPage     []byte       // nil uses a built-in page
	GonePage         []byte       // 410 for purged artifacts; nil uses a built-in page
	Logger           *slog.Logger // nil uses slog.Default()
	// ImmutableMaxAge caps caching of pinned versions; zero means
	// MaxImmutableAge. Keep it within the tombstone period, or a freed
	// slug's <slug>--v1 may be answered from a cache of the old owner's.
	ImmutableMaxAge time.Duration
}

// MaxImmutableAge is the default and the cap of ImmutableMaxAge.
const MaxImmutableAge = 365 * 24 * time.Hour

var (
	//go:embed pages/404.html
	defaultNotFoundPage []byte
	//go:embed pages/410.html
	defaultGonePage []byte
)

const cacheLatest = "no-cache"

const indexFile = "index.html"

type handler struct {
	resolver  Resolver
	blobs     blob.Store
	opts      Options
	log       *slog.Logger
	immutable string
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
	return &handler{resolver: r, blobs: blobs, opts: opts, log: log, immutable: immutable}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength != 0 {
		// GET and HEAD carry no body. Closing stops net/http waiting, with
		// no deadline, to discard a body the client announces but never sends.
		w.Header().Set("Connection", "close")
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		h.writeError(w, r, http.StatusMethodNotAllowed)
		return
	}

	slug, version, ok := ParseHost(r.Host, h.opts.BaseDomain)
	if !ok {
		h.writeError(w, r, http.StatusNotFound)
		return
	}

	p, err := h.resolve(r.Context(), slug, version)
	if err != nil {
		if errors.Is(err, artifact.ErrNotFound) || errors.Is(err, artifact.ErrVersionNotFound) {
			h.writeError(w, r, http.StatusNotFound)
			return
		}
		h.internalError(w, r, fmt.Errorf("resolve %s: %w", slug, err))
		return
	}
	if !p.Servable() {
		if p.PurgedAt != nil {
			h.writeError(w, r, http.StatusGone)
			return
		}
		h.writeError(w, r, http.StatusNotFound)
		return
	}

	files, err := h.resolver.Files(r.Context(), p.VersionID)
	if err != nil {
		h.internalError(w, r, fmt.Errorf("load files of %s version %d: %w", slug, p.VersionNumber, err))
		return
	}

	h.serveVersion(w, r, p, version != 0, files)
}

func (h *handler) resolve(ctx context.Context, slug string, version int) (*artifact.Published, error) {
	if version == 0 {
		return h.resolver.ResolvePublished(ctx, slug)
	}
	return h.resolver.ResolveVersion(ctx, slug, version)
}

func (h *handler) serveVersion(w http.ResponseWriter, r *http.Request, p *artifact.Published, pinned bool, files map[string]artifact.File) {
	clean, dir, ok := cleanPath(r.URL.Path)
	if !ok {
		h.writeError(w, r, http.StatusNotFound)
		return
	}
	name := clean
	if dir {
		name = path.Join(clean, indexFile)
	}

	if f, ok := files[name]; ok {
		h.serveFile(w, r, p, pinned, f)
		return
	}

	// Redirect so relative links in the directory's index.html resolve
	// against it.
	if !dir {
		if _, ok := files[clean+"/"+indexFile]; ok {
			h.redirectDir(w, r, p, pinned, clean)
			return
		}
	}

	// SPA routes have no extension; assets that miss still 404.
	if p.SPA && path.Ext(clean) == "" {
		if f, ok := files[indexFile]; ok {
			h.serveFile(w, r, p, pinned, f)
			return
		}
	}

	if f, ok := files["404.html"]; ok {
		h.serveNotFoundFile(w, r, p, f)
		return
	}
	h.writeError(w, r, http.StatusNotFound)
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

func (h *handler) serveFile(w http.ResponseWriter, r *http.Request, p *artifact.Published, pinned bool, f artifact.File) {
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
	http.ServeContent(w, r, f.Path, time.Time{}, rc)
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
	w.Header().Set("Content-Length", strconv.FormatInt(f.Size, 10))
	w.WriteHeader(http.StatusNotFound)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := io.Copy(w, rc); err != nil && !errors.Is(err, context.Canceled) {
		h.log.WarnContext(r.Context(), "site: copy 404.html", "slug", p.Slug, "err", err)
	}
}

// writeError always uses DefaultCSP: the page is the instance's, not the
// artifact's.
func (h *handler) writeError(w http.ResponseWriter, r *http.Request, status int) {
	var body []byte
	switch status {
	case http.StatusNotFound:
		body = h.opts.NotFoundPage
	case http.StatusGone:
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
	if pinned {
		hdr.Set("Cache-Control", h.immutable)
	} else {
		hdr.Set("Cache-Control", cacheLatest)
	}
}

func (h *handler) setErrorHeaders(w http.ResponseWriter, p *artifact.Published) {
	h.setCommonHeaders(w, p, false)
	w.Header().Set("X-Robots-Tag", "noindex")
}

func (h *handler) csp(p *artifact.Published) string {
	if p != nil && p.CSP != "" && h.opts.AllowCSPOverride {
		return p.CSP
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
