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

// Attribute keys of the built-in accounts' metrics, each with a handful of
// values: never a handle, a link's id or anything of a password.
const (
	linkPurposeKey   = attribute.Key("kazari.user.link.purpose")   // invite | reset | setup
	linkOperationKey = attribute.Key("kazari.user.link.operation") // create | use | revoke
	hashOperationKey = attribute.Key("kazari.user.password.operation")
)

// What is done with a link, and with a password.
const (
	linkCreate = "create"
	linkUse    = "use"
	linkRevoke = "revoke"
	hashHash   = "hash"
	hashVerify = "verify"
)

type metrics struct {
	links metric.Int64Counter
	// hashes times a password hash from the moment it asks for one of the
	// bounded slots, so a queue behind Options.MaxHashes shows as time.
	hashes metric.Float64Histogram
}

func newMetrics(m metric.Meter) metrics {
	return metrics{
		links: instrument.Handle(m.Int64Counter("kazari.user.link.operations",
			metric.WithUnit("{operation}"), metric.WithDescription("Invite, reset and setup links created, used and revoked."))),
		hashes: instrument.Handle(m.Float64Histogram("kazari.user.password.hash.duration",
			metric.WithUnit("s"), metric.WithDescription("Time to hash or verify a password, waiting for a free slot included."),
			metric.WithExplicitBucketBoundaries(0.05, 0.1, 0.2, 0.4, 0.8, 1.6, 3.2, 6.4))),
	}
}

// link counts one operation on a link and names its purpose on the span.
func (s *Service) link(ctx context.Context, operation string, purpose Purpose) {
	s.m.links.Add(ctx, 1, metric.WithAttributes(linkPurposeKey.String(string(purpose)), linkOperationKey.String(operation)))
	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.SetAttributes(linkPurposeKey.String(string(purpose)))
	}
}

// endSpan ends a user span with the outcome only: never a handle, email,
// OIDC subject or key prefix.
func endSpan(span trace.Span, err error) {
	instrument.End(span, err, errorCode(err))
}

func errorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrKeyNotFound), errors.Is(err, ErrLinkNotFound):
		return "not_found"
	case errors.Is(err, ErrBadPassword):
		return "unauthorized"
	case errors.Is(err, ErrBusy):
		return "busy"
	case errors.Is(err, ErrHasPassword):
		return "invalid_argument"
	case errors.Is(err, ErrLastAdmin), errors.Is(err, ErrSetupDone), errors.Is(err, ErrTooManyKeys):
		return "conflict"
	case errors.Is(err, ErrHandleTaken):
		return "handle_taken"
	case errors.Is(err, ErrEmailTaken):
		return "email_taken"
	case errors.Is(err, ErrNotProvisioned), errors.Is(err, ErrDisabled), errors.Is(err, ErrSelf), errors.Is(err, ErrKeyTooWide), errors.Is(err, ErrLocalOff):
		return "forbidden"
	case errors.Is(err, ErrInvalidHandle), errors.Is(err, ErrInvalidRole), errors.Is(err, ErrInvalidScope), errors.Is(err, ErrInvalidIdentity),
		errors.Is(err, ErrInvalidKey), errors.Is(err, ErrWeakPassword):
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
