package ratelimit

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/PeterBooker/kazari/internal/instrument"
)

// BackoffOptions configure a Backoff.
type BackoffOptions struct {
	// Free is how many failures in a row cost nothing.
	Free int
	// Base is the wait after the first failure past Free; each further
	// failure doubles it, up to Max.
	Base, Max time.Duration
	// Forget is how long after its last failure a key starts afresh
	// (default 15 minutes).
	Forget time.Duration
	Now    func() time.Time // nil means time.Now
	// Scope is what the keys stand for, ScopeIP or ScopeAccount, on the
	// refusals it counts.
	Scope     string
	Telemetry instrument.Providers
}

// Backoff makes guessing slow without ever locking anyone out: each failure
// for a key makes the next attempt wait longer, up to BackoffOptions.Max,
// and a key that stops failing is forgotten. A lockout would let anyone
// who knows a person's name keep them out; a bounded wait cannot. A nil
// *Backoff never waits.
type Backoff struct {
	o        BackoffOptions
	rejected metric.Int64Counter

	mu        sync.Mutex
	entries   map[string]*failures
	lastSweep time.Time
}

type failures struct {
	n    int
	last time.Time
}

// NewBackoff returns a Backoff.
func NewBackoff(o BackoffOptions) *Backoff {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Forget <= 0 {
		o.Forget = 15 * time.Minute
	}
	o.Scope = instrument.OneOf(o.Scope, ScopeIP, ScopeAccount)
	return &Backoff{o: o, entries: map[string]*failures{},
		// The limiters' counter: a refusal for coming too early is a rate
		// limit like theirs.
		rejected: instrument.Handle(o.Telemetry.Meter(scope).Int64Counter("kazari.rate_limit.rejected_requests",
			metric.WithDescription("Requests refused by a rate limit."),
			metric.WithUnit("{request}")))}
}

// Wait returns how long until an attempt for key may be made, 0 when it may
// be made now. An attempt that has to wait is refused by the caller: it is
// counted against the surface in ctx and marked on its span, and it does
// not extend the wait.
func (b *Backoff) Wait(ctx context.Context, key string) time.Duration {
	if b == nil {
		return 0
	}
	d := b.wait(key)
	if d > 0 {
		b.rejected.Add(ctx, 1, metric.WithAttributes(
			instrument.Surface.String(instrument.SurfaceFrom(ctx)),
			instrument.RateLimitScope.String(b.o.Scope),
		))
		trace.SpanFromContext(ctx).AddEvent("kazari.rate_limited",
			trace.WithAttributes(instrument.RateLimitScope.String(b.o.Scope)))
	}
	return d
}

func (b *Backoff) wait(key string) time.Duration {
	now := b.o.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	f, ok := b.entries[key]
	if !ok {
		return 0
	}
	if now.Sub(f.last) >= b.o.Forget {
		delete(b.entries, key)
		return 0
	}
	return max(0, f.last.Add(b.delay(f.n)).Sub(now))
}

// delay is the wait that n failures in a row impose.
func (b *Backoff) delay(n int) time.Duration {
	over := n - b.o.Free
	if over <= 0 {
		return 0
	}
	d := b.o.Base
	for range over - 1 {
		if d >= b.o.Max {
			break
		}
		d *= 2
	}
	return min(d, b.o.Max)
}

// Fail records a failed attempt for key.
func (b *Backoff) Fail(key string) {
	if b == nil {
		return
	}
	now := b.o.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	f, ok := b.entries[key]
	if ok && now.Sub(f.last) >= b.o.Forget {
		f.n = 0
	}
	if !ok {
		b.makeRoom(now)
		f = &failures{}
		b.entries[key] = f
	}
	f.n++
	f.last = now
}

// makeRoom bounds memory as Limiter does: forgotten keys go once a minute
// past sweepAt entries, and at MaxEntries an arbitrary one goes first.
func (b *Backoff) makeRoom(now time.Time) {
	if len(b.entries) > sweepAt && now.Sub(b.lastSweep) >= sweepEvery {
		for k, f := range b.entries {
			if now.Sub(f.last) >= b.o.Forget {
				delete(b.entries, k)
			}
		}
		b.lastSweep = now
	}
	if len(b.entries) >= MaxEntries {
		for k := range b.entries {
			delete(b.entries, k)
			break
		}
	}
}

// Reset forgets key's failures, after an attempt that succeeded.
func (b *Backoff) Reset(key string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	delete(b.entries, key)
	b.mu.Unlock()
}

// Len reports the number of keys held.
func (b *Backoff) Len() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.entries)
}
