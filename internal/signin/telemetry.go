package signin

import (
	"context"
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/instrument"
)

const scope = "github.com/PeterBooker/kazari/internal/signin"

const (
	signInMethodKey = attribute.Key("kazari.sign_in.method")
	signInResultKey = attribute.Key("kazari.sign_in.result")
)

// Results of a sign-in, in the sign-in metric.
const (
	resultOK          = "ok"
	resultNotAdmitted = "not_admitted"
	resultFailed      = "failed"
	resultDelayed     = "delayed" // refused unseen, too soon after failures
)

// signInCounter counts sign-ins by method and result.
type signInCounter struct{ c metric.Int64Counter }

func newSignInCounter(m metric.Meter) signInCounter {
	return signInCounter{instrument.Handle(m.Int64Counter("kazari.user.sign_ins",
		metric.WithUnit("{sign_in}"), metric.WithDescription("Sign-ins by method and result.")))}
}

// signedIn counts a sign-in and puts its method and result on the span.
func (s *Service) signedIn(ctx context.Context, method, result string) {
	s.signIns.c.Add(ctx, 1, metric.WithAttributes(signInMethodKey.String(method), signInResultKey.String(result)))
	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.SetAttributes(signInMethodKey.String(method), signInResultKey.String(result))
	}
}

// CheckKey authenticates an API key a person typed into a form, and counts
// it as a sign-in with a key, refused when it is not accepted.
func (s *Service) CheckKey(ctx context.Context, key string) (auth.Principal, error) {
	p, err := s.keys.Authenticate(ctx, key)
	result := resultOK
	if err != nil {
		result = resultFailed
	}
	s.signedIn(ctx, ViaAPIKey, result)
	return p, err
}

// handle starts a handler's span.
func (s *Service) handle(r *http.Request, name string) (*http.Request, trace.Span) {
	ctx, span := instrument.StartChild(r.Context(), s.tracer, name)
	return r.WithContext(ctx), span
}
