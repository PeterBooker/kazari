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

// defaultPollEvery is how often Files checks its files when
// FilesOptions.PollEvery is not positive.
const defaultPollEvery = time.Minute

// FilesOptions configures a Files source. The zero value is valid.
type FilesOptions struct {
	// PollEvery is how often Run checks the files for changes. Values <= 0
	// mean one minute.
	PollEvery time.Duration
	// Logger receives reload results. Nil means slog.Default().
	Logger *slog.Logger
}

// Files serves a certificate and key loaded from PEM files and reloads them
// when either file changes (for example after an external renewal).
type Files struct {
	certFile, keyFile string
	pollEvery         time.Duration
	logger            *slog.Logger

	cert atomic.Pointer[tls.Certificate]

	mu    sync.Mutex // serialises reloads and guards stamp
	stamp fileStamp
}

var _ Source = (*Files)(nil)

// fileStamp identifies a version of the certificate and key files.
type fileStamp struct {
	certMod, keyMod   time.Time
	certSize, keySize int64
}

// NewFiles loads the key pair from certFile and keyFile and fails if the
// files are missing, malformed or do not match.
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

// Reload reads both files now and swaps in the new certificate. On error
// the previous certificate stays in use.
func (f *Files) Reload() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	stamp, err := f.statFiles()
	if err != nil {
		return err
	}
	return f.reloadLocked(stamp)
}

// Run checks the files every PollEvery and reloads them when their size or
// modification time changes. A failed reload is logged, the old certificate
// is kept, and the reload is retried on the next poll.
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

// reloadLocked loads the pair and records stamp. f.mu must be held.
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
