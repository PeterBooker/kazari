package config

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Origin says where a loaded configuration came from.
type Origin struct {
	// File is the configuration file, "" without one.
	File string
	// Preset is "--local" for the Local preset, else "".
	Preset string
	// EnvFiles are the dotenv files read, each with "(references only)" when
	// it fed ${NAME} references and no KAZARI_* overrides.
	EnvFiles []string
	// Unreadable is a dotenv file beside the configuration file that exists
	// but could not be read, with the reason; it was skipped.
	Unreadable string
	// Ignored are the KAZARI_* variables of a dotenv file that fed
	// references only and that no reference used: they changed nothing.
	Ignored []string
	// Keys maps a key path, such as listen.http, to where its value was
	// set. A key left at its default is absent.
	Keys map[string]Source
}

// Source is where one key's value was set: a line of the configuration
// file, a KAZARI_* variable, or both, the variable winning.
type Source struct {
	Line int // in Origin.File; 0 when the file does not set it
	// Env is the variable that set it, "" for none; EnvFile is the dotenv
	// file Env came from, "" for the process environment.
	Env, EnvFile string
}

// Describe says where key's value came from: "default" for a key nobody
// set, else the file's line and the variable, such as
// "KAZARI_DATA_DIR in /etc/kazari/kazari.env, over line 3 of /etc/kazari/kazari.yaml".
func (o Origin) Describe(key string) string {
	s, ok := o.Keys[key]
	if !ok {
		return "default"
	}
	var parts []string
	if s.Env != "" {
		env := s.Env
		if s.EnvFile != "" {
			env += " in " + s.EnvFile
		} else {
			env += " in the environment"
		}
		parts = append(parts, env)
	}
	if s.Line > 0 {
		line := fmt.Sprintf("line %d of %s", s.Line, o.File)
		if s.Env != "" {
			line = "over " + line
		}
		parts = append(parts, line)
	}
	return strings.Join(parts, ", ")
}

// Overrides lists the keys a KAZARI_* variable set, sorted.
func (o Origin) Overrides() []string {
	var out []string
	for _, k := range slices.Sorted(maps.Keys(o.Keys)) {
		if o.Keys[k].Env != "" {
			out = append(out, k)
		}
	}
	return out
}

// Sources describes every key that is not at its default, as Describe does.
func (o Origin) Sources() map[string]string {
	out := make(map[string]string, len(o.Keys))
	for k := range o.Keys {
		out[k] = o.Describe(k)
	}
	return out
}

// Summary names the configuration's source and the dotenv files read, for
// kazari check and the start log: "/etc/kazari/kazari.yaml with /etc/kazari/kazari.env".
func (o Origin) Summary() string {
	src := o.File
	switch {
	case o.Preset != "":
		src = o.Preset
	case src == "":
		src = "environment only"
	}
	if len(o.EnvFiles) > 0 {
		src += " with " + strings.Join(o.EnvFiles, ", ")
	}
	return src
}
