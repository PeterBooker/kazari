package middleware

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

type clientIPKey struct{}

// RealIP stores the client IP in the request context for ClientIP. In
// proxy mode it trusts the last X-Forwarded-For entry, which is the one the
// proxy in front of Agalma added (proxies that append keep whatever the
// client sent to the left of it), then X-Real-IP. Only enable it behind
// exactly one proxy that sets, overwrites or appends to those headers.
// Otherwise, and when the headers hold no valid address, it uses the host
// part of RemoteAddr.
func RealIP(proxyMode bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := ""
			if proxyMode {
				ip = forwardedIP(r)
			}
			if ip == "" {
				ip = remoteHost(r.RemoteAddr)
			}
			ctx := context.WithValue(r.Context(), clientIPKey{}, ip)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ClientIP returns the IP stored by RealIP, or the host part of RemoteAddr
// when RealIP did not run.
func ClientIP(r *http.Request) string {
	if ip, ok := r.Context().Value(clientIPKey{}).(string); ok {
		return ip
	}
	return remoteHost(r.RemoteAddr)
}

func forwardedIP(r *http.Request) string {
	if vals := r.Header.Values("X-Forwarded-For"); len(vals) > 0 {
		xff := vals[len(vals)-1]
		last := xff[strings.LastIndex(xff, ",")+1:]
		if ip, ok := parseIP(last); ok {
			return ip
		}
	}
	if ip, ok := parseIP(r.Header.Get("X-Real-IP")); ok {
		return ip
	}
	return ""
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
