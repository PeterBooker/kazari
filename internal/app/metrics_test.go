package app_test

import (
	"context"
	"flag"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/cli"
)

var updateMetrics = flag.Bool("update-metrics", false, "rewrite testdata/metrics*.txt from a scrape")

// TestMetricNames scrapes every family the server registers in both
// exposition formats and compares the # TYPE lines with the golden files, so
// any rename shows in review, whether Kazari, the exporter or a semantic
// conventions upgrade made it.
func TestMetricNames(t *testing.T) {
	in := start(t, testConfig(t))
	hc := client(in.app.Addrs().HTTP)
	for _, url := range []string{
		"http://hello.localhost/",
		"http://api.localhost/v1/artifacts",
		"http://mcp.localhost/.well-known/oauth-protected-resource",
		"http://localhost/health",
		"http://admin.localhost/health",
	} {
		get(t, hc, url)
	}
	do(t, hc, http.MethodGet, "http://admin.localhost/v1/nothing", adminToken, "")
	key := issueKey(t, hc)
	mcpCall(t, hc, key, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_artifacts","arguments":{}}}`)
	user := cli.NewClient("http://api.localhost", key, hc)
	if _, err := user.CreateBundle(context.Background(), api.CreateManifest{Slug: "demo"}, bundle(t, map[string]string{"index.html": indexHTML})); err != nil {
		t.Fatal(err)
	}
	get(t, hc, "http://demo.localhost/")
	if err := in.app.Scheduler().Trigger(context.Background(), "sweep"); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct{ file, accept string }{
		{"metrics.txt", "text/plain;version=0.0.4"},
		{"metrics.openmetrics.txt", "application/openmetrics-text;version=1.0.0"},
	} {
		t.Run(tt.file, func(t *testing.T) {
			got := typeLines(t, scrape(t, hc, tt.accept))
			path := filepath.Join("testdata", tt.file)
			if *updateMetrics {
				if err := os.WriteFile(path, []byte(strings.Join(got, "\n")+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			want := strings.Split(strings.TrimSpace(string(data)), "\n")
			for _, l := range want {
				if !slices.Contains(got, l) {
					t.Errorf("missing: %s", l)
				}
			}
			for _, l := range got {
				if !slices.Contains(want, l) {
					t.Errorf("unexpected: %s", l)
				}
			}
			if t.Failed() {
				t.Log("run go test ./internal/app -run TestMetricNames -update-metrics if the change is intended")
			}
		})
	}
}

// issueKey creates a member and returns an API key for them.
func issueKey(t *testing.T, hc *http.Client) string {
	t.Helper()
	admin := cli.NewClient("http://admin.localhost", adminToken, hc)
	if _, err := admin.CreateUser(context.Background(), api.CreateUserRequest{Handle: "metrics"}); err != nil {
		t.Fatal(err)
	}
	issued, err := admin.IssueKey(context.Background(), "metrics", api.IssueKeyRequest{})
	if err != nil {
		t.Fatal(err)
	}
	return issued.Key
}

// mcpCall posts one JSON-RPC request in the 2025-11-25 protocol, which
// needs no per-request headers.
func mcpCall(t *testing.T, hc *http.Client, key, body string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://mcp.localhost/", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mcp: status %d", resp.StatusCode)
	}
}

func scrape(t *testing.T, hc *http.Client, accept string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://admin.localhost/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", accept)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics: status %d: %s", resp.StatusCode, body)
	}
	return string(body)
}

// typeLines leaves out go_memory_limit, which exists only when the
// environment sets a memory limit.
func typeLines(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for line := range strings.SplitSeq(body, "\n") {
		if strings.HasPrefix(line, "# TYPE ") && !strings.HasPrefix(line, "# TYPE go_memory_limit") {
			out = append(out, line)
		}
	}
	slices.Sort(out)
	return out
}

func TestMetricsScrapeToken(t *testing.T) {
	const token = "scrape-0123456789abcdef0123456789"
	public := func(t *testing.T, metricsToken string) *http.Client {
		cfg := testConfig(t)
		cfg.BaseDomain = "art.test"
		cfg.Auth.AdminToken = ""
		cfg.Telemetry.Metrics.Token = metricsToken
		in := start(t, cfg)
		return client(in.app.Addrs().HTTP)
	}
	t.Run("no token", func(t *testing.T) {
		wantStatus(t, get(t, public(t, ""), "http://admin.art.test/metrics"), http.StatusNotFound, "public /metrics without a token")
	})
	t.Run("token", func(t *testing.T) {
		hc := public(t, token)
		wantStatus(t, get(t, hc, "http://admin.art.test/metrics"), http.StatusUnauthorized, "no bearer")
		wantStatus(t, do(t, hc, http.MethodGet, "http://admin.art.test/metrics", "wrong", ""), http.StatusUnauthorized, "wrong bearer")
		wantStatus(t, do(t, hc, http.MethodGet, "http://admin.art.test/metrics", token, ""), http.StatusOK, "right bearer")
	})
	t.Run("admin listener", func(t *testing.T) {
		cfg := testConfig(t)
		cfg.BaseDomain = "art.test"
		cfg.Auth.AdminToken = ""
		cfg.Admin.Listen = "127.0.0.1:0"
		in := start(t, cfg)
		wantStatus(t, get(t, client(in.app.Addrs().Admin), "http://admin.art.test/metrics"), http.StatusOK, "admin listener")
		wantStatus(t, get(t, client(in.app.Addrs().HTTP), "http://admin.art.test/metrics"), http.StatusNotFound, "public listener")
	})
}
