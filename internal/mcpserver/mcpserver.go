// Package mcpserver is Kazari's MCP surface over stateless Streamable HTTP. Each
// request builds a fresh server closed over its principal, so no session state exists.
package mcpserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/buildinfo"
	"github.com/PeterBooker/kazari/internal/fetch"
	"github.com/PeterBooker/kazari/internal/instrument"
	"github.com/PeterBooker/kazari/internal/middleware"
	"github.com/PeterBooker/kazari/internal/present"
	"github.com/PeterBooker/kazari/internal/ratelimit"
	"github.com/PeterBooker/kazari/internal/upload"
)

const (
	DefaultInlineGuidanceBytes = 2 << 20
	DefaultMaxInlineReadBytes  = 1 << 20
	// MaxInlineBlobBytes is the largest binary read_file embeds; larger ones get a download URL.
	MaxInlineBlobBytes = 256 << 10
)

// bodyOverhead leaves room for the JSON-RPC envelope and non-content arguments.
const bodyOverhead = 1 << 20

// Deps are the services the MCP server calls.
type Deps struct {
	Artifacts *artifact.Service
	Uploads   *upload.Service
	Auth      auth.Authenticator
	Links     present.Links
	Logger    *slog.Logger
	// Writes limits write and delete calls per credential; it is the user API's
	// limiter, so a key has one budget on both. Nil disables it.
	Writes *ratelimit.Limiter
	// Fetch downloads files named by url; nil refuses them.
	Fetch *fetch.Client
}

// Options configures the server. Its limits only describe the services' limits to
// the model, so set them to the same values.
type Options struct {
	// BaseDomain is the artifact domain, e.g. art.example.com, shown in descriptions.
	BaseDomain string
	// ResourceMetadataURL is advertised on 401 responses, e.g.
	// https://mcp.<base>/.well-known/oauth-protected-resource.
	ResourceMetadataURL string
	// AllowedOrigins are accepted besides an absent Origin and https://<request host>.
	AllowedOrigins []string
	// InlineGuidanceBytes is the size above which descriptions advise begin_upload.
	InlineGuidanceBytes int64
	MaxVersionBytes     int64
	MaxFileBytes        int64
	MaxFiles            int
	MaxChunkBytes       int64
	MaxTotalBytes       int64
	CSP                 string
	ReservedSlugs       []string
	// MaxInlineReadBytes is the largest text file read_file returns inline and the
	// largest file of any kind the file resource returns.
	MaxInlineReadBytes int64
	// APIBaseURL is the REST API root for download links, e.g. https://api.<base>/v1.
	APIBaseURL string
	// MCPURL is the address the skill gives for connecting, e.g. https://mcp.<base>/; optional.
	MCPURL string
	// DefaultVisibility is the visibility of artifacts created without
	// one (default public); MaxShareLinkTTL the longest a share link may
	// live (default artifact.DefaultMaxShareLinkTTL).
	DefaultVisibility string
	MaxShareLinkTTL   time.Duration
	// Fetch says the instance fetches URLs: files[].url is described and
	// import_page is offered.
	Fetch bool
	// Now is the clock that decides whether a share link is active
	// (default time.Now).
	Now       func() time.Time
	Telemetry instrument.Providers
}

func (o Options) withDefaults(d Deps) Options {
	if o.InlineGuidanceBytes <= 0 {
		o.InlineGuidanceBytes = DefaultInlineGuidanceBytes
	}
	if o.MaxVersionBytes <= 0 {
		o.MaxVersionBytes = artifact.DefaultMaxVersionBytes
	}
	if o.MaxFileBytes <= 0 {
		o.MaxFileBytes = artifact.DefaultMaxFileBytes
	}
	if o.MaxFiles <= 0 {
		o.MaxFiles = artifact.DefaultMaxFiles
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
	if o.MaxInlineReadBytes <= 0 {
		o.MaxInlineReadBytes = DefaultMaxInlineReadBytes
	}
	o.APIBaseURL = strings.TrimSuffix(o.APIBaseURL, "/")
	if o.DefaultVisibility == "" {
		o.DefaultVisibility = string(artifact.Public)
	}
	if o.MaxShareLinkTTL <= 0 {
		o.MaxShareLinkTTL = artifact.DefaultMaxShareLinkTTL
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// builder holds what every per-request server shares.
type builder struct {
	d          Deps
	o          Options
	log        *slog.Logger
	sdkLog     *slog.Logger // warnings only: the SDK logs every stateless connect at info
	text       texts
	schema     *mcp.SchemaCache
	editSchema *jsonschema.Schema
	preview    string
	previewUI  map[string]any
}

func newBuilder(d Deps, o Options) *builder {
	o = o.withDefaults(d)
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	text := newTexts(d.Links, o)
	preview, ui := newPreview(text, o.CSP)
	return &builder{
		d:          d,
		o:          o,
		log:        log,
		sdkLog:     slog.New(minLevel{min: slog.LevelWarn, Handler: log.Handler()}),
		text:       text,
		schema:     mcp.NewSchemaCache(),
		editSchema: editFileSchema(),
		preview:    preview,
		previewUI:  ui,
	}
}

// NewServer builds an MCP server whose handlers act as p.
func NewServer(d Deps, o Options, p auth.Principal) *mcp.Server {
	return newBuilder(d, o).server(p)
}

func (b *builder) server(p auth.Principal) *mcp.Server {
	caps := &mcp.ServerCapabilities{}
	caps.AddExtension(skillsExtension, nil)
	caps.AddExtension(appsExtension, map[string]any{"mimeTypes": []string{appMIME}})
	s := mcp.NewServer(
		&mcp.Implementation{Name: "kazari", Title: "Kazari", Version: buildinfo.Get().Version},
		&mcp.ServerOptions{
			Instructions: b.text.instructions,
			Logger:       b.sdkLog,
			Capabilities: caps,
			SchemaCache:  b.schema,
		},
	)
	b.addTools(s, p)
	b.addResources(s, p)
	b.addPrompt(s)
	b.addSkill(s, p)
	b.addPreview(s)
	return s
}

// NewHandler returns the handler for the MCP endpoint. It checks Origin and
// MCP-Protocol-Version before bearer authentication, then builds one server per request.
func NewHandler(d Deps, o Options) http.Handler {
	b := newBuilder(d, o)
	stream := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		p, ok := auth.FromContext(r.Context())
		if !ok {
			return nil // the SDK answers 400; RequireBearer makes this unreachable
		}
		return b.server(p)
	}, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: false,
		Logger:       b.sdkLog,
		// checkOrigin replaces the SDK's localhost protection, which breaks local reverse proxies.
		DisableLocalhostProtection: true,
		MaxRequestBodyBytes:        b.bodyLimit(),
	})
	authed := auth.RequireBearer(d.Auth, auth.Options{ResourceMetadataURL: b.o.ResourceMetadataURL, Telemetry: b.o.Telemetry})(stream)
	return b.checkOrigin(checkProtocolVersion(authed))
}

// bodyLimit fits a whole version or a full chunk in base64, whichever is larger;
// the services enforce the real limits.
func (b *builder) bodyLimit() int64 {
	n := max(b.o.MaxVersionBytes, b.o.MaxChunkBytes)
	return (n+2)/3*4 + bodyOverhead
}

// checkOrigin rejects a foreign Origin header, defending against DNS rebinding.
func (b *builder) checkOrigin(next http.Handler) http.Handler {
	allowed := make([]string, 0, len(b.o.AllowedOrigins))
	for _, o := range b.o.AllowedOrigins {
		allowed = append(allowed, normalizeOrigin(o))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			o := normalizeOrigin(origin)
			if o != normalizeOrigin("https://"+r.Host) && !slices.Contains(allowed, o) {
				writeError(w, r, http.StatusForbidden, api.CodeForbidden, "origin "+origin+" is not allowed")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func normalizeOrigin(o string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(o), "/"))
}

func checkProtocolVersion(next http.Handler) http.Handler {
	supported := mcp.SupportedProtocolVersions()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := r.Header.Get("MCP-Protocol-Version"); v != "" && !slices.Contains(supported, v) {
			writeError(w, r, http.StatusBadRequest, api.CodeInvalidArgument,
				"unsupported MCP-Protocol-Version "+v+"; supported: "+strings.Join(supported, ", "))
			return
		}
		next.ServeHTTP(w, r)
	})
}

type minLevel struct {
	min slog.Level
	slog.Handler
}

func (h minLevel) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= h.min && h.Handler.Enabled(ctx, l)
}

func (h minLevel) WithAttrs(attrs []slog.Attr) slog.Handler {
	return minLevel{min: h.min, Handler: h.Handler.WithAttrs(attrs)}
}

func (h minLevel) WithGroup(name string) slog.Handler {
	return minLevel{min: h.min, Handler: h.Handler.WithGroup(name)}
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	middleware.SetErrorCode(r.Context(), code)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(api.ErrorResponse{Error: api.Error{Code: code, Message: msg}})
}
