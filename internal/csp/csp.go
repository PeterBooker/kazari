// Package csp reads Content-Security-Policy values the way browsers do, for
// the parts of Kazari that reason about the instance policy.
package csp

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
)

// Parse keeps the first of a repeated directive, as browsers do.
func Parse(policy string) map[string][]string {
	out := map[string][]string{}
	for d := range strings.SplitSeq(policy, ";") {
		f := strings.Fields(d)
		if len(f) == 0 {
			continue
		}
		name := strings.ToLower(f[0])
		if _, dup := out[name]; !dup {
			out[name] = f[1:]
		}
	}
	return out
}

// Sources returns the list that governs directive: its own, else default-src's.
func Sources(dirs map[string][]string, directive string) ([]string, bool) {
	if src, ok := dirs[directive]; ok {
		return src, true
	}
	src, ok := dirs["default-src"]
	return src, ok
}

// HostAllowed reports whether a host-source expression in sources names
// origin, such as https://cdn.example.com or https://*.example.com. Scheme
// sources ("https:"), "*" and keywords allow a class of origins, not this
// one, so they do not count.
func HostAllowed(sources []string, origin string) bool {
	o, err := url.Parse(strings.ToLower(origin))
	if err != nil || o.Host == "" {
		return false
	}
	for _, s := range sources {
		if matchHost(strings.ToLower(s), o) {
			return true
		}
	}
	return false
}

func matchHost(expr string, o *url.URL) bool {
	if expr == "*" || strings.HasPrefix(expr, "'") || strings.HasSuffix(expr, ":") {
		return false
	}
	scheme := ""
	if i := strings.Index(expr, "://"); i >= 0 {
		scheme, expr = expr[:i], expr[i+3:]
	}
	if i := strings.IndexByte(expr, '/'); i >= 0 {
		expr = expr[:i]
	}
	host, port := expr, ""
	if i := strings.LastIndexByte(expr, ':'); i >= 0 {
		host, port = expr[:i], expr[i+1:]
	}
	if scheme != "" && scheme != o.Scheme {
		return false
	}
	switch {
	case port == "*":
	case port == "":
		if p := o.Port(); p != "" && p != defaultPort(o.Scheme) {
			return false
		}
	default:
		if p := o.Port(); p != port && !(p == "" && port == defaultPort(o.Scheme)) {
			return false
		}
	}
	if rest, ok := strings.CutPrefix(host, "*."); ok {
		return strings.HasSuffix(o.Hostname(), "."+rest)
	}
	return host == o.Hostname()
}

func defaultPort(scheme string) string {
	if scheme == "http" {
		return "80"
	}
	return "443"
}

// fetchDirectives are the directives an artifact's addition may extend. The
// rest (frame-ancestors, base-uri, form-action, sandbox, reporting) stay the
// instance's.
var fetchDirectives = map[string]bool{
	"default-src": true, "script-src": true, "script-src-elem": true, "script-src-attr": true,
	"style-src": true, "style-src-elem": true, "style-src-attr": true, "font-src": true,
	"img-src": true, "media-src": true, "connect-src": true, "frame-src": true,
	"child-src": true, "worker-src": true, "manifest-src": true,
}

// ValidateAddition checks an artifact's additions to the instance policy:
// fetch directives only, each naming sources to add. A wildcard or a bare
// scheme would allow every origin, and 'none' cannot be added to a list.
func ValidateAddition(add string) error {
	for d := range strings.SplitSeq(add, ";") {
		f := strings.Fields(d)
		if len(f) == 0 {
			continue
		}
		name := strings.ToLower(f[0])
		if !fetchDirectives[name] {
			return fmt.Errorf("%s cannot be changed; only fetch directives such as script-src, style-src, font-src, img-src, connect-src and frame-src take additions", name)
		}
		if len(f) == 1 {
			return fmt.Errorf("%s names no source to add", name)
		}
		for _, s := range f[1:] {
			if !addable(s) {
				return fmt.Errorf("%s: %s cannot be added; name the origin, such as https://cdn.example.com", name, s)
			}
		}
	}
	return nil
}

func addable(s string) bool {
	switch strings.ToLower(s) {
	case "*", "'none'", "http:", "https:", "ws:", "wss:":
		return false
	}
	return true
}

// Merge returns policy with an artifact's additions: each source is appended
// to its directive, a directive the policy lacks starting from default-src's
// list. Parts ValidateAddition refuses are left out.
func Merge(policy, add string) string {
	if strings.TrimSpace(add) == "" {
		return policy
	}
	names, dirs := parseOrdered(policy)
	for d := range strings.SplitSeq(add, ";") {
		f := strings.Fields(d)
		if len(f) < 2 {
			continue
		}
		name := strings.ToLower(f[0])
		if !fetchDirectives[name] {
			continue
		}
		src, ok := dirs[name]
		if !ok {
			src = slices.Clone(dirs["default-src"])
		}
		for _, s := range f[1:] {
			if addable(s) && !slices.Contains(src, s) {
				src = append(src, s)
			}
		}
		if len(src) > 1 {
			src = slices.DeleteFunc(src, func(s string) bool { return strings.EqualFold(s, "'none'") })
		}
		if !ok && len(src) == 0 {
			continue
		}
		if !ok {
			names = append(names, name)
		}
		dirs[name] = src
	}
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, strings.TrimSpace(n+" "+strings.Join(dirs[n], " ")))
	}
	return strings.Join(parts, "; ")
}

func parseOrdered(policy string) ([]string, map[string][]string) {
	var names []string
	dirs := map[string][]string{}
	for d := range strings.SplitSeq(policy, ";") {
		f := strings.Fields(d)
		if len(f) == 0 {
			continue
		}
		name := strings.ToLower(f[0])
		if _, dup := dirs[name]; dup {
			continue
		}
		names = append(names, name)
		dirs[name] = f[1:]
	}
	return names, dirs
}

// Allows reports whether sources let a page load u, an absolute http(s) URL
// on another origin: a scheme source, "*" or a host source must cover it.
func Allows(sources []string, u string) bool {
	o, err := url.Parse(strings.ToLower(u))
	if err != nil || o.Host == "" {
		return false
	}
	for _, s := range sources {
		s = strings.ToLower(s)
		switch {
		case s == "*":
			if o.Scheme == "http" || o.Scheme == "https" {
				return true
			}
		case strings.HasSuffix(s, ":") && !strings.Contains(s, "/"):
			if s == o.Scheme+":" || (s == "http:" && o.Scheme == "https") {
				return true
			}
		case matchHost(s, o):
			return true
		}
	}
	return false
}
