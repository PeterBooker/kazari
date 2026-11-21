package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var checked = time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)

// server answers every request with status and body, counting them.
func server(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if ua := r.Header.Get("User-Agent"); ua != "kazari" {
			t.Errorf("User-Agent = %q, want kazari", ua)
		}
		if r.URL.RawQuery != "" || r.Header.Get("Referer") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Errorf("the request carries more than it should: %q %v", r.URL.RawQuery, r.Header)
		}
		for name, values := range r.Header {
			for _, v := range values {
				if strings.Contains(v, "0.7.0") || strings.Contains(v, "example.com") {
					t.Errorf("header %s identifies the instance: %q", name, v)
				}
			}
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func checker(current, url string) *Checker {
	return New(Options{Current: current, URL: url, Now: func() time.Time { return checked }})
}

func TestCheck(t *testing.T) {
	tests := []struct {
		name, current, body string
		want                Result
	}{
		{"newer", "0.7.0", `{"tag_name":"0.8.0"}`, Result{Latest: "0.8.0", Newer: true, CheckedAt: checked}},
		{"newer patch", "0.7.0", `{"tag_name":"0.7.1"}`, Result{Latest: "0.7.1", Newer: true, CheckedAt: checked}},
		{"numeric, not lexical", "0.9.0", `{"tag_name":"0.10.0"}`, Result{Latest: "0.10.0", Newer: true, CheckedAt: checked}},
		{"leading v", "v0.7.0", `{"tag_name":"v1.0.0"}`, Result{Latest: "1.0.0", Newer: true, CheckedAt: checked}},
		{"same", "0.7.0", `{"tag_name":"0.7.0"}`, Result{Latest: "0.7.0", CheckedAt: checked}},
		{"same, shorter", "1.0.0", `{"tag_name":"1.0"}`, Result{Latest: "1.0", CheckedAt: checked}},
		{"older", "0.8.0", `{"tag_name":"0.7.3"}`, Result{Latest: "0.7.3", CheckedAt: checked}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, hits := server(t, http.StatusOK, tt.body)
			c := checker(tt.current, srv.URL)
			if !c.Enabled() {
				t.Fatal("not enabled")
			}
			if err := c.Check(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := c.Result(); got != tt.want {
				t.Errorf("Result = %+v, want %+v", got, tt.want)
			}
			if hits.Load() != 1 {
				t.Errorf("requests = %d, want 1", hits.Load())
			}
		})
	}
}

func TestCheckFailureKeepsLastResult(t *testing.T) {
	failures := map[string]struct {
		status int
		body   string
	}{
		"server error":  {http.StatusInternalServerError, `{"tag_name":"9.9.9"}`},
		"rate limited":  {http.StatusForbidden, `{"message":"API rate limit exceeded"}`},
		"malformed":     {http.StatusOK, `<html>not json`},
		"no tag":        {http.StatusOK, `{}`},
		"not a version": {http.StatusOK, `{"tag_name":"nightly"}`},
		"pre-release":   {http.StatusOK, `{"tag_name":"9.9.9","prerelease":true}`},
		"draft":         {http.StatusOK, `{"tag_name":"9.9.9","draft":true}`},
		"rc tag":        {http.StatusOK, `{"tag_name":"9.9.9-rc.1"}`},
		"huge body":     {http.StatusOK, `{"tag_name":"9.9.9","body":"` + strings.Repeat("x", maxBodyBytes) + `"}`},
	}
	for name, f := range failures {
		t.Run(name, func(t *testing.T) {
			ok, _ := server(t, http.StatusOK, `{"tag_name":"0.7.0"}`)
			bad, _ := server(t, f.status, f.body)
			c := checker("0.7.0", ok.URL)
			if err := c.Check(context.Background()); err != nil {
				t.Fatal(err)
			}
			before := c.Result()
			c.url = bad.URL
			if err := c.Check(context.Background()); err == nil {
				t.Error("Check = nil, want an error for the scheduler to log")
			}
			if got := c.Result(); got != before || got.Newer {
				t.Errorf("Result after a failure = %+v, want %+v", got, before)
			}
		})
	}
}

func TestCheckUnreachable(t *testing.T) {
	srv, _ := server(t, http.StatusOK, `{"tag_name":"9.9.9"}`)
	srv.Close()
	c := checker("0.7.0", srv.URL)
	if err := c.Check(context.Background()); err == nil {
		t.Error("Check against a closed server = nil")
	}
	if got := c.Result(); got != (Result{}) {
		t.Errorf("Result = %+v, want none", got)
	}
}

func TestUnstampedBuildNeverAsks(t *testing.T) {
	for _, current := range []string{"", "dev", "(devel)", "v0.7.1-0.20261121093000-abcdef123456", "0.7.0+dirty", "0.7.0-rc.1", "1.2.3.4.5", "1..2"} {
		srv, hits := server(t, http.StatusOK, `{"tag_name":"9.9.9"}`)
		c := checker(current, srv.URL)
		if c.Enabled() {
			t.Errorf("%q: enabled", current)
		}
		if err := c.Check(context.Background()); err != nil {
			t.Errorf("%q: Check = %v", current, err)
		}
		if hits.Load() != 0 || c.Result() != (Result{}) {
			t.Errorf("%q: %d requests, Result %+v; want none", current, hits.Load(), c.Result())
		}
	}
}
