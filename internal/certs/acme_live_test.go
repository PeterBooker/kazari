//go:build acme_live

package certs

import (
	"context"
	"crypto/tls"
	"os"
	"testing"
	"time"
)

// TestACMELiveStaging obtains a real wildcard certificate from the Let's
// Encrypt staging CA. Run it with:
//
//	KAZARI_ACME_LIVE_DOMAIN=art.example.com \
//	KAZARI_ACME_LIVE_DNS_PROVIDER=cloudflare \
//	KAZARI_ACME_LIVE_DNS_TOKEN=... \
//	go test -tags acme_live -run TestACMELiveStaging -timeout 10m ./internal/certs/
func TestACMELiveStaging(t *testing.T) {
	domain := os.Getenv("KAZARI_ACME_LIVE_DOMAIN")
	provider := os.Getenv("KAZARI_ACME_LIVE_DNS_PROVIDER")
	if domain == "" || provider == "" {
		t.Skip("set KAZARI_ACME_LIVE_DOMAIN and KAZARI_ACME_LIVE_DNS_PROVIDER to run")
	}
	dns, err := NewDNSProvider(provider, os.Getenv("KAZARI_ACME_LIVE_DNS_TOKEN"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewACME(ACMEConfig{
		Email:      os.Getenv("KAZARI_ACME_LIVE_EMAIL"),
		CA:         "letsencrypt-staging",
		Domains:    []string{domain, "*." + domain},
		StorageDir: t.TempDir(),
		Sync:       true,
	}, dns)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	addr := serveTLS(t, a)
	deadline := time.Now().Add(8 * time.Minute)
	for {
		select {
		case err := <-done:
			t.Fatalf("Run returned early: %v", err)
		default:
		}
		conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: "demo." + domain, InsecureSkipVerify: true})
		if err == nil {
			leaf := conn.ConnectionState().PeerCertificates[0]
			conn.Close()
			if err := leaf.VerifyHostname("demo." + domain); err != nil {
				t.Fatalf("served certificate: %v", err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no certificate before deadline: %v", err)
		}
		time.Sleep(5 * time.Second)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v", err)
	}
}
