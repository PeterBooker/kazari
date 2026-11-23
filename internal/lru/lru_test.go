package lru_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/PeterBooker/kazari/internal/lru"
)

func held(c *lru.Cache[string, int], keys ...string) []string {
	var out []string
	for _, k := range keys {
		if c.Contains(k) {
			out = append(out, k)
		}
	}
	return out
}

func TestEvictsLeastRecent(t *testing.T) {
	tests := []struct {
		name       string
		maxEntries int
		maxBytes   int64
	}{
		{"entries", 3, 1 << 20},
		{"bytes", 0, 30},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := lru.New[string, int](tt.maxEntries, tt.maxBytes)
			for i, k := range []string{"a", "b", "c"} {
				if n := c.Add(k, i, 10); n != 0 {
					t.Fatalf("Add(%q) evicted %d, want 0", k, n)
				}
			}
			if v, ok := c.Get("a"); !ok || v != 0 {
				t.Fatalf("Get(a) = %d, %v; want 0, true", v, ok)
			}
			if n := c.Add("d", 3, 10); n != 1 {
				t.Errorf("Add(d) evicted %d, want 1", n)
			}
			if got := fmt.Sprint(held(c, "a", "b", "c", "d")); got != "[a c d]" {
				t.Errorf("held after Add(d) = %s, want [a c d]: b was the least recent", got)
			}
			if entries, bytes := c.Size(); entries != 3 || bytes != 30 {
				t.Errorf("Size = %d, %d; want 3, 30", entries, bytes)
			}
		})
	}
}

func TestContainsKeepsOrder(t *testing.T) {
	c := lru.New[string, int](2, 1<<20)
	c.Add("a", 1, 1)
	c.Add("b", 2, 1)
	if !c.Contains("a") {
		t.Fatal("Contains(a) = false")
	}
	c.Add("c", 3, 1)
	if got := fmt.Sprint(held(c, "a", "b", "c")); got != "[b c]" {
		t.Errorf("held = %s, want [b c]: Contains does not make a more recent", got)
	}
}

func TestAddKeepsHeldValue(t *testing.T) {
	c := lru.New[string, int](0, 100)
	c.Add("a", 1, 10)
	c.Add("a", 2, 50)
	if v, _ := c.Get("a"); v != 1 {
		t.Errorf("Get(a) = %d, want 1, the first value", v)
	}
	if _, bytes := c.Size(); bytes != 10 {
		t.Errorf("bytes = %d, want 10", bytes)
	}
}

func TestOversizedEntryNotStored(t *testing.T) {
	c := lru.New[string, int](0, 100)
	c.Add("a", 1, 60)
	if n := c.Add("big", 2, 101); n != 0 || c.Contains("big") {
		t.Errorf("Add of an entry over the budget: evicted %d, held %v; want 0, false", n, c.Contains("big"))
	}
	if !c.Contains("a") {
		t.Error("an entry over the budget pushed out a")
	}
	if n := c.Add("b", 2, 100); n != 1 || c.Contains("a") {
		t.Errorf("Add of an entry the size of the budget: evicted %d; want 1, a pushed out", n)
	}
}

func TestZeroBudgetStoresNothing(t *testing.T) {
	c := lru.New[string, int](0, 0)
	c.Add("a", 1, 1)
	if _, ok := c.Get("a"); ok {
		t.Error("a zero budget stored an entry")
	}
}

func TestConcurrentUse(t *testing.T) {
	c := lru.New[int, int](50, 1<<20)
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for i := range 1000 {
				k := (g*1000 + i) % 200
				if _, ok := c.Get(k); !ok {
					c.Add(k, i, 8)
				}
			}
		})
	}
	wg.Wait()
	if entries, bytes := c.Size(); entries > 50 || bytes != int64(entries)*8 {
		t.Errorf("Size = %d, %d; want at most 50 entries of 8 bytes each", entries, bytes)
	}
}
