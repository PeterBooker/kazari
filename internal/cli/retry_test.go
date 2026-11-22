package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/api"
)

// busyServer answers status with Retry-After for the first busy requests,
// then 200 with ok, and records each request's method and body.
func busyServer(t *testing.T, busy, status int, retryAfter string, ok any) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, r.Method+" "+string(b))
		n := len(seen)
		mu.Unlock()
		if n <= busy {
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
			reply(w, status, api.ErrorResponse{Error: api.Error{Code: api.CodeRateLimited, Message: "slow down"}})
			return
		}
		reply(w, 200, ok)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func recordingClient(url string) (*Client, *[]time.Duration) {
	var waits []time.Duration
	c := NewClient(url, "tok", nil)
	c.sleep = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}
	return c, &waits
}

func TestClientRetries(t *testing.T) {
	slow := api.ArtifactDetail{Artifact: demo}
	tests := []struct {
		name       string
		busy       int
		status     int
		retryAfter string
		call       func(c *Client) error
		wantSeen   int
		wantWaits  []time.Duration
		wantErr    string
	}{
		{
			name: "read after Retry-After", busy: 2, status: 429, retryAfter: "3",
			call:     func(c *Client) error { _, err := c.Get(t.Context(), "demo", false); return err },
			wantSeen: 3, wantWaits: []time.Duration{3 * time.Second, 3 * time.Second},
		},
		{
			name: "read gives up after two retries", busy: 5, status: 503,
			call:     func(c *Client) error { _, err := c.Get(t.Context(), "demo", false); return err },
			wantSeen: 3, wantWaits: []time.Duration{time.Second, time.Second}, wantErr: api.CodeRateLimited,
		},
		{
			name: "a long Retry-After is cut short", busy: 1, status: 429, retryAfter: "3600",
			call:     func(c *Client) error { _, err := c.Get(t.Context(), "demo", false); return err },
			wantSeen: 2, wantWaits: []time.Duration{30 * time.Second},
		},
		{
			name: "write that expects a version", busy: 1, status: 429, retryAfter: "1",
			call: func(c *Client) error {
				_, err := c.Update(t.Context(), "demo", api.UpdateArtifactRequest{ExpectedVersion: 3, Label: "x"})
				return err
			},
			wantSeen: 2, wantWaits: []time.Duration{time.Second},
		},
		{
			name: "write that expects no version", busy: 1, status: 429, retryAfter: "1",
			call: func(c *Client) error {
				_, err := c.Create(t.Context(), api.CreateArtifactRequest{Slug: "demo"})
				return err
			},
			wantSeen: 1, wantErr: api.CodeRateLimited,
		},
		{
			name: "streamed bundle", busy: 1, status: 429, retryAfter: "1",
			call: func(c *Client) error {
				_, err := c.UpdateBundle(t.Context(), "demo", api.UpdateArtifactRequest{ExpectedVersion: 3}, strings.NewReader("x"))
				return err
			},
			wantSeen: 1, wantErr: api.CodeRateLimited,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, seen := busyServer(t, tt.busy, tt.status, tt.retryAfter, slow)
			c, waits := recordingClient(srv.URL)
			err := tt.call(c)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("err = %v, want none", err)
			case tt.wantErr != "" && !IsCode(err, tt.wantErr):
				t.Errorf("err = %v, want %s", err, tt.wantErr)
			}
			got := seen()
			if len(got) != tt.wantSeen {
				t.Errorf("requests = %d, want %d", len(got), tt.wantSeen)
			}
			for i := 1; i < len(got); i++ {
				if got[i] != got[0] {
					t.Errorf("request %d = %q, want the first one again, %q", i+1, got[i], got[0])
				}
			}
			if diff := cmp.Diff(tt.wantWaits, *waits); diff != "" {
				t.Errorf("waits (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRetryAfter(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want time.Duration
	}{
		{"", time.Second}, {"0", 0}, {"7", 7 * time.Second}, {"31", 30 * time.Second}, {"-3", time.Second}, {"soon", time.Second},
	} {
		if got := retryAfter(tt.in); got != tt.want {
			t.Errorf("retryAfter(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestRunTimeout(t *testing.T) {
	r := newRunner(t.Context(), nil, strings.NewReader(""), io.Discard, io.Discard, envMap(nil))
	if err := r.run([]string{"--timeout", "90s", "version"}); err != nil {
		t.Fatal(err)
	}
	hc := r.httpClient()
	tr, ok := hc.Transport.(*http.Transport)
	if hc.Timeout != 90*time.Second || !ok || tr.ResponseHeaderTimeout != time.Minute {
		t.Errorf("http client: timeout %v, transport %T; want 90s and a 60s header timeout", hc.Timeout, hc.Transport)
	}
}
