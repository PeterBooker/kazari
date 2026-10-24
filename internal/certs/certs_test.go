package certs

import (
	"crypto/tls"
	"slices"
	"testing"
)

func TestTLSConfig(t *testing.T) {
	s, err := NewSelfSigned("localhost")
	if err != nil {
		t.Fatal(err)
	}
	cfg := TLSConfig(s)
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %x, want TLS 1.2", cfg.MinVersion)
	}
	if want := []string{"h2", "http/1.1"}; !slices.Equal(cfg.NextProtos, want) {
		t.Errorf("NextProtos = %v, want %v", cfg.NextProtos, want)
	}
	if cfg.GetCertificate == nil {
		t.Fatal("GetCertificate is nil")
	}
	got, err := cfg.GetCertificate(&tls.ClientHelloInfo{ServerName: "localhost"})
	if err != nil || got != s.cert {
		t.Errorf("GetCertificate = %p, %v; want the source's certificate", got, err)
	}
}
