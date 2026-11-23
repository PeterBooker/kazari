package app_test

import (
	"bytes"
	"log/slog"
	"math"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/PeterBooker/kazari/internal/app"
	"github.com/PeterBooker/kazari/internal/memlimit"
)

// fakeMemory stands in for the system: 8 GiB of physical memory, no Go
// limit, and the cgroup files in root, if any.
func fakeMemory(root fstest.MapFS) memlimit.Options {
	if root == nil {
		root = fstest.MapFS{}
	}
	return memlimit.Options{
		Root:     root,
		GoLimit:  func() int64 { return math.MaxInt64 },
		Physical: func() (int64, error) { return 8 << 30, nil },
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// The default serve.compression_cache is a share of the cgroup's limit,
// which the start log states.
func TestCompressionCacheFromCgroup(t *testing.T) {
	var logs syncBuffer
	startWith(t, testConfig(t), app.Options{
		Logger: slog.New(slog.NewTextHandler(&logs, nil)),
		Memory: fakeMemory(fstest.MapFS{
			"proc/self/cgroup":         {Data: []byte("0::/\n")},
			"sys/fs/cgroup/memory.max": {Data: []byte("1073741824\n")},
		}),
	})
	const want = `msg="compression cache 51.2 MiB (5% of 1 GiB cgroup limit)"`
	if !strings.Contains(logs.String(), want) {
		t.Errorf("start log lacks %s:\n%s", want, logs.String())
	}
}
