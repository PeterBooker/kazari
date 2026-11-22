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

// fail makes one failed attempt for key, and reports whether it was let in.
func fail(b *ratelimit.Backoff, key string) bool {
	a, wait := b.Begin(key)
	a.Failed()
	return wait == 0
}

// waitFor is how long the next attempt for key would have to wait.
func waitFor(b *ratelimit.Backoff, key string) time.Duration {
	a, wait := b.Begin(key)
	a.Done()
	return wait
}

func TestBackoffGrows(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	b := ratelimit.NewBackoff(ratelimit.BackoffOptions{Free: 2, Base: time.Second, Max: 8 * time.Second, Now: func() time.Time { return now }})
	if d := waitFor(b, "ana"); d != 0 {
		t.Fatalf("wait before any failure = %s", d)
	}
	// Two free failures, then 1s, 2s, 4s, 8s and never more.
	for i, want := range []time.Duration{0, 0, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second} {
		if !fail(b, "ana") {
			t.Fatalf("attempt %d was refused once its wait had passed", i+1)
		}
		if got := waitFor(b, "ana"); got != want {
			t.Fatalf("wait after %d failures = %s, want %s", i+1, got, want)
		}
		if got := waitFor(b, "ben"); got != 0 {
			t.Fatalf("wait for another key = %s", got)
		}
		now = now.Add(want)
		if got := waitFor(b, "ana"); got != 0 {
			t.Fatalf("wait once %s have passed = %s", want, got)
		}
	}
}

// An attempt refused for coming too early does not extend the wait.
func TestBackoffRefusalDoesNotExtend(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	b := ratelimit.NewBackoff(ratelimit.BackoffOptions{Base: 10 * time.Second, Max: time.Minute, Now: func() time.Time { return now }})
	fail(b, "ana")
	for range 5 {
		now = now.Add(time.Second)
		if fail(b, "ana") {
			t.Fatal("an early attempt was let in")
		}
	}
	if got := waitFor(b, "ana"); got != 5*time.Second {
		t.Errorf("wait after five early attempts = %s, want 5s", got)
	}
}

// Attempts under way count as failures until they end, so guesses made at
// once get no further than guesses made in turn.
func TestBackoffParallelAttempts(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	b := ratelimit.NewBackoff(ratelimit.BackoffOptions{Free: 3, Base: time.Second, Max: time.Minute, Now: func() time.Time { return now }})
	var admitted []ratelimit.Attempt
	for range 20 {
		if a, wait := b.Begin("ana"); wait == 0 {
			admitted = append(admitted, a)
		}
	}
	if len(admitted) != 3 {
		t.Fatalf("%d parallel attempts admitted, want the 3 free ones", len(admitted))
	}
	for _, a := range admitted {
		a.Failed()
	}
	// The free failures are spent: from here it is one attempt at a time.
	a, wait := b.Begin("ana")
	if wait != 0 {
		t.Fatalf("an attempt after three failures waits %s, want 0: the wait starts with the fourth", wait)
	}
	if _, wait := b.Begin("ana"); wait == 0 {
		t.Error("a second attempt was admitted while one past the free failures was under way")
	}
	a.Failed()
	if got := waitFor(b, "ana"); got != time.Second {
		t.Errorf("wait after the fourth failure = %s, want 1s", got)
	}
	// One that succeeds frees its place and counts for nothing.
	b.Reset("ana")
	ok, _ := b.Begin("ana")
	ok.Done()
	if b.Len() != 0 {
		t.Errorf("Len = %d after an attempt that did not fail, want 0", b.Len())
	}
}

func TestBackoffResetAndForget(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	b := ratelimit.NewBackoff(ratelimit.BackoffOptions{Base: time.Minute, Max: time.Hour, Forget: 10 * time.Minute, Now: func() time.Time { return now }})
	fail(b, "ana")
	b.Reset("ana")
	if d := waitFor(b, "ana"); d != 0 || b.Len() != 0 {
		t.Errorf("after Reset: wait = %s, Len = %d", d, b.Len())
	}

	fail(b, "ana")
	now = now.Add(time.Minute)
	fail(b, "ana")
	now = now.Add(10 * time.Minute)
	fail(b, "ana")
	if got := waitFor(b, "ana"); got != time.Minute {
		t.Errorf("first failure after the key was forgotten waits %s, want the base", got)
	}
	now = now.Add(10 * time.Minute)
	if d := waitFor(b, "ana"); d != 0 || b.Len() != 0 {
		t.Errorf("a forgotten key: wait = %s, Len = %d", d, b.Len())
	}
}

func TestBackoffBounded(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	b := ratelimit.NewBackoff(ratelimit.BackoffOptions{Base: time.Second, Max: time.Minute, Now: func() time.Time { return now }})
	for i := range ratelimit.MaxEntries + 100 {
		fail(b, strconv.Itoa(i))
	}
	if n := b.Len(); n > ratelimit.MaxEntries {
		t.Errorf("Len = %d, want at most %d", n, ratelimit.MaxEntries)
	}
}

func TestBackoffNil(t *testing.T) {
	var b *ratelimit.Backoff
	a, wait := b.Begin("ana")
	a.Failed()
	a.Done()
	b.Reset("ana")
	b.Refused(context.Background())
	if wait != 0 || b.Len() != 0 {
		t.Error("a nil Backoff waits")
	}
	p, wait := ratelimit.BeginPair(context.Background(), nil, "ana", nil, "10.0.0.1")
	p.Failed()
	if wait != 0 {
		t.Error("a pair of nil Backoffs waits")
	}
}

// One refused request is one refusal, counted against the Backoff that
// asked for the longer wait, and marked once on the request's span.
func TestBackoffTelemetry(t *testing.T) {
	rec := instrumenttest.New(t)
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	account := ratelimit.NewBackoff(ratelimit.BackoffOptions{Base: time.Minute, Max: time.Hour, Now: clock,
		Scope: ratelimit.ScopeAccount, Telemetry: rec.Providers})
	address := ratelimit.NewBackoff(ratelimit.BackoffOptions{Base: time.Second, Max: time.Hour, Now: clock,
		Scope: ratelimit.ScopeIP, Telemetry: rec.Providers})
	ctx, end := rec.Start("POST /oauth/signin")

	p, wait := ratelimit.BeginPair(ctx, account, "ana", address, "10.0.0.1")
	if wait != 0 {
		t.Fatalf("the first attempt waits %s", wait)
	}
	p.Failed()
	for range 2 {
		if _, wait := ratelimit.BeginPair(ctx, account, "ana", address, "10.0.0.1"); wait != time.Minute {
			t.Fatalf("wait = %s, want the account's minute", wait)
		}
	}
	// Another account from the same address waits for the address alone.
	if _, wait := ratelimit.BeginPair(ctx, account, "ben", address, "10.0.0.1"); wait != time.Second {
		t.Fatalf("wait = %s, want the address's second", wait)
	}
	end()

	rec.Want("kazari.rate_limit.rejected_requests", 2, instrument.RateLimitScope.String(ratelimit.ScopeAccount))
	rec.Want("kazari.rate_limit.rejected_requests", 1, instrument.RateLimitScope.String(ratelimit.ScopeIP))
	span, _ := rec.Span("POST /oauth/signin")
	if ev := span.Events(); len(ev) != 3 || ev[0].Name != "kazari.rate_limited" {
		t.Errorf("span events = %v, want three kazari.rate_limited", ev)
	}
	// A refused pair holds no place on either side.
	now = now.Add(time.Hour)
	if _, wait := ratelimit.BeginPair(ctx, account, "ana", address, "10.0.0.1"); wait != 0 {
		t.Errorf("after the waits have passed: wait = %s", wait)
	}
}
