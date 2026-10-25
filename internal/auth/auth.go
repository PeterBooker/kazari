// Package auth defines who is calling (Principal), how a credential becomes
// a Principal (Authenticator) and the HTTP middleware that enforces it.
package auth

import (
	"context"
	"errors"
	"slices"
)

const (
	ScopeRead   = "artifacts:read"
	ScopeWrite  = "artifacts:write"
	ScopeDelete = "artifacts:delete"
	ScopeAdmin  = "admin"
)

// AllScopes is every scope, in display order.
var AllScopes = []string{ScopeRead, ScopeWrite, ScopeDelete, ScopeAdmin}

var MemberScopes = []string{ScopeRead, ScopeWrite, ScopeDelete}

const (
	SourceAPIKey     = "api_key"
	SourceOAuth      = "oauth"
	SourceAdminToken = "admin_token"
)

// ErrInvalidToken is returned by an Authenticator that does not recognise
// the credential. Chain tries the next authenticator only on this error.
var ErrInvalidToken = errors.New("auth: invalid token")

// Principal is the authenticated caller.
type Principal struct {
	Handle     string
	Admin      bool
	Scopes     []string
	Source     string // SourceAPIKey | SourceOAuth | SourceAdminToken
	KeyID      string // key prefix or token id, for audit and rate limiting
	QuotaBytes int64  // owner quota, 0 = unlimited
}

// HasScope reports whether p holds scope; admins hold every scope.
func (p Principal) HasScope(scope string) bool {
	return p.Admin || slices.Contains(p.Scopes, scope)
}

// Authenticator turns a bearer token into a Principal.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (Principal, error)
}

// AuthenticatorFunc adapts a function to Authenticator.
type AuthenticatorFunc func(ctx context.Context, token string) (Principal, error)

func (f AuthenticatorFunc) Authenticate(ctx context.Context, token string) (Principal, error) {
	return f(ctx, token)
}

// Identity is what an upstream identity provider asserts about a person.
type Identity struct {
	Issuer        string
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
}

type chain []Authenticator

// Chain returns the first Principal its authenticators yield, in order. Any
// error other than ErrInvalidToken stops the chain.
func Chain(auths ...Authenticator) Authenticator {
	return chain(auths)
}

func (c chain) Authenticate(ctx context.Context, token string) (Principal, error) {
	for _, a := range c {
		p, err := a.Authenticate(ctx, token)
		if err == nil {
			return p, nil
		}
		if !errors.Is(err, ErrInvalidToken) {
			return Principal{}, err
		}
	}
	return Principal{}, ErrInvalidToken
}

type ctxKey struct{}

// ContextWithPrincipal returns ctx carrying p.
func ContextWithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext returns the Principal placed by the middleware.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}
