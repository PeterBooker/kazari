package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/config"
)

// emit prints v as JSON with --json, and else what text writes.
func (r *runner) emit(v any, text func(w io.Writer) error) error {
	if r.g.JSON {
		return r.printJSON(v)
	}
	return text(r.stdout)
}

func (r *runner) printJSON(v any) error {
	enc := json.NewEncoder(r.stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("encode output: %w", err)
	}
	return nil
}

// table lines up cells that hold text other members wrote: each cell stays
// on its line and in its column, and goes through termSafe.
type table struct{ w *tabwriter.Writer }

func newTable(w io.Writer) *table { return newTablePadded(w, 2) }

func newTablePadded(w io.Writer, padding int) *table {
	return &table{tabwriter.NewWriter(termSafe{w}, 0, 0, padding, ' ', 0)}
}

// row writes one line of cells, each as fmt.Sprint writes it.
func (t *table) row(cells ...any) {
	parts := make([]string, len(cells))
	for i, c := range cells {
		parts[i] = oneLine(fmt.Sprint(c))
	}
	fmt.Fprintln(t.w, strings.Join(parts, "\t"))
}

func (t *table) flush() error { return t.w.Flush() }

// termSafe replaces control characters but tab and newline, and the bidi
// controls, with U+FFFD, so server text cannot drive or reorder the terminal.
// Writes must not split a UTF-8 sequence; tabwriter writes whole cells and
// fmt.Fprintf makes one Write per call.
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

func fmtQuota(used, quota int64) string {
	if quota <= 0 {
		return artifact.FormatBytes(used) + " used (no quota)"
	}
	return fmt.Sprintf("%s of %s used", artifact.FormatBytes(used), artifact.FormatBytes(quota))
}

// parseSize reads a size as the configuration writes one, such as 500MiB.
func parseSize(s string) (int64, error) {
	b, err := config.ParseBytes(s)
	return int64(b), err
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
