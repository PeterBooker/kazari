package certs

import (
	"context"
	"crypto/tls"
	"log/slog"
	"strings"
	"testing"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/libdns"
)

// fakeDNS satisfies certmagic.DNSProvider without touching the network.
type fakeDNS struct{}

func (fakeDNS) AppendRecords(_ context.Context, _ string, recs []libdns.Record) ([]libdns.Record, error) {
	return recs, nil
}

func (fakeDNS) DeleteRecords(_ context.Context, _ string, recs []libdns.Record) ([]libdns.Record, error) {
	return recs, nil
}

func newTestACME(t *testing.T, cfg ACMEConfig) *ACME {
	t.Helper()
	a, err := NewACME(cfg, fakeDNS{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.stop)
	return a
}

func TestNewACME(t *testing.T) {
	dir := t.TempDir()
	a := newTestACME(t, ACMEConfig{
		Email:      "ops@example.com",
		CA:         "letsencrypt-staging",
		Domains:    []string{"art.example.com", "*.art.example.com"},
		StorageDir: dir,
		Logger:     slog.New(slog.DiscardHandler),
	})

	fs, ok := a.magic.Storage.(*certmagic.FileStorage)
	if !ok || fs.Path != dir {
		t.Errorf("Storage = %#v, want FileStorage at %s", a.magic.Storage, dir)
	}
	if len(a.magic.Issuers) != 1 {
		t.Fatalf("Issuers = %v, want one", a.magic.Issuers)
	}
	iss, ok := a.magic.Issuers[0].(*certmagic.ACMEIssuer)
	if !ok {
		t.Fatalf("issuer is %T, want *certmagic.ACMEIssuer", a.magic.Issuers[0])
	}
	if iss.CA != certmagic.LetsEncryptStagingCA || iss.Email != "ops@example.com" || !iss.Agreed {
		t.Errorf("issuer CA=%q Email=%q Agreed=%v", iss.CA, iss.Email, iss.Agreed)
	}
	solver, ok := iss.DNS01Solver.(*certmagic.DNS01Solver)
	if !ok {
		t.Fatalf("DNS01Solver is %T", iss.DNS01Solver)
	}
	if _, ok := solver.DNSProvider.(fakeDNS); !ok {
		t.Errorf("DNSProvider = %T, want fakeDNS", solver.DNSProvider)
	}
	if strings.Join(a.domains, ",") != "art.example.com,*.art.example.com" {
		t.Errorf("domains = %v", a.domains)
	}

	// Nothing is managed yet, so a handshake gets an error, not a network call.
	addr := serveTLS(t, a)
	conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: "demo.art.example.com", InsecureSkipVerify: true})
	if err == nil {
		conn.Close()
		t.Error("handshake with nothing managed succeeded")
	}
}

func TestNewACMEErrors(t *testing.T) {
	valid := ACMEConfig{Domains: []string{"art.test"}, StorageDir: t.TempDir()}
	tests := []struct {
		name    string
		mutate  func(*ACMEConfig)
		nilDNS  bool
		wantErr string
	}{
		{name: "no dns", nilDNS: true, wantErr: "DNS provider"},
		{name: "no domains", mutate: func(c *ACMEConfig) { c.Domains = nil }, wantErr: "domain"},
		{name: "no storage", mutate: func(c *ACMEConfig) { c.StorageDir = "" }, wantErr: "storage"},
		{name: "bad ca", mutate: func(c *ACMEConfig) { c.CA = "zerossl" }, wantErr: "directory URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid
			if tt.mutate != nil {
				tt.mutate(&cfg)
			}
			var dns certmagic.DNSProvider = fakeDNS{}
			if tt.nilDNS {
				dns = nil
			}
			_, err := NewACME(cfg, dns)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestCAURL(t *testing.T) {
	tests := []struct {
		in, want string
		wantErr  bool
	}{
		{in: "", want: certmagic.LetsEncryptProductionCA},
		{in: "letsencrypt", want: certmagic.LetsEncryptProductionCA},
		{in: "letsencrypt-staging", want: certmagic.LetsEncryptStagingCA},
		{in: "https://acme.example.com/directory", want: "https://acme.example.com/directory"},
		{in: "ftp://acme.example.com", wantErr: true},
		{in: "https://", wantErr: true},
		{in: "letsencrypt-prod", wantErr: true},
	}
	for _, tt := range tests {
		got, err := caURL(tt.in)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("caURL(%q) = %q, %v; want %q, err=%v", tt.in, got, err, tt.want, tt.wantErr)
		}
	}
}
