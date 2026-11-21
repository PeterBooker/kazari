package ratelimit_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/instrument"
	"github.com/PeterBooker/kazari/internal/instrument/instrumenttest"
	"github.com/PeterBooker/kazari/internal/ratelimit"
)

var ctx = context.Background()

func TestBackoffGrows(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	b := ratelimit.NewBackoff(ratelimit.BackoffOptions{Free: 2, Base: time.Second, Max: 8 * time.Second, Now: func() time.Time { return now }})
	if d := b.Wait(ctx, "ana"); d != 0 {
		t.Fatalf("Wait before any failure = %s", d)
	}
	// Two free failures, then 1s, 2s, 4s, 8s and never more.
	for i, want := range []time.Duration{0, 0, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second} {
		b.Fail("ana")
		if got := b.Wait(ctx, "ana"); got != want {
			t.Fatalf("Wait after %d failures = %s, want %s", i+1, got, want)
		}
		if got := b.Wait(ctx, "ben"); got != 0 {
			t.Fatalf("Wait for another key = %s", got)
		}
		now = now.Add(want)
		if got := b.Wait(ctx, "ana"); got != 0 {
			t.Fatalf("Wait once %s have passed = %s", want, got)
		}
	}
}

func TestBackoffWaitDoesNotExtend(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	b := ratelimit.NewBackoff(ratelimit.BackoffOptions{Base: 10 * time.Second, Max: time.Minute, Now: func() time.Time { return now }})
	b.Fail("ana")
	for range 5 {
		now = now.Add(time.Second)
		b.Wait(ctx, "ana")
	}
	if got := b.Wait(ctx, "ana"); got != 5*time.Second {
		t.Errorf("Wait after five early looks = %s, want 5s", got)
	}
}

func TestBackoffResetAndForget(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	b := ratelimit.NewBackoff(ratelimit.BackoffOptions{Base: time.Minute, Max: time.Hour, Forget: 10 * time.Minute, Now: func() time.Time { return now }})
	for range 4 {
		b.Fail("ana")
	}
	b.Reset("ana")
	if d := b.Wait(ctx, "ana"); d != 0 || b.Len() != 0 {
		t.Errorf("after Reset: Wait = %s, Len = %d", d, b.Len())
	}

	for range 3 {
		b.Fail("ana")
	}
	now = now.Add(10 * time.Minute)
	b.Fail("ana")
	if got := b.Wait(ctx, "ana"); got != time.Minute {
		t.Errorf("first failure after the key was forgotten waits %s, want the base", got)
	}
	now = now.Add(10 * time.Minute)
	if d := b.Wait(ctx, "ana"); d != 0 || b.Len() != 0 {
		t.Errorf("a forgotten key: Wait = %s, Len = %d", d, b.Len())
	}
}

func TestBackoffBounded(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	b := ratelimit.NewBackoff(ratelimit.BackoffOptions{Base: time.Second, Max: time.Minute, Now: func() time.Time { return now }})
	for i := range ratelimit.MaxEntries + 100 {
		b.Fail(strconv.Itoa(i))
	}
	if n := b.Len(); n > ratelimit.MaxEntries {
		t.Errorf("Len = %d, want at most %d", n, ratelimit.MaxEntries)
	}
}

func TestBackoffNil(t *testing.T) {
	var b *ratelimit.Backoff
	b.Fail("ana")
	b.Reset("ana")
	if b.Wait(ctx, "ana") != 0 || b.Len() != 0 {
		t.Error("a nil Backoff waits")
	}
}

// A refusal for coming too early is counted like any rate limit, by what
// the key stands for, and marked on the request's span.
func TestBackoffTelemetry(t *testing.T) {
	rec := instrumenttest.New(t)
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	b := ratelimit.NewBackoff(ratelimit.BackoffOptions{Base: time.Minute, Max: time.Hour, Now: func() time.Time { return now },
		Scope: ratelimit.ScopeAccount, Telemetry: rec.Providers})
	ctx, end := rec.Start("POST /oauth/signin")
	b.Wait(ctx, "ana")
	b.Fail("ana")
	b.Wait(ctx, "ana")
	b.Wait(ctx, "ana")
	b.Wait(ctx, "ben")
	end()
	rec.Want("kazari.rate_limit.rejected_requests", 2, instrument.RateLimitScope.String(ratelimit.ScopeAccount))
	span, _ := rec.Span("POST /oauth/signin")
	if ev := span.Events(); len(ev) != 2 || ev[0].Name != "kazari.rate_limited" {
		t.Errorf("span events = %v, want two kazari.rate_limited", ev)
	}
}
