package instrument

import (
	"context"
	"sync"
	"time"
)

// Census settings: Prometheus scrapes every few seconds and the OTLP reader
// pushes every minute, so a value read from the database is reused for
// CensusTTL, and a slow query gives up after CensusTimeout.
const (
	CensusTTL     = 30 * time.Second
	CensusTimeout = 5 * time.Second
)

// Cached is a value an observable instrument's callback reads, loaded at
// most once per ttl.
type Cached[T any] struct {
	load    func(context.Context) (T, error)
	ttl     time.Duration
	timeout time.Duration
	now     func() time.Time

	mu    sync.Mutex
	value T
	at    time.Time
	ok    bool
}

// NewCached returns a Cached that loads with load; a nil now means time.Now.
func NewCached[T any](ttl, timeout time.Duration, now func() time.Time, load func(context.Context) (T, error)) *Cached[T] {
	if now == nil {
		now = time.Now
	}
	return &Cached[T]{load: load, ttl: ttl, timeout: timeout, now: now}
}

// Get returns the cached value, loading it again once ttl has passed. A
// failed load keeps nothing, so the next call tries again.
func (c *Cached[T]) Get(ctx context.Context) (T, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ok && c.now().Sub(c.at) < c.ttl {
		return c.value, nil
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	v, err := c.load(ctx)
	if err != nil {
		var zero T
		return zero, err
	}
	c.value, c.at, c.ok = v, c.now(), true
	return v, nil
}
