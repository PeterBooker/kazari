package seal_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/seal"
)

var t0 = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

func TestOpenOnce(t *testing.T) {
	now := t0
	o := seal.NewOnce(time.Hour, func() time.Time { return now })
	v := o.Seal("form", []byte("payload"))
	got, err := o.Open("form", v)
	if err != nil || string(got) != "payload" {
		t.Fatalf("Open = %q, %v; want payload", got, err)
	}
	if _, err := o.Open("form", v); !errors.Is(err, seal.ErrUsed) {
		t.Errorf("second Open: err = %v, want ErrUsed", err)
	}
}

func TestOpenRefuses(t *testing.T) {
	now := t0
	clock := func() time.Time { return now }
	o := seal.NewOnce(time.Hour, clock)
	v := o.Seal("form", []byte("payload"))
	body, sig, _ := strings.Cut(v, ".")
	tests := []struct {
		name    string
		purpose string
		value   string
	}{
		{"another purpose", "other", v},
		{"empty", "form", ""},
		{"no signature", "form", body},
		{"another body", "form", "AAAAAAAAAAA." + sig},
		{"another signature", "form", body + ".AAAA"},
		{"made by another process", "form", seal.NewOnce(time.Hour, clock).Seal("form", []byte("payload"))},
	}
	for _, tt := range tests {
		if _, err := o.Open(tt.purpose, tt.value); !errors.Is(err, seal.ErrExpired) {
			t.Errorf("%s: Open(%q, %q): err = %v, want ErrExpired", tt.name, tt.purpose, tt.value, err)
		}
	}
	if _, err := o.Open("form", v); err != nil {
		t.Errorf("the value after the refusals: %v", err)
	}
}

func TestOpenExpires(t *testing.T) {
	now := t0
	o := seal.NewOnce(time.Hour, func() time.Time { return now })
	old := o.Seal("form", nil)
	now = t0.Add(time.Hour + time.Second)
	if _, err := o.Open("form", old); !errors.Is(err, seal.ErrExpired) {
		t.Errorf("Open after the TTL: err = %v, want ErrExpired", err)
	}
	now = t0.Add(2 * time.Minute)
	future := o.Seal("form", nil)
	now = t0
	if _, err := o.Open("form", future); !errors.Is(err, seal.ErrExpired) {
		t.Errorf("Open of a value made two minutes ahead: err = %v, want ErrExpired", err)
	}
}

// A used value is remembered as long as it could be opened, and no longer.
func TestOpenForgetsExpiredValues(t *testing.T) {
	now := t0
	o := seal.NewOnce(time.Hour, func() time.Time { return now })
	v := o.Seal("form", nil)
	if _, err := o.Open("form", v); err != nil {
		t.Fatal(err)
	}
	now = t0.Add(30 * time.Minute)
	if _, err := o.Open("form", v); !errors.Is(err, seal.ErrUsed) {
		t.Errorf("Open within the TTL: err = %v, want ErrUsed", err)
	}
	now = t0.Add(2 * time.Hour)
	if _, err := o.Open("form", v); !errors.Is(err, seal.ErrExpired) {
		t.Errorf("Open after the TTL: err = %v, want ErrExpired", err)
	}
}
