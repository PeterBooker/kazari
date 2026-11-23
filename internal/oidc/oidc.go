// Package oidc is the OpenID Connect relying party that the OAuth
// authorization server signs people in with, as an oauth.IdentityProvider.
package oidc

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/oauth2"

	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/errcode"
	"github.com/PeterBooker/kazari/internal/fetch"
	"github.com/PeterBooker/kazari/internal/instrument"
)

const scope = "github.com/PeterBooker/kazari/internal/oidc"

var DefaultScopes = []string{gooidc.ScopeOpenID, "email", "profile"}

// googleHost issues Google tokens. Google's email_verified only proves the
// person once received mail there, and a consumer account on a work address
// stays verified after they leave; so a non-Google address counts as
// verified only when the hd (hosted domain) claim equals its domain.
const googleHost = "accounts.google.com"

// googleConsumerDomains are Google's own domains, which carry no hd claim.
var googleConsumerDomains = []string{"gmail.com", "googlemail.com"}

// entraHost issues Microsoft Entra ID tokens. Single-tenant apps get no
// email_verified, but the tenant's mail addresses are verified.
const entraHost = "login.microsoftonline.com"

// Config describes the upstream provider.
type Config struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	// RedirectURL is https://mcp.<base>/oauth/callback.
	RedirectURL string
	// Scopes default to DefaultScopes.
	Scopes     []string
	HTTPClient *http.Client
	// InsecureSkipIssuerCheck accepts a discovery document and ID tokens of
	// another issuer. For tests only.
	InsecureSkipIssuerCheck bool
	Telemetry               instrument.Providers
	// Fetch records the requests to the provider; nil records nothing.
	Fetch *fetch.Observer
}

// Provider is a discovered OpenID Connect provider.
type Provider struct {
	issuer   string
	oauth    oauth2.Config
	verifier *gooidc.IDTokenVerifier
	client   *http.Client
	tracer   trace.Tracer
	fetch    *fetch.Observer
}

// New discovers the provider; it fails when the discovery document names a
// different issuer.
func New(ctx context.Context, cfg Config) (_ *Provider, err error) {
	switch {
	case cfg.Issuer == "":
		return nil, errors.New("oidc: issuer is required")
	case cfg.ClientID == "":
		return nil, errors.New("oidc: client id is required")
	case cfg.RedirectURL == "":
		return nil, errors.New("oidc: redirect URL is required")
	}
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = DefaultScopes
	}
	if cfg.HTTPClient == nil && cfg.Fetch != nil {
		cfg.HTTPClient = &http.Client{Transport: cfg.Fetch.Transport(http.DefaultTransport)}
	}
	if cfg.HTTPClient != nil {
		ctx = gooidc.ClientContext(ctx, cfg.HTTPClient)
	}
	if cfg.InsecureSkipIssuerCheck {
		ctx = gooidc.InsecureIssuerURLContext(ctx, cfg.Issuer)
	}
	tracer := cfg.Telemetry.Tracer(scope)
	ctx, span := instrument.StartChild(fetch.WithPurpose(ctx, fetch.PurposeOIDC), tracer, "oidc.discover")
	defer func() { instrument.End(span, err, "") }()
	start := time.Now()
	p, err := gooidc.NewProvider(ctx, cfg.Issuer)
	cfg.Fetch.Done(ctx, start, 0, errorType(err))
	if err != nil {
		return nil, fmt.Errorf("oidc: discover %s: %w", cfg.Issuer, err)
	}
	return &Provider{
		issuer: cfg.Issuer,
		oauth: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectURL,
			Endpoint:     p.Endpoint(),
			Scopes:       cfg.Scopes,
		},
		verifier: p.Verifier(&gooidc.Config{
			ClientID:        cfg.ClientID,
			SkipIssuerCheck: cfg.InsecureSkipIssuerCheck,
		}),
		client: cfg.HTTPClient,
		tracer: tracer,
		fetch:  cfg.Fetch,
	}, nil
}

// errorType is error.type of a failed request to the provider: the
// libraries' errors carry no status, only whether it timed out.
func errorType(err error) string {
	if err == nil {
		return ""
	}
	return fetch.ErrorType(err)
}

// Issuer returns the configured issuer URL.
func (p *Provider) Issuer() string { return p.issuer }

// AuthURL takes the S256 PKCE challenge.
func (p *Provider) AuthURL(state, nonce, codeChallenge string) string {
	return p.oauth.AuthCodeURL(state,
		gooidc.Nonce(nonce),
		oauth2.SetAuthURLParam("code_challenge", codeChallenge),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
	)
}

// Exchange redeems the callback code and verifies the ID token's signature,
// issuer, audience, expiry and nonce.
func (p *Provider) Exchange(ctx context.Context, code, nonce, codeVerifier string) (auth.Identity, error) {
	if code == "" {
		return auth.Identity{}, errors.New("oidc: callback has no code")
	}
	ctx = fetch.WithPurpose(ctx, fetch.PurposeOIDC)
	if p.client != nil {
		ctx = context.WithValue(ctx, oauth2.HTTPClient, p.client)
		ctx = gooidc.ClientContext(ctx, p.client)
	}
	tok, err := p.exchange(ctx, code, codeVerifier)
	if err != nil {
		return auth.Identity{}, fmt.Errorf("oidc: exchange code: %w", err)
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok || raw == "" {
		return auth.Identity{}, errors.New("oidc: token response has no id_token")
	}
	idt, err := p.verify(ctx, raw)
	if err != nil {
		return auth.Identity{}, fmt.Errorf("oidc: verify id token: %w", err)
	}
	if nonce == "" || subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(nonce)) != 1 {
		return auth.Identity{}, errors.New("oidc: id token nonce does not match")
	}
	var c claims
	if err := idt.Claims(&c); err != nil {
		return auth.Identity{}, fmt.Errorf("oidc: id token claims: %w", err)
	}
	return p.identity(idt.Issuer, idt.Subject, c), nil
}

func (p *Provider) exchange(ctx context.Context, code, codeVerifier string) (_ *oauth2.Token, err error) {
	ctx, span := instrument.StartChild(ctx, p.tracer, "oidc.exchange")
	defer func() { instrument.End(span, err, "") }()
	start := time.Now()
	tok, err := p.oauth.Exchange(ctx, code, oauth2.VerifierOption(codeVerifier))
	p.fetch.Done(ctx, start, 0, errorType(err))
	return tok, err
}

// verify checks the ID token; the keys it fetches, when not cached, show as
// a client span below it.
func (p *Provider) verify(ctx context.Context, raw string) (_ *gooidc.IDToken, err error) {
	ctx, span := instrument.StartChild(ctx, p.tracer, "oidc.verify")
	defer func() { instrument.End(span, err, errcode.Unauthorized) }()
	return p.verifier.Verify(ctx, raw)
}

type claims struct {
	Email             string    `json:"email"`
	EmailVerified     *flexBool `json:"email_verified"`
	Name              string    `json:"name"`
	PreferredUsername string    `json:"preferred_username"`
	HostedDomain      string    `json:"hd"`
}

// identity maps verified claims to an Identity. Only for Entra does an
// email-like preferred_username stand in for a missing email, and a missing
// email_verified count as verified: elsewhere preferred_username may be any
// string the person chose.
func (p *Provider) identity(issuer, subject string, c claims) auth.Identity {
	id := auth.Identity{Issuer: issuer, Subject: subject, Email: c.Email, Name: c.Name}
	entra := issuerHost(issuer, entraHost)
	if id.Email == "" && entra && strings.Contains(c.PreferredUsername, "@") {
		id.Email = c.PreferredUsername
	}
	switch {
	case c.EmailVerified != nil:
		id.EmailVerified = bool(*c.EmailVerified)
	default:
		id.EmailVerified = entra
	}
	if id.Email == "" {
		id.EmailVerified = false
	}
	if id.EmailVerified && issuerHost(issuer, googleHost) {
		_, domain, _ := strings.Cut(strings.ToLower(id.Email), "@")
		managed := c.HostedDomain != "" && strings.EqualFold(c.HostedDomain, domain)
		id.EmailVerified = managed || slices.Contains(googleConsumerDomains, domain)
	}
	return id
}

func issuerHost(issuer, host string) bool {
	u, err := url.Parse(issuer)
	return err == nil && strings.EqualFold(u.Hostname(), host)
}

// flexBool also accepts the strings "true" and "false", which some providers
// send for email_verified.
type flexBool bool

func (b *flexBool) UnmarshalJSON(data []byte) error {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	switch v := v.(type) {
	case bool:
		*b = flexBool(v)
	case string:
		*b = flexBool(strings.EqualFold(v, "true"))
	case nil:
		*b = false
	default:
		return fmt.Errorf("oidc: email_verified is %T, want a boolean", v)
	}
	return nil
}

// ErrUnavailable is returned while a Lazy provider has not been discovered.
var ErrUnavailable = errors.New("oidc: the identity provider has not been reached yet")

// Lazy is a Provider discovered once the provider answers, so the server
// starts, and serves its pages, while the provider is unreachable. Until
// Discover succeeds, Ready returns ErrUnavailable and the sign-in pages say
// so. It is safe for concurrent use.
type Lazy struct {
	cfg Config
	p   atomic.Pointer[Provider]

	mu      sync.Mutex // serialises discovery and guards lastErr
	lastErr error
}

// NewLazy checks cfg without any network I/O.
func NewLazy(cfg Config) (*Lazy, error) {
	switch {
	case cfg.Issuer == "":
		return nil, errors.New("oidc: issuer is required")
	case cfg.ClientID == "":
		return nil, errors.New("oidc: client id is required")
	case cfg.RedirectURL == "":
		return nil, errors.New("oidc: redirect URL is required")
	}
	return &Lazy{cfg: cfg}, nil
}

// Discover runs discovery unless it has succeeded already, and returns its
// error.
func (l *Lazy) Discover(ctx context.Context) error {
	if l.p.Load() != nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.p.Load() != nil {
		return nil
	}
	p, err := New(ctx, l.cfg)
	if err != nil {
		l.lastErr = err
		return err
	}
	l.lastErr = nil
	l.p.Store(p)
	return nil
}

// Ready returns nil once discovery has succeeded, else ErrUnavailable with
// the last discovery error.
func (l *Lazy) Ready() error {
	if l.p.Load() != nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lastErr == nil {
		return ErrUnavailable
	}
	return fmt.Errorf("%w: %w", ErrUnavailable, l.lastErr)
}

// Issuer returns the configured issuer URL.
func (l *Lazy) Issuer() string { return l.cfg.Issuer }

// AuthURL is Provider.AuthURL, or "" before discovery; callers check Ready
// first.
func (l *Lazy) AuthURL(state, nonce, codeChallenge string) string {
	p := l.p.Load()
	if p == nil {
		return ""
	}
	return p.AuthURL(state, nonce, codeChallenge)
}

// Exchange is Provider.Exchange, or ErrUnavailable before discovery.
func (l *Lazy) Exchange(ctx context.Context, code, nonce, codeVerifier string) (auth.Identity, error) {
	p := l.p.Load()
	if p == nil {
		return auth.Identity{}, ErrUnavailable
	}
	return p.Exchange(ctx, code, nonce, codeVerifier)
}
