package site

import (
	"net"
	"strconv"
	"strings"

	"github.com/PeterBooker/kazari/internal/artifact"
)

// maxVersionDigits keeps the version within an int.
const maxVersionDigits = 9

// ParseHost parses <slug>.<base> or <slug>--v<N>.<base>, ignoring case and
// port; version is 0 for the latest host. ok is false unless host is one
// valid slug label under base with a canonical version. Reserved labels such
// as api parse like any other.
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
	// ValidateSlug rejects any "--" left, so stray separators and
	// non-numeric "--v" suffixes fail here.
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
