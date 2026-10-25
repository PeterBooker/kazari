package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"kazari/internal/api"
)

func (r *runner) printJSON(v any) error {
	enc := json.NewEncoder(r.stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("encode output: %w", err)
	}
	return nil
}

// newTable goes through termSafe because its cells hold text other members
// wrote.
func newTable(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(termSafe{w}, 0, 0, 2, ' ', 0)
}

// termSafe replaces control characters but tab and newline, and the bidi
// controls, with U+FFFD, so server text cannot drive or reorder the terminal.
// Writes must not split a UTF-8 sequence; tabwriter writes whole cells.
type termSafe struct{ w io.Writer }

func (t termSafe) Write(p []byte) (int, error) {
	if !strings.ContainsFunc(string(p), unsafeRune) && utf8.Valid(p) {
		return t.w.Write(p)
	}
	clean := strings.Map(func(r rune) rune {
		if unsafeRune(r) {
			return utf8.RuneError
		}
		return r
	}, string(p))
	if _, err := io.WriteString(t.w, clean); err != nil {
		return 0, err
	}
	return len(p), nil
}

func unsafeRune(r rune) bool {
	switch {
	case r == '\t' || r == '\n':
		return false
	case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f: // C0, DEL, C1
		return true
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069: // bidi embeddings, overrides, isolates
		return true
	}
	return false
}

// oneLine stops a cell value shifting columns or faking rows.
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, s)
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format(time.RFC3339)
}

func fmtTimePtr(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return fmtTime(*t)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 5; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func fmtQuota(used, quota int64) string {
	if quota <= 0 {
		return humanSize(used) + " used (no quota)"
	}
	return fmt.Sprintf("%s of %s used", humanSize(used), humanSize(quota))
}

func parseSize(s string) (int64, error) {
	t := strings.TrimSpace(s)
	units := []struct {
		suffix string
		mult   int64
	}{
		{"kib", 1 << 10}, {"mib", 1 << 20}, {"gib", 1 << 30}, {"tib", 1 << 40},
		{"kb", 1e3}, {"mb", 1e6}, {"gb", 1e9}, {"tb", 1e12},
		{"k", 1 << 10}, {"m", 1 << 20}, {"g", 1 << 30}, {"t", 1 << 40},
		{"b", 1},
	}
	mult := int64(1)
	lower := strings.ToLower(t)
	for _, u := range units {
		if strings.HasSuffix(lower, u.suffix) {
			mult = u.mult
			t = strings.TrimSpace(t[:len(t)-len(u.suffix)])
			break
		}
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return int64(f * float64(mult)), nil
}

func shortHash(h string) string {
	algo, sum, ok := strings.Cut(h, ":")
	if !ok {
		algo, sum = "", h
	}
	if len(sum) > 12 {
		sum = sum[:12]
	}
	if algo == "" {
		return sum
	}
	return algo + ":" + sum
}

func artifactState(a *api.Artifact) string {
	switch {
	case a.PurgedAt != nil:
		return "purged"
	case a.DeletedAt != nil:
		return "deleted"
	case a.UnpublishedAt != nil || a.Visibility == "unpublished":
		return "unpublished"
	case a.StaleAt != nil:
		return "stale"
	default:
		return "live"
	}
}
