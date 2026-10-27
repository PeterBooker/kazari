package app_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/app"
	"github.com/PeterBooker/kazari/internal/cli"
	"github.com/PeterBooker/kazari/internal/config"
)

const adminToken = "test-admin"

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func testConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.BaseDomain = "localhost"
	cfg.DataDir = t.TempDir()
	cfg.ExternalScheme = "http"
	cfg.Listen = config.Listen{HTTP: "127.0.0.1:0", ProxyMode: true}
	cfg.Auth.AdminToken = adminToken
	cfg.Limits.MaxVersionBytes = 4 * config.MiB
	cfg.Limits.MaxFileBytes = 2 * config.MiB
	cfg.Limits.MaxChunkBytes = 1 * config.MiB
	cfg.Lifecycle = config.Lifecycle{
		DefaultGrace: config.Duration(14 * 24 * time.Hour),
		Retention:    config.Duration(30 * 24 * time.Hour),
		Tombstone:    config.Duration(90 * 24 * time.Hour),
		// Never due: the tests trigger jobs through the admin API.
		SweepInterval: config.Duration(1000 * time.Hour),
		GCInterval:    config.Duration(1000 * time.Hour),
	}
	return cfg
}

type instance struct {
	app   *app.App
	clock *clock
	stop  context.CancelFunc
	done  chan error
}

func start(t *testing.T, cfg config.Config) *instance {
	t.Helper()
	clk := &clock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	ctx, cancel := context.WithCancel(context.Background())
	a, err := app.New(ctx, cfg, app.Options{Now: clk.Now, Logger: testLogger()})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	in := &instance{app: a, clock: clk, stop: cancel, done: make(chan error, 1)}
	go func() { in.done <- a.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-in.done:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("Run did not return after cancel")
		}
	})
	select {
	case <-a.Started():
	case err := <-in.done:
		in.done <- err
		t.Fatalf("Run returned before listening: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("listeners not bound within 10s")
	}
	return in
}

func testLogger() *slog.Logger {
	if os.Getenv("KAZARI_TEST_LOG") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.DiscardHandler)
}

// pinnedTransport dials addr for every host, so test URLs need no DNS or port.
func pinnedTransport(addr string) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}
}

func client(addr string) *http.Client {
	return &http.Client{
		Transport:     pinnedTransport(addr),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       30 * time.Second,
	}
}

type response struct {
	status int
	header http.Header
	body   string
}

func do(t *testing.T, hc *http.Client, method, url, token, host string) response {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if host != "" {
		req.Host = host
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response{status: resp.StatusCode, header: resp.Header, body: string(body)}
}

func get(t *testing.T, hc *http.Client, url string) response {
	t.Helper()
	return do(t, hc, http.MethodGet, url, "", "")
}

func wantStatus(t *testing.T, r response, want int, what string) {
	t.Helper()
	if r.status != want {
		t.Fatalf("%s: status %d, want %d; body %q", what, r.status, want, truncate(r.body))
	}
}

func truncate(s string) string {
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}

func bundle(t *testing.T, files map[string]string) *bytes.Buffer {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	if err := cli.TarGz(dir, &buf); err != nil {
		t.Fatal(err)
	}
	return &buf
}

type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(r)
}

const (
	indexHTML = "<!doctype html><title>Demo</title><link rel=stylesheet href=style.css><h1>Hello</h1>"
	styleCSS  = "h1 { color: teal; }"
)

func TestEndToEnd(t *testing.T) {
	cfg := testConfig(t)
	in := start(t, cfg)
	addr := in.app.Addrs().HTTP
	if addr == "" || in.app.Addrs().HTTPS != "" || in.app.Addrs().Admin != "" {
		t.Fatalf("Addrs = %+v", in.app.Addrs())
	}
	hc := client(addr)
	ctx := context.Background()
	const day = 24 * time.Hour

	admin := cli.NewClient("http://admin.localhost", adminToken, hc)
	if _, err := admin.CreateUser(ctx, api.CreateUserRequest{Handle: "peter", Email: "peter@example.com"}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	issued, err := admin.IssueKey(ctx, "peter", api.IssueKeyRequest{Label: "e2e"})
	if err != nil {
		t.Fatalf("issue key: %v", err)
	}
	key := issued.Key
	user := cli.NewClient("http://api.localhost", key, hc)

	files := map[string]string{"index.html": indexHTML, "style.css": styleCSS}
	res, err := user.CreateBundle(ctx, api.CreateManifest{Slug: "demo", Name: "Demo", Description: "e2e demo"}, bundle(t, files))
	if err != nil {
		t.Fatalf("create bundle: %v", err)
	}
	if res.URL != "http://demo.localhost/" || res.Version != 1 {
		t.Fatalf("create = %+v", res)
	}

	page := get(t, hc, "http://demo.localhost/")
	wantStatus(t, page, http.StatusOK, "GET demo /")
	if page.body != indexHTML {
		t.Errorf("index body = %q", page.body)
	}
	for k, want := range map[string]string{
		"Content-Security-Policy": config.DefaultCSP,
		"X-Content-Type-Options":  "nosniff",
		"Cache-Control":           "no-cache",
		"X-Robots-Tag":            "noindex",
	} {
		if got := page.header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if page.header.Get("ETag") == "" {
		t.Error("no ETag")
	}
	if ct := page.header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("index Content-Type = %q", ct)
	}
	css := get(t, hc, "http://demo.localhost/style.css")
	wantStatus(t, css, http.StatusOK, "GET style.css")
	if css.body != styleCSS || !strings.HasPrefix(css.header.Get("Content-Type"), "text/css") {
		t.Errorf("style.css = %q (%s)", css.body, css.header.Get("Content-Type"))
	}

	health := get(t, hc, "http://localhost/health")
	wantStatus(t, health, http.StatusOK, "apex /health")
	var h api.Health
	if err := json.Unmarshal([]byte(health.body), &h); err != nil || h.Status != "ok" || h.Version != "" {
		t.Errorf("apex health = %q (%v); want status ok and no version", health.body, err)
	}
	apex := get(t, hc, "http://localhost/")
	wantStatus(t, apex, http.StatusOK, "apex /")
	if !strings.Contains(apex.body, "localhost") || !strings.HasPrefix(apex.header.Get("Content-Type"), "text/html") ||
		strings.Contains(apex.body, "version") {
		t.Errorf("apex page = %q", truncate(apex.body))
	}
	wantStatus(t, get(t, hc, "http://localhost/nope"), http.StatusNotFound, "apex /nope")

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "v0"}, nil).Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             "http://mcp.localhost/",
		HTTPClient:           &http.Client{Transport: bearer{key, pinnedTransport(addr)}},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		t.Fatalf("mcp connect: %v", err)
	}
	defer cs.Close()
	call, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "update_artifact", Arguments: map[string]any{
		"slug":  "demo",
		"files": []map[string]any{{"path": "about.html", "content": "<h1>About</h1>"}},
	}})
	if err != nil {
		t.Fatalf("update_artifact: %v", err)
	}
	if call.IsError || len(call.Content) == 0 {
		t.Fatalf("update_artifact result = %+v", call)
	}
	text, _ := call.Content[0].(*mcp.TextContent)
	if text == nil || !strings.Contains(text.Text, "version 2") {
		t.Fatalf("update_artifact text = %+v", call.Content[0])
	}

	wantStatus(t, get(t, hc, "http://demo.localhost/about.html"), http.StatusOK, "latest about.html")
	wantStatus(t, get(t, hc, "http://demo--v1.localhost/about.html"), http.StatusNotFound, "v1 about.html")
	v1 := get(t, hc, "http://demo--v1.localhost/")
	wantStatus(t, v1, http.StatusOK, "v1 /")
	if v1.body != indexHTML || !strings.Contains(v1.header.Get("Cache-Control"), "immutable") {
		t.Errorf("v1 / = %q, Cache-Control %q", v1.body, v1.header.Get("Cache-Control"))
	}

	detail, err := user.Get(ctx, "demo", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Versions) != 2 || detail.Artifact.LatestVersion != 2 {
		t.Fatalf("get demo: %d versions, latest %d", len(detail.Versions), detail.Artifact.LatestVersion)
	}

	stale, err := user.MarkStale(ctx, "demo", "")
	if err != nil {
		t.Fatalf("mark stale: %v", err)
	}
	if want := in.clock.Now().Add(14 * day); !stale.ExpiresAt.Equal(want) {
		t.Errorf("expires_at = %v, want %v", stale.ExpiresAt, want)
	}
	wantStatus(t, get(t, hc, "http://demo.localhost/"), http.StatusOK, "stale but in grace")

	sweep := func() {
		t.Helper()
		if err := admin.RunJob(ctx, "sweep"); err != nil {
			t.Fatalf("run sweep: %v", err)
		}
	}
	in.clock.Advance(15 * day)
	sweep()
	wantStatus(t, get(t, hc, "http://demo.localhost/"), http.StatusNotFound, "soft-deleted")
	list, err := admin.AdminListArtifacts(ctx, cli.ListParams{Deleted: "include"})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].Slug != "demo" || list.Items[0].DeletedAt == nil {
		t.Fatalf("admin listing after soft delete = %+v", list.Items)
	}

	in.clock.Advance(31 * day)
	sweep()
	gone := get(t, hc, "http://demo.localhost/")
	wantStatus(t, gone, http.StatusGone, "purged")

	if _, err := user.CreateBundle(ctx, api.CreateManifest{Slug: "demo", Name: "Demo", Description: "x"}, bundle(t, files)); err == nil {
		t.Fatal("re-creating a tombstoned slug succeeded")
	}

	in.clock.Advance(91 * day)
	sweep()
	wantStatus(t, get(t, hc, "http://demo.localhost/"), http.StatusNotFound, "tombstone removed")
	again, err := user.CreateBundle(ctx, api.CreateManifest{Slug: "demo", Name: "Demo again", Description: "x"}, bundle(t, files))
	if err != nil {
		t.Fatalf("re-create after tombstone: %v", err)
	}
	if again.Version != 1 {
		t.Errorf("re-created version = %d, want 1", again.Version)
	}
	wantStatus(t, get(t, hc, "http://demo.localhost/"), http.StatusOK, "re-created")

	wantStatus(t, do(t, hc, http.MethodPost, "http://api.localhost/v1/artifacts", "", ""), http.StatusUnauthorized, "unauthenticated create")
	wantStatus(t, do(t, hc, http.MethodGet, "http://api.localhost/v1/artifacts", "kzk_bogus", ""), http.StatusUnauthorized, "bogus key")
	wantStatus(t, do(t, hc, http.MethodGet, "http://admin.localhost/v1/users", "", ""), http.StatusUnauthorized, "admin without token")
	wantStatus(t, do(t, hc, http.MethodGet, "http://admin.localhost/v1/users", key, ""), http.StatusForbidden, "admin with member key")

	meta := get(t, hc, "http://mcp.localhost/.well-known/oauth-protected-resource")
	wantStatus(t, meta, http.StatusOK, "protected resource metadata")
	var rm struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
	}
	if err := json.Unmarshal([]byte(meta.body), &rm); err != nil {
		t.Fatalf("metadata %q: %v", meta.body, err)
	}
	if rm.Resource != "http://mcp.localhost/" || len(rm.AuthorizationServers) != 1 || rm.AuthorizationServers[0] != "http://mcp.localhost" {
		t.Errorf("metadata = %+v", rm)
	}
	unauth := do(t, hc, http.MethodPost, "http://mcp.localhost/mcp", "", "")
	wantStatus(t, unauth, http.StatusUnauthorized, "mcp without token")
	if got := unauth.header.Get("WWW-Authenticate"); !strings.Contains(got, `resource_metadata="http://mcp.localhost/.well-known/oauth-protected-resource"`) {
		t.Errorf("WWW-Authenticate = %q", got)
	}

	metrics := get(t, hc, "http://admin.localhost/metrics")
	wantStatus(t, metrics, http.StatusOK, "metrics")
	for _, want := range []string{"kazari_http_requests_total", `surface="site"`, `surface="mcp"`, `job="sweep"`} {
		if !strings.Contains(metrics.body, want) {
			t.Errorf("metrics lack %s", want)
		}
	}

	notAPI := do(t, hc, http.MethodGet, "http://api.localhost/", key, "")
	wantStatus(t, notAPI, http.StatusNotFound, "api /")
	if !strings.HasPrefix(notAPI.header.Get("Content-Type"), "application/json") {
		t.Errorf("api / Content-Type = %q", notAPI.header.Get("Content-Type"))
	}
	if notAPI.header.Get("Referrer-Policy") != "no-referrer" || notAPI.header.Get("Cache-Control") != "no-store" {
		t.Errorf("api / lacks the control-plane headers: %v", notAPI.header)
	}

	mixed := do(t, hc, http.MethodGet, "http://api.localhost/health", "", "Admin.Localhost")
	wantStatus(t, mixed, http.StatusOK, "Admin.Localhost /health")
	wantStatus(t, do(t, hc, http.MethodGet, "http://api.localhost/v1/users", adminToken, "ADMIN.localhost."), http.StatusOK, "ADMIN.localhost. /v1/users")
	wantStatus(t, do(t, hc, http.MethodGet, "http://x/", "", "Demo.LOCALHOST"), http.StatusOK, "Demo.LOCALHOST /")
}

func TestHTTPSSelfSigned(t *testing.T) {
	cfg := testConfig(t)
	cfg.ExternalScheme = "https"
	cfg.TLS.Mode = "self-signed"
	cfg.Listen = config.Listen{HTTPS: "127.0.0.1:0", HTTP: "127.0.0.1:0"}
	in := start(t, cfg)
	addrs := in.app.Addrs()
	if addrs.HTTPS == "" || addrs.HTTP == "" {
		t.Fatalf("Addrs = %+v", addrs)
	}

	tr := pinnedTransport(addrs.HTTPS)
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true, ServerName: "demo.localhost"}
	hc := &http.Client{Transport: tr, Timeout: 30 * time.Second}
	resp, err := hc.Get("https://demo.localhost/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "Not found") {
		t.Errorf("unknown slug over https: %d %q", resp.StatusCode, truncate(string(body)))
	}
	if resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 {
		t.Fatal("no peer certificate")
	}
	if err := resp.TLS.PeerCertificates[0].VerifyHostname("demo.localhost"); err != nil {
		t.Errorf("certificate does not cover demo.localhost: %v", err)
	}

	health := do(t, hc, http.MethodGet, "https://localhost/health", "", "")
	wantStatus(t, health, http.StatusOK, "apex health over https")

	plain := client(addrs.HTTP)
	r := do(t, plain, http.MethodGet, "http://127.0.0.1/x?y=1", "", "demo.localhost")
	wantStatus(t, r, http.StatusMovedPermanently, "http redirect")
	if loc := r.header.Get("Location"); loc != "https://demo.localhost/x?y=1" {
		t.Errorf("Location = %q", loc)
	}
	r = do(t, plain, http.MethodGet, "http://127.0.0.1/x", "", "demo.localhost:8080")
	if loc := r.header.Get("Location"); loc != "https://demo.localhost/x" {
		t.Errorf("Location with port = %q", loc)
	}
	wantStatus(t, do(t, plain, http.MethodGet, "http://127.0.0.1/x", "", "evil.example"), http.StatusNotFound, "redirect for a foreign host")
	wantStatus(t, do(t, plain, http.MethodGet, "http://127.0.0.1/health", "", "demo.localhost"), http.StatusOK, "plain /health")
}

func TestAdminListener(t *testing.T) {
	cfg := testConfig(t)
	cfg.Admin.Listen = "127.0.0.1:0"
	in := start(t, cfg)
	addr := in.app.Addrs().Admin
	if addr == "" {
		t.Fatalf("Addrs = %+v", in.app.Addrs())
	}
	hc := client(addr)
	wantStatus(t, do(t, hc, http.MethodGet, "http://"+addr+"/health", "", ""), http.StatusOK, "admin listener /health")
	wantStatus(t, do(t, hc, http.MethodGet, "http://"+addr+"/v1/users", "", ""), http.StatusUnauthorized, "admin listener without token")
	wantStatus(t, do(t, hc, http.MethodGet, "http://"+addr+"/v1/users", adminToken, "anything.example"), http.StatusOK, "admin listener with token")
	jobs := do(t, hc, http.MethodGet, "http://"+addr+"/v1/jobs", adminToken, "")
	wantStatus(t, jobs, http.StatusOK, "jobs")
	for _, name := range []string{"sweep", "gc", "uploads-expire", "oauth-cleanup"} {
		if !strings.Contains(jobs.body, `"`+name+`"`) {
			t.Errorf("jobs lack %s: %s", name, jobs.body)
		}
	}
	for _, name := range []string{"gc", "uploads-expire", "oauth-cleanup"} {
		if err := in.app.Scheduler().Trigger(context.Background(), name); err != nil {
			t.Errorf("trigger %s: %v", name, err)
		}
	}

	pub := client(in.app.Addrs().HTTP)
	wantStatus(t, get(t, pub, "http://admin.localhost/metrics"), http.StatusNotFound, "public admin /metrics")
	wantStatus(t, do(t, pub, http.MethodGet, "http://admin.localhost/v1/users", adminToken, ""), http.StatusNotFound, "public admin /v1/users")
}

func TestWriteBudgetShared(t *testing.T) {
	cfg := testConfig(t)
	cfg.Limits.WriteRatePerMinute = 2
	in := start(t, cfg)
	hc := client(in.app.Addrs().HTTP)
	ctx := context.Background()
	admin := cli.NewClient("http://admin.localhost", adminToken, hc)
	if _, err := admin.CreateUser(ctx, api.CreateUserRequest{Handle: "peter"}); err != nil {
		t.Fatal(err)
	}
	issued, err := admin.IssueKey(ctx, "peter", api.IssueKeyRequest{Label: "rate"})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		wantStatus(t, do(t, hc, http.MethodPost, "http://api.localhost/v1/uploads", issued.Key, ""), http.StatusCreated,
			fmt.Sprintf("REST write %d", i+1))
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "v0"}, nil).Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             "http://mcp.localhost/",
		HTTPClient:           &http.Client{Transport: bearer{issued.Key, pinnedTransport(in.app.Addrs().HTTP)}},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		t.Fatalf("mcp connect: %v", err)
	}
	defer cs.Close()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "begin_upload", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := res.Content[0].(*mcp.TextContent); !res.IsError || text == nil || !strings.HasPrefix(text.Text, "rate_limited: ") {
		t.Errorf("MCP write over the shared budget = %+v", res.Content)
	}
}

func TestGracefulShutdown(t *testing.T) {
	cfg := testConfig(t)
	clk := &clock{t: time.Now()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, err := app.New(ctx, cfg, app.Options{Now: clk.Now, Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	select {
	case <-a.Started():
	case err := <-done:
		t.Fatalf("Run: %v", err)
	}
	addr := a.Addrs().HTTP
	wantStatus(t, get(t, client(addr), "http://localhost/health"), http.StatusOK, "health before shutdown")

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s")
	}
	if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		conn.Close()
		t.Error("port still accepting connections after shutdown")
	}
	if err := a.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestListenError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cfg := testConfig(t)
	cfg.Listen.HTTP = ln.Addr().String()
	a, err := app.New(context.Background(), cfg, app.Options{Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- a.Run(context.Background()) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "listen") {
			t.Errorf("Run = %v, want a listen error", err)
		}
	case <-a.Started():
		t.Fatal("Started closed although the port is taken")
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not fail")
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	cfg := testConfig(t)
	cfg.BaseDomain = "Bad Domain"
	if _, err := app.New(context.Background(), cfg, app.Options{}); err == nil || !strings.Contains(err.Error(), "base_domain") {
		t.Errorf("New = %v, want a base_domain error", err)
	}
	cfg = testConfig(t)
	cfg.Listen.ProxyMode = false
	cfg.Listen.HTTPS = "127.0.0.1:0"
	cfg.TLS.Mode = "files"
	cfg.TLS.CertFile = filepath.Join(t.TempDir(), "missing.pem")
	cfg.TLS.KeyFile = cfg.TLS.CertFile
	_, err := app.New(context.Background(), cfg, app.Options{})
	if err == nil {
		t.Fatal("New with missing certificate files succeeded")
	}
	if errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
