// Package memlimit tells how much memory the process may use: the lowest
// of its cgroup's memory limit, Go's memory limit and the machine's
// physical memory. A container is sized by its own limit, not the host's.
package memlimit

import (
	"bufio"
	"bytes"
	"io/fs"
	"math"
	"os"
	"path"
	"runtime/debug"
	"strconv"
	"strings"
)

// Where a Limit comes from.
const (
	Cgroup   = "cgroup limit"
	GoLimit  = "Go memory limit"
	Physical = "physical memory"
)

// Limit is the memory the process may use and the source of the figure.
type Limit struct {
	Bytes  int64
	Source string
}

// Options let a test stand in for the system. The zero value reads it.
type Options struct {
	// Root holds proc/self/cgroup and sys/fs/cgroup; nil is the root of
	// the filesystem.
	Root fs.FS
	// GoLimit returns Go's memory limit; nil asks the runtime.
	GoLimit func() int64
	// Physical returns the machine's memory; nil asks the system.
	Physical func() (int64, error)
}

// unlimited is where a cgroup v1 limit means none: the kernel reports no
// limit as the largest page-aligned value near math.MaxInt64.
const unlimited = 1 << 62

// Detect returns the lowest limit it can read, and false when it can read
// none.
func Detect(o Options) (Limit, bool) {
	if o.Root == nil {
		o.Root = os.DirFS("/")
	}
	if o.GoLimit == nil {
		o.GoLimit = func() int64 { return debug.SetMemoryLimit(-1) }
	}
	if o.Physical == nil {
		o.Physical = physical
	}
	var best Limit
	consider := func(n int64, source string) {
		if n > 0 && n < unlimited && (best.Bytes == 0 || n < best.Bytes) {
			best = Limit{Bytes: n, Source: source}
		}
	}
	consider(cgroupLimit(o.Root), Cgroup)
	if n := o.GoLimit(); n != math.MaxInt64 {
		consider(n, GoLimit)
	}
	if n, err := o.Physical(); err == nil {
		consider(n, Physical)
	}
	return best, best.Bytes > 0
}

// cgroupLimit is the lowest memory limit of the process's cgroup and the
// ones above it, under cgroup v2 or v1, or 0 when none is set. The root of
// the mount is read too: inside a container it is the container's cgroup.
func cgroupLimit(root fs.FS) int64 {
	v2, v1 := selfCgroups(root)
	var low int64
	read := func(name string) {
		if !fs.ValidPath(name) {
			return
		}
		b, err := fs.ReadFile(root, name)
		if err != nil {
			return
		}
		s := strings.TrimSpace(string(b))
		if s == "max" {
			return
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err == nil && n > 0 && n < unlimited && (low == 0 || n < low) {
			low = n
		}
	}
	walk := func(mount, dir, file string) {
		for {
			read(path.Join(mount, dir, file))
			if dir == "/" || dir == "." || dir == "" {
				return
			}
			dir = path.Dir(dir)
		}
	}
	walk("sys/fs/cgroup", v2, "memory.max")
	walk("sys/fs/cgroup/memory", v1, "memory.limit_in_bytes")
	return low
}

// selfCgroups reads the process's cgroup v2 path and its cgroup v1 memory
// path from proc/self/cgroup; "/" for one it cannot tell.
func selfCgroups(root fs.FS) (v2, v1 string) {
	v2, v1 = "/", "/"
	b, err := fs.ReadFile(root, "proc/self/cgroup")
	if err != nil {
		return v2, v1
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		id, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		controllers, p, ok := strings.Cut(rest, ":")
		if !ok || !strings.HasPrefix(p, "/") {
			continue
		}
		switch {
		case id == "0" && controllers == "":
			v2 = p
		case containsController(controllers, "memory"):
			v1 = p
		}
	}
	return v2, v1
}

func containsController(list, name string) bool {
	for c := range strings.SplitSeq(list, ",") {
		if c == name {
			return true
		}
	}
	return false
}
