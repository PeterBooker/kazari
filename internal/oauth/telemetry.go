package oauth

import (
	"context"
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/PeterBooker/kazari/internal/instrument"
)

const scope = "github.com/PeterBooker/kazari/internal/oauth"

const (
	grantTypeKey          = attribute.Key("kazari.oauth.grant_type")
	registrationMethodKey = attribute.Key("kazari.oauth.registration.method")
	signInMethodKey       = attribute.Key("kazari.sign_in.method")
	signInResultKey       = attribute.Key("kazari.sign_in.result")
)

// Sign-in methods and results.
const (
	viaOIDC           = "oidc"
	viaAPIKey         = "api_key"
	signInOK          = "ok"
	signInNotAdmitted = "not_admitted"
	signInFailed      = "failed"
)

// errorCodes are the RFC 6749 and RFC 7591 error codes Kazari answers with,
// the bounded set of error.type.
var errorCodes = []string{
	"invalid_request", "invalid_client", "invalid_grant", "unauthorized_client", "unsupported_grant_type",
	"invalid_scope", "invalid_target", "server_error", "invalid_client_metadata", "invalid_redirect_uri",
}

type metrics struct {
	tokens        metric.Int64Counter
	registrations metric.Int64Counter
	signIns       metric.Int64Counter
}

func newMetrics(m metric.Meter) metrics {
	counter := func(name, unit, desc string) metric.Int64Counter {
		return instrument.Handle(m.Int64Counter(name, metric.WithUnit(unit), metric.WithDescription(desc)))
	}
	return metrics{
		tokens:        counter("kazari.oauth.token.requests", "{request}", "Requests to the token endpoint by grant type."),
		registrations: counter("kazari.oauth.client.registrations", "{registration}", "OAuth clients registered, by DCR or a client ID metadata document."),
		signIns:       counter("kazari.user.sign_ins", "{sign_in}", "Sign-ins by method and result."),
	}
}

// outcome is the OAuth error a handler answered with, for its metric.
type outcome struct{ code string }

type outcomeKey struct{}

// handle starts a handler's span and gives its request an outcome.
func (s *Service) handle(r *http.Request, name string) (*http.Request, *outcome, trace.Span) {
	ctx, span := instrument.StartChild(r.Context(), s.tracer, name)
	o := &outcome{}
	return r.WithContext(context.WithValue(ctx, outcomeKey{}, o)), o, span
}

// endHandle marks the span failed on a server error only: the other codes
// describe the client's request.
func endHandle(span trace.Span, o *outcome) {
	if o.code != "" && span.IsRecording() {
		span.SetAttributes(semconv.ErrorTypeKey.String(instrument.OneOf(o.code, errorCodes...)))
		if o.code == "server_error" {
			span.SetStatus(codes.Error, o.code)
		}
	}
	span.End()
}

func (o *outcome) attrs(kv ...attribute.KeyValue) metric.AddOption {
	if o.code != "" {
		kv = append(kv, semconv.ErrorTypeKey.String(instrument.OneOf(o.code, errorCodes...)))
	}
	return metric.WithAttributes(kv...)
}

func (s *Service) signedIn(ctx context.Context, method, result string) {
	s.m.signIns.Add(ctx, 1, metric.WithAttributes(signInMethodKey.String(method), signInResultKey.String(result)))
	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.SetAttributes(signInMethodKey.String(method), signInResultKey.String(result))
	}
}

// keySignIn counts a sign-in with an API key; a refused key is failed.
func (s *Service) keySignIn(ctx context.Context, err error) {
	result := signInOK
	if err != nil {
		result = signInFailed
	}
	s.signedIn(ctx, viaAPIKey, result)
}
