package middleware

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

type clientIPKey struct{}

// RealIP stores the client IP for ClientIP. A non-empty header names the one
// header the proxy in front sets to the client's address; for
// X-Forwarded-For that is its last entry, the one the proxy appended
// (appending proxies keep what the client sent to its left). Use it only
// behind exactly one proxy. With header empty, or with no valid address in
// it, RealIP uses RemoteAddr's host.
func RealIP(header string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := ""
			if header != "" {
				ip = headerIP(r.Header, header)
			}
			if ip == "" {
				ip = remoteHost(r.RemoteAddr)
			}
			ctx := context.WithValue(r.Context(), clientIPKey{}, ip)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ClientIP returns the IP RealIP stored, else RemoteAddr's host.
func ClientIP(r *http.Request) string {
	if ip, ok := r.Context().Value(clientIPKey{}).(string); ok {
		return ip
	}
	return remoteHost(r.RemoteAddr)
}

func headerIP(h http.Header, name string) string {
	vals := h.Values(name)
	if len(vals) == 0 {
		return ""
	}
	v := vals[len(vals)-1]
	if http.CanonicalHeaderKey(name) == "X-Forwarded-For" {
		v = v[strings.LastIndex(v, ",")+1:]
	}
	ip, _ := parseIP(v)
	return ip
}

func parseIP(s string) (string, bool) {
	addr, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil {
		return "", false
	}
	return addr.Unmap().String(), true
}

func remoteHost(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}
