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
	n       int // failures in a row
	pending int // attempts under way
	last    time.Time
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
	// The limiters' counter: a refusal for coming too early is a rate limit
	// like theirs.
	return &Backoff{o: o, entries: map[string]*failures{}, rejected: rejectedCounter(o.Telemetry)}
}

// Attempt is one try that Begin admitted. Exactly one of Failed and Done
// ends it.
type Attempt struct {
	b   *Backoff
	key string
}

// Begin asks to make an attempt for key. It returns how long to wait, 0
// when the attempt may be made now; the caller then ends the Attempt with
// Failed or Done. Attempts that are under way count as failures until they
// end, so a burst of parallel guesses is held to what one after the other
// would be: past the free failures, one at a time. A refusal records
// nothing but itself (Refused): an attempt that came too early does not
// extend the wait.
func (b *Backoff) Begin(key string) (Attempt, time.Duration) {
	if b == nil {
		return Attempt{}, 0
	}
	now := b.o.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	f, ok := b.entries[key]
	if ok && now.Sub(f.last) >= b.o.Forget && f.pending == 0 {
		delete(b.entries, key)
		ok = false
	}
	if !ok {
		b.makeRoom(now)
		f = &failures{}
		b.entries[key] = f
	}
	if d := f.last.Add(b.delay(f.n)).Sub(now); f.n > 0 && d > 0 {
		return Attempt{}, d
	}
	// With attempts under way that may yet fail, a further one is let in
	// only while the failures so far and those would all still be free.
	if f.pending > 0 && f.n+f.pending >= b.o.Free {
		return Attempt{}, b.o.Base
	}
	f.pending++
	return Attempt{b: b, key: key}, 0
}

// Refused counts a request the caller turned away because Begin said to
// wait, against the surface in ctx, and marks it on the request's span. A
// caller that asks several Backoffs counts the refusal once, with the one
// that asked for the longest wait.
func (b *Backoff) Refused(ctx context.Context) {
	if b == nil {
		return
	}
	b.rejected.Add(ctx, 1, metric.WithAttributes(
		instrument.Surface.String(instrument.SurfaceFrom(ctx)),
		instrument.RateLimitScope.String(b.o.Scope),
	))
	trace.SpanFromContext(ctx).AddEvent("kazari.rate_limited",
		trace.WithAttributes(instrument.RateLimitScope.String(b.o.Scope)))
}

// Failed ends the attempt as a failure, which makes the next one wait.
func (a Attempt) Failed() { a.end(true) }

// Done ends the attempt without counting it: it succeeded, or was never
// judged.
func (a Attempt) Done() { a.end(false) }

func (a Attempt) end(failed bool) {
	b := a.b
	if b == nil {
		return
	}
	now := b.o.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	f, ok := b.entries[a.key]
	if !ok {
		// Evicted, or reset by a success, while this attempt ran.
		if !failed {
			return
		}
		b.makeRoom(now)
		f = &failures{pending: 1}
		b.entries[a.key] = f
	}
	f.pending = max(0, f.pending-1)
	switch {
	case failed:
		if f.n > 0 && now.Sub(f.last) >= b.o.Forget {
			f.n = 0
		}
		f.n++
		f.last = now
	case f.n == 0 && f.pending == 0:
		delete(b.entries, a.key)
	}
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

// makeRoom bounds memory as Limiter does: forgotten keys go once a minute
// past sweepAt entries, and at MaxEntries an arbitrary one goes first.
func (b *Backoff) makeRoom(now time.Time) {
	if len(b.entries) > sweepAt && now.Sub(b.lastSweep) >= sweepEvery {
		for k, f := range b.entries {
			if f.pending == 0 && now.Sub(f.last) >= b.o.Forget {
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

// Pair is one attempt admitted by two Backoffs, such as an account's and
// an address's.
type Pair struct{ first, second Attempt }

// BeginPair is Begin on two Backoffs. The attempt may be made only when
// both admit it; otherwise it returns the longer wait and counts the one
// refusal against the Backoff that asked for it.
func BeginPair(ctx context.Context, first *Backoff, firstKey string, second *Backoff, secondKey string) (Pair, time.Duration) {
	a, wa := first.Begin(firstKey)
	b, wb := second.Begin(secondKey)
	if wa == 0 && wb == 0 {
		return Pair{a, b}, 0
	}
	a.Done()
	b.Done()
	if wa >= wb {
		first.Refused(ctx)
	} else {
		second.Refused(ctx)
	}
	return Pair{}, max(wa, wb)
}

// Failed ends the attempt as a failure on both.
func (p Pair) Failed() { p.first.Failed(); p.second.Failed() }

// Done ends the attempt on both without counting it.
func (p Pair) Done() { p.first.Done(); p.second.Done() }
