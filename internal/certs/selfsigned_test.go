package certs

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"slices"
	"testing"
	"time"
)

func serveTLS(t *testing.T, s Source) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	cfg := TLSConfig(s)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				tc := tls.Server(conn, cfg)
				_ = tc.SetDeadline(time.Now().Add(5 * time.Second))
				_ = tc.Handshake()
			}()
		}
	}()
	return ln.Addr().String()
}

func TestSelfSignedHandshake(t *testing.T) {
	s, err := NewSelfSigned([]string{"localhost", "art.test"}, SelfSignedOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(s.cert.Leaf)
	addr := serveTLS(t, s)

	tests := []struct {
		serverName string
		wantErr    bool
	}{
		{"localhost", false},
		{"demo.localhost", false},
		{"art.test", false},
		{"demo.art.test", false},
		{"127.0.0.1", false},
		{"a.b.localhost", true}, // wildcards cover one label only
		{"example.com", true},
	}
	for _, tt := range tests {
		t.Run(tt.serverName, func(t *testing.T) {
			conn, err := tls.Dial("tcp", addr, &tls.Config{
				RootCAs:    pool,
				ServerName: tt.serverName,
				NextProtos: []string{"h2", "http/1.1"},
			})
			if tt.wantErr {
				if err == nil {
					conn.Close()
					t.Fatal("handshake succeeded, want verification error")
				}
				if _, ok := errors.AsType[*tls.CertificateVerificationError](err); !ok {
					t.Fatalf("err = %v, want certificate verification error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("handshake: %v", err)
			}
			defer conn.Close()
			if got := conn.ConnectionState().NegotiatedProtocol; got != "h2" {
				t.Errorf("ALPN = %q, want h2", got)
			}
		})
	}
}

func TestSelfSignedSANs(t *testing.T) {
	s, err := NewSelfSigned([]string{"Localhost.", "art.test", "*.wild.test", "192.0.2.1", ""}, SelfSignedOptions{})
	if err != nil {
		t.Fatal(err)
	}
	leaf := s.cert.Leaf
	wantNames := []string{"localhost", "*.localhost", "art.test", "*.art.test", "*.wild.test"}
	if !slices.Equal(leaf.DNSNames, wantNames) {
		t.Errorf("DNSNames = %v, want %v", leaf.DNSNames, wantNames)
	}
	var ips []string
	for _, ip := range leaf.IPAddresses {
		ips = append(ips, ip.String())
	}
	if want := []string{"127.0.0.1", "::1", "192.0.2.1"}; !slices.Equal(ips, want) {
		t.Errorf("IPAddresses = %v, want %v", ips, want)
	}
	if leaf.Subject.CommonName != "localhost" {
		t.Errorf("CommonName = %q", leaf.Subject.CommonName)
	}
	if life := leaf.NotAfter.Sub(leaf.NotBefore); life < 365*24*time.Hour {
		t.Errorf("validity = %v, want at least a year", life)
	}
	if leaf.PublicKeyAlgorithm != x509.ECDSA {
		t.Errorf("PublicKeyAlgorithm = %v, want ECDSA", leaf.PublicKeyAlgorithm)
	}
}

func TestSelfSignedDefaultsAndRun(t *testing.T) {
	s, err := NewSelfSigned(nil, SelfSignedOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"localhost", "*.localhost"}; !slices.Equal(s.cert.Leaf.DNSNames, want) {
		t.Errorf("DNSNames = %v, want %v", s.cert.Leaf.DNSNames, want)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
}

func TestSelfSignedNotAfter(t *testing.T) {
	now := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	s, err := NewSelfSigned([]string{"art.test"}, SelfSignedOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := s.NotAfter(), now.AddDate(1, 0, 0); !got.Equal(want) {
		t.Errorf("NotAfter = %v, want %v", got, want)
	}
	if got, want := s.cert.Leaf.NotBefore, now.Add(-time.Hour); !got.Equal(want) {
		t.Errorf("NotBefore = %v, want %v", got, want)
	}
}
