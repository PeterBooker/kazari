package seal_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/seal"
)

// FuzzOpen: a value from a browser never panics and opens only when it is
// one Seal made; one Seal made opens once, with its payload.
func FuzzOpen(f *testing.F) {
	now := func() time.Time { return t0 }
	f.Add("form", []byte("payload"), "")
	f.Add("", []byte{}, "AAAAAAAAAAA.AAAA")
	f.Add("x", []byte{0, 1, 2}, "....")
	f.Fuzz(func(t *testing.T, purpose string, payload []byte, value string) {
		o := seal.NewOnce(time.Hour, now)
		if _, err := o.Open(purpose, value); err == nil {
			t.Fatalf("Open(%q, %q) took a value Seal did not make", purpose, value)
		} else if !errors.Is(err, seal.ErrExpired) {
			t.Fatalf("Open(%q, %q): err = %v, want ErrExpired", purpose, value, err)
		}
		v := o.Seal(purpose, payload)
		got, err := o.Open(purpose, v)
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("Open(Seal(%q, %q)) = %q, %v", purpose, payload, got, err)
		}
		if _, err := o.Open(purpose, v); !errors.Is(err, seal.ErrUsed) {
			t.Fatalf("second Open: err = %v, want ErrUsed", err)
		}
	})
}
