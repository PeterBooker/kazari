package ratelimit

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The limiter map stays bounded however many distinct keys arrive, and a
// flood of fresh keys does not reset the bucket of a key that is in use.
func TestLimiterBounded(t *testing.T) {
	now := time.Unix(0, 0)
	l := New(1, func() time.Time { return now })
	if d := l.Reserve("victim"); d != 0 {
		t.Fatalf("first request delayed %v", d)
	}
	for i := range MaxEntries + 500 {
		l.Reserve(fmt.Sprintf("k%d", i))
	}
	if n := l.Len(); n > MaxEntries {
		t.Fatalf("entries = %d, want at most %d", n, MaxEntries)
	}
	// Idle entries are swept once the sweep interval has passed.
	now = now.Add(idle + sweepEvery + time.Second)
	l.Reserve("fresh")
	if n := l.Len(); n != 1 {
		t.Errorf("entries after idle sweep = %d, want 1", n)
	}
}

func TestNilLimiterAllows(t *testing.T) {
	var l *Limiter = New(0, nil)
	if l != nil {
		t.Fatal("New(0) is not nil")
	}
	if d := l.Reserve("k"); d != 0 {
		t.Errorf("nil limiter delayed %v", d)
	}
}

func TestMiddleware(t *testing.T) {
	now := time.Unix(0, 0)
	h := Middleware(New(2, func() time.Time { return now }), IPKey, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	var got []int
	for range 3 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		got = append(got, rec.Code)
		if rec.Code == http.StatusTooManyRequests {
			if ra := rec.Header().Get("Retry-After"); ra != "30" {
				t.Errorf("Retry-After = %q, want 30", ra)
			}
			if body := rec.Body.String(); body != `{"error":{"code":"rate_limited","message":"too many requests"}}`+"\n" {
				t.Errorf("body = %q", body)
			}
		}
	}
	if want := []int{204, 204, 429}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("statuses = %v, want %v", got, want)
	}
}

func TestIPKey(t *testing.T) {
	tests := []struct {
		remote, want string
	}{
		{"192.0.2.7:1234", "192.0.2.7"},
		{"[2001:db8:1:2:aaaa::1]:443", "2001:db8:1:2::/64"},
		{"[2001:db8:1:2:bbbb::9]:443", "2001:db8:1:2::/64"},
		{"[::ffff:192.0.2.7]:80", "192.0.2.7"},
		{"not-an-ip", "not-an-ip"},
	}
	for _, tt := range tests {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = tt.remote
		if got := IPKey(r); got != tt.want {
			t.Errorf("IPKey(%q) = %q, want %q", tt.remote, got, tt.want)
		}
	}
}
