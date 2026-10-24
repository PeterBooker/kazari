// Package blobtest is a conformance suite for blob.Store implementations.
package blobtest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"kazari/internal/blob"
)

// Run exercises a blob.Store. newStore must return an empty store; it is
// called once per subtest.
func Run(t *testing.T, newStore func(t *testing.T) blob.Store) {
	t.Run("PutOpenRoundTrip", func(t *testing.T) { testRoundTrip(t, newStore(t)) })
	t.Run("PutEmpty", func(t *testing.T) { testPutEmpty(t, newStore(t)) })
	t.Run("PutIdempotent", func(t *testing.T) { testIdempotent(t, newStore(t)) })
	t.Run("Stat", func(t *testing.T) { testStat(t, newStore(t)) })
	t.Run("Delete", func(t *testing.T) { testDelete(t, newStore(t)) })
	t.Run("OpenMissing", func(t *testing.T) { testOpenMissing(t, newStore(t)) })
	t.Run("FailedPutLeavesNothing", func(t *testing.T) { testFailedPut(t, newStore(t)) })
	t.Run("ConcurrentPutSameContent", func(t *testing.T) { testConcurrentPut(t, newStore(t)) })
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func put(t *testing.T, s blob.Store, content []byte) string {
	t.Helper()
	hash, size, err := s.Put(context.Background(), bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if want := sum(content); hash != want {
		t.Fatalf("Put hash = %s, want %s", hash, want)
	}
	if size != int64(len(content)) {
		t.Fatalf("Put size = %d, want %d", size, len(content))
	}
	return hash
}

func readAll(t *testing.T, s blob.Store, hash string) []byte {
	t.Helper()
	rc, err := s.Open(context.Background(), hash)
	if err != nil {
		t.Fatalf("Open(%s): %v", hash, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %s: %v", hash, err)
	}
	return b
}

func testRoundTrip(t *testing.T, s blob.Store) {
	content := []byte(strings.Repeat("hello, kazari\n", 10000))
	hash := put(t, s, content)
	if got := readAll(t, s, hash); !bytes.Equal(got, content) {
		t.Fatalf("content mismatch: got %d bytes, want %d", len(got), len(content))
	}

	// The reader must seek, for http.ServeContent.
	rc, err := s.Open(context.Background(), hash)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	end, err := rc.Seek(0, io.SeekEnd)
	if err != nil {
		t.Fatalf("Seek end: %v", err)
	}
	if end != int64(len(content)) {
		t.Fatalf("Seek end = %d, want %d", end, len(content))
	}
	if _, err := rc.Seek(7, io.SeekStart); err != nil {
		t.Fatalf("Seek start: %v", err)
	}
	buf := make([]byte, 6)
	if _, err := io.ReadFull(rc, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "kazari" {
		t.Fatalf("read after seek = %q, want %q", buf, "kazari")
	}
}

func testPutEmpty(t *testing.T, s blob.Store) {
	hash := put(t, s, nil)
	if got := readAll(t, s, hash); len(got) != 0 {
		t.Fatalf("empty blob read %d bytes", len(got))
	}
}

func testIdempotent(t *testing.T, s blob.Store) {
	content := []byte("same bytes")
	h1 := put(t, s, content)
	h2 := put(t, s, content)
	if h1 != h2 {
		t.Fatalf("second Put hash = %s, want %s", h2, h1)
	}
	if got := readAll(t, s, h1); !bytes.Equal(got, content) {
		t.Fatalf("content after second Put = %q", got)
	}
}

func testStat(t *testing.T, s blob.Store) {
	content := []byte("twelve bytes")
	hash := put(t, s, content)
	size, err := s.Stat(context.Background(), hash)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if size != int64(len(content)) {
		t.Fatalf("Stat size = %d, want %d", size, len(content))
	}
	if _, err := s.Stat(context.Background(), sum([]byte("absent"))); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("Stat missing: err = %v, want ErrNotFound", err)
	}
}

func testDelete(t *testing.T, s blob.Store) {
	ctx := context.Background()
	hash := put(t, s, []byte("doomed"))
	keep := put(t, s, []byte("kept"))
	if err := s.Delete(ctx, hash); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Open(ctx, hash); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("Open after Delete: err = %v, want ErrNotFound", err)
	}
	if _, err := s.Stat(ctx, hash); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("Stat after Delete: err = %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, hash); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
	if got := readAll(t, s, keep); string(got) != "kept" {
		t.Fatalf("other blob = %q after Delete", got)
	}
	// Putting deleted content again brings it back.
	put(t, s, []byte("doomed"))
	if got := readAll(t, s, hash); string(got) != "doomed" {
		t.Fatalf("re-put blob = %q", got)
	}
}

func testOpenMissing(t *testing.T, s blob.Store) {
	_, err := s.Open(context.Background(), sum([]byte("never stored")))
	if !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("Open missing: err = %v, want ErrNotFound", err)
	}
}

// failingReader yields its data, then an error instead of EOF.
type failingReader struct {
	data []byte
	err  error
}

func (f *failingReader) Read(p []byte) (int, error) {
	if len(f.data) == 0 {
		return 0, f.err
	}
	n := copy(p, f.data)
	f.data = f.data[n:]
	return n, nil
}

func testFailedPut(t *testing.T, s blob.Store) {
	ctx := context.Background()
	partial := []byte(strings.Repeat("x", 4096))
	boom := errors.New("boom")
	_, _, err := s.Put(ctx, &failingReader{data: partial, err: boom})
	if !errors.Is(err, boom) {
		t.Fatalf("Put with failing reader: err = %v, want boom", err)
	}
	if _, err := s.Stat(ctx, sum(partial)); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("partial blob visible: Stat err = %v", err)
	}
}

func testConcurrentPut(t *testing.T, s blob.Store) {
	content := []byte(strings.Repeat("concurrent content ", 5000))
	want := sum(content)
	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hash, _, err := s.Put(context.Background(), bytes.NewReader(content))
			if err != nil {
				errs <- err
				return
			}
			if hash != want {
				errs <- errors.New("hash mismatch: " + hash)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent Put: %v", err)
	}
	if got := readAll(t, s, want); !bytes.Equal(got, content) {
		t.Fatalf("content after concurrent Put: %d bytes, want %d", len(got), len(content))
	}
}
