package httpapi

import (
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"agalma/internal/api"
)

// Limiter housekeeping: once the map holds more than limiterSweepAt
// entries, inserting a new one first drops those idle for limiterIdle.
const (
	limiterIdle    = 10 * time.Minute
	limiterSweepAt = 1024
)

// limiter is a set of token buckets keyed by caller (client IP or API key),
// each allowing perMinute requests a minute with a burst of perMinute.
type limiter struct {
	perMinute int
	now       func() time.Time

	mu      sync.Mutex
	entries map[string]*limiterEntry
}

type limiterEntry struct {
	lim  *rate.Limiter
	seen time.Time
}

// newLimiter returns nil when perMinute <= 0, which disables limiting.
func newLimiter(perMinute int, now func() time.Time) *limiter {
	if perMinute <= 0 {
		return nil
	}
	return &limiter{perMinute: perMinute, now: now, entries: map[string]*limiterEntry{}}
}

// reserve takes a token for key. It returns 0 when the request may proceed,
// otherwise how long until it could.
func (l *limiter) reserve(key string) time.Duration {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	if !ok {
		if len(l.entries) > limiterSweepAt {
			l.evict(now)
		}
		e = &limiterEntry{lim: rate.NewLimiter(rate.Limit(float64(l.perMinute)/60), l.perMinute)}
		l.entries[key] = e
	}
	e.seen = now
	res := e.lim.ReserveN(now, 1)
	if !res.OK() {
		return time.Minute
	}
	if d := res.DelayFrom(now); d > 0 {
		res.CancelAt(now)
		return d
	}
	return 0
}

func (l *limiter) evict(now time.Time) {
	for k, e := range l.entries {
		if now.Sub(e.seen) > limiterIdle {
			delete(l.entries, k)
		}
	}
}

// limit rejects requests over l's rate with 429 and Retry-After. key picks
// the bucket; a nil limiter passes everything through.
func limit(l *limiter, key func(*http.Request) string, b *base) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if l == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if d := l.reserve(key(r)); d > 0 {
				b.writeError(w, r, &statusError{status: http.StatusTooManyRequests,
					code: api.CodeRateLimited, msg: "too many requests", retryAfter: d})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
