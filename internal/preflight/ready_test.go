package preflight

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PeterBooker/kazari/internal/config"

	"go.opentelemetry.io/otel/attribute"

	"github.com/PeterBooker/kazari/internal/instrument/instrumenttest"
)

var readyNow = time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)

func diskUsage(free, total uint64) func(string) (uint64, uint64, error) {
	return func(string) (uint64, uint64, error) { return free, total, nil }
}

// readyOptions describe a healthy instance: records that point here, a
// provider that accepts the token, a certificate two months from its end and
// a half-empty disk.
func readyOptions(dns *fakeDNS) ReadyOptions {
	return ReadyOptions{
		Options: Options{
			Lookup: lookupAll("203.0.113.10"), LocalAddrs: localAddrs("203.0.113.10"), DNSProvider: dns, Zone: "example.com.",
		},
		CertNotAfter: func() time.Time { return readyNow.Add(60 * 24 * time.Hour) },
		DiskUsage:    diskUsage(50<<30, 100<<30),
		Now:          func() time.Time { return readyNow },
	}
}

func one(t *testing.T, findings []Finding, check string) Finding {
	t.Helper()
	f := byCheck(findings, check)
	if len(f) != 1 {
		t.Fatalf("%s findings = %+v, want one", check, f)
	}
	return f[0]
}

func TestReadyPassing(t *testing.T) {
	cfg := acmeConfig(t)
	if err := os.WriteFile(cfg.DBPath(), make([]byte, 3<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.BlobDir(), "ab"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.BlobDir(), "ab", "abcd"), make([]byte, 2048), 0o600); err != nil {
		t.Fatal(err)
	}
	dns := &fakeDNS{}
	findings := Ready(context.Background(), cfg, readyOptions(dns))
	for _, f := range findings {
		if f.Status != OK {
			t.Errorf("%s: %s %s", f.Check, f.Status, f.Detail)
		}
	}
	for _, absent := range []string{"ports", "admin socket", "data_dir", "telemetry"} {
		if f := byCheck(findings, absent); len(f) != 0 {
			t.Errorf("%s checked in a running server: %+v", absent, f)
		}
	}
	if dns.written != 1 || len(byCheck(findings, "dns")) != 3 {
		t.Errorf("TXT records written = %d, dns findings = %+v", dns.written, byCheck(findings, "dns"))
	}
	for check, want := range map[string]string{
		"config":      "the configuration is valid",
		"dns token":   "wrote and removed a TXT record",
		"certificate": "valid until 2027-01-20 (60 days)",
		"disk":        "50.0 GiB free of 100.0 GiB on the disk holding " + cfg.DataDir + " (50%)",
		"database":    "3.0 MiB in " + cfg.DBPath(),
		"blobs":       "2.0 KiB in " + cfg.BlobDir(),
		"dashboard":   "https://admin.art.example.com",
	} {
		if f := one(t, findings, check); !strings.Contains(f.Detail, want) {
			t.Errorf("%s = %q, want it to contain %q", check, f.Detail, want)
		}
	}
}

func TestReadyProblems(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*config.Config, *ReadyOptions)
		check  string
		status Status
		want   []string // parts of the detail, the next step last
	}{
		{"certificate near its end", func(_ *config.Config, o *ReadyOptions) {
			o.CertNotAfter = func() time.Time { return readyNow.Add(13*24*time.Hour + time.Hour) }
		}, "certificate", Warn, []string{"expires on 2026-12-04, in 13 days", "check the log for certificate errors and run kazari check"}},
		{"certificate at the window", func(_ *config.Config, o *ReadyOptions) {
			o.CertNotAfter = func() time.Time { return readyNow.Add(CertWarnWindow) }
		}, "certificate", OK, []string{"valid until 2026-12-05 (14 days)"}},
		{"certificate expired", func(_ *config.Config, o *ReadyOptions) {
			o.CertNotAfter = func() time.Time { return readyNow.Add(-time.Hour) }
		}, "certificate", Fail, []string{"expired on 2026-11-21", "check the log for certificate errors and run kazari check"}},
		{"certificate files expired", func(c *config.Config, o *ReadyOptions) {
			c.TLS.Mode, c.TLS.CertFile, c.TLS.KeyFile = "files", "/etc/kazari/tls.crt", "/etc/kazari/tls.key"
			o.CertNotAfter = func() time.Time { return readyNow.Add(-time.Hour) }
		}, "certificate", Fail, []string{"replace tls.cert_file and tls.key_file"}},
		{"no certificate yet", func(_ *config.Config, o *ReadyOptions) {
			o.CertNotAfter = func() time.Time { return time.Time{} }
		}, "certificate", Skip, []string{"no certificate is loaded yet"}},
		{"proxy mode", func(c *config.Config, o *ReadyOptions) {
			c.Listen = config.Listen{HTTP: "127.0.0.1:0", ProxyMode: true}
			o.CertNotAfter = nil
		}, "certificate", Skip, []string{"proxy mode"}},
		{"disk filling", func(_ *config.Config, o *ReadyOptions) { o.DiskUsage = diskUsage(9<<30, 100<<30) },
			"disk", Warn, []string{"9.0 GiB free of 100.0 GiB", "(9%)", "purge pages from the bin, lower lifecycle.retention or grow the disk"}},
		{"disk at the threshold", func(_ *config.Config, o *ReadyOptions) { o.DiskUsage = diskUsage(10<<30, 100<<30) },
			"disk", OK, []string{"(10%)"}},
		{"disk full", func(_ *config.Config, o *ReadyOptions) { o.DiskUsage = diskUsage(1<<30, 100<<30) },
			"disk", Fail, []string{"publishing fails when it is full", "grow the disk"}},
		{"disk unknown", func(_ *config.Config, o *ReadyOptions) {
			o.DiskUsage = func(string) (uint64, uint64, error) { return 0, 0, errors.New("no such file or directory") }
		}, "disk", Warn, []string{"is unknown: no such file or directory", "check that the data directory exists"}},
		{"token refused", func(_ *config.Config, o *ReadyOptions) { o.DNSProvider = &fakeDNS{err: errors.New("403 Forbidden")} },
			"dns token", Fail, []string{"cloudflare cannot manage zone example.com."}},
		{"invalid configuration", func(c *config.Config, _ *ReadyOptions) { c.Serve.DefaultVisibility = "secret" },
			"config", Fail, []string{"serve.default_visibility: must be one of", "correct the configuration and restart"}},
		{"dashboard without an address", func(c *config.Config, _ *ReadyOptions) { c.Admin.Listen = "127.0.0.1:9443" },
			"dashboard", Warn, []string{"set admin.public_url"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := acmeConfig(t)
			opts := readyOptions(&fakeDNS{})
			tt.mutate(&cfg, &opts)
			findings := Ready(context.Background(), cfg, opts)
			f := one(t, findings, tt.check)
			if f.Status != tt.status {
				t.Errorf("%s is %s (%s), want %s", tt.check, f.Status, f.Detail, tt.status)
			}
			at := 0
			for _, part := range tt.want {
				i := strings.Index(f.Detail[at:], part)
				if i < 0 {
					t.Fatalf("%s = %q, want %q after offset %d", tt.check, f.Detail, part, at)
				}
				at += i
			}
			if (tt.status == Fail) != Failed(findings) {
				t.Errorf("Failed = %v with %+v", Failed(findings), findings)
			}
		})
	}
}

func TestDashboardAddress(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*config.Config)
		status Status // "" for no finding
		want   string
	}{
		{"default", func(*config.Config) {}, OK, "https://admin.art.example.com"},
		{"private listener without an address", func(c *config.Config) { c.Admin.Listen = "127.0.0.1:9443" },
			Warn, "off: admin.listen is set and admin.public_url is not; set admin.public_url"},
		{"private listener through a tunnel", func(c *config.Config) {
			c.Admin.Listen, c.Admin.PublicURL = "127.0.0.1:9443", "http://localhost:9443"
		}, OK, "http://localhost:9443"},
		{"private listener behind a proxy", func(c *config.Config) {
			c.Admin.Listen, c.Admin.PublicURL = "10.0.0.5:9443", "https://kazari-admin.vpn.example.com/"
		}, OK, "https://kazari-admin.vpn.example.com"},
		{"plain http across the network", func(c *config.Config) { c.ExternalScheme = "http" },
			Fail, "http://admin.art.example.com is plain http"},
		{"plain http address", func(c *config.Config) { c.Admin.PublicURL = "http://10.0.0.5:9443" },
			Fail, "http://10.0.0.5:9443 is plain http"},
		{"dashboard off", func(c *config.Config) { c.Admin.Dashboard = false; c.Admin.Listen = "127.0.0.1:9443" }, "", ""},
	}
	opts := Options{Lookup: lookupAll("203.0.113.10"), LocalAddrs: localAddrs("203.0.113.10"), DNSProvider: &fakeDNS{}, Zone: "example.com."}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := acmeConfig(t)
			tt.mutate(&cfg)
			got := byCheck(Run(context.Background(), cfg, opts), "dashboard")
			if tt.status == "" {
				if len(got) != 0 {
					t.Errorf("dashboard = %+v, want nothing", got)
				}
				return
			}
			if len(got) != 1 || got[0].Status != tt.status || !strings.Contains(got[0].Detail, tt.want) {
				t.Errorf("dashboard = %+v, want %s with %q", got, tt.status, tt.want)
			}
		})
	}

	local := config.Local()
	local.DataDir = t.TempDir()
	local.Listen.HTTP = "127.0.0.1:0"
	if got := byCheck(Run(context.Background(), local, Options{}), "dashboard"); len(got) != 1 || got[0].Status != OK ||
		got[0].Detail != "http://admin.localhost:8080" {
		t.Errorf("dashboard of serve --local = %+v", got)
	}
}

func TestChecker(t *testing.T) {
	cfg := acmeConfig(t)
	dns := &fakeDNS{}
	now := readyNow
	opts := readyOptions(dns)
	opts.Now = func() time.Time { return now }
	c := NewChecker(cfg, opts, 0)
	ctx := context.Background()

	if _, ok := c.Last(); ok {
		t.Error("Last before the first run reports one")
	}
	first, err := c.Check(ctx, false)
	if err != nil || first.Failed() || !first.At.Equal(now) || len(first.Findings) == 0 {
		t.Fatalf("first Check = %+v, %v", first, err)
	}
	if last, ok := c.Last(); !ok || !last.At.Equal(first.At) {
		t.Errorf("Last = %+v, %v", last, ok)
	}

	now = now.Add(DefaultReadyTTL - time.Second)
	if r, err := c.Check(ctx, false); err != nil || !r.At.Equal(first.At) || dns.written != 1 {
		t.Errorf("Check within the TTL ran again: %+v, %v, %d runs", r.At, err, dns.written)
	}
	now = now.Add(time.Second)
	if r, err := c.Check(ctx, false); err != nil || !r.At.Equal(now) || dns.written != 2 {
		t.Errorf("Check at the TTL = %+v, %v, %d runs; want a second run", r.At, err, dns.written)
	}
	if r, err := c.Check(ctx, true); err != nil || dns.written != 3 || !r.At.Equal(now) {
		t.Errorf("fresh Check = %+v, %v, %d runs; want a third run", r.At, err, dns.written)
	}
}

func TestCheckerSharesOneRun(t *testing.T) {
	cfg := acmeConfig(t)
	dns := &fakeDNS{}
	opts := readyOptions(dns)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	opts.Lookup = func(context.Context, string) ([]netip.Addr, error) {
		once.Do(func() { close(entered) })
		<-release
		return []netip.Addr{netip.MustParseAddr("203.0.113.10")}, nil
	}
	c := NewChecker(cfg, opts, time.Hour)

	// The caller that starts the run gives up; the run goes on.
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan error, 1)
	go func() {
		_, err := c.Check(ctx, false)
		started <- err
	}()
	<-entered
	cancel()
	if err := <-started; !errors.Is(err, context.Canceled) {
		t.Fatalf("Check with a cancelled context = %v", err)
	}

	const callers = 8
	reports := make(chan Report, callers)
	for range callers {
		go func() {
			r, err := c.Check(context.Background(), false)
			if err != nil {
				t.Error(err)
			}
			reports <- r
		}()
	}
	close(release)
	for range callers {
		if r := <-reports; r.Failed() || len(r.Findings) == 0 {
			t.Errorf("report = %+v", r)
		}
	}
	if dns.written != 1 {
		t.Errorf("runs = %d, want the one every caller shared", dns.written)
	}
	if _, ok := c.Last(); !ok {
		t.Error("no report kept")
	}
}

func TestCheckerTelemetry(t *testing.T) {
	rec := instrumenttest.New(t)
	opts := readyOptions(&fakeDNS{})
	opts.Now = func() time.Time { return readyNow }
	opts.Providers = rec.Providers
	c := NewChecker(acmeConfig(t), opts, 0)
	if _, ok := rec.Sum("kazari.ready.checks"); ok {
		t.Error("findings are reported before any run")
	}

	ctx, end := rec.Start("GET /v1/ready")
	report, err := c.Check(ctx, true)
	end()
	if err != nil {
		t.Fatal(err)
	}
	status := attribute.Key("kazari.ready.status")
	rec.Want("kazari.ready.checks", float64(len(report.Findings)))
	rec.Want("kazari.ready.checks", 0, status.String(string(Fail)))
	if n, _ := rec.Sum("kazari.ready.checks", status.String(string(OK))); n == 0 {
		t.Error("no ok finding is reported for a passing instance")
	}
	rec.Want("kazari.ready.timestamp", float64(readyNow.Unix()))
	if _, found := rec.Span("preflight.ready"); !found {
		t.Fatal("no preflight.ready span")
	}
	if got := rec.SpanAttr("preflight.ready", "kazari.ready.failed"); got != "0" {
		t.Errorf("kazari.ready.failed = %q, want 0", got)
	}
	// Every check name the gauge carries is one of the fixed list.
	known := map[string]bool{"config": true, "dns": true, "dns token": true, "dashboard": true, "certificate": true,
		"disk": true, "database": true, "blobs": true}
	for _, f := range report.Findings {
		if !known[f.Check] {
			t.Errorf("check %q is not in the documented list", f.Check)
		}
	}
}
