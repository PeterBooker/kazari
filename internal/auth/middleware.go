package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/PeterBooker/kazari/internal/errcode"
	"github.com/PeterBooker/kazari/internal/instrument"
	"github.com/PeterBooker/kazari/internal/middleware"
)

const scope = "github.com/PeterBooker/kazari/internal/auth"

// Options tune the middleware.
type Options struct {
	// ResourceMetadataURL, if set, is advertised as resource_metadata on 401s
	// (RFC 9728) so MCP clients can discover the authorization server.
	ResourceMetadataURL string
	Telemetry           instrument.Providers
}

// Results of an authentication, the values of kazari.auth.result. A refused
// token is invalid whether unknown, expired or revoked: the authenticators
// cannot tell those apart.
const (
	resultOK        = "ok"
	resultMissing   = "missing"
	resultInvalid   = "invalid"
	resultForbidden = "forbidden"
)

// Results of an authentication, for a Sessions caller.
const (
	ResultOK        = resultOK
	ResultMissing   = resultMissing
	ResultInvalid   = resultInvalid
	ResultForbidden = resultForbidden
)

// Sessions counts the authentications of browser sessions, which pass
// through no bearer middleware: the hosts that take a session cookie, the
// dashboard and the account pages, report each request's outcome here, so
// kazari.auth.requests covers every credential.
type Sessions struct{ rec recorder }

// NewSessions returns a Sessions recording to p.
func NewSessions(p instrument.Providers) Sessions { return Sessions{newRecorder(p)} }

// Record counts one request by its result: ResultOK with the principal the
// session stands for, ResultMissing without a cookie, ResultInvalid for a
// session that has ended, ResultForbidden for a person the host refuses.
func (s Sessions) Record(ctx context.Context, result string, p *Principal) {
	if result != ResultOK {
		p = nil
	}
	s.rec.record(ctx, SourceSession, result, p)
}

type recorder struct{ requests metric.Int64Counter }

func newRecorder(p instrument.Providers) recorder {
	return recorder{requests: instrument.Handle(p.Meter(scope).Int64Counter("kazari.auth.requests",
		metric.WithDescription("Authentications by credential type and result."),
		metric.WithUnit("{request}")))}
}

// record counts one authentication and, once it succeeded, names the
// credential's type and OAuth client on the request's span. Never the handle
// or the credential's id: who did what stays in the audit log.
func (rec recorder) record(ctx context.Context, source, result string, p *Principal) {
	source = instrument.OneOf(source, SourceAPIKey, SourceOAuth, SourceAdminToken, SourceLocal, SourceSession)
	rec.requests.Add(ctx, 1, metric.WithAttributes(
		instrument.Surface.String(instrument.SurfaceFrom(ctx)),
		instrument.CredentialType.String(source),
		instrument.AuthResult.String(result),
	))
	if span := trace.SpanFromContext(ctx); p != nil && span.IsRecording() {
		attrs := []attribute.KeyValue{instrument.CredentialType.String(source)}
		if p.ClientID != "" {
			attrs = append(attrs, instrument.OAuthClientID.String(p.ClientID))
		}
		span.SetAttributes(attrs...)
	}
}

// RequireBearer authenticates the bearer token with a and puts the Principal
// in the request context. Missing or invalid credentials get a JSON 401.
func RequireBearer(a Authenticator, opts Options) func(http.Handler) http.Handler {
	rec := newRecorder(opts.Telemetry)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := BearerToken(r)
			if !ok {
				rec.record(r.Context(), "", resultMissing, nil)
				unauthorized(w, r, opts, "missing bearer token")
				return
			}
			p, err := a.Authenticate(r.Context(), token)
			if err != nil {
				rec.record(r.Context(), CredentialType(a, token), resultInvalid, nil)
				unauthorized(w, r, opts, "invalid or expired token")
				return
			}
			rec.record(r.Context(), p.Source, resultOK, &p)
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

// RequireScope rejects principals without scope (admins pass). It must run
// after RequireBearer or RequireAdmin.
func RequireScope(scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := FromContext(r.Context())
			if !ok {
				unauthorized(w, r, Options{}, "missing bearer token")
				return
			}
			if !p.HasScope(scope) {
				writeError(w, r, http.StatusForbidden, errcode.Forbidden, fmt.Sprintf("scope %q required", scope))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireAdmin accepts the admin token (compared in constant time) or any
// credential fallback resolves to an admin principal. An empty token disables
// the token path.
func RequireAdmin(token string, fallback Authenticator, opts Options) func(http.Handler) http.Handler {
	var want [sha256.Size]byte
	if token != "" {
		want = sha256.Sum256([]byte(token))
	}
	rec := newRecorder(opts.Telemetry)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t, ok := BearerToken(r)
			if !ok {
				rec.record(r.Context(), "", resultMissing, nil)
				unauthorized(w, r, opts, "missing bearer token")
				return
			}
			if token != "" {
				got := sha256.Sum256([]byte(t))
				if subtle.ConstantTimeCompare(got[:], want[:]) == 1 {
					p := Principal{Handle: "admin", Admin: true, Role: "admin", Scopes: AllScopes, Source: SourceAdminToken}
					rec.record(r.Context(), p.Source, resultOK, &p)
					next.ServeHTTP(w, r.WithContext(ContextWithPrincipal(r.Context(), p)))
					return
				}
			}
			if fallback != nil {
				p, err := fallback.Authenticate(r.Context(), t)
				if err == nil {
					if !p.Admin || !slices.Contains(p.Scopes, ScopeAdmin) {
						rec.record(r.Context(), p.Source, resultForbidden, &p)
						writeError(w, r, http.StatusForbidden, errcode.Forbidden, "admin role required")
						return
					}
					rec.record(r.Context(), p.Source, resultOK, &p)
					next.ServeHTTP(w, r.WithContext(ContextWithPrincipal(r.Context(), p)))
					return
				}
				rec.record(r.Context(), CredentialType(fallback, t), resultInvalid, nil)
			} else {
				rec.record(r.Context(), "", resultInvalid, nil)
			}
			unauthorized(w, r, opts, "invalid or expired token")
		})
	}
}

// RequireToken accepts only the bearer token, compared in constant time, for
// an endpoint that serves a machine rather than a principal.
func RequireToken(token string) func(http.Handler) http.Handler {
	want := sha256.Sum256([]byte(token))
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t, _ := BearerToken(r)
			got := sha256.Sum256([]byte(t))
			if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
				unauthorized(w, r, Options{}, "invalid or missing bearer token")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// AsLocalAdmin treats every request as the admin principal, with
// SourceLocal. Only a listener whose transport is the credential, such as a
// Unix socket owned by the service user, may use it.
func AsLocalAdmin(opts Options) func(http.Handler) http.Handler {
	rec := newRecorder(opts.Telemetry)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := Principal{Handle: "admin", Admin: true, Role: "admin", Scopes: AllScopes, Source: SourceLocal}
			rec.record(r.Context(), p.Source, resultOK, &p)
			next.ServeHTTP(w, r.WithContext(ContextWithPrincipal(r.Context(), p)))
		})
	}
}

func unauthorized(w http.ResponseWriter, r *http.Request, opts Options, msg string) {
	challenge := `Bearer error="invalid_token"`
	if opts.ResourceMetadataURL != "" {
		challenge += fmt.Sprintf(`, resource_metadata=%q`, opts.ResourceMetadataURL)
	}
	w.Header().Set("WWW-Authenticate", challenge)
	writeError(w, r, http.StatusUnauthorized, errcode.Unauthorized, msg)
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	middleware.SetErrorCode(r.Context(), code)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"code": code, "message": msg},
	})
}
