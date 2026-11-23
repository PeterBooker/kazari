package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Bytes is a size in bytes, written as an integer with an optional
// case-insensitive unit: B, KiB to TiB (powers of 1024) or KB to TB (powers
// of 1000), e.g. "50MiB" or "2 GiB".
type Bytes int64

const (
	KiB Bytes = 1 << (10 * (iota + 1))
	MiB
	GiB
	TiB
)

var byteUnits = map[string]Bytes{
	"b":   1,
	"kib": KiB, "mib": MiB, "gib": GiB, "tib": TiB,
	"kb": 1e3, "mb": 1e6, "gb": 1e9, "tb": 1e12,
}

// ParseBytes parses a size such as "4MiB", "1000 KB" or "52428800".
func ParseBytes(s string) (Bytes, error) {
	t := strings.TrimSpace(s)
	i := strings.IndexFunc(t, func(r rune) bool { return r < '0' || r > '9' })
	if i < 0 {
		i = len(t)
	}
	num, unit := t[:i], strings.ToLower(strings.TrimSpace(t[i:]))
	if num == "" {
		return 0, fmt.Errorf("invalid size %q: want an integer with an optional unit (B, KiB, MiB, GiB, TiB, KB, MB, GB, TB)", s)
	}
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", s, err)
	}
	mult := Bytes(1)
	if unit != "" {
		var ok bool
		if mult, ok = byteUnits[unit]; !ok {
			return 0, fmt.Errorf("invalid size %q: unknown unit %q (want B, KiB, MiB, GiB, TiB, KB, MB, GB or TB)", s, t[i:])
		}
	}
	if Bytes(n) > math.MaxInt64/mult {
		return 0, fmt.Errorf("invalid size %q: too large", s)
	}
	return Bytes(n) * mult, nil
}

// String uses the largest binary unit that divides b exactly, e.g. "1536KiB".
func (b Bytes) String() string {
	for _, u := range []struct {
		size   Bytes
		suffix string
	}{{TiB, "TiB"}, {GiB, "GiB"}, {MiB, "MiB"}, {KiB, "KiB"}} {
		if b != 0 && b%u.size == 0 {
			return strconv.FormatInt(int64(b/u.size), 10) + u.suffix
		}
	}
	return strconv.FormatInt(int64(b), 10) + "B"
}

// MarshalText implements encoding.TextMarshaler using String.
func (b Bytes) MarshalText() ([]byte, error) { return []byte(b.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler using ParseBytes.
func (b *Bytes) UnmarshalText(text []byte) error {
	v, err := ParseBytes(string(text))
	if err != nil {
		return err
	}
	*b = v
	return nil
}

// Memory is an amount of memory: a size, written as Bytes is, or a share
// of the memory the process may use, written as a percentage with at most
// two decimals ("5%", "2.5%"), which the server resolves at start.
type Memory struct {
	Size Bytes
	// Share is in hundredths of a percent. Above 0, it is the amount and
	// Size is unused.
	Share int
}

// ParseMemory parses a size such as "64MiB" or a percentage such as "5%".
func ParseMemory(s string) (Memory, error) {
	t := strings.TrimSpace(s)
	num, ok := strings.CutSuffix(t, "%")
	if !ok {
		b, err := ParseBytes(s)
		if err != nil {
			return Memory{}, err
		}
		return Memory{Size: b}, nil
	}
	whole, frac, _ := strings.Cut(strings.TrimSpace(num), ".")
	if !isDigits(whole) || len(whole) > 3 || strings.Contains(num, ".") && (!isDigits(frac) || len(frac) > 2) {
		return Memory{}, fmt.Errorf("invalid percentage %q: want a number with at most two decimals, such as 5%% or 2.5%%", s)
	}
	w, _ := strconv.Atoi(whole)
	f := 0
	if frac != "" {
		f, _ = strconv.Atoi(frac)
		if len(frac) == 1 {
			f *= 10
		}
	}
	share := w*100 + f
	if share == 0 || share > 100*100 {
		return Memory{}, fmt.Errorf("invalid percentage %q: want more than 0%% and at most 100%%; 0 without %% turns it off", s)
	}
	return Memory{Share: share}, nil
}

// String writes a share as a percentage ("5%", "2.5%") and a size as Bytes
// does.
func (m Memory) String() string {
	if m.Share <= 0 {
		return m.Size.String()
	}
	s := strconv.Itoa(m.Share / 100)
	if f := m.Share % 100; f != 0 {
		s += strings.TrimRight(fmt.Sprintf(".%02d", f), "0")
	}
	return s + "%"
}

// Of returns the amount for a process that may use limit bytes.
func (m Memory) Of(limit int64) int64 {
	if m.Share <= 0 {
		return int64(m.Size)
	}
	return int64(float64(limit) * float64(m.Share) / (100 * 100))
}

// MarshalText implements encoding.TextMarshaler using String.
func (m Memory) MarshalText() ([]byte, error) { return []byte(m.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler using ParseMemory.
func (m *Memory) UnmarshalText(text []byte) error {
	v, err := ParseMemory(string(text))
	if err != nil {
		return err
	}
	*m = v
	return nil
}

const (
	second = time.Second
	minute = time.Minute
	hour   = time.Hour
	day    = 24 * time.Hour
)

// maxDays keeps a day count within time.Duration's range.
const maxDays = math.MaxInt64 / int64(day)

// Duration is a time.Duration written as a Go duration string ("15m",
// "336h") with an optional leading whole-day component ("14d", "1d12h").
type Duration time.Duration

// ParseDuration is time.ParseDuration plus optional leading whole weeks
// and days ("2w", "14d", "1w2d").
func ParseDuration(s string) (Duration, error) {
	t := strings.TrimSpace(s)
	var days time.Duration
	if i := strings.IndexByte(t, 'w'); i >= 0 {
		n, err := strconv.ParseInt(t[:i], 10, 64)
		if err != nil || n < 0 || n > maxDays/7 || !isDigits(t[:i]) {
			return 0, fmt.Errorf("invalid duration %q: weeks must be a whole number before \"w\"", s)
		}
		days = time.Duration(n) * 7 * day
		t = t[i+1:]
		if t == "" {
			return Duration(days), nil
		}
		if t[0] == '-' || t[0] == '+' {
			return 0, fmt.Errorf("invalid duration %q: unexpected sign after weeks", s)
		}
	}
	if i := strings.IndexByte(t, 'd'); i >= 0 {
		n, err := strconv.ParseInt(t[:i], 10, 64)
		if err != nil || n < 0 || n > maxDays || !isDigits(t[:i]) {
			return 0, fmt.Errorf("invalid duration %q: days must be a whole number before \"d\"", s)
		}
		if time.Duration(n)*day > math.MaxInt64-days {
			return 0, fmt.Errorf("invalid duration %q: too large", s)
		}
		days += time.Duration(n) * day
		t = t[i+1:]
		if t == "" {
			return Duration(days), nil
		}
		if t[0] == '-' || t[0] == '+' {
			return 0, fmt.Errorf("invalid duration %q: unexpected sign after days", s)
		}
	}
	d, err := time.ParseDuration(t)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %w", s, err)
	}
	if d > math.MaxInt64-days {
		return 0, fmt.Errorf("invalid duration %q: too large", s)
	}
	return Duration(days + d), nil
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// Std returns d as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// String is time.Duration.String without trailing zero units: "1h", not "1h0m0s".
func (d Duration) String() string {
	s := time.Duration(d).String()
	if strings.HasSuffix(s, "m0s") {
		s = s[:len(s)-2]
	}
	if strings.HasSuffix(s, "h0m") {
		s = s[:len(s)-2]
	}
	return s
}

// MarshalText implements encoding.TextMarshaler using String.
func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler using ParseDuration.
func (d *Duration) UnmarshalText(text []byte) error {
	v, err := ParseDuration(string(text))
	if err != nil {
		return err
	}
	*d = v
	return nil
}
