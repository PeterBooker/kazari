package httpapi

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"agalma/internal/artifact"
)

type tarEntry struct {
	name string
	typ  byte
	body string
}

func gzipTar(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o644, Size: int64(len(e.body))}
		if e.typ != tar.TypeReg {
			hdr.Size = 0
			hdr.Linkname = "target"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func many(n int, typ byte) []tarEntry {
	out := make([]tarEntry, n)
	for i := range out {
		out[i] = tarEntry{name: fmt.Sprintf("f%d", i), typ: typ}
	}
	return out
}

// Header blocks count against the uncompressed cap, so a bundle of many
// empty or skipped entries stops early instead of decoding without bound.
func TestReadBundleEntryCap(t *testing.T) {
	const limit = 64 << 10
	tests := []struct {
		name    string
		entries []tarEntry
		files   int
		tooBig  bool
	}{
		{"small bundle", []tarEntry{{"index.html", tar.TypeReg, "hi"}, {"./a/b.css", tar.TypeReg, "x"}}, 2, false},
		{"skipped links still parse", []tarEntry{{"l", tar.TypeSymlink, ""}, {"index.html", tar.TypeReg, "hi"}}, 1, false},
		{"many empty files", many(1000, tar.TypeReg), 0, true},
		{"many skipped symlinks", many(1000, tar.TypeSymlink), 0, true},
		{"many hard links", many(1000, tar.TypeLink), 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := gzipTar(t, tt.entries)
			b, err := readBundle(bytes.NewReader(raw), t.TempDir(), limit)
			if tt.tooBig {
				if !errors.Is(err, artifact.ErrTooLarge) {
					t.Fatalf("readBundle err = %v, want too_large", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			if len(b.files) != tt.files {
				t.Errorf("files = %d, want %d", len(b.files), tt.files)
			}
		})
	}
}

// The limiter map stays bounded however many distinct keys arrive, and a
// flood of fresh keys does not reset the bucket of a key that is in use.
func TestLimiterBounded(t *testing.T) {
	now := time.Unix(0, 0)
	l := newLimiter(1, func() time.Time { return now })
	if d := l.reserve("victim"); d != 0 {
		t.Fatalf("first request delayed %v", d)
	}
	for i := range limiterMaxEntries + 500 {
		l.reserve(fmt.Sprintf("k%d", i))
	}
	if n := len(l.entries); n > limiterMaxEntries {
		t.Fatalf("entries = %d, want at most %d", n, limiterMaxEntries)
	}
	// Idle entries are swept once the sweep interval has passed.
	now = now.Add(limiterIdle + limiterSweepEvery + time.Second)
	l.reserve("fresh")
	if n := len(l.entries); n != 1 {
		t.Errorf("entries after idle sweep = %d, want 1", n)
	}
}

func TestIPBucket(t *testing.T) {
	tests := []struct {
		remote, want string
	}{
		{"192.0.2.7:1234", "192.0.2.7"},
		{"[2001:db8:1:2:aaaa::1]:443", "2001:db8:1:2::/64"},
		{"[2001:db8:1:2:bbbb::9]:443", "2001:db8:1:2::/64"},
		{"[::ffff:192.0.2.7]:80", "192.0.2.7"},
		{"not-an-ip", "not-an-ip"},
	}
	for _, tt := range tests {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = tt.remote
		if got := ipBucket(r); got != tt.want {
			t.Errorf("ipBucket(%q) = %q, want %q", tt.remote, got, tt.want)
		}
	}
}
