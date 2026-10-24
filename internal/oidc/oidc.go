// Package oidc is the OpenID Connect relying party that the OAuth
// authorization server signs people in with. It implements
// oauth.IdentityProvider: it builds the provider's authorization URL and
// turns the callback code into a verified auth.Identity.
package oidc

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"agalma/internal/auth"
)

// DefaultScopes are requested when Config.Scopes is empty.
var DefaultScopes = []string{gooidc.ScopeOpenID, "email", "profile"}

// entraHost issues Microsoft Entra ID tokens. Entra single-tenant apps do
// not send email_verified, and the tenant's mail addresses are verified.
const entraHost = "login.microsoftonline.com"

// Config describes the upstream provider.
type Config struct {
	// Issuer is the provider's issuer URL; discovery reads
	// <Issuer>/.well-known/openid-configuration.
	Issuer       string
	ClientID     string
	ClientSecret string
	// RedirectURL is https://mcp.<base>/oauth/callback.
	RedirectURL string
	// Scopes default to DefaultScopes.
	Scopes []string
	// HTTPClient is used for discovery, JWKS and token requests (default
	// http.DefaultClient).
	HTTPClient *http.Client
	// InsecureSkipIssuerCheck accepts a discovery document and ID tokens
	// whose issuer differs from Issuer. For tests only.
	InsecureSkipIssuerCheck bool
}

// Provider is a discovered OpenID Connect provider.
type Provider struct {
	issuer   string
	oauth    oauth2.Config
	verifier *gooidc.IDTokenVerifier
	client   *http.Client
}

// New discovers the provider. It fails when the provider is unreachable or
// its discovery document names a different issuer.
func New(ctx context.Context, cfg Config) (*Provider, error) {
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
	if cfg.HTTPClient != nil {
		ctx = gooidc.ClientContext(ctx, cfg.HTTPClient)
	}
	if cfg.InsecureSkipIssuerCheck {
		ctx = gooidc.InsecureIssuerURLContext(ctx, cfg.Issuer)
	}
	p, err := gooidc.NewProvider(ctx, cfg.Issuer)
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
	}, nil
}

// Issuer returns the configured issuer URL.
func (p *Provider) Issuer() string { return p.issuer }

// AuthURL returns the provider's authorization URL for one sign-in.
// codeChallenge is the S256 PKCE challenge.
func (p *Provider) AuthURL(state, nonce, codeChallenge string) string {
	return p.oauth.AuthCodeURL(state,
		gooidc.Nonce(nonce),
		oauth2.SetAuthURLParam("code_challenge", codeChallenge),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
	)
}

// Exchange redeems the callback code with the PKCE verifier, verifies the
// ID token (signature against the provider's JWKS, issuer, audience,
// expiry) and its nonce, and maps its claims to an Identity.
func (p *Provider) Exchange(ctx context.Context, code, nonce, codeVerifier string) (auth.Identity, error) {
	if code == "" {
		return auth.Identity{}, errors.New("oidc: callback has no code")
	}
	if p.client != nil {
		ctx = context.WithValue(ctx, oauth2.HTTPClient, p.client)
	}
	tok, err := p.oauth.Exchange(ctx, code, oauth2.VerifierOption(codeVerifier))
	if err != nil {
		return auth.Identity{}, fmt.Errorf("oidc: exchange code: %w", err)
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok || raw == "" {
		return auth.Identity{}, errors.New("oidc: token response has no id_token")
	}
	idt, err := p.verifier.Verify(ctx, raw)
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

// claims are the ID token claims Agalma reads.
type claims struct {
	Email             string    `json:"email"`
	EmailVerified     *flexBool `json:"email_verified"`
	Name              string    `json:"name"`
	PreferredUsername string    `json:"preferred_username"`
}

// identity maps verified claims to an Identity. preferred_username stands
// in for a missing email when it looks like one (Entra). A missing
// email_verified counts as verified only for Entra.
func (p *Provider) identity(issuer, subject string, c claims) auth.Identity {
	id := auth.Identity{Issuer: issuer, Subject: subject, Email: c.Email, Name: c.Name}
	if id.Email == "" && strings.Contains(c.PreferredUsername, "@") {
		id.Email = c.PreferredUsername
	}
	switch {
	case c.EmailVerified != nil:
		id.EmailVerified = bool(*c.EmailVerified)
	default:
		u, err := url.Parse(issuer)
		id.EmailVerified = err == nil && strings.EqualFold(u.Hostname(), entraHost)
	}
	if id.Email == "" {
		id.EmailVerified = false
	}
	return id
}

// flexBool accepts a JSON boolean or the strings "true" and "false", which
// some providers send for email_verified.
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
