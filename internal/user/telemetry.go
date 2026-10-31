package user

import (
	"errors"

	"go.opentelemetry.io/otel/trace"

	"github.com/PeterBooker/kazari/internal/instrument"
)

const scope = "github.com/PeterBooker/kazari/internal/user"

// endSpan ends a user span with the outcome only: never a handle, email,
// OIDC subject or key prefix.
func endSpan(span trace.Span, err error) {
	instrument.End(span, err, errorCode(err))
}

func errorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrKeyNotFound):
		return "not_found"
	case errors.Is(err, ErrHandleTaken):
		return "handle_taken"
	case errors.Is(err, ErrEmailTaken):
		return "email_taken"
	case errors.Is(err, ErrNotProvisioned), errors.Is(err, ErrDisabled):
		return "forbidden"
	case errors.Is(err, ErrInvalidHandle), errors.Is(err, ErrInvalidRole), errors.Is(err, ErrInvalidScope), errors.Is(err, ErrInvalidIdentity):
		return "invalid_argument"
	default:
		return instrument.CodeInternal
	}
}
