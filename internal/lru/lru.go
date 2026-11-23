// Package lru is a least-recently-used cache bounded by a number of entries
// and by a budget of bytes, safe for concurrent use. The caller gives the
// size of each entry.
package lru

import "sync"

// Cache holds values by key until the bounds push them out, the least
// recently used first. The zero value is not usable; call New.
type Cache[K comparable, V any] struct {
	maxEntries int
	maxBytes   int64

	mu      sync.Mutex
	bytes   int64
	root    entry[K, V] // root.next is the most recently used, root.prev the least
	entries map[K]*entry[K, V]
}

type entry[K comparable, V any] struct {
	key        K
	value      V
	size       int64
	prev, next *entry[K, V]
}

// New returns a Cache whose sizes add up to at most maxBytes, of at most
// maxEntries entries, or of any number when maxEntries is 0 or less.
func New[K comparable, V any](maxEntries int, maxBytes int64) *Cache[K, V] {
	c := &Cache[K, V]{maxEntries: maxEntries, maxBytes: maxBytes, entries: map[K]*entry[K, V]{}}
	c.root.prev, c.root.next = &c.root, &c.root
	return c
}

// Get returns the value of k and makes it the most recently used.
func (c *Cache[K, V]) Get(k K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[k]
	if !ok {
		var zero V
		return zero, false
	}
	c.unlink(e)
	c.pushFront(e)
	return e.value, true
}

// Contains reports whether k is held, without making it more recent.
func (c *Cache[K, V]) Contains(k K) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.entries[k]
	return ok
}

// Add stores v under k as the most recently used, unless k is held already
// or size alone is over the byte budget, and returns how many entries it
// pushed out.
func (c *Cache[K, V]) Add(k K, v V, size int64) (evicted int) {
	if size > c.maxBytes {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[k]; ok {
		return 0
	}
	e := &entry[K, V]{key: k, value: v, size: size}
	c.entries[k] = e
	c.pushFront(e)
	c.bytes += size
	for c.bytes > c.maxBytes || c.maxEntries > 0 && len(c.entries) > c.maxEntries {
		last := c.root.prev
		c.unlink(last)
		delete(c.entries, last.key)
		c.bytes -= last.size
		evicted++
	}
	return evicted
}

// Size returns how many entries are held and the sum of their sizes.
func (c *Cache[K, V]) Size() (entries int, bytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries), c.bytes
}

func (c *Cache[K, V]) pushFront(e *entry[K, V]) {
	e.prev, e.next = &c.root, c.root.next
	e.prev.next, e.next.prev = e, e
}

func (c *Cache[K, V]) unlink(e *entry[K, V]) {
	e.prev.next, e.next.prev = e.next, e.prev
	e.prev, e.next = nil, nil
}
