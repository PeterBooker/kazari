package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"kazari/internal/buildinfo"
	"kazari/internal/certs"
	"kazari/internal/middleware"
)

// Server timeouts. readTimeout bounds reading a whole request and
// writeTimeout a whole response, so a client that stalls cannot hold a
// connection for ever; both leave room for a full-size upload or download
// at well under 1 Mbit/s.
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 10 * time.Minute
	writeTimeout      = 15 * time.Minute
	idleTimeout       = 120 * time.Second
	maxHeaderBytes    = 64 << 10
	shutdownTimeout   = 10 * time.Second
)

// server is one bound listener and the http.Server that serves it.
type server struct {
	name string
	srv  *http.Server
	ln   net.Listener
	tls  bool
}

// Run binds every configured listener, then serves them alongside the
// scheduler and the certificate source until ctx is done. It then shuts the
// servers down gracefully (10 s), stops the background work, closes the
// database and returns nil. A listener that cannot be bound or a server
// that fails ends Run with that error. Run must be called at most once.
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
		"oidc", a.oidcOn,
	)

	// The scheduler and certificate source outlive the servers' shutdown,
	// so in-flight requests finish against a working system.
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

// listen binds every configured listener and records the addresses. On
// failure it closes whatever it already bound.
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
				middleware.RequestID(), middleware.RealIP(false), middleware.Logger(a.log), middleware.Recover(a.log))
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

	a.mu.Lock()
	a.addrs = addrs
	a.mu.Unlock()
	return servers, nil
}

// newServer returns an http.Server with Kazari's limits and its error log
// bridged to slog.
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

// shutdown stops every server gracefully within shutdownTimeout and closes
// the ones that do not finish in time.
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

// errorLogWriter turns net/http's error log lines into slog records. TLS
// handshake failures, routine on a public listener, are logged at debug.
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
