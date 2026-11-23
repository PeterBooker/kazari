package user

import (
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

// With every slot taken, HashPassword and VerifyPassword say so after the
// bounded wait, as an account's sign-in does.
func TestHashAndVerifyPasswordBusy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &Service{hashing: *newTestHashing()}
		s.hashing.slots <- struct{}{}
		began := time.Now()
		if _, err := s.HashPassword(t.Context(), "correct horse battery"); !errors.Is(err, ErrBusy) {
			t.Errorf("HashPassword with no free slot: err = %v, want ErrBusy", err)
		}
		if _, err := s.VerifyPassword(t.Context(), "$argon2id$v=19$m=64,t=1,p=1$c2FsdHNhbHQ$a2V5a2V5a2V5a2V5a2V5", "x"); !errors.Is(err, ErrBusy) {
			t.Errorf("VerifyPassword with no free slot: err = %v, want ErrBusy", err)
		}
		if got := time.Since(began); got != 2*slotWait {
			t.Errorf("the two calls waited %v, want %v", got, 2*slotWait)
		}
	})
}
