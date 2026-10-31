package certs

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const defaultPollEvery = time.Minute

// FilesOptions configures a Files source. The zero value is valid.
type FilesOptions struct {
	PollEvery time.Duration // <= 0 means one minute
	Logger    *slog.Logger  // nil means slog.Default()
}

// Files serves a PEM key pair, reloaded when either file changes.
type Files struct {
	certFile, keyFile string
	pollEvery         time.Duration
	logger            *slog.Logger

	cert atomic.Pointer[tls.Certificate]

	mu    sync.Mutex // serialises reloads and guards stamp
	stamp fileStamp
}

var _ Source = (*Files)(nil)

type fileStamp struct {
	certMod, keyMod   time.Time
	certSize, keySize int64
}

// NewFiles fails unless certFile and keyFile hold a matching key pair.
func NewFiles(certFile, keyFile string, opts FilesOptions) (*Files, error) {
	f := &Files{
		certFile:  certFile,
		keyFile:   keyFile,
		pollEvery: opts.PollEvery,
		logger:    opts.Logger,
	}
	if f.pollEvery <= 0 {
		f.pollEvery = defaultPollEvery
	}
	if f.logger == nil {
		f.logger = slog.Default()
	}
	if err := f.Reload(); err != nil {
		return nil, err
	}
	return f, nil
}

// GetCertificate returns the most recently loaded certificate.
func (f *Files) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return f.cert.Load(), nil
}

// Reload rereads both files; on error the previous certificate stays.
func (f *Files) Reload() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	stamp, err := f.statFiles()
	if err != nil {
		return err
	}
	return f.reloadLocked(stamp)
}

// Run reloads the files when their size or modification time changes. A
// failed reload is logged and retried on the next poll.
func (f *Files) Run(ctx context.Context) error {
	ticker := time.NewTicker(f.pollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		if err := f.reloadIfChanged(); err != nil {
			f.logger.Warn("certs: reload failed, keeping previous certificate",
				"cert_file", f.certFile, "key_file", f.keyFile, "err", err)
		}
	}
}

func (f *Files) reloadIfChanged() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	stamp, err := f.statFiles()
	if err != nil {
		return err
	}
	if stamp.equal(f.stamp) {
		return nil
	}
	if err := f.reloadLocked(stamp); err != nil {
		return err
	}
	leaf := f.cert.Load().Leaf
	f.logger.Info("certs: reloaded certificate",
		"cert_file", f.certFile, "subject", leaf.Subject.CommonName, "not_after", leaf.NotAfter)
	return nil
}

func (f *Files) reloadLocked(stamp fileStamp) error {
	cert, err := tls.LoadX509KeyPair(f.certFile, f.keyFile)
	if err != nil {
		return fmt.Errorf("certs: load key pair %s, %s: %w", f.certFile, f.keyFile, err)
	}
	if cert.Leaf == nil {
		return errors.New("certs: key pair has no leaf certificate")
	}
	f.cert.Store(&cert)
	f.stamp = stamp
	return nil
}

func (a fileStamp) equal(b fileStamp) bool {
	return a.certMod.Equal(b.certMod) && a.keyMod.Equal(b.keyMod) &&
		a.certSize == b.certSize && a.keySize == b.keySize
}

func (f *Files) statFiles() (fileStamp, error) {
	c, err := os.Stat(f.certFile)
	if err != nil {
		return fileStamp{}, fmt.Errorf("certs: %w", err)
	}
	k, err := os.Stat(f.keyFile)
	if err != nil {
		return fileStamp{}, fmt.Errorf("certs: %w", err)
	}
	return fileStamp{
		certMod: c.ModTime(), keyMod: k.ModTime(),
		certSize: c.Size(), keySize: k.Size(),
	}, nil
}

// NotAfter implements Source.
func (f *Files) NotAfter() time.Time { return f.cert.Load().Leaf.NotAfter }

// GetClientCertificate is GetCertificate for the client side of TLS. It
// reloads changed files itself, so a client with no Run loop, such as a
// telemetry exporter, still presents a rotated pair.
func (f *Files) GetClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	if err := f.reloadIfChanged(); err != nil {
		f.logger.Warn("certs: reload failed, keeping previous certificate",
			"cert_file", f.certFile, "key_file", f.keyFile, "err", err)
	}
	return f.cert.Load(), nil
}
