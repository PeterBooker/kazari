package update

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/instrument/instrumenttest"
)

func TestAvailableGauge(t *testing.T) {
	rec := instrumenttest.New(t)
	srv, _ := server(t, http.StatusOK, `{"tag_name":"0.8.0"}`)
	check := func(current string) *Checker {
		c := New(Options{Current: current, URL: srv.URL, Now: func() time.Time { return checked }, Telemetry: rec.Providers})
		if err := c.Check(context.Background()); err != nil {
			t.Fatal(err)
		}
		return c
	}
	New(Options{Current: "0.7.0", URL: srv.URL, Telemetry: rec.Providers})
	if _, ok := rec.Sum("kazari.update.available"); ok {
		t.Error("the gauge reports before any check has got an answer")
	}
	check("0.7.0")
	rec.Want("kazari.update.available", 1)
}

func TestAvailableGaugeUpToDate(t *testing.T) {
	rec := instrumenttest.New(t)
	srv, _ := server(t, http.StatusOK, `{"tag_name":"0.8.0"}`)
	c := New(Options{Current: "0.8.0", URL: srv.URL, Telemetry: rec.Providers})
	if err := c.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, ok := rec.Sum("kazari.update.available"); !ok || got != 0 {
		t.Errorf("kazari.update.available = %v (reported %v), want 0", got, ok)
	}
}
