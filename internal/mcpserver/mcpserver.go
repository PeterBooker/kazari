// Package mcpserver is the MCP surface of Agalma: twelve tools, two resource
// templates and one prompt over the Streamable HTTP transport, running
// stateless. Each request builds a fresh server whose tool handlers close
// over the authenticated principal, so no session state exists anywhere.
//
// Every tool calls the same artifact and upload services as the REST API and
// returns the same wire types from package api.
package mcpserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"agalma/internal/api"
	"agalma/internal/artifact"
	"agalma/internal/auth"
	"agalma/internal/buildinfo"
	"agalma/internal/present"
	"agalma/internal/ratelimit"
	"agalma/internal/upload"
)

// Defaults applied to zero Options fields.
const (
	DefaultInlineGuidanceBytes = 2 << 20
	DefaultMaxInlineReadBytes  = 1 << 20
	// MaxInlineBlobBytes is the largest binary file read_file returns as a
	// base64 blob; larger binaries get a download URL instead.
	MaxInlineBlobBytes = 256 << 10
)

// bodyOverhead is the room left in the request body limit for the JSON-RPC
// envelope and the non-content arguments of a call.
const bodyOverhead = 1 << 20

// Deps are the services the MCP server calls.
type Deps struct {
	Artifacts *artifact.Service
	Uploads   *upload.Service
	Auth      auth.Authenticator
	Links     present.Links
	Logger    *slog.Logger
	// Writes limits write and delete tool calls per credential. It is the
	// user API's limiter, so a key has one budget on both; nil disables it.
	Writes *ratelimit.Limiter
}

// Options configures the server. Limits are only described to the model
// here; the services enforce the real ones, so they should be set to the
// same values the services use.
type Options struct {
	// BaseDomain is the artifact domain, e.g. art.example.com, used to show
	// the URL shape https://<slug>.<base>/ in descriptions.
	BaseDomain string
	// ResourceMetadataURL is advertised on 401 responses, e.g.
	// https://mcp.<base>/.well-known/oauth-protected-resource.
	ResourceMetadataURL string
	// AllowedOrigins are Origin header values accepted besides an absent
	// header and the MCP host itself (https://<request host>).
	AllowedOrigins []string
	// InlineGuidanceBytes is the size above which descriptions tell the
	// model to switch to begin_upload. Default 2 MiB.
	InlineGuidanceBytes int64
	MaxVersionBytes     int64
	MaxFileBytes        int64
	MaxFiles            int
	MaxChunkBytes       int64
	MaxTotalBytes       int64
	// CSP is the instance default Content-Security-Policy.
	CSP           string
	ReservedSlugs []string
	// MaxInlineReadBytes is the largest text file read_file returns inline
	// (default 1 MiB), and the largest file of any kind the file resource
	// returns.
	MaxInlineReadBytes int64
	// APIBaseURL is the REST API root, e.g. https://api.<base>/v1, used
	// for read_file download links.
	APIBaseURL string
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
	return o
}

// builder holds what every per-request server shares: the dependencies,
// the options, the generated texts and the schema cache.
type builder struct {
	d      Deps
	o      Options
	log    *slog.Logger
	sdkLog *slog.Logger // log filtered to warnings: the SDK logs every stateless connect at info
	text   texts
	schema *mcp.SchemaCache
}

func newBuilder(d Deps, o Options) *builder {
	o = o.withDefaults(d)
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	return &builder{
		d:      d,
		o:      o,
		log:    log,
		sdkLog: slog.New(minLevel{min: slog.LevelWarn, Handler: log.Handler()}),
		text:   newTexts(d.Links, o),
		schema: mcp.NewSchemaCache(),
	}
}

// NewServer builds an MCP server whose handlers act as p. NewHandler calls
// it once per request; tests use it with in-memory transports.
func NewServer(d Deps, o Options, p auth.Principal) *mcp.Server {
	return newBuilder(d, o).server(p)
}

func (b *builder) server(p auth.Principal) *mcp.Server {
	s := mcp.NewServer(
		&mcp.Implementation{Name: "agalma", Title: "Agalma", Version: buildinfo.Get().Version},
		&mcp.ServerOptions{
			Instructions: b.text.instructions,
			Logger:       b.sdkLog,
			Capabilities: &mcp.ServerCapabilities{},
			SchemaCache:  b.schema,
		},
	)
	b.addTools(s, p)
	b.addResources(s, p)
	b.addPrompt(s)
	return s
}

// NewHandler returns the HTTP handler for the MCP endpoint ("/" on the mcp
// host): Origin check, MCP-Protocol-Version check, bearer authentication,
// then the stateless Streamable HTTP transport with one server per request.
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
		// The Origin check below replaces the SDK's localhost protection,
		// which would reject every request behind a local reverse proxy.
		DisableLocalhostProtection: true,
		MaxRequestBodyBytes:        b.bodyLimit(),
	})
	authed := auth.RequireBearer(d.Auth, auth.BearerOptions{ResourceMetadataURL: b.o.ResourceMetadataURL})(stream)
	return b.checkOrigin(checkProtocolVersion(authed))
}

// bodyLimit lets one request carry a whole version inline in base64 or one
// full chunk, whichever is larger; the services enforce the real limits.
func (b *builder) bodyLimit() int64 {
	n := max(b.o.MaxVersionBytes, b.o.MaxChunkBytes)
	return (n+2)/3*4 + bodyOverhead
}

// checkOrigin rejects requests whose Origin header is present and is
// neither the MCP host itself nor in AllowedOrigins (DNS rebinding defence).
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
				writeError(w, http.StatusForbidden, api.CodeForbidden, "origin "+origin+" is not allowed")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func normalizeOrigin(o string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(o), "/"))
}

// checkProtocolVersion rejects an MCP-Protocol-Version header naming a
// version the SDK does not speak.
func checkProtocolVersion(next http.Handler) http.Handler {
	supported := mcp.SupportedProtocolVersions()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := r.Header.Get("MCP-Protocol-Version"); v != "" && !slices.Contains(supported, v) {
			writeError(w, http.StatusBadRequest, api.CodeInvalidArgument,
				"unsupported MCP-Protocol-Version "+v+"; supported: "+strings.Join(supported, ", "))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// minLevel drops records below min.
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

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(api.ErrorResponse{Error: api.Error{Code: code, Message: msg}})
}
