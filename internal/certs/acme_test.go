package certs

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/libdns"
)

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

func TestCertEvents(t *testing.T) {
	h := &recordHandler{}
	a := &ACME{}
	on := a.certEvents(slog.New(h), "cloudflare")
	ctx := context.Background()
	on(ctx, "cert_obtaining", map[string]any{"identifier": "*.art.example.com"})
	on(ctx, "cert_obtained", map[string]any{"identifier": "*.art.example.com", "issuer": "acme-v02.api.letsencrypt.org-directory"})
	if err := a.LastError(); err != nil {
		t.Errorf("LastError after an obtained certificate = %v, want nil", err)
	}
	failure := errors.New("presenting for challenge: adding temporary record for zone example.com.: HTTP 401 unauthorized")
	on(ctx, "cert_failed", map[string]any{"identifier": "*.art.example.com", "error": failure, "renewal": true})
	if err := a.LastError(); !errors.Is(err, failure) {
		t.Errorf("LastError after a failed order = %v, want %v", err, failure)
	}
	on(ctx, "cached_managed_cert", map[string]any{"sans": []string{"art.example.com"}})
	var got []string
	for _, r := range h.all() {
		got = append(got, r.Level.String()+" "+r.Message)
	}
	want := []string{"INFO obtaining certificate", "INFO certificate obtained", "ERROR certificate not obtained"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("logged %v, want %v", got, want)
	}
	failed := h.all()[2]
	var hint, name string
	failed.Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case "hint":
			hint = a.Value.String()
		case "name":
			name = a.Value.String()
		}
		return true
	})
	if name != "*.art.example.com" || !strings.Contains(hint, "the provider refused the token") {
		t.Errorf("failed event name %q hint %q", name, hint)
	}
	on(ctx, "cert_obtained", map[string]any{"identifier": "*.art.example.com"})
	if err := a.LastError(); err != nil {
		t.Errorf("LastError after a later success = %v, want nil", err)
	}
}

func TestObtainHint(t *testing.T) {
	tests := []struct {
		name, err, want string
	}{
		{"token refused", "presenting for challenge: adding temporary record for zone \"example.com.\": HTTP 400: [{Code:6003 Message:Invalid format for Authorization header}]",
			"the provider refused the token"},
		{"no permission", "presenting for challenge: adding temporary record for zone example.com.: HTTP 403 Forbidden", "the token lacks permission"},
		{"ca unreachable", `registering account with server: Get "https://acme-v02.api.letsencrypt.org/directory": dial tcp: lookup acme-v02.api.letsencrypt.org: no such host`,
			"the CA cannot be reached"},
		{"caa", "urn:ietf:params:acme:error:caa: CAA record for art.example.com prevents issuance", "a CAA record"},
		{"rate limit", "urn:ietf:params:acme:error:rateLimited: too many certificates already issued", "rate limit"},
		{"other", "urn:ietf:params:acme:error:unauthorized: Incorrect TXT record found at _acme-challenge.art.example.com", "check the DNS records"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ObtainHint("cloudflare", errors.New(tt.err)); !strings.Contains(got, tt.want) {
				t.Errorf("ObtainHint(%q) = %q, want it to contain %q", tt.err, got, tt.want)
			}
		})
	}
}

func TestDNSAdvice(t *testing.T) {
	tests := []struct {
		name, err, want string
	}{
		{"bad header", "HTTP 400: [{Code:6003 Message:Invalid format for Authorization header}]",
			"the provider refused the token: check that tls.dns.api_token (KAZARI_TLS_DNS_API_TOKEN) holds the whole token"},
		{"unauthorized", "HTTP 401: Unauthorized", "the provider refused the token"},
		{"forbidden", "got error status: HTTP 403: Forbidden", "the token lacks permission on zone example.com; cloudflare needs an API token with Zone.DNS:Edit on the zone"},
		{"zone", "expected 1 zone, got 0 for example.com: zone not found", "the provider's account has no zone example.com"},
		{"timestamp", "request 14001 failed: connection reset", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DNSAdvice("cloudflare", "example.com.", errors.New(tt.err))
			if (tt.want == "") != (got == "") || !strings.Contains(got, tt.want) {
				t.Errorf("DNSAdvice(%q) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}
