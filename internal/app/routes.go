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

	"github.com/PeterBooker/kazari/internal/config"
	"github.com/PeterBooker/kazari/internal/httpapi"
	"github.com/PeterBooker/kazari/internal/instrument"
	"github.com/PeterBooker/kazari/internal/middleware"
)

//go:embed apex.html
var apexPage string

type hosts struct {
	api, mcp, admin, apex, site http.Handler
}

func (a *App) routes(h hosts) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(a.cfg.Host("api")+"/", h.api)
	mux.Handle(a.cfg.Host("mcp")+"/", h.mcp)
	mux.Handle(a.cfg.Host("admin")+"/", h.admin)
	mux.Handle(a.cfg.Host("")+"/", h.apex)
	mux.Handle("/", h.site)
	return normalizeHost(mux)
}

// probePaths are counted but never traced, and log at debug when they
// succeed: probes and scrapes arrive every few seconds and say nothing about
// what an agent did.
var probePaths = []string{"/health", "/v1/health", "/metrics"}

func (a *App) surface(name string, control bool, h http.Handler) http.Handler {
	observe := middleware.ObserveOptions{
		ControlPlane: name == instrument.SurfaceAPI || name == instrument.SurfaceMCP || name == instrument.SurfaceAdmin,
		Skip:         probePaths,
	}
	// On an artifact host these are pages like any other.
	var probes []string
	if control {
		probes = probePaths
	}
	mws := []func(http.Handler) http.Handler{
		middleware.RequestID(),
		middleware.RealIP(a.cfg.Listen.ProxyMode),
		a.obs.Observe(name, observe),
		middleware.Logger(a.log, probes...),
		middleware.Recover(a.log),
	}
	if control {
		mws = append(mws, middleware.SecurityHeaders())
	}
	return middleware.Chain(h, mws...)
}

// mcpMux answers MCP on every non-OAuth path, "/" and "/mcp" alike.
func mcpMux(oauthH, mcpH http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/.well-known/", oauthH)
	mux.Handle("/oauth/", oauthH)
	mux.Handle("/", mcpH)
	return middleware.Routed(mux)
}

// normalizeHost makes host patterns match API.Example.com and example.com.
// alike.
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

func canonicalHost(host string) string {
	host = strings.ToLower(host)
	if name, port, err := net.SplitHostPort(host); err == nil {
		return net.JoinHostPort(strings.TrimSuffix(name, "."), port)
	}
	return strings.TrimSuffix(host, ".")
}

func hostname(host string) string {
	if name, _, err := net.SplitHostPort(host); err == nil {
		return name
	}
	return host
}

// apexHandler serves /health and the instance page; neither discloses the
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
	return middleware.Routed(mux), nil
}

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
