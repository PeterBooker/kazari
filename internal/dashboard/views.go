package dashboard

import (
	"context"
	"fmt"
	"html/template"
	"strconv"
	"strings"
	"time"

	"github.com/PeterBooker/kazari/internal/views"
)

// viewDays is the range every views panel covers.
const viewDays = 30

// overviewTop is how many of the most viewed pages the overview lists.
const overviewTop = 5

// The chart's geometry, in viewBox units: a slot per day, a bar inside it
// with a gap to the next, and a plot this high above the baseline.
const (
	chartSlot   = 12
	chartWidth  = 10
	chartHeight = 80
	chartRadius = 2
)

// chartBar is one day of a chart.
type chartBar struct {
	X     int    // the left of the day's slot, which takes the pointer
	Path  string // empty on a day without views
	Title string
}

// viewChart is a bar chart of views a day, drawn on the server as SVG.
type viewChart struct {
	Width, Height, Slot int
	Bars                []chartBar
	// Label says what the chart shows, for a screen reader.
	Label    string
	From, To time.Time // the first and the last day
	Days     []views.DayTotals
}

func newViewChart(days []views.DayTotals) viewChart {
	c := viewChart{Width: len(days) * chartSlot, Height: chartHeight, Slot: chartSlot, Days: days}
	if len(days) == 0 {
		return c
	}
	c.From, c.To = days[0].Day, days[len(days)-1].Day
	var peak, total int64
	for _, d := range days {
		peak = max(peak, d.Views)
		total += d.Views
	}
	for i, d := range days {
		b := chartBar{X: i * chartSlot, Title: d.Day.Format("2 Jan") + ": " + howMany(d.Views, "view") + ", " + howMany(d.Visitors, "visitor")}
		if d.Views > 0 {
			// A day with a view never draws flatter than the radius.
			h := max(float64(chartRadius), float64(d.Views)/float64(peak)*chartHeight)
			b.Path = barPath(float64(i*chartSlot+(chartSlot-chartWidth)/2), chartHeight-h, chartWidth, h)
		}
		c.Bars = append(c.Bars, b)
	}
	c.Label = fmt.Sprintf("Views a day from %s to %s: %s in all, at most %s on one day.",
		c.From.Format(dayLayout), c.To.Format(dayLayout), howMany(total, "view"), howMany(peak, "view"))
	return c
}

// barPath is a bar standing on the baseline with its top corners rounded.
func barPath(x, y, w, h float64) string {
	r := min(float64(chartRadius), h/2, w/2)
	n := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
	var b strings.Builder
	base := y + h
	b.WriteString("M" + n(x) + "," + n(base))
	b.WriteString("V" + n(y+r))
	b.WriteString("Q" + n(x) + "," + n(y) + " " + n(x+r) + "," + n(y))
	b.WriteString("H" + n(x+w-r))
	b.WriteString("Q" + n(x+w) + "," + n(y) + " " + n(x+w) + "," + n(y+r))
	b.WriteString("V" + n(base) + "Z")
	return b.String()
}

// viewsPanel is the views section of the overview.
type viewsPanel struct {
	Off   bool // analytics is off
	Chart viewChart
	Total views.Totals
	// Week and LastWeek are the last seven days and the seven before.
	Week, LastWeek views.Totals
	Top            []views.ArtifactTotals
}

func (s *server) viewsPanel(ctx context.Context) (viewsPanel, error) {
	if s.d.Views == nil {
		return viewsPanel{Off: true}, nil
	}
	sum, err := s.d.Views.Summary(ctx, viewDays)
	if err != nil {
		return viewsPanel{}, err
	}
	p := viewsPanel{Chart: newViewChart(sum.Days), Total: sum.Totals, Top: sum.Top[:min(len(sum.Top), overviewTop)]}
	n := len(sum.Days)
	p.Week = sumDays(sum.Days[max(0, n-7):])
	p.LastWeek = sumDays(sum.Days[max(0, n-14):max(0, n-7)])
	return p, nil
}

func sumDays(days []views.DayTotals) views.Totals {
	var t views.Totals
	for _, d := range days {
		t.Views += d.Views
		t.Visitors += d.Visitors
		t.MemberViews += d.MemberViews
		t.LinkViews += d.LinkViews
	}
	return t
}

// pageViews is the views section of a page's own page.
type pageViews struct {
	Off     bool
	Chart   viewChart
	Total   views.Totals
	Files   []views.FileViews
	Readers []views.Reader
	// Members is a members page, the one kind that records its readers.
	Members bool
	// ReadersOff: readers are not recorded on this instance.
	ReadersOff bool
}

func (s *server) pageViews(ctx context.Context, slug string, members bool) (pageViews, error) {
	if s.d.Views == nil {
		return pageViews{Off: true}, nil
	}
	rep, err := s.d.Views.Report(ctx, slug, viewDays)
	if err != nil {
		return pageViews{}, err
	}
	return pageViews{Chart: newViewChart(rep.Days), Total: rep.Totals, Files: rep.Files, Readers: rep.Readers,
		Members: members, ReadersOff: !s.o.ViewReaders}, nil
}

// trend says how the last week compares with the one before.
func trend(now, before int64) string {
	switch {
	case before == 0 && now == 0:
		return "none in the week before either"
	case before == 0:
		return "none in the week before"
	case now == before:
		return "the same as the week before"
	}
	pct := (now - before) * 100 / before
	if pct > 0 {
		return fmt.Sprintf("up %d%% on the week before", pct)
	}
	if pct == 0 {
		return "about the same as the week before"
	}
	return fmt.Sprintf("down %d%% on the week before", -pct)
}

var viewFuncs = template.FuncMap{
	"trend": trend,
}
