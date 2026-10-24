package certs

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"strings"
	"time"
)

// SelfSigned serves a single self-signed certificate generated at startup.
// It is meant for development and tests; clients must trust it explicitly.
type SelfSigned struct {
	cert *tls.Certificate
}

var _ Source = (*SelfSigned)(nil)

// NewSelfSigned generates an ECDSA P-256 certificate valid for one year.
// Its SANs are every given domain plus the wildcard "*.<domain>" for each,
// so "localhost" also covers "demo.localhost", and the IPs 127.0.0.1 and
// ::1. Domains that are IP addresses become IP SANs. With no domains it
// uses "localhost".
func NewSelfSigned(domains ...string) (*SelfSigned, error) {
	der, key, err := selfSignedDER(time.Now(), domains)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("certs: parse self-signed certificate: %w", err)
	}
	return &SelfSigned{cert: &tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
		Leaf:        leaf,
	}}, nil
}

// GetCertificate returns the self-signed certificate for every handshake.
func (s *SelfSigned) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return s.cert, nil
}

// Run blocks until ctx is done; a self-signed certificate needs no upkeep.
func (s *SelfSigned) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// selfSignedDER builds the self-signed certificate and returns its DER
// encoding and private key.
func selfSignedDER(now time.Time, domains []string) ([]byte, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("certs: generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("certs: generate serial: %w", err)
	}

	dnsNames, ips := selfSignedSANs(domains)
	cn := "localhost"
	if len(dnsNames) > 0 {
		cn = dnsNames[0]
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn, Organization: []string{"Agalma self-signed"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(1, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("certs: create self-signed certificate: %w", err)
	}
	return der, key, nil
}

// selfSignedSANs normalises domains into DNS SANs (each with its wildcard)
// and IP SANs (always including the loopback addresses).
func selfSignedSANs(domains []string) ([]string, []net.IP) {
	if len(domains) == 0 {
		domains = []string{"localhost"}
	}
	ips := []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	var names []string
	seen := map[string]bool{}
	addName := func(n string) {
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	for _, d := range domains {
		d = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".")
		if d == "" {
			continue
		}
		if ip := net.ParseIP(d); ip != nil {
			if !ip.IsLoopback() {
				ips = append(ips, ip)
			}
			continue
		}
		addName(d)
		if !strings.HasPrefix(d, "*.") {
			addName("*." + d)
		}
	}
	return names, ips
}
