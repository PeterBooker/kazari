package user

import (
	"context"
	"errors"
	"testing"
	"time"
)

// With every slot taken a hash waits a bounded time and then says so, where
// a queue without a bound would hold every sign-in behind it.
func TestHashingBusy(t *testing.T) {
	h := hashing{
		params: Argon2{Memory: 64, Time: 1, Threads: 1},
		slots:  make(chan struct{}, 1),
		wait:   time.Millisecond,
		took:   func(context.Context, string, time.Duration) {},
	}
	h.slots <- struct{}{}
	if _, err := h.hash(context.Background(), "a password"); !errors.Is(err, ErrBusy) {
		t.Errorf("hash with no free slot: err = %v, want ErrBusy", err)
	}
	if _, _, err := h.verify(context.Background(), "", "a password"); !errors.Is(err, ErrBusy) {
		t.Errorf("verify with no free slot: err = %v, want ErrBusy", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.wait = time.Hour
	if _, err := h.hash(ctx, "a password"); !errors.Is(err, context.Canceled) {
		t.Errorf("hash for a caller that has gone: err = %v, want context.Canceled", err)
	}
	<-h.slots
	if _, err := h.hash(context.Background(), "a password"); err != nil {
		t.Errorf("hash once a slot is free: %v", err)
	}
}
