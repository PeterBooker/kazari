// Package ratelimit provides the token-bucket limiters every HTTP surface
// shares: per client IP before authentication, per credential for writes.
package ratelimit

import (
	"encoding/json"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/auth"
	"github.com/PeterBooker/kazari/internal/middleware"
)

const DefaultIPPerMinute = 120

// Past sweepAt entries a new key evicts those idle for idle, at most once per
// sweepEvery so a flood of new keys cannot make every insert scan the map.
// MaxEntries bounds memory: at the cap an arbitrary entry is dropped first.
const (
	idle       = 10 * time.Minute
	sweepAt    = 1024
	sweepEvery = time.Minute
	MaxEntries = 1 << 16
)

// Limiter holds a token bucket per key, each allowing perMinute requests a
// minute with a burst of perMinute. A nil *Limiter allows everything.
type Limiter struct {
	perMinute int
	now       func() time.Time

	mu        sync.Mutex
	entries   map[string]*entry
	lastSweep time.Time
}

type entry struct {
	lim  *rate.Limiter
	seen time.Time
}

// New returns nil, which allows everything, when perMinute <= 0. A nil now
// means time.Now.
func New(perMinute int, now func() time.Time) *Limiter {
	if perMinute <= 0 {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	return &Limiter{perMinute: perMinute, now: now, entries: map[string]*entry{}}
}

// Reserve takes a token for key and returns 0, or returns how long until the
// request could proceed.
func (l *Limiter) Reserve(key string) time.Duration {
	if l == nil {
		return 0
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	if !ok {
		if len(l.entries) > sweepAt && now.Sub(l.lastSweep) >= sweepEvery {
			l.evict(now)
			l.lastSweep = now
		}
		if len(l.entries) >= MaxEntries {
			for k := range l.entries {
				delete(l.entries, k)
				break
			}
		}
		e = &entry{lim: rate.NewLimiter(rate.Limit(float64(l.perMinute)/60), l.perMinute)}
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

// Len reports the number of buckets held.
func (l *Limiter) Len() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

func (l *Limiter) evict(now time.Time) {
	for k, e := range l.entries {
		if now.Sub(e.seen) > idle {
			delete(l.entries, k)
		}
	}
}

// Middleware rejects requests over l's rate with reject, nil meaning
// WriteTooMany. key picks the bucket; a nil l passes everything.
func Middleware(l *Limiter, key func(*http.Request) string, reject func(http.ResponseWriter, *http.Request, time.Duration)) func(http.Handler) http.Handler {
	if reject == nil {
		reject = WriteTooMany
	}
	return func(next http.Handler) http.Handler {
		if l == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if d := l.Reserve(key(r)); d > 0 {
				reject(w, r, d)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// WriteTooMany answers 429 with Retry-After and the API's JSON error.
func WriteTooMany(w http.ResponseWriter, _ *http.Request, retryAfter time.Duration) {
	w.Header().Set("Retry-After", RetryAfter(retryAfter))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(api.ErrorResponse{Error: api.Error{Code: api.CodeRateLimited, Message: "too many requests"}})
}

// RetryAfter renders d as whole seconds, rounded up, at least 1.
func RetryAfter(d time.Duration) string {
	return strconv.Itoa(max(1, int((d+time.Second-1)/time.Second)))
}

// CredentialKey is the key prefix or OAuth grant id, else the handle.
func CredentialKey(p auth.Principal) string {
	if p.KeyID != "" {
		return p.KeyID
	}
	return p.Handle
}

// IPKey is the client address, IPv6 reduced to its /64 so one host cannot
// mint fresh buckets by rotating through its own prefix.
func IPKey(r *http.Request) string {
	ip := middleware.ClientIP(r)
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	addr = addr.Unmap()
	if addr.Is6() {
		if p, err := addr.Prefix(64); err == nil {
			return p.String()
		}
	}
	return addr.String()
}
