package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/PeterBooker/kazari/internal/app"
	"github.com/PeterBooker/kazari/internal/config"
	"github.com/PeterBooker/kazari/internal/dirlock"
)

func TestDataDirHeldByOneServer(t *testing.T) {
	cfg := testConfig(t)
	in := start(t, cfg)
	socket := in.app.Addrs().Socket

	second := cfg
	second.Admin.Socket = filepath.Join(t.TempDir(), "other.sock")
	_, err := app.New(t.Context(), second, app.Options{Logger: testLogger()})
	var locked *dirlock.LockedError
	if !errors.As(err, &locked) || locked.PID != os.Getpid() {
		t.Fatalf("second app.New on the same data_dir = %v, want a LockedError naming pid %d", err, os.Getpid())
	}
	if !strings.Contains(err.Error(), "another kazari (pid ") || !strings.Contains(err.Error(), cfg.DataDir) {
		t.Errorf("second app.New error = %q, want it to name the holder and the directory", err)
	}
	if _, err := os.Stat(socket); err != nil {
		t.Errorf("the first server's admin socket after the second failed: %v", err)
	}
}

func TestAdminSocketOfAnotherServer(t *testing.T) {
	cfg := testConfig(t)
	in := start(t, cfg)
	socket := in.app.Addrs().Socket

	// Another data directory, the same absolute socket.
	second := testConfig(t)
	second.Admin.Socket = socket
	a, err := app.New(t.Context(), second, app.Options{Logger: testLogger(), Now: in.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	err = a.Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "another server answers on the admin socket") {
		t.Errorf("Run with a live socket at admin.socket = %v, want a refusal", err)
	}
	if fi, err := os.Stat(socket); err != nil || fi.Mode().Type() != os.ModeSocket {
		t.Fatalf("the first server's socket after the second gave up: %v", err)
	}
	if _, err := (&net.Dialer{}).DialContext(t.Context(), "unix", socket); err != nil {
		t.Errorf("dial the first server's socket: %v", err)
	}
}

func TestHealthByAddressInProxyMode(t *testing.T) {
	in := start(t, testConfig(t))
	hc := client(in.app.Addrs().HTTP)
	for _, host := range []string{"10.0.0.5:8080", "127.0.0.1", "backend.internal"} {
		r := do(t, hc, http.MethodGet, "http://"+host+"/health", "", host)
		if r.status != http.StatusOK || !strings.Contains(r.body, `"status":"ok"`) {
			t.Errorf("GET /health with Host %s = %d %q, want the liveness answer", host, r.status, truncate(r.body))
		}
	}
	// An artifact host's /health is the artifact's own path.
	r := do(t, hc, http.MethodGet, "http://demo.localhost/health", "", "demo.localhost")
	if r.status != http.StatusNotFound {
		t.Errorf("GET demo.localhost/health = %d, want the site's 404 for an unknown artifact", r.status)
	}
}

func TestSettingSources(t *testing.T) {
	cfg := testConfig(t)
	origin := config.Origin{File: "/etc/kazari/kazari.yaml", Keys: map[string]config.Source{
		"base_domain": {Line: 1},
		"data_dir":    {Env: "KAZARI_DATA_DIR", EnvFile: "/etc/kazari/kazari.env"},
	}}
	in := startWith(t, cfg, app.Options{Origin: origin})
	r := do(t, client(in.app.Addrs().HTTP), http.MethodGet, "http://admin.localhost/v1/settings/sources", adminToken, "")
	wantStatus(t, r, http.StatusOK, "GET /v1/settings/sources")
	var got map[string]string
	if err := json.Unmarshal([]byte(r.body), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"base_domain": "line 1 of /etc/kazari/kazari.yaml",
		"data_dir":    "KAZARI_DATA_DIR in /etc/kazari/kazari.env",
	}
	if len(got) != len(want) || got["base_domain"] != want["base_domain"] || got["data_dir"] != want["data_dir"] {
		t.Errorf("sources = %v, want %v", got, want)
	}
}

// lateProvider is an OpenID Connect provider that answers discovery only
// once it is up.
type lateProvider struct {
	srv *httptest.Server
	mu  sync.Mutex
	up  bool
}

func newLateProvider(t *testing.T) *lateProvider {
	t.Helper()
	p := &lateProvider{}
	p.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		up := p.up
		p.mu.Unlock()
		if !up || r.URL.Path != "/.well-known/openid-configuration" {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 p.srv.URL,
			"authorization_endpoint": p.srv.URL + "/authorize",
			"token_endpoint":         p.srv.URL + "/token",
			"jwks_uri":               p.srv.URL + "/jwks",
		})
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *lateProvider) start() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.up = true
}

func TestIdentityProviderComesUpLate(t *testing.T) {
	idp := newLateProvider(t)
	cfg := testConfig(t)
	cfg.Auth.OIDC = &config.OIDC{Issuer: idp.srv.URL, ClientID: "kazari", ClientSecret: "secret", AutoProvision: true}
	in := startWith(t, cfg, app.Options{OIDCClient: idp.srv.Client()})
	hc := client(in.app.Addrs().HTTP)

	// The pages serve while the provider is down.
	wantStatus(t, get(t, hc, "http://localhost/health"), http.StatusOK, "the apex's /health with the provider down")

	signIn := func() response {
		t.Helper()
		b := newBrowser(t, in.app.Addrs().HTTP)
		return b.get(b.follow(b.get("http://admin.localhost/"), "the dashboard without a session"))
	}
	r := signIn()
	if r.status != http.StatusServiceUnavailable || !strings.Contains(r.body, "Sign-in is temporarily unavailable") {
		t.Fatalf("sign-in with the provider down = %d %q, want 503 saying so", r.status, truncate(r.body))
	}

	idp.start()
	if err := in.app.Scheduler().Trigger(context.Background(), "oidc-discovery"); err != nil {
		t.Fatalf("oidc-discovery with the provider up: %v", err)
	}
	r = signIn()
	if r.status != http.StatusFound || !strings.HasPrefix(r.header.Get("Location"), idp.srv.URL+"/authorize?") {
		t.Errorf("sign-in with the provider up = %d to %q, want the provider's authorize endpoint", r.status, r.header.Get("Location"))
	}
}
