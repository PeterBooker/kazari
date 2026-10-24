package middleware

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
)

const internalErrorBody = `{"error":{"code":"internal","message":"internal error"}}` + "\n"

// Recover turns a panic in the handler into a logged error and, if nothing
// was written yet, a 500 JSON error. http.ErrAbortHandler is re-panicked so
// net/http can abort the response as intended.
func Recover(l *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rw := wrap(w)
			defer func() {
				v := recover()
				if v == nil {
					return
				}
				if v == http.ErrAbortHandler {
					panic(v)
				}
				l.ErrorContext(r.Context(), "panic",
					slog.String("panic", fmt.Sprint(v)),
					slog.String("stack", string(debug.Stack())),
					slog.String("method", r.Method),
					slog.String("host", r.Host),
					slog.String("path", r.URL.Path),
					slog.String("request_id", RequestIDFrom(r.Context())),
				)
				if rw.wroteHeader {
					return
				}
				h := rw.Header()
				for _, k := range []string{"Content-Length", "Content-Encoding", "ETag", "Last-Modified"} {
					h.Del(k)
				}
				h.Set("Content-Type", "application/json")
				h.Set("Cache-Control", "no-store")
				h.Set("X-Content-Type-Options", "nosniff")
				rw.WriteHeader(http.StatusInternalServerError)
				_, _ = rw.Write([]byte(internalErrorBody))
			}()
			next.ServeHTTP(rw, r)
		})
	}
}
