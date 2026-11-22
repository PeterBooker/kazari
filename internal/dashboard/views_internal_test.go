package dashboard

import (
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/views"
)

func TestTrend(t *testing.T) {
	tests := []struct {
		now, before int64
		want        string
	}{
		{0, 0, "none in the week before either"},
		{3, 0, "none in the week before"},
		{4, 4, "the same as the week before"},
		{6, 4, "up 50% on the week before"},
		{1, 4, "down 75% on the week before"},
		{1000, 1001, "about the same as the week before"},
	}
	for _, tt := range tests {
		if got := trend(tt.now, tt.before); got != tt.want {
			t.Errorf("trend(%d, %d) = %q, want %q", tt.now, tt.before, got, tt.want)
		}
	}
}

func TestViewChart(t *testing.T) {
	day := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	c := newViewChart([]views.DayTotals{
		{Day: day, Views: 4, Visitors: 2},
		{Day: day.AddDate(0, 0, 1)},
		{Day: day.AddDate(0, 0, 2), Views: 1, Visitors: 1},
	})
	if c.Width != 3*chartSlot || c.Height != chartHeight {
		t.Errorf("newViewChart: %dx%d, want %dx%d", c.Width, c.Height, 3*chartSlot, chartHeight)
	}
	if want := "M1,80V2Q1,0 3,0H9Q11,0 11,2V80Z"; c.Bars[0].Path != want {
		t.Errorf("the peak's bar = %q, want %q", c.Bars[0].Path, want)
	}
	if c.Bars[1].Path != "" {
		t.Errorf("a day without views has the bar %q, want none", c.Bars[1].Path)
	}
	if want := "M25,80V62Q25,60 27,60H33Q35,60 35,62V80Z"; c.Bars[2].Path != want {
		t.Errorf("a quarter of the peak = %q, want %q", c.Bars[2].Path, want)
	}
	if want := "20 Nov: 4 views, 2 visitors"; c.Bars[0].Title != want {
		t.Errorf("Title = %q, want %q", c.Bars[0].Title, want)
	}
	if want := "Views a day from 20 Nov 2026 to 22 Nov 2026: 5 views in all, at most 4 views on one day."; c.Label != want {
		t.Errorf("Label = %q, want %q", c.Label, want)
	}
}
