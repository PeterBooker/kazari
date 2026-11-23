package memlimit_test

import (
	"errors"
	"math"
	"testing"
	"testing/fstest"

	"github.com/PeterBooker/kazari/internal/memlimit"
)

const gib = 1 << 30

func file(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }

func TestDetect(t *testing.T) {
	noGo := func() int64 { return math.MaxInt64 }
	ram := func(n int64) func() (int64, error) { return func() (int64, error) { return n, nil } }
	noRAM := func() (int64, error) { return 0, errors.New("unknown") }
	service := "0::/system.slice/kazari.service\n"

	tests := []struct {
		name     string
		root     fstest.MapFS
		goLimit  func() int64
		physical func() (int64, error)
		want     memlimit.Limit
		ok       bool
	}{
		{name: "physical only", root: fstest.MapFS{}, goLimit: noGo, physical: ram(64 * gib),
			want: memlimit.Limit{Bytes: 64 * gib, Source: memlimit.Physical}, ok: true},
		{name: "v2 container", root: fstest.MapFS{
			"proc/self/cgroup":         file("0::/\n"),
			"sys/fs/cgroup/memory.max": file("1073741824\n"),
		}, goLimit: noGo, physical: ram(64 * gib), want: memlimit.Limit{Bytes: gib, Source: memlimit.Cgroup}, ok: true},
		{name: "v2 max", root: fstest.MapFS{
			"proc/self/cgroup":         file("0::/\n"),
			"sys/fs/cgroup/memory.max": file("max\n"),
		}, goLimit: noGo, physical: ram(8 * gib), want: memlimit.Limit{Bytes: 8 * gib, Source: memlimit.Physical}, ok: true},
		{name: "v2 systemd unit", root: fstest.MapFS{
			"proc/self/cgroup": file(service),
			"sys/fs/cgroup/system.slice/kazari.service/memory.max": file("536870912\n"),
			"sys/fs/cgroup/system.slice/memory.max":                file("max\n"),
		}, goLimit: noGo, physical: ram(8 * gib), want: memlimit.Limit{Bytes: gib / 2, Source: memlimit.Cgroup}, ok: true},
		{name: "v2 parent slice lower", root: fstest.MapFS{
			"proc/self/cgroup": file(service),
			"sys/fs/cgroup/system.slice/kazari.service/memory.max": file("max\n"),
			"sys/fs/cgroup/system.slice/memory.max":                file("268435456\n"),
		}, goLimit: noGo, physical: ram(8 * gib), want: memlimit.Limit{Bytes: gib / 4, Source: memlimit.Cgroup}, ok: true},
		{name: "v1 limit", root: fstest.MapFS{
			"proc/self/cgroup": file("12:cpu,cpuacct:/\n4:memory:/docker/abc\n"),
			"sys/fs/cgroup/memory/docker/abc/memory.limit_in_bytes": file("2147483648\n"),
		}, goLimit: noGo, physical: ram(8 * gib), want: memlimit.Limit{Bytes: 2 * gib, Source: memlimit.Cgroup}, ok: true},
		{name: "v1 unlimited", root: fstest.MapFS{
			"proc/self/cgroup":                           file("4:memory:/\n"),
			"sys/fs/cgroup/memory/memory.limit_in_bytes": file("9223372036854771712\n"),
		}, goLimit: noGo, physical: ram(8 * gib), want: memlimit.Limit{Bytes: 8 * gib, Source: memlimit.Physical}, ok: true},
		{name: "v1 mount root in a container", root: fstest.MapFS{
			"proc/self/cgroup":                           file("4:memory:/docker/abc\n"),
			"sys/fs/cgroup/memory/memory.limit_in_bytes": file("1073741824\n"),
		}, goLimit: noGo, physical: ram(8 * gib), want: memlimit.Limit{Bytes: gib, Source: memlimit.Cgroup}, ok: true},
		{name: "go limit lowest", root: fstest.MapFS{
			"sys/fs/cgroup/memory.max": file("2147483648\n"),
		}, goLimit: func() int64 { return gib }, physical: ram(8 * gib), want: memlimit.Limit{Bytes: gib, Source: memlimit.GoLimit}, ok: true},
		{name: "physical lowest", root: fstest.MapFS{
			"sys/fs/cgroup/memory.max": file("17179869184\n"),
		}, goLimit: func() int64 { return 32 * gib }, physical: ram(4 * gib), want: memlimit.Limit{Bytes: 4 * gib, Source: memlimit.Physical}, ok: true},
		{name: "garbage", root: fstest.MapFS{
			"proc/self/cgroup":         file("nonsense\n0::relative\n"),
			"sys/fs/cgroup/memory.max": file("lots\n"),
		}, goLimit: noGo, physical: ram(gib), want: memlimit.Limit{Bytes: gib, Source: memlimit.Physical}, ok: true},
		{name: "nothing readable", root: fstest.MapFS{}, goLimit: noGo, physical: noRAM},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := memlimit.Detect(memlimit.Options{Root: tt.root, GoLimit: tt.goLimit, Physical: tt.physical})
			if got != tt.want || ok != tt.ok {
				t.Errorf("Detect = %+v, %v; want %+v, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

// The real readers, on the machine running the test: whatever they find is
// a positive amount, from a known source.
func TestDetectSystem(t *testing.T) {
	got, ok := memlimit.Detect(memlimit.Options{Root: fstest.MapFS{}, GoLimit: func() int64 { return math.MaxInt64 }})
	if ok && (got.Bytes <= 0 || got.Source != memlimit.Physical) {
		t.Errorf("Detect with the physical reader alone = %+v, want a positive physical amount", got)
	}
}
