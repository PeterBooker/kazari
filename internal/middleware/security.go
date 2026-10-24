package middleware

import "net/http"

// SecurityHeaders sets the default headers of control-plane hosts:
// Cross-Origin-Opener-Policy, nosniff, Referrer-Policy: no-referrer and
// Cache-Control: no-store. They are set before the handler runs, so a
// handler can override any of them (for example a cacheable Cache-Control).
func SecurityHeaders() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("Cross-Origin-Opener-Policy", "same-origin")
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Cache-Control", "no-store")
			next.ServeHTTP(w, r)
		})
	}
}
