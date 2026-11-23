// Package bytesize writes sizes for people to read.
package bytesize

import (
	"strconv"
	"strings"
)

// Format writes n in the largest binary unit that keeps it whole or to one
// decimal place: 25 MiB, 1.5 GiB, 512 B.
func Format(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	v, exp := float64(n), 0
	for v >= unit && exp < 5 {
		v /= unit
		exp++
	}
	s := strconv.FormatFloat(v, 'f', 1, 64)
	s = strings.TrimSuffix(s, ".0")
	return s + " " + string("KMGTPE"[exp-1]) + "iB"
}

// Exact writes n in the largest binary unit that divides it, else in bytes:
// 2 MiB, 1536 KiB, 1500 bytes. A limit reads as it is configured, never
// rounded.
func Exact(n int64) string {
	for _, u := range []struct {
		shift uint
		name  string
	}{{30, "GiB"}, {20, "MiB"}, {10, "KiB"}} {
		if n >= 1<<u.shift && n%(1<<u.shift) == 0 {
			return strconv.FormatInt(n>>u.shift, 10) + " " + u.name
		}
	}
	return strconv.FormatInt(n, 10) + " bytes"
}
