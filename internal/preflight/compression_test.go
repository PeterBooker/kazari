package preflight

import (
	"fmt"
	"math"
	"testing"
	"testing/fstest"

	"github.com/PeterBooker/kazari/internal/config"
	"github.com/PeterBooker/kazari/internal/memlimit"
)

func TestCheckCompression(t *testing.T) {
	container := memlimit.Options{
		Root: fstest.MapFS{
			"proc/self/cgroup":         {Data: []byte("0::/\n")},
			"sys/fs/cgroup/memory.max": {Data: []byte("2147483648\n")},
		},
		GoLimit:  func() int64 { return math.MaxInt64 },
		Physical: func() (int64, error) { return 64 << 30, nil },
	}
	tests := []struct {
		name   string
		set    config.Memory
		status Status
		detail string
	}{
		{"default", config.Memory{Share: 500}, OK, "a cache of 102.4 MiB (5% of 2 GiB cgroup limit)"},
		{"size", config.Memory{Size: 32 * config.MiB}, OK, "a cache of 32 MiB (serve.compression_cache)"},
		{"off", config.Memory{}, Skip, "off: serve.compression_cache is 0, so every file is sent as stored"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Local()
			cfg.Serve.CompressionCache = tt.set
			var got []Finding
			checkCompression(cfg, container, func(check string, s Status, format string, args ...any) {
				got = append(got, Finding{Check: check, Status: s, Detail: fmt.Sprintf(format, args...)})
			})
			if len(got) != 1 || got[0].Check != "compression" || got[0].Status != tt.status || got[0].Detail != tt.detail {
				t.Errorf("checkCompression(%v) = %+v, want one %s finding %q", tt.set, got, tt.status, tt.detail)
			}
		})
	}
}
