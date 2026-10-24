// Package ratelimit provides the token-bucket limiters that every HTTP
// surface shares: per client IP before authentication, and per credential
// for writes.
package ratelimit

import (
	"encoding/json"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"kazari/internal/api"
	"kazari/internal/auth"
	"kazari/internal/middleware"
)

// DefaultIPPerMinute is the per-IP request rate the control-plane hosts
// apply before authentication.
const DefaultIPPerMinute = 120

// Limiter housekeeping: once the map holds more than sweepAt entries,
// inserting a new one drops those idle for idle, at most once per
// sweepEvery so a flood of new keys cannot make every insert scan the whole
// map. MaxEntries is a hard cap: at the cap an arbitrary entry is dropped
// before a new one is added, bounding memory whatever the number of
// distinct keys.
const (
	idle       = 10 * time.Minute
	sweepAt    = 1024
	sweepEvery = time.Minute
	MaxEntries = 1 << 16
)

// Limiter is a set of token buckets keyed by caller (client IP or
// credential), each allowing perMinute requests a minute with a burst of
// perMinute. A nil *Limiter allows everything.
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

// New returns nil when perMinute <= 0, which disables limiting. now is the
// clock; nil means time.Now.
func New(perMinute int, now func() time.Time) *Limiter {
	if perMinute <= 0 {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	return &Limiter{perMinute: perMinute, now: now, entries: map[string]*entry{}}
}

// Reserve takes a token for key. It returns 0 when the request may
// proceed, otherwise how long until it could.
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
			for k := range l.entries { // map order is random: drop any one
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

// Middleware rejects requests over l's rate. key picks the bucket; reject
// writes the refusal and defaults to WriteTooMany. A nil limiter passes
// everything through.
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

// WriteTooMany answers 429 with Retry-After and the JSON error body every
// Kazari API uses.
func WriteTooMany(w http.ResponseWriter, _ *http.Request, retryAfter time.Duration) {
	w.Header().Set("Retry-After", RetryAfter(retryAfter))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(api.ErrorResponse{Error: api.Error{Code: api.CodeRateLimited, Message: "too many requests"}})
}

// RetryAfter renders d as a Retry-After value: whole seconds, at least 1.
func RetryAfter(d time.Duration) string {
	return strconv.Itoa(max(1, int((d+time.Second-1)/time.Second)))
}

// CredentialKey is the per-credential bucket: the key prefix or OAuth grant
// id, else the handle.
func CredentialKey(p auth.Principal) string {
	if p.KeyID != "" {
		return p.KeyID
	}
	return p.Handle
}

// IPKey is the per-IP bucket: the client address, with IPv6 addresses
// reduced to their /64 so one host cannot mint fresh buckets by rotating
// through the addresses of its own prefix.
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
