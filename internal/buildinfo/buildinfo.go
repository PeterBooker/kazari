// Package buildinfo reports the version, commit and build date, set with
// -ldflags "-X agalma/internal/buildinfo.version=..." and falling back to
// the module's own build information.
package buildinfo

import (
	"runtime"
	"runtime/debug"
)

var (
	version = ""
	commit  = ""
	date    = ""
)

// Info describes the running binary.
type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	Go      string `json:"go"`
}

// Get returns the build information.
func Get() Info {
	info := Info{Version: version, Commit: commit, Date: date, Go: runtime.Version()}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		if info.Version == "" {
			info.Version = "dev"
		}
		return info
	}
	if info.Version == "" {
		if bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			info.Version = bi.Main.Version
		} else {
			info.Version = "dev"
		}
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if info.Commit == "" {
				info.Commit = s.Value
			}
		case "vcs.time":
			if info.Date == "" {
				info.Date = s.Value
			}
		}
	}
	return info
}

// String is "version (commit)".
func (i Info) String() string {
	if i.Commit == "" {
		return i.Version
	}
	c := i.Commit
	if len(c) > 12 {
		c = c[:12]
	}
	return i.Version + " (" + c + ")"
}
