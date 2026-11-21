package httpapi

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"github.com/PeterBooker/kazari/internal/artifact"
)

// lineFile is a generated text file that keeps none of its bytes in memory, so
// the heap of a test measures the code under test: lineLen bytes, the last of
// them a line break.
type lineFile struct {
	size, lineLen, off int64
}

func (f *lineFile) Read(p []byte) (int, error) {
	if f.off >= f.size {
		return 0, io.EOF
	}
	n := min(int64(len(p)), f.size-f.off)
	for i := range n {
		p[i] = 'x'
		if (f.off+i+1)%f.lineLen == 0 {
			p[i] = '\n'
		}
	}
	f.off += n
	return int(n), nil
}

func (f *lineFile) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += f.off
	case io.SeekEnd:
		offset += f.size
	}
	f.off = offset
	return offset, nil
}

// stalledClient accepts nothing: its first Write blocks like a full socket
// until release closes, then fails like a write deadline.
type stalledClient struct {
	header  http.Header
	stalled chan struct{}
	release chan struct{}
}

func (c *stalledClient) Header() http.Header { return c.header }
func (c *stalledClient) WriteHeader(int)     {}

func (c *stalledClient) Write([]byte) (int, error) {
	close(c.stalled)
	<-c.release
	return 0, errors.New("write deadline exceeded")
}

func heapAfterGC() int64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.HeapAlloc)
}

// A client that stops reading decides how long a response stays in flight, so
// the file must not be in memory for that long, however its lines are cut.
func TestAnchoredReadHoldsNoFileForStalledClient(t *testing.T) {
	const size, limit = 4 << 20, 1 << 20
	for _, tc := range []struct {
		name    string
		lineLen int64
	}{
		{"short lines", 8},
		{"one long line", size + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &stalledClient{header: http.Header{}, stalled: make(chan struct{}), release: make(chan struct{})}
			before := heapAfterGC()
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer func() {
					if v := recover(); v != nil && v != http.ErrAbortHandler {
						panic(v)
					}
				}()
				_ = writeAnchored(client, artifact.File{Path: "a.txt"}, &lineFile{size: size, lineLen: tc.lineLen})
			}()
			<-client.stalled
			held := heapAfterGC() - before
			close(client.release)
			<-done
			if held >= limit {
				t.Errorf("a stalled anchored read of %d bytes holds %d bytes of heap, want under %d", size, held, limit)
			}
		})
	}
}

type brokenFile struct {
	lineFile
	failed bool
}

// Read works for the check of the file and fails on the way to the client.
func (f *brokenFile) Read(p []byte) (int, error) {
	if f.failed {
		return 0, errors.New("disk failed")
	}
	return f.lineFile.Read(p)
}

func (f *brokenFile) Seek(offset int64, whence int) (int64, error) {
	f.failed = true
	return f.lineFile.Seek(offset, whence)
}

// The status is out by then, so a clean end would pass off part of the file as
// all of it.
func TestAnchoredReadAbortsOnFailedRead(t *testing.T) {
	defer func() {
		if v := recover(); v != http.ErrAbortHandler {
			t.Errorf("recovered %v, want http.ErrAbortHandler", v)
		}
	}()
	_ = writeAnchored(httptest.NewRecorder(), artifact.File{Path: "a.txt"}, &brokenFile{lineFile: lineFile{size: 100, lineLen: 8}})
}

func TestAnchoredReadRefusesBinary(t *testing.T) {
	rec := httptest.NewRecorder()
	err := writeAnchored(rec, artifact.File{Path: "a.bin"}, strings.NewReader(strings.Repeat("x", 5000)+"\x00"))
	if !errors.Is(err, artifact.ErrInvalidArgument) || rec.Body.Len() != 0 || rec.Header().Get("Content-Type") != "" {
		t.Errorf("err = %v, body %d bytes, headers %v", err, rec.Body.Len(), rec.Header())
	}
}
