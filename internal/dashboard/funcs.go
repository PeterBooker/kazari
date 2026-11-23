package dashboard

import (
	"fmt"
	"html/template"
	"strconv"
	"strings"
	"time"

	"github.com/PeterBooker/kazari/internal/bytesize"
)

// commonFuncs are the template functions every page uses.
var commonFuncs = template.FuncMap{
	"when":    when,
	"day":     day,
	"bytes":   humanBytes,
	"percent": percent,
	"span":    span,
	"join":    strings.Join,
	"dict":    dict,
	// scopeWords says what scopes allow, in words, joined.
	"scopeWords": scopeWords,
}

func scopeWords(scopes []string) string {
	words := make([]string, 0, len(scopes))
	for _, sc := range scopes {
		if w := peopleScopeNames[sc]; w != "" {
			sc = w
		}
		words = append(words, sc)
	}
	return strings.Join(words, "; ")
}

const (
	timeLayout = "2 Jan 2006, 15:04 UTC"
	dayLayout  = "2 Jan 2006"
)

// when renders a time as a <time> element in absolute UTC, which the script
// rewrites as a relative time in the browser's zone. A nil or zero time is
// a dash. It takes a time.Time or a *time.Time.
func when(v any) template.HTML {
	var t time.Time
	switch x := v.(type) {
	case time.Time:
		t = x
	case *time.Time:
		if x != nil {
			t = *x
		}
	}
	if t.IsZero() {
		return "—"
	}
	t = t.UTC()
	return template.HTML(`<time datetime="` + t.Format(time.RFC3339) + `">` + t.Format(timeLayout) + `</time>`)
}

// day renders a date, for a deadline whose hour does not matter.
func day(v any) template.HTML {
	var t time.Time
	switch x := v.(type) {
	case time.Time:
		t = x
	case *time.Time:
		if x != nil {
			t = *x
		}
	}
	if t.IsZero() {
		return "—"
	}
	t = t.UTC()
	return template.HTML(`<time datetime="` + t.Format(time.RFC3339) + `">` + t.Format(dayLayout) + `</time>`)
}

// humanBytes renders a size in binary units, as the configuration writes
// them: 1.5 GiB.
func humanBytes(v any) string {
	var n int64
	switch x := v.(type) {
	case int64:
		n = x
	case int:
		n = int64(x)
	case uint64:
		n = int64(x)
	}
	return bytesize.Format(n)
}

// percent is used of quota as a whole number from 0 to 100; 0 when there
// is no quota.
func percent(used, quota int64) int {
	if quota <= 0 || used <= 0 {
		return 0
	}
	return int(min(100, used*100/quota))
}

// span renders a duration in the largest unit that divides it: 14 days,
// 12 hours, 90 minutes.
func span(d time.Duration) string {
	unit := func(n int64, name string) string {
		if n == 1 {
			return "1 " + name
		}
		return strconv.FormatInt(n, 10) + " " + name + "s"
	}
	switch {
	case d <= 0:
		return "none"
	case d%(24*time.Hour) == 0:
		return unit(int64(d/(24*time.Hour)), "day")
	case d%time.Hour == 0:
		return unit(int64(d/time.Hour), "hour")
	case d%time.Minute == 0:
		return unit(int64(d/time.Minute), "minute")
	}
	return d.Round(time.Second).String()
}

// dict builds a map from pairs, to pass several values to a template.
func dict(pairs ...any) (map[string]any, error) {
	if len(pairs)%2 != 0 {
		return nil, fmt.Errorf("dict: odd number of arguments")
	}
	m := make(map[string]any, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		k, ok := pairs[i].(string)
		if !ok {
			return nil, fmt.Errorf("dict: key %v is not a string", pairs[i])
		}
		m[k] = pairs[i+1]
	}
	return m, nil
}
