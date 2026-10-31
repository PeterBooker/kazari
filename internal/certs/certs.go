// Package certs provides TLS certificate sources: self-signed for
// development, files reloaded on change, and ACME DNS-01 through CertMagic.
package certs

import (
	"context"
	"crypto/tls"
	"time"
)

// Source supplies the serving certificate for TLS handshakes.
type Source interface {
	GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error)
	// Run renews or watches the certificate until ctx is done.
	Run(ctx context.Context) error
	// NotAfter is when the certificate served now expires, the earliest of
	// several; zero before there is one.
	NotAfter() time.Time
}

// TLSConfig returns a server tls.Config that takes its certificates from s.
func TLSConfig(s Source) *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: s.GetCertificate,
		NextProtos:     []string{"h2", "http/1.1"},
	}
}
