package app

import (
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/PeterBooker/kazari/internal/account"
	"github.com/PeterBooker/kazari/internal/config"
	"github.com/PeterBooker/kazari/internal/httpapi"
	"github.com/PeterBooker/kazari/internal/instrument"
	"github.com/PeterBooker/kazari/internal/middleware"
	"github.com/PeterBooker/kazari/internal/theme"
)

type hosts struct {
	api, mcp, admin, apex, account, site http.Handler
}

func (a *App) routes(h hosts) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(a.cfg.Host(config.APIHost)+"/", h.api)
	mux.Handle(a.cfg.Host(config.MCPHost)+"/", h.mcp)
	mux.Handle(a.cfg.Host(config.AdminHost)+"/", h.admin)
	// The account pages share the apex's host and nothing else: they are a
	// surface of their own, traced and counted as control plane.
	mux.Handle(a.cfg.Host("")+"/account", h.account)
	mux.Handle(a.cfg.Host("")+"/account/", h.account)
	mux.Handle(a.cfg.Host("")+"/", h.apex)
	mux.Handle("/", h.site)
	if !a.cfg.Listen.ProxyMode {
		return normalizeHost(mux)
	}
	// A probe that targets the backend by its address, as load balancers
	// and container platforms do, names no host of the instance.
	return normalizeHost(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" && !a.underBase(r.Host) {
			h.apex.ServeHTTP(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	}))
}

// underBase reports whether host, with or without a port, is the base
// domain or a name under it.
func (a *App) underBase(host string) bool {
	name := hostname(host)
	return name == a.cfg.BaseDomain || strings.HasSuffix(name, "."+a.cfg.BaseDomain)
}

// probePaths are counted but never traced, and log at debug when they
// succeed: probes and scrapes arrive every few seconds and say nothing about
// what an agent did.
var probePaths = []string{"/health", "/v1/health", "/metrics"}

func (a *App) surface(name string, control bool, h http.Handler) http.Handler {
	observe := middleware.ObserveOptions{
		ControlPlane: name == instrument.SurfaceAPI || name == instrument.SurfaceMCP || name == instrument.SurfaceAdmin ||
			name == instrument.SurfaceAccount,
		Skip: probePaths,
	}
	// On an artifact host these are pages like any other.
	var probes []string
	if control {
		probes = probePaths
	}
	ipHeader := ""
	if a.cfg.Listen.ProxyMode {
		ipHeader = a.cfg.Listen.ClientIPHeader
	}
	mws := []func(http.Handler) http.Handler{
		middleware.RequestID(),
		middleware.RealIP(ipHeader),
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
func apexHandler(home http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", httpapi.Liveness)
	mux.Handle("GET /{$}", home)
	mux.HandleFunc("/", http.NotFound)
	return middleware.Routed(mux)
}

// frontPage answers a browser that opens the root of a host whose root is
// no page, the connector or the API address, with page; every other request
// goes on to next.
func frontPage(page, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" && middleware.Navigation(r) {
			page.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// apiHost serves the API with its front page and the theme that page loads.
func apiHost(page, api http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET "+account.APIAssetPrefix, theme.Handler(account.APIAssetPrefix))
	mux.Handle("/", frontPage(page, api))
	return mux
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
