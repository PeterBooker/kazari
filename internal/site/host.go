package site

import (
	"net"
	"strconv"
	"strings"

	"kazari/internal/artifact"
)

// maxVersionDigits bounds the version suffix so it always fits in an int.
const maxVersionDigits = 9

// ParseHost extracts the artifact slug and pinned version from a request
// host of the form <slug>.<base> or <slug>--v<N>.<base>. The host is
// lower-cased and its port stripped. version is 0 for the latest host.
//
// ok is false for anything that is not exactly one plausible slug label in
// front of base: the apex, deeper subdomains, another domain, a version of
// zero or with leading zeros, and labels with a stray "--". Reserved hosts
// such as api.<base> parse like any other label; the host router sends them
// elsewhere before this is called.
func ParseHost(host, base string) (slug string, version int, ok bool) {
	host = strings.ToLower(host)
	base = strings.TrimSuffix(strings.ToLower(base), ".")
	if base == "" {
		return "", 0, false
	}
	if strings.Contains(host, ":") {
		h, _, err := net.SplitHostPort(host)
		if err != nil {
			return "", 0, false
		}
		host = h
	}
	host = strings.TrimSuffix(host, ".")

	label, found := strings.CutSuffix(host, "."+base)
	if !found || label == "" || strings.Contains(label, ".") {
		return "", 0, false
	}

	slug = label
	if i := strings.LastIndex(label, artifact.VersionSeparator); i >= 0 {
		digits := label[i+len(artifact.VersionSeparator):]
		if isDigits(digits) {
			if len(digits) > maxVersionDigits || digits[0] == '0' {
				return "", 0, false
			}
			n, err := strconv.Atoi(digits)
			if err != nil {
				return "", 0, false
			}
			slug, version = label[:i], n
		}
	}
	// ValidateSlug rejects any "--" left in the slug, so a label with a
	// stray separator or a non-numeric "--v" suffix fails here.
	if artifact.ValidateSlug(slug, nil) != nil {
		return "", 0, false
	}
	return slug, version, true
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
