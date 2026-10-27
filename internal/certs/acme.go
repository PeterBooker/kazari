package certs

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sync"
	"sync/atomic"

	"github.com/caddyserver/certmagic"
)

// ACMEConfig configures an ACME source.
type ACMEConfig struct {
	Email string // account contact; optional
	// CA is "letsencrypt" (default), "letsencrypt-staging" or an ACME
	// directory URL.
	CA      string
	Domains []string // normally the base domain and its wildcard
	// StorageDir holds the ACME account, certificates and locks.
	StorageDir string
	// Sync makes Run obtain or renew every certificate before waiting, and
	// fail if that fails. Otherwise failures are only logged.
	Sync   bool
	Logger *slog.Logger // nil means slog.Default()
}

// ACME obtains and renews certificates by DNS-01, which wildcard
// certificates require.
type ACME struct {
	magic      *certmagic.Config
	cache      *certmagic.Cache
	stopOnce   sync.Once
	domains    []string
	manageSync bool
}

var _ Source = (*ACME)(nil)

// NewACME solves DNS-01 challenges through dns. It does no network I/O;
// certificates are obtained when Run starts.
func NewACME(cfg ACMEConfig, dns certmagic.DNSProvider) (*ACME, error) {
	if dns == nil {
		return nil, errors.New("certs: acme: a DNS provider is required")
	}
	if len(cfg.Domains) == 0 {
		return nil, errors.New("certs: acme: at least one domain is required")
	}
	if cfg.StorageDir == "" {
		return nil, errors.New("certs: acme: storage dir is required")
	}
	ca, err := caURL(cfg.CA)
	if err != nil {
		return nil, err
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	zl := newZapLogger(logger.With("component", "certmagic"))

	// The cache asks for each certificate's one config, which exists only
	// after the cache does, hence atomic.
	var current atomic.Pointer[certmagic.Config]
	cache := certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(certmagic.Certificate) (*certmagic.Config, error) {
			return current.Load(), nil
		},
		Logger: zl,
	})
	magic := certmagic.New(cache, certmagic.Config{
		Storage: &certmagic.FileStorage{Path: cfg.StorageDir},
		Logger:  zl,
		OnEvent: certEvents(logger),
	})
	issuer := certmagic.NewACMEIssuer(magic, certmagic.ACMEIssuer{
		CA:     ca,
		Email:  cfg.Email,
		Agreed: true,
		DNS01Solver: &certmagic.DNS01Solver{
			DNSManager: certmagic.DNSManager{DNSProvider: dns, Logger: zl},
		},
		Logger: zl,
	})
	magic.Issuers = []certmagic.Issuer{issuer}
	current.Store(magic)

	return &ACME{
		magic:      magic,
		cache:      cache,
		domains:    append([]string(nil), cfg.Domains...),
		manageSync: cfg.Sync,
	}, nil
}

// GetCertificate returns the managed certificate matching the handshake.
func (a *ACME) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	return a.magic.GetCertificate(hello)
}

// Run manages the domains until ctx is done, then stops CertMagic's
// maintenance. With ACMEConfig.Sync it returns an error if the initial
// obtain fails.
func (a *ACME) Run(ctx context.Context) error {
	defer a.stop()
	manage := a.magic.ManageAsync
	if a.manageSync {
		manage = a.magic.ManageSync
	}
	if err := manage(ctx, a.domains); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("certs: acme: manage %v: %w", a.domains, err)
	}
	<-ctx.Done()
	return nil
}

func (a *ACME) stop() {
	a.stopOnce.Do(a.cache.Stop)
}

// certEvents logs the outcome of each certificate order in plain words, so
// an operator watching the log after the first start learns whether the
// wildcard certificate arrived without reading CertMagic's own lines.
func certEvents(logger *slog.Logger) func(context.Context, string, map[string]any) error {
	return func(ctx context.Context, event string, data map[string]any) error {
		name, _ := data["identifier"].(string)
		renewal, _ := data["renewal"].(bool)
		switch event {
		case "cert_obtaining":
			logger.InfoContext(ctx, "obtaining certificate", "name", name, "renewal", renewal)
		case "cert_obtained":
			logger.InfoContext(ctx, "certificate obtained", "name", name, "renewal", renewal, "issuer", data["issuer"])
		case "cert_failed":
			logger.ErrorContext(ctx, "certificate not obtained", "name", name, "renewal", renewal, "error", data["error"],
				"hint", "check the DNS records and the DNS API token with kazari check; retries continue in the background")
		}
		return nil
	}
}

func caURL(ca string) (string, error) {
	switch ca {
	case "", "letsencrypt":
		return certmagic.LetsEncryptProductionCA, nil
	case "letsencrypt-staging":
		return certmagic.LetsEncryptStagingCA, nil
	}
	u, err := url.Parse(ca)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", fmt.Errorf("certs: acme: CA %q is not letsencrypt, letsencrypt-staging or a directory URL", ca)
	}
	return ca, nil
}
