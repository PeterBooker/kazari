// Package csp reads Content-Security-Policy values the way browsers do, for
// the parts of Kazari that reason about the instance policy.
package csp

import (
	"net/url"
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
