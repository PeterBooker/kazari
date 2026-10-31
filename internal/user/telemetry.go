package user

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

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

// observeUsers reports the users by state and role from a cached count;
// every pair is reported, zero included.
func (s *Service) observeUsers(m metric.Meter) {
	count := instrument.Handle(m.Int64ObservableUpDownCounter("kazari.user.count",
		metric.WithUnit("{user}"), metric.WithDescription("Users by state and role.")))
	attrs := map[[2]string]metric.ObserveOption{}
	for _, state := range []string{"active", "disabled"} {
		for _, role := range []Role{RoleAdmin, RoleMember} {
			attrs[[2]string{state, string(role)}] = metric.WithAttributes(
				attribute.String("kazari.user.state", state), attribute.String("kazari.user.role", string(role)))
		}
	}
	cache := instrument.NewCached(instrument.CensusTTL, instrument.CensusTimeout, s.now, s.store.CountUsers)
	_ = instrument.Handle(m.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		counts, err := cache.Get(ctx)
		if err != nil {
			return err
		}
		by := map[[2]string]int64{}
		for _, c := range counts {
			state := "active"
			if c.Disabled {
				state = "disabled"
			}
			by[[2]string{state, string(c.Role)}] += c.Count
		}
		for key, opt := range attrs {
			o.ObserveInt64(count, by[key], opt)
		}
		return nil
	}, count))
}
