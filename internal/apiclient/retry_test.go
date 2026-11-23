package apiclient_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/apiclient"
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

func recordingClient(url string) (*apiclient.Client, *[]time.Duration) {
	var waits []time.Duration
	c := apiclient.New(url, "tok", apiclient.Options{Sleep: func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}})
	return c, &waits
}

func TestClientRetries(t *testing.T) {
	slow := api.ArtifactDetail{Artifact: api.Artifact{Slug: "demo", PublishedVersion: 2, LatestVersion: 3}}
	tests := []struct {
		name       string
		busy       int
		status     int
		retryAfter string
		call       func(c *apiclient.Client) error
		wantSeen   int
		wantWaits  []time.Duration
		wantErr    string
	}{
		{
			name: "read after Retry-After", busy: 2, status: 429, retryAfter: "3",
			call:     func(c *apiclient.Client) error { _, err := c.Get(t.Context(), "demo", false); return err },
			wantSeen: 3, wantWaits: []time.Duration{3 * time.Second, 3 * time.Second},
		},
		{
			name: "read gives up after two retries", busy: 5, status: 503,
			call:     func(c *apiclient.Client) error { _, err := c.Get(t.Context(), "demo", false); return err },
			wantSeen: 3, wantWaits: []time.Duration{time.Second, time.Second}, wantErr: api.CodeRateLimited,
		},
		{
			name: "a long Retry-After is cut short", busy: 1, status: 429, retryAfter: "3600",
			call:     func(c *apiclient.Client) error { _, err := c.Get(t.Context(), "demo", false); return err },
			wantSeen: 2, wantWaits: []time.Duration{30 * time.Second},
		},
		{
			name: "write that expects a version", busy: 1, status: 429, retryAfter: "1",
			call: func(c *apiclient.Client) error {
				_, err := c.Update(t.Context(), "demo", api.UpdateArtifactRequest{ExpectedVersion: 3, Label: "x"})
				return err
			},
			wantSeen: 2, wantWaits: []time.Duration{time.Second},
		},
		{
			name: "write that expects no version", busy: 1, status: 429, retryAfter: "1",
			call: func(c *apiclient.Client) error {
				_, err := c.Create(t.Context(), api.CreateArtifactRequest{Slug: "demo"})
				return err
			},
			wantSeen: 1, wantErr: api.CodeRateLimited,
		},
		{
			name: "streamed bundle", busy: 1, status: 429, retryAfter: "1",
			call: func(c *apiclient.Client) error {
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
			case tt.wantErr != "" && !apiclient.IsCode(err, tt.wantErr):
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

// roundTrip answers every request without a network.
type roundTrip func(*http.Request) *http.Response

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r), nil }

func answer(status int, header http.Header, body string) roundTrip {
	return func(r *http.Request) *http.Response {
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: r}
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"", time.Second}, {"0", 0}, {"7", 7 * time.Second}, {"31", 30 * time.Second}, {"-3", time.Second}, {"soon", time.Second},
		{now.Add(12 * time.Second).Format(http.TimeFormat), 12 * time.Second},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0},
	}
	for _, tt := range tests {
		var waits []time.Duration
		c := apiclient.New("https://api.art.test", "tok", apiclient.Options{
			HTTP: &http.Client{Transport: answer(http.StatusTooManyRequests, http.Header{"Retry-After": {tt.in}}, "")},
			Now:  func() time.Time { return now },
			Sleep: func(_ context.Context, d time.Duration) error {
				waits = append(waits, d)
				return nil
			},
		})
		_, _ = c.Health(t.Context())
		if len(waits) == 0 || waits[0] != tt.want {
			t.Errorf("Retry-After %q: waits %v, want %v first", tt.in, waits, tt.want)
		}
	}
}

func TestNotKazari(t *testing.T) {
	tests := []struct {
		base, host, want string
	}{
		{"https://mcp.art.test", "api", "; try --url https://api.art.test"},
		{"http://admin.kazari.localhost:8080", "api", "; try --url http://api.kazari.localhost:8080"},
		{"https://api.art.test", "admin", "; try --url https://admin.art.test"},
		{"https://art.test", "api", "https://api.<base>"},
		{"https://hello.art.test", "api", "https://api.<base>"},
		{"https://api.art.test", "api", "https://api.<base>"},
	}
	for _, tt := range tests {
		c := apiclient.New(tt.base, "", apiclient.Options{Host: tt.host,
			HTTP: &http.Client{Transport: answer(http.StatusNotFound, http.Header{}, "404 page not found\n")}})
		_, err := c.Health(t.Context())
		if err == nil || !strings.HasSuffix(err.Error(), tt.want) {
			t.Errorf("a 404 for %s on %s: %v, want an error that ends %q", tt.host, tt.base, err, tt.want)
		}
	}
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
