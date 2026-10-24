package app

import (
	"bytes"
	_ "embed"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"strconv"
	"strings"

	"agalma/internal/config"
	"agalma/internal/httpapi"
	"agalma/internal/metrics"
	"agalma/internal/middleware"
)

//go:embed apex.html
var apexPage string

// hosts are the fully wrapped handlers of each surface.
type hosts struct {
	api, mcp, admin, apex, site http.Handler
}

// routes returns the host router: api., mcp. and admin. hosts and the apex
// by host pattern, every other host (the artifacts) to the site handler.
// Host names are lower-cased and stripped of a trailing dot first.
func (a *App) routes(h hosts) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(a.cfg.Host("api")+"/", h.api)
	mux.Handle(a.cfg.Host("mcp")+"/", h.mcp)
	mux.Handle(a.cfg.Host("admin")+"/", h.admin)
	mux.Handle(a.cfg.Host("")+"/", h.apex)
	mux.Handle("/", h.site)
	return normalizeHost(mux)
}

// surface wraps a host's handler with the shared middleware stack and
// its metrics; control-plane hosts also get the security headers.
func (a *App) surface(m *metrics.Metrics, name string, control bool, h http.Handler) http.Handler {
	mws := []func(http.Handler) http.Handler{
		middleware.RequestID(),
		middleware.RealIP(a.cfg.Listen.ProxyMode),
		middleware.Logger(a.log),
		middleware.Recover(a.log),
		m.HTTP(name),
	}
	if control {
		mws = append(mws, middleware.SecurityHeaders())
	}
	return middleware.Chain(h, mws...)
}

// mcpMux serves the OAuth metadata and endpoints beside the MCP endpoint,
// which answers on every other path ("/" and "/mcp" alike).
func mcpMux(oauthH, mcpH http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/.well-known/", oauthH)
	mux.Handle("/oauth/", oauthH)
	mux.Handle("/", mcpH)
	return mux
}

// normalizeHost lower-cases r.Host and drops a trailing dot so that host
// patterns match API.Example.com and example.com. alike.
func normalizeHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if host := canonicalHost(r.Host); host != r.Host {
			r2 := new(http.Request)
			*r2 = *r
			r2.Host = host
			r = r2
		}
		next.ServeHTTP(w, r)
	})
}

// canonicalHost lower-cases host and removes a trailing dot from its name,
// keeping any port.
func canonicalHost(host string) string {
	host = strings.ToLower(host)
	if name, port, err := net.SplitHostPort(host); err == nil {
		return net.JoinHostPort(strings.TrimSuffix(name, "."), port)
	}
	return strings.TrimSuffix(host, ".")
}

// hostname returns host without its port.
func hostname(host string) string {
	if name, _, err := net.SplitHostPort(host); err == nil {
		return name
	}
	return host
}

// apexHandler serves the base domain: /health and a static page naming
// the instance at /; everything else is a plain 404. Neither discloses the
// version.
func apexHandler(cfg config.Config) (http.Handler, error) {
	t, err := template.New("apex").Parse(apexPage)
	if err != nil {
		return nil, fmt.Errorf("app: apex page: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, struct{ BaseDomain string }{cfg.BaseDomain}); err != nil {
		return nil, fmt.Errorf("app: apex page: %w", err)
	}
	page := buf.Bytes()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", httpapi.Liveness)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Length", strconv.Itoa(len(page)))
		if r.Method != http.MethodHead {
			w.Write(page)
		}
	})
	mux.HandleFunc("/", http.NotFound)
	return mux, nil
}

// redirectHandler is the plain-HTTP listener outside proxy mode: /health
// answers directly, every other request for the base domain or one of its
// subdomains is sent permanently to the same host and path over https.
func redirectHandler(cfg config.Config) http.Handler {
	port := ""
	if p := cfg.ExternalPort; cfg.ExternalScheme == "https" && p != 0 && p != 443 {
		port = ":" + strconv.Itoa(p)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", httpapi.Liveness)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		host := hostname(r.Host)
		if host != cfg.BaseDomain && !strings.HasSuffix(host, "."+cfg.BaseDomain) {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "https://"+host+port+r.URL.RequestURI(), http.StatusMovedPermanently)
	})
	return normalizeHost(mux)
}
