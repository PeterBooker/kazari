package preflight

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/libdns/libdns"

	"github.com/PeterBooker/kazari/internal/config"
	"github.com/PeterBooker/kazari/internal/telemetry"
)

type fakeDNS struct {
	err     error
	written int
}

func (f *fakeDNS) AppendRecords(_ context.Context, _ string, recs []libdns.Record) ([]libdns.Record, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.written += len(recs)
	return recs, nil
}

func (f *fakeDNS) DeleteRecords(_ context.Context, _ string, recs []libdns.Record) ([]libdns.Record, error) {
	return recs, nil
}

func acmeConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.BaseDomain = "art.example.test"
	cfg.DataDir = t.TempDir()
	cfg.TLS.DNS.APIToken = "token"
	cfg.Listen = config.Listen{HTTPS: "127.0.0.1:0", HTTP: "127.0.0.1:0"}
	return cfg
}

func lookupAll(ip string) func(context.Context, string) ([]netip.Addr, error) {
	return func(_ context.Context, host string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr(ip)}, nil
	}
}

func localAddrs(ips ...string) func() ([]net.Addr, error) {
	return func() ([]net.Addr, error) {
		var out []net.Addr
		for _, ip := range ips {
			out = append(out, &net.IPNet{IP: net.ParseIP(ip), Mask: net.CIDRMask(24, 32)})
		}
		return out, nil
	}
}

func byCheck(findings []Finding, check string) []Finding {
	var out []Finding
	for _, f := range findings {
		if f.Check == check {
			out = append(out, f)
		}
	}
	return out
}

func TestRunAllGood(t *testing.T) {
	cfg := acmeConfig(t)
	cfg.Telemetry.Metrics.Token = "scrape-0123456789abcdef0123456789"
	dns := &fakeDNS{}
	findings := Run(context.Background(), cfg, Options{
		Lookup: lookupAll("203.0.113.10"), LocalAddrs: localAddrs("203.0.113.10"), DNSProvider: dns, Zone: "example.com.",
	})
	if Failed(findings) {
		t.Fatalf("failed: %+v", findings)
	}
	for _, f := range findings {
		if f.Status != OK {
			t.Errorf("%s: %s %s", f.Check, f.Status, f.Detail)
		}
	}
	if d := byCheck(findings, "dns"); len(d) != 3 || !strings.HasPrefix(d[2].Detail, "check-") || !strings.Contains(d[2].Detail, "this host") {
		t.Errorf("dns findings = %+v", d)
	}
	if dns.written != 1 {
		t.Errorf("TXT records written = %d, want 1", dns.written)
	}
	if tok := byCheck(findings, "dns token"); len(tok) != 1 || !strings.Contains(tok[0].Detail, "cloudflare wrote and removed a TXT record in zone example.com.") {
		t.Errorf("dns token = %+v", tok)
	}
	if s := byCheck(findings, "admin socket"); len(s) != 1 || !strings.Contains(s[0].Detail, "no server listening yet") {
		t.Errorf("admin socket = %+v", s)
	}

	var buf bytes.Buffer
	Write(&buf, findings)
	if !strings.HasSuffix(buf.String(), "\nall checks passed\n") || !strings.Contains(buf.String(), "data_dir     ok    ") {
		t.Errorf("report:\n%s", buf.String())
	}
}

func TestRunProblems(t *testing.T) {
	cfg := acmeConfig(t)
	findings := Run(context.Background(), cfg, Options{
		Lookup: func(_ context.Context, host string) ([]netip.Addr, error) {
			if strings.HasPrefix(host, "check-") {
				return nil, errors.New("NXDOMAIN")
			}
			return []netip.Addr{netip.MustParseAddr("198.51.100.7")}, nil
		},
		LocalAddrs:  localAddrs("203.0.113.10"),
		DNSProvider: &fakeDNS{err: errors.New("403 Forbidden")},
		Zone:        "example.com.",
		InContainer: func() bool { return false },
	})
	if !Failed(findings) {
		t.Fatal("no failure reported")
	}
	d := byCheck(findings, "dns")
	if len(d) != 3 || d[0].Status != Warn || !strings.Contains(d[0].Detail, "not an address of this host") ||
		d[2].Status != Fail || !strings.Contains(d[2].Detail, "does not resolve: add the wildcard record") {
		t.Errorf("dns findings = %+v", d)
	}
	if tok := byCheck(findings, "dns token"); tok[0].Status != Fail ||
		!strings.Contains(tok[0].Detail, "cloudflare cannot manage zone example.com.: the token lacks permission on zone example.com; cloudflare needs an API token with Zone.DNS:Edit") {
		t.Errorf("dns token = %+v", tok)
	}

	var buf bytes.Buffer
	Write(&buf, findings)
	if !strings.Contains(buf.String(), "2 check(s) failed") {
		t.Errorf("report:\n%s", buf.String())
	}
}

func TestRunSkips(t *testing.T) {
	cfg := config.Local()
	cfg.DataDir = t.TempDir()
	cfg.Listen.HTTP = "127.0.0.1:0"
	findings := Run(context.Background(), cfg, Options{})
	if Failed(findings) {
		t.Fatalf("failed: %+v", findings)
	}
	if d := byCheck(findings, "dns"); len(d) != 1 || d[0].Status != Skip {
		t.Errorf("dns = %+v, want one skip for localhost", d)
	}
	if tok := byCheck(findings, "dns token"); len(tok) != 1 || tok[0].Status != Skip || !strings.Contains(tok[0].Detail, "proxy mode") {
		t.Errorf("dns token = %+v", tok)
	}
	if p := byCheck(findings, "ports"); len(p) != 1 || p[0].Status != OK {
		t.Errorf("ports = %+v, want the one plain listener", p)
	}

	cfg.Admin.Socket = ""
	if s := byCheck(Run(context.Background(), cfg, Options{}), "admin socket"); len(s) != 1 || s[0].Status != Skip {
		t.Errorf("admin socket = %+v, want skip", s)
	}
}

func TestPortInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cfg := config.Local()
	cfg.DataDir = t.TempDir()
	cfg.Listen.HTTP = ln.Addr().String()
	p := byCheck(Run(context.Background(), cfg, Options{}), "ports")
	if len(p) != 1 || p[0].Status != Warn || !strings.Contains(p[0].Detail, "in use (expected while Kazari is running)") {
		t.Errorf("ports = %+v", p)
	}
}

func TestDataDirNotWritable(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes anywhere")
	}
	cfg := config.Local()
	cfg.DataDir = filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(cfg.DataDir, 0o500); err != nil {
		t.Fatal(err)
	}
	cfg.Listen.HTTP = "127.0.0.1:0"
	d := byCheck(Run(context.Background(), cfg, Options{}), "data_dir")
	if len(d) != 1 || d[0].Status != Fail || !strings.Contains(d[0].Detail, "not writable") || !strings.Contains(d[0].Detail, "sudo kazari check") {
		t.Errorf("data_dir = %+v, want a failure that says what to do", d)
	}
}

func TestDescribe(t *testing.T) {
	cfg := acmeConfig(t)
	cfg.DataDir = "/var/lib/kazari"
	origin := config.Origin{File: "/etc/kazari/kazari.yaml", EnvFiles: []string{"/etc/kazari/kazari.env"}}
	if got, want := Describe(cfg, origin), "/etc/kazari/kazari.yaml with /etc/kazari/kazari.env, base domain art.example.test, mode acme via cloudflare, data in /var/lib/kazari"; got != want {
		t.Errorf("Describe = %q, want %q", got, want)
	}
	local := config.Local()
	if got := Describe(local, config.Origin{}); got != "environment only, base domain kazari.localhost, mode proxy, data in .data" {
		t.Errorf("Describe(local) = %q", got)
	}
}

func TestMetricsTokenWarning(t *testing.T) {
	cfg := acmeConfig(t)
	findings := byCheck(Run(context.Background(), cfg, Options{
		Lookup: lookupAll("203.0.113.10"), LocalAddrs: localAddrs("203.0.113.10"), DNSProvider: &fakeDNS{}, Zone: "example.com.",
	}), "metrics")
	if len(findings) != 1 || findings[0].Status != Skip {
		t.Errorf("metrics findings = %+v, want a skip without a token", findings)
	}
	cfg.Admin.Listen = "127.0.0.1:9443"
	if f := byCheck(Run(context.Background(), cfg, Options{
		Lookup: lookupAll("203.0.113.10"), LocalAddrs: localAddrs("203.0.113.10"), DNSProvider: &fakeDNS{}, Zone: "example.com.",
	}), "metrics"); len(f) != 0 {
		t.Errorf("metrics findings with admin.listen = %+v", f)
	}
}

func TestTelemetryCheck(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer ok.Close()
	refused := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer refused.Close()

	run := func(o telemetry.Options) []Finding {
		var out []Finding
		checkTelemetry(context.Background(), o, func(check string, s Status, format string, args ...any) {
			out = append(out, Finding{Check: check, Status: s, Detail: fmt.Sprintf(format, args...)})
		})
		return out
	}
	none := run(telemetry.Options{Traces: telemetry.Traces{OTLP: true}})
	if len(none) != 1 || none[0].Status != Skip {
		t.Errorf("no endpoint: %+v", none)
	}
	good := run(telemetry.Options{OTLP: telemetry.OTLP{Endpoint: ok.URL + "/otlp", Protocol: telemetry.ProtocolHTTP},
		Traces: telemetry.Traces{OTLP: true}, Metrics: telemetry.Metrics{OTLP: true}, Logs: telemetry.Logs{OTLP: true}})
	if len(good) != 1 || good[0].Status != OK || good[0].Detail != "http/protobuf "+ok.URL+"/otlp: traces, metrics, logs accepted" {
		t.Errorf("accepted: %+v", good)
	}
	bad := run(telemetry.Options{OTLP: telemetry.OTLP{Endpoint: refused.URL, Protocol: telemetry.ProtocolHTTP}, Traces: telemetry.Traces{OTLP: true}})
	if len(bad) != 1 || bad[0].Status != Fail || !strings.Contains(bad[0].Detail, "check telemetry.otlp.headers") {
		t.Errorf("refused: %+v", bad)
	}
}

func TestDNSInContainer(t *testing.T) {
	cfg := acmeConfig(t)
	d := byCheck(Run(t.Context(), cfg, Options{
		Lookup: lookupAll("198.51.100.7"), LocalAddrs: localAddrs("172.17.0.2"), DNSProvider: &fakeDNS{}, Zone: "example.com.",
		InContainer: func() bool { return true },
	}), "dns")
	if len(d) != 3 {
		t.Fatalf("dns findings = %+v, want 3", d)
	}
	for _, f := range d {
		if f.Status != OK || !strings.Contains(f.Detail, "in a container") {
			t.Errorf("dns finding in a container = %+v, want ok saying so", f)
		}
	}
}

func TestProxyListener(t *testing.T) {
	tests := []struct {
		addr string
		warn bool
	}{
		{"127.0.0.1:0", false},
		{"[::1]:0", false},
		{"localhost:0", false},
		{"10.0.0.5:8080", false},
		{":0", true},
		{"0.0.0.0:0", true},
		{"203.0.113.10:8080", true},
	}
	for _, tt := range tests {
		cfg := config.Local()
		cfg.DataDir = t.TempDir()
		cfg.Listen.HTTP = tt.addr
		var warned bool
		for _, f := range byCheck(Run(t.Context(), cfg, Options{}), "ports") {
			if f.Status == Warn && strings.Contains(f.Detail, "accepts connections from any network") {
				warned = true
			}
		}
		if warned != tt.warn {
			t.Errorf("listen.http %s in proxy mode: warned = %v, want %v", tt.addr, warned, tt.warn)
		}
	}
}

func TestSources(t *testing.T) {
	origin := config.Origin{
		File:       "/etc/kazari/kazari.yaml",
		Unreadable: "/etc/kazari/kazari.env: permission denied",
		Ignored:    []string{"KAZARI_LISTEN_PROXY_MODE"},
		Keys: map[string]config.Source{
			"base_domain": {Line: 1},
			"data_dir":    {Line: 2, Env: "KAZARI_DATA_DIR"},
		},
	}
	got := Sources(origin)
	want := []Finding{
		{Check: "env file", Status: Warn, Detail: "/etc/kazari/kazari.env: permission denied, so it was skipped; " +
			"run kazari check as root, as the service reads it, or name a readable file with --env-file"},
		{Check: "env file", Status: Warn, Detail: "KAZARI_LISTEN_PROXY_MODE in .env changed nothing: beside a configuration file, " +
			"an unnamed .env only fills ${NAME} references; name the file with --env-file, or move the line to kazari.env beside the configuration file"},
		{Check: "override", Status: OK, Detail: "data_dir from KAZARI_DATA_DIR in the environment, over line 2 of /etc/kazari/kazari.yaml"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Sources (-want +got):\n%s", diff)
	}
}

func TestOIDCDiscovery(t *testing.T) {
	var issuer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"issuer": %q}`, issuer)
	}))
	defer srv.Close()
	tests := []struct {
		name, configured, served string
		status                   Status
		detail                   string
	}{
		{"match", srv.URL, srv.URL, OK, srv.URL + " answers discovery"},
		{"trailing slash", srv.URL + "/", srv.URL, Fail, "set auth.oidc.issuer to exactly " + fmt.Sprintf("%q", srv.URL)},
		{"not found", srv.URL + "/tenant", srv.URL, Fail, "answered 404 Not Found; check auth.oidc.issuer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issuer = tt.served
			cfg := config.Local()
			cfg.DataDir = t.TempDir()
			cfg.Auth.OIDC = &config.OIDC{Issuer: tt.configured, ClientID: "id", ClientSecret: "s"}
			o := byCheck(Run(t.Context(), cfg, Options{HTTPClient: srv.Client()}), "oidc")
			if len(o) != 1 || o[0].Status != tt.status || !strings.Contains(o[0].Detail, tt.detail) {
				t.Errorf("oidc = %+v, want %s containing %q", o, tt.status, tt.detail)
			}
		})
	}
	srv.Close()
	cfg := config.Local()
	cfg.DataDir = t.TempDir()
	cfg.Auth.OIDC = &config.OIDC{Issuer: srv.URL, ClientID: "id", ClientSecret: "s"}
	if o := byCheck(Run(t.Context(), cfg, Options{}), "oidc"); len(o) != 1 || o[0].Status != Fail || !strings.Contains(o[0].Detail, "cannot reach") {
		t.Errorf("oidc with the provider down = %+v", o)
	}
}

func TestCertFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	write := func(name string, notAfter time.Time, names ...string) (string, string) {
		t.Helper()
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: names, NotBefore: now.Add(-time.Hour), NotAfter: notAfter}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		kder, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		cert, keyFile := filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
		if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600); err != nil {
			t.Fatal(err)
		}
		return cert, keyFile
	}
	good, goodKey := write("good", now.Add(60*24*time.Hour), "art.example.test", "*.art.example.test")
	apex, apexKey := write("apex", now.Add(60*24*time.Hour), "art.example.test")
	old, oldKey := write("old", now.Add(-time.Hour), "art.example.test", "*.art.example.test")
	tests := []struct {
		name, cert, key string
		status          Status
		detail          string
	}{
		{"good", good, goodKey, OK, "covers art.example.test, *.art.example.test, valid until 2026-12-31"},
		{"no wildcard", apex, apexKey, Fail, "covers art.example.test, not api.art.example.test"},
		{"expired", old, oldKey, Fail, "expired on 2026-10-31"},
		{"missing", filepath.Join(dir, "none.crt"), goodKey, Fail, "the user Kazari runs as must read both files"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := acmeConfig(t)
			cfg.TLS.Mode, cfg.TLS.CertFile, cfg.TLS.KeyFile = "files", tt.cert, tt.key
			var got []Finding
			checkCertFiles(cfg, now, func(check string, s Status, format string, args ...any) {
				got = append(got, Finding{Check: check, Status: s, Detail: fmt.Sprintf(format, args...)})
			})
			if len(got) != 1 || got[0].Status != tt.status || !strings.Contains(got[0].Detail, tt.detail) {
				t.Errorf("checkCertFiles = %+v, want %s containing %q", got, tt.status, tt.detail)
			}
		})
	}
}
