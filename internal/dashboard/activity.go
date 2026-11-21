package dashboard

import (
	"encoding/csv"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/PeterBooker/kazari/internal/audit"
)

var activityFuncs = template.FuncMap{
	"describe": describe,
}

func (s *server) activityRoutes(mux *http.ServeMux) {
	s.view(mux, "GET /activity", s.activity)
	s.action(mux, "POST /activity/export", s.activityExport)
}

const (
	activityPageSize = 50
	// exportPageSize is the store's largest page; exportMaxRows bounds one
	// download.
	exportPageSize = 1000
	exportMaxRows  = 100_000
	dateLayout     = "2006-01-02"
	maxFilterLen   = 100
)

// activityKinds group the actions by what they are about, for the filter. A
// key is what travels in the URL; an entry ending in "." is a prefix, as
// audit.Query reads it.
var activityKinds = []struct {
	Key, Label string
	actions    []string
}{
	{"pages", "Pages", []string{"artifact."}},
	{"links", "Share links", []string{"share_link."}},
	{"people", "People", []string{"user.", "invite.", "password.", "session."}},
	{"keys", "Keys", []string{"key."}},
	{"apps", "Connected apps and sign-ins", []string{"oauth.", "dashboard."}},
	{"operations", "Operations", []string{"backup.", "audit."}},
}

// activityFilter is the filter of the Activity page as the person typed it.
// It travels in the URL of the page and in the hidden fields of the export
// form.
type activityFilter struct {
	Actor, Slug, Kind string
	From, To          string // dates, UTC; To includes its whole day
	// FromError and ToError say why a date was not understood; the filter
	// then leaves it out.
	FromError, ToError string
}

func readActivityFilter(v url.Values) activityFilter {
	clip := func(s string) string {
		s = strings.TrimSpace(s)
		if len(s) > maxFilterLen {
			s = s[:maxFilterLen]
		}
		return s
	}
	return activityFilter{
		Actor: clip(v.Get("person")), Slug: clip(v.Get("slug")), Kind: clip(v.Get("kind")),
		From: clip(v.Get("from")), To: clip(v.Get("to")),
	}
}

// values are the filter's fields that are set, as a query string keeps them.
func (f activityFilter) values() url.Values {
	v := url.Values{}
	for k, val := range map[string]string{"person": f.Actor, "slug": f.Slug, "kind": f.Kind, "from": f.From, "to": f.To} {
		if val != "" {
			v.Set(k, val)
		}
	}
	return v
}

// Set reports whether the filter narrows the list at all.
func (f activityFilter) Set() bool { return len(f.values()) > 0 }

// address is the Activity page under the filter, continued at cursor.
func (f activityFilter) address(cursor string) string {
	v := f.values()
	if cursor != "" {
		v.Set("cursor", cursor)
	}
	if len(v) == 0 {
		return "/activity"
	}
	return "/activity?" + v.Encode()
}

// query turns the filter into the store's, and notes on f the dates it
// could not read.
func (f *activityFilter) query() audit.Query {
	q := audit.Query{Actor: f.Actor, Slug: f.Slug}
	for _, k := range activityKinds {
		if k.Key == f.Kind {
			q.Actions = k.actions
		}
	}
	const hint = "Use a date such as 2026-11-21."
	if f.From != "" {
		if t, err := time.Parse(dateLayout, f.From); err == nil {
			q.Since = t
		} else {
			f.FromError = hint
		}
	}
	if f.To != "" {
		// Until is exclusive, and the day named is meant to be included.
		if t, err := time.Parse(dateLayout, f.To); err == nil {
			q.Until = t.AddDate(0, 0, 1)
		} else {
			f.ToError = hint
		}
	}
	return q
}

type activityData struct {
	Filter activityFilter
	Kinds  any
	Events []audit.Event
	// Next is the address of the next, older page, "" on the last. The
	// cursor only goes forward, so the way back is First, set on every page
	// but the first.
	Next, First string
}

func (s *server) activity(w http.ResponseWriter, r *http.Request, c *request) error {
	f := readActivityFilter(r.URL.Query())
	q := f.query()
	q.Limit = activityPageSize
	q.Cursor = r.URL.Query().Get("cursor")
	found, err := s.d.Audit.List(r.Context(), q)
	if errors.Is(err, audit.ErrInvalidQuery) {
		return refused("That page of results is no longer there. Start again from the first page of the activity.")
	}
	if err != nil {
		return err
	}
	data := activityData{Filter: f, Kinds: activityKinds, Events: found.Events}
	if q.Cursor != "" {
		data.First = f.address("")
	}
	if found.NextCursor != "" {
		data.Next = f.address(found.NextCursor)
	}
	return s.render(w, r, c, http.StatusOK, "activity", page{Title: "Activity", Nav: "activity"}, data)
}

// activityExport answers the filtered log as CSV, every page of it, and
// records that it was exported: the log names people, and a copy leaves
// the instance.
func (s *server) activityExport(w http.ResponseWriter, r *http.Request, c *request) error {
	ctx := r.Context()
	f := readActivityFilter(r.PostForm)
	q := f.query()
	if f.FromError != "" || f.ToError != "" {
		return refused("The dates of the filter could not be read. Go back, correct them and export again.")
	}
	q.Limit = exportPageSize
	// The first page is read before a byte is written, so a failure is
	// still an error page and not half a file.
	found, err := s.d.Audit.List(ctx, q)
	if err != nil {
		return err
	}

	h := w.Header()
	h.Set("Content-Type", "text/csv; charset=utf-8")
	h.Set("Content-Disposition", `attachment; filename="kazari-activity-`+s.now().UTC().Format(dateLayout)+`.csv"`)
	h.Set("Cache-Control", "no-store")
	out := csv.NewWriter(w)
	_ = out.Write([]string{"time", "actor", "action", "slug", "what happened", "details"})
	rows, cut := 0, false
	for {
		for _, e := range found.Events {
			if rows == exportMaxRows {
				cut = true
				break
			}
			_ = out.Write([]string{
				e.At.UTC().Format(time.RFC3339), inert(e.Actor), inert(e.Action), inert(e.Slug),
				inert(describe(e).String()), inert(detailPairs(e.Details)),
			})
			rows++
		}
		if cut || found.NextCursor == "" {
			break
		}
		out.Flush()
		q.Cursor = found.NextCursor
		if found, err = s.d.Audit.List(ctx, q); err != nil {
			// The status is sent; the file can only say it is incomplete.
			s.log.ErrorContext(ctx, "dashboard: export activity", "err", err)
			_ = out.Write([]string{"", "", "", "", "The export stopped here: the server could not read further.", ""})
			break
		}
	}
	if cut {
		_ = out.Write([]string{"", "", "", "", "The export was cut at " + strconv.Itoa(exportMaxRows) + " rows. Narrow the dates to get the rest.", ""})
	}
	out.Flush()

	details := map[string]string{"rows": strconv.Itoa(rows)}
	for k, v := range f.values() {
		details[k] = v[0]
	}
	if err := s.d.Recorder.Record(ctx, audit.Event{At: s.now(), Actor: c.me.Handle, Action: audit.AuditExport, Details: details}); err != nil {
		s.log.WarnContext(ctx, "dashboard: audit", "err", err)
	}
	return nil
}

// inert keeps a spreadsheet from running a cell as a formula: one that
// starts with =, +, -, @, a tab or a carriage return gets an apostrophe
// first. The log holds names that people and agents chose.
func inert(cell string) string {
	if cell != "" && strings.ContainsRune("=+-@\t\r", rune(cell[0])) {
		return "'" + cell
	}
	return cell
}
