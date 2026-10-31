package certs

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writePair stamps both files with mtime so a poll sees the change whatever
// the filesystem's timestamp granularity.
func writePair(t *testing.T, certFile, keyFile, cn string, mtime time.Time) {
	t.Helper()
	der, key, err := selfSignedDER(time.Now(), []string{cn})
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), mtime)
	writeFile(t, keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), mtime)
}

func writeFile(t *testing.T, name string, data []byte, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(name, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(name, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func pairPaths(t *testing.T) (string, string) {
	dir := t.TempDir()
	return filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
}

func servedCN(t *testing.T, f *Files) string {
	t.Helper()
	c, err := f.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	return c.Leaf.Subject.CommonName
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestFilesReload(t *testing.T) {
	certFile, keyFile := pairPaths(t)
	base := time.Now().Add(-time.Hour)
	writePair(t, certFile, keyFile, "first.test", base)

	f, err := NewFiles(certFile, keyFile, FilesOptions{Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	if got := servedCN(t, f); got != "first.test" {
		t.Fatalf("CN = %q, want first.test", got)
	}
	if f.pollEvery != time.Minute {
		t.Errorf("default pollEvery = %v, want 1m", f.pollEvery)
	}

	writePair(t, certFile, keyFile, "second.test", base.Add(time.Minute))
	if err := f.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := servedCN(t, f); got != "second.test" {
		t.Fatalf("CN after reload = %q, want second.test", got)
	}
}

func TestFilesBadRewriteKeepsOld(t *testing.T) {
	certFile, keyFile := pairPaths(t)
	base := time.Now().Add(-time.Hour)
	writePair(t, certFile, keyFile, "good.test", base)
	f, err := NewFiles(certFile, keyFile, FilesOptions{Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	goodKey, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}

	otherCert, otherKey := pairPaths(t)
	writePair(t, otherCert, otherKey, "other.test", base)
	otherCertPEM, err := os.ReadFile(otherCert)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		cert, key []byte
	}{
		{"garbage cert", []byte("not a certificate"), goodKey},
		{"mismatched key", otherCertPEM, goodKey},
		{"empty key", otherCertPEM, nil},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mtime := base.Add(time.Duration(i+1) * time.Minute)
			writeFile(t, certFile, tt.cert, mtime)
			writeFile(t, keyFile, tt.key, mtime)
			if err := f.Reload(); err == nil {
				t.Fatal("Reload succeeded, want error")
			}
			if got := servedCN(t, f); got != "good.test" {
				t.Errorf("CN = %q, want good.test kept", got)
			}
		})
	}
}

func TestNewFilesErrors(t *testing.T) {
	certFile, keyFile := pairPaths(t)
	if _, err := NewFiles(certFile, keyFile, FilesOptions{}); err == nil {
		t.Error("NewFiles with missing files succeeded")
	}
	writeFile(t, certFile, []byte("junk"), time.Now())
	writeFile(t, keyFile, []byte("junk"), time.Now())
	if _, err := NewFiles(certFile, keyFile, FilesOptions{}); err == nil {
		t.Error("NewFiles with junk files succeeded")
	}
}

func TestFilesRunPolls(t *testing.T) {
	certFile, keyFile := pairPaths(t)
	base := time.Now().Add(-time.Hour)
	writePair(t, certFile, keyFile, "first.test", base)

	logs := &recordHandler{}
	f, err := NewFiles(certFile, keyFile, FilesOptions{
		PollEvery: 5 * time.Millisecond,
		Logger:    slog.New(logs),
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx) }()

	writePair(t, certFile, keyFile, "second.test", base.Add(time.Minute))
	waitFor(t, "poll to pick up the new pair", func() bool {
		c, _ := f.GetCertificate(nil)
		return c.Leaf.Subject.CommonName == "second.test"
	})

	writeFile(t, certFile, []byte("broken"), base.Add(2*time.Minute))
	waitFor(t, "reload failure to be logged", func() bool {
		return logs.has("certs: reload failed, keeping previous certificate")
	})
	if got := servedCN(t, f); got != "second.test" {
		t.Errorf("CN after bad rewrite = %q, want second.test", got)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
}

func TestFilesClientCertificateReloads(t *testing.T) {
	certFile, keyFile := pairPaths(t)
	base := time.Now().Add(-time.Hour)
	writePair(t, certFile, keyFile, "client-1.test", base)
	f, err := NewFiles(certFile, keyFile, FilesOptions{Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	cn := func() string {
		t.Helper()
		c, err := f.GetClientCertificate(nil)
		if err != nil {
			t.Fatal(err)
		}
		return c.Leaf.Subject.CommonName
	}
	if got := cn(); got != "client-1.test" {
		t.Fatalf("CN = %q", got)
	}
	writePair(t, certFile, keyFile, "client-2.test", base.Add(time.Minute))
	if got := cn(); got != "client-2.test" {
		t.Errorf("CN after rotation = %q, want client-2.test without Run", got)
	}
	writeFile(t, certFile, []byte("half written"), base.Add(2*time.Minute))
	if got := cn(); got != "client-2.test" {
		t.Errorf("CN after a broken write = %q, want client-2.test kept", got)
	}
}
