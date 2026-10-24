package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// BearerOptions tunes RequireBearer.
type BearerOptions struct {
	// ResourceMetadataURL, when set, is advertised on 401 responses as
	// WWW-Authenticate: Bearer resource_metadata="..." (RFC 9728), which
	// MCP clients use to discover the authorization server.
	ResourceMetadataURL string
}

// RequireBearer authenticates the Authorization: Bearer header with a and
// places the Principal in the request context. Missing or invalid
// credentials get a 401 with a JSON error body.
func RequireBearer(a Authenticator, opts BearerOptions) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := BearerToken(r)
			if !ok {
				unauthorized(w, opts, "missing bearer token")
				return
			}
			p, err := a.Authenticate(r.Context(), token)
			if err != nil {
				unauthorized(w, opts, "invalid or expired token")
				return
			}
			next.ServeHTTP(w, r.WithContext(ContextWithPrincipal(r.Context(), p)))
		})
	}
}

// BearerToken extracts the token from the Authorization header.
func BearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if len(h) < 7 || !strings.EqualFold(h[:7], "Bearer ") {
		return "", false
	}
	t := strings.TrimSpace(h[7:])
	return t, t != ""
}

// RequireScope rejects principals without the scope (admins always pass).
// It must run after RequireBearer or RequireAdmin.
func RequireScope(scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := FromContext(r.Context())
			if !ok {
				unauthorized(w, BearerOptions{}, "missing bearer token")
				return
			}
			if !p.HasScope(scope) {
				writeError(w, http.StatusForbidden, "forbidden", fmt.Sprintf("scope %q required", scope))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireAdmin guards the admin host. It accepts the configured admin token
// (compared in constant time), or any credential that fallback resolves to
// an admin principal. An empty token disables the token path.
func RequireAdmin(token string, fallback Authenticator) func(http.Handler) http.Handler {
	var want [sha256.Size]byte
	if token != "" {
		want = sha256.Sum256([]byte(token))
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t, ok := BearerToken(r)
			if !ok {
				unauthorized(w, BearerOptions{}, "missing bearer token")
				return
			}
			if token != "" {
				got := sha256.Sum256([]byte(t))
				if subtle.ConstantTimeCompare(got[:], want[:]) == 1 {
					p := Principal{Handle: "admin", Admin: true, Scopes: AllScopes, Source: SourceAdminToken}
					next.ServeHTTP(w, r.WithContext(ContextWithPrincipal(r.Context(), p)))
					return
				}
			}
			if fallback != nil {
				p, err := fallback.Authenticate(r.Context(), t)
				if err == nil {
					if !p.Admin {
						writeError(w, http.StatusForbidden, "forbidden", "admin role required")
						return
					}
					next.ServeHTTP(w, r.WithContext(ContextWithPrincipal(r.Context(), p)))
					return
				}
			}
			unauthorized(w, BearerOptions{}, "invalid or expired token")
		})
	}
}

func unauthorized(w http.ResponseWriter, opts BearerOptions, msg string) {
	challenge := `Bearer error="invalid_token"`
	if opts.ResourceMetadataURL != "" {
		challenge += fmt.Sprintf(`, resource_metadata=%q`, opts.ResourceMetadataURL)
	}
	w.Header().Set("WWW-Authenticate", challenge)
	writeError(w, http.StatusUnauthorized, "unauthorized", msg)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"code": code, "message": msg},
	})
}
