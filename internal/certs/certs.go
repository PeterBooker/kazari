// Package certs provides the TLS certificate sources Kazari can serve from:
// a self-signed certificate for development, certificate files on disk
// reloaded when they change, and ACME (DNS-01, wildcard) through CertMagic.
package certs

import (
	"context"
	"crypto/tls"
)

// Source supplies the serving certificate for TLS handshakes.
type Source interface {
	// GetCertificate returns the certificate for a handshake. It has the
	// signature of tls.Config.GetCertificate.
	GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error)
	// Run performs background maintenance (renewal or file watching). It
	// blocks until ctx is done and then returns nil.
	Run(ctx context.Context) error
}

// TLSConfig returns a server tls.Config that takes its certificates from s.
func TLSConfig(s Source) *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: s.GetCertificate,
		NextProtos:     []string{"h2", "http/1.1"},
	}
}
