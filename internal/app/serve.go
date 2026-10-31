package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/PeterBooker/kazari/internal/buildinfo"
	"github.com/PeterBooker/kazari/internal/certs"
	"github.com/PeterBooker/kazari/internal/instrument"
	"github.com/PeterBooker/kazari/internal/middleware"
)

// readTimeout and writeTimeout stop a stalled client holding a connection
// for ever, yet allow a full-size upload or download at well under 1 Mbit/s.
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 10 * time.Minute
	writeTimeout      = 15 * time.Minute
	idleTimeout       = 120 * time.Second
	maxHeaderBytes    = 64 << 10
	shutdownTimeout   = 10 * time.Second
)

type server struct {
	name string
	srv  *http.Server
	ln   net.Listener
	tls  bool
}

// Run serves until ctx is done, then shuts down gracefully, closes the
// database and returns nil. A bind or server failure ends it with that
// error. Call it at most once.
func (a *App) Run(ctx context.Context) (err error) {
	defer func() {
		if cerr := a.Close(); err == nil {
			err = cerr
		}
	}()

	servers, err := a.listen()
	if err != nil {
		return err
	}
	close(a.started)
	addrs := a.Addrs()
	a.log.Info("kazari started",
		"version", buildinfo.Get().Version,
		"base_domain", a.cfg.BaseDomain,
		"mode", a.mode,
		"https", addrs.HTTPS,
		"http", addrs.HTTP,
		"admin", addrs.Admin,
		"socket", addrs.Socket,
		"oidc", a.oidcOn,
	)
	a.log.Info("serving",
		"pages", a.cfg.ExternalURL("<slug>"),
		"api", a.cfg.ExternalURL("api"),
		"mcp", a.cfg.ExternalURL("mcp"),
		"admin", a.cfg.ExternalURL("admin"),
	)
	a.firstRunHint(context.WithoutCancel(ctx))

	// Background work outlives the servers' shutdown so in-flight requests
	// finish against a working system.
	bg, stopBG := context.WithCancel(context.WithoutCancel(ctx))
	defer stopBG()

	g, gctx := errgroup.WithContext(ctx)
	for _, s := range servers {
		g.Go(func() error {
			var err error
			if s.tls {
				err = s.srv.ServeTLS(s.ln, "", "")
			} else {
				err = s.srv.Serve(s.ln)
			}
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return fmt.Errorf("app: %s server: %w", s.name, err)
		})
	}
	g.Go(func() error { return a.sched.Run(bg) })
	if a.certs != nil {
		g.Go(func() error {
			if err := a.certs.Run(bg); err != nil {
				return fmt.Errorf("app: certificates: %w", err)
			}
			return nil
		})
	}
	g.Go(func() error {
		<-gctx.Done()
		defer stopBG()
		return shutdown(context.WithoutCancel(ctx), servers)
	})

	err = g.Wait()
	if err != nil {
		a.log.Error("kazari stopped", "err", err)
	} else {
		a.log.Info("kazari stopped")
	}
	return err
}

// firstRunHint tells an operator watching the log of a fresh instance how
// to create the first admin, over the socket on this host or with the admin
// token from anywhere.
func (a *App) firstRunHint(ctx context.Context) {
	users, err := a.users.ListUsers(ctx)
	if err != nil || len(users) > 0 {
		return
	}
	how := "set KAZARI_AUTH_ADMIN_TOKEN and run it with KAZARI_ADMIN_URL=" + strings.TrimSuffix(a.cfg.ExternalURL("admin"), "/")
	if socket := a.Addrs().Socket; socket != "" {
		how = "run it on this host (as the service user or root, or with docker exec), which uses the admin socket " + socket
	}
	a.log.WarnContext(ctx, "no users yet", "next", "kazari admin users create <handle> --role admin, then kazari admin keys issue <handle>", "how", how)
}

// listen closes whatever it bound if it fails.
func (a *App) listen() (servers []server, err error) {
	defer func() {
		if err != nil {
			for _, s := range servers {
				s.ln.Close()
			}
		}
	}()
	bind := func(name, addr string, h http.Handler, tlsConfig *tls.Config) (string, error) {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return "", fmt.Errorf("app: listen %s on %s: %w", name, addr, err)
		}
		srv := a.newServer(h)
		srv.TLSConfig = tlsConfig
		servers = append(servers, server{name: name, srv: srv, ln: ln, tls: tlsConfig != nil})
		return ln.Addr().String(), nil
	}

	var addrs Addrs
	if a.cfg.Listen.ProxyMode {
		if addrs.HTTP, err = bind("http", a.cfg.Listen.HTTP, a.router, nil); err != nil {
			return servers, err
		}
	} else {
		if addrs.HTTPS, err = bind("https", a.cfg.Listen.HTTPS, a.router, certs.TLSConfig(a.certs)); err != nil {
			return servers, err
		}
		if a.cfg.Listen.HTTP != "" {
			redirect := middleware.Chain(redirectHandler(a.cfg),
				middleware.RequestID(), middleware.RealIP(false), a.obs.Observe(instrument.SurfaceRedirect, middleware.ObserveOptions{NoTraces: true}), middleware.Logger(a.log, probePaths...), middleware.Recover(a.log))
			if addrs.HTTP, err = bind("http", a.cfg.Listen.HTTP, redirect, nil); err != nil {
				return servers, err
			}
		}
	}
	if a.cfg.Admin.Listen != "" {
		if addrs.Admin, err = bind("admin", a.cfg.Admin.Listen, a.admin, nil); err != nil {
			return servers, err
		}
	}
	if path := a.cfg.AdminSocketPath(); path != "" {
		if addrs.Socket, err = a.bindSocket(path, &servers); err != nil {
			return servers, err
		}
	}

	a.mu.Lock()
	a.addrs = addrs
	a.mu.Unlock()
	return servers, nil
}

// bindSocket serves the credential-free admin API on the Unix socket at
// path, readable and writable by the service user alone. A socket file a
// previous run left behind is replaced; the listener removes its own on
// close.
func (a *App) bindSocket(path string, servers *[]server) (string, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("app: remove stale admin socket %s: %w", path, err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return "", fmt.Errorf("app: listen admin socket on %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return "", fmt.Errorf("app: admin socket %s: %w", path, err)
	}
	*servers = append(*servers, server{name: "admin socket", srv: a.newServer(a.adminLocal), ln: ln})
	return path, nil
}

func (a *App) newServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          log.New(errorLogWriter{a.log}, "", 0),
	}
}

func shutdown(ctx context.Context, servers []server) error {
	ctx, cancel := context.WithTimeout(ctx, shutdownTimeout)
	defer cancel()
	errs := make(chan error, len(servers))
	for _, s := range servers {
		go func() {
			err := s.srv.Shutdown(ctx)
			if err != nil {
				s.srv.Close()
				err = fmt.Errorf("app: shut down %s server: %w", s.name, err)
			}
			errs <- err
		}()
	}
	var all []error
	for range servers {
		all = append(all, <-errs)
	}
	return errors.Join(all...)
}

// errorLogWriter logs TLS handshake failures, routine on a public listener,
// at debug.
type errorLogWriter struct{ log *slog.Logger }

func (w errorLogWriter) Write(p []byte) (int, error) {
	msg := strings.TrimSpace(string(p))
	level := slog.LevelWarn
	if strings.Contains(msg, "TLS handshake error") {
		level = slog.LevelDebug
	}
	w.log.Log(context.Background(), level, "http server", "error", msg)
	return len(p), nil
}
