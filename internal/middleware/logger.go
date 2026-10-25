package middleware

import (
	"log/slog"
	"net/http"
	"time"
	"unicode/utf8"
)

const maxUserAgent = 200

// Logger writes one line per completed request, at Error for 5xx.
func Logger(l *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rw := wrap(w)
			next.ServeHTTP(rw, r)

			status := rw.Status()
			level := slog.LevelInfo
			if status >= 500 {
				level = slog.LevelError
			}
			l.LogAttrs(r.Context(), level, "request",
				slog.String("method", r.Method),
				slog.String("host", r.Host),
				slog.String("path", r.URL.Path),
				slog.Int("status", status),
				slog.Int64("bytes", rw.bytes),
				slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
				slog.String("request_id", RequestIDFrom(r.Context())),
				slog.String("ip", ClientIP(r)),
				slog.String("user_agent", truncate(r.UserAgent(), maxUserAgent)),
			)
		})
	}
}

// truncate cuts s to at most n bytes without splitting a UTF-8 sequence.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
