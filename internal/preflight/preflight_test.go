package preflight

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/libdns/libdns"

	"github.com/PeterBooker/kazari/internal/config"
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
	cfg.BaseDomain = "art.example.com"
	cfg.DataDir = t.TempDir()
	cfg.TLS.Email = "ops@example.com"
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
	})
	if !Failed(findings) {
		t.Fatal("no failure reported")
	}
	d := byCheck(findings, "dns")
	if len(d) != 3 || d[0].Status != Warn || !strings.Contains(d[0].Detail, "not an address of this host") ||
		d[2].Status != Fail || !strings.Contains(d[2].Detail, "does not resolve: add the wildcard record") {
		t.Errorf("dns findings = %+v", d)
	}
	if tok := byCheck(findings, "dns token"); tok[0].Status != Fail || !strings.Contains(tok[0].Detail, "cloudflare cannot manage zone example.com.: ") {
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
	if len(d) != 1 || d[0].Status != Fail || !strings.Contains(d[0].Detail, "not writable") {
		t.Errorf("data_dir = %+v", d)
	}
}

func TestDescribe(t *testing.T) {
	cfg := acmeConfig(t)
	cfg.DataDir = "/var/lib/kazari"
	if got := Describe(cfg, "/etc/kazari/kazari.yaml"); got != "/etc/kazari/kazari.yaml, base domain art.example.com, mode acme via cloudflare, data in /var/lib/kazari" {
		t.Errorf("Describe = %q", got)
	}
	local := config.Local()
	if got := Describe(local, ""); got != "environment only, base domain localhost, mode proxy, data in .data" {
		t.Errorf("Describe(local) = %q", got)
	}
}
