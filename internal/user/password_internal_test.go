package user

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func newTestHashing() *hashing {
	return &hashing{
		params: Argon2{Memory: 64, Time: 1, Threads: 1},
		slots:  make(chan struct{}, 1),
		wait:   slotWait,
		took:   func(context.Context, string, time.Duration) {},
	}
}

// With every slot taken a hash waits a bounded time and then says so, where
// a queue without a bound would hold every sign-in behind it.
func TestHashingBusy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTestHashing()
		h.slots <- struct{}{}

		began := time.Now()
		if _, err := h.hash(t.Context(), "a password"); !errors.Is(err, ErrBusy) {
			t.Errorf("hash with no free slot: err = %v, want ErrBusy", err)
		}
		if got := time.Since(began); got != slotWait {
			t.Errorf("hash with no free slot waited %v, want %v", got, slotWait)
		}
		if _, _, err := h.verify(t.Context(), "", "a password"); !errors.Is(err, ErrBusy) {
			t.Errorf("verify with no free slot: err = %v, want ErrBusy", err)
		}

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		began = time.Now()
		if _, err := h.hash(ctx, "a password"); !errors.Is(err, context.Canceled) {
			t.Errorf("hash for a caller that has gone: err = %v, want context.Canceled", err)
		}
		if got := time.Since(began); got != 0 {
			t.Errorf("hash for a caller that has gone waited %v, want no wait", got)
		}
	})
}

func TestHashingTakesAFreedSlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTestHashing()
		h.slots <- struct{}{}
		go func() {
			time.Sleep(slotWait / 2)
			h.release()
		}()

		began := time.Now()
		if _, err := h.hash(t.Context(), "a password"); err != nil {
			t.Errorf("hash with a slot freed during the wait: %v", err)
		}
		if got := time.Since(began); got != slotWait/2 {
			t.Errorf("hash waited %v for the slot, want %v", got, slotWait/2)
		}
		if len(h.slots) != 0 {
			t.Errorf("%d slots held after the hash, want 0", len(h.slots))
		}
	})
}
