package middleware

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"

	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/PeterBooker/kazari/internal/errcode"
)

const internalErrorBody = `{"error":{"code":"internal","message":"internal error"}}` + "\n"

// Recover logs a panic and, if nothing was written yet, answers a 500 JSON
// error. http.ErrAbortHandler is re-panicked so net/http aborts the response.
func Recover(l *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rw := wrap(w)
			defer func() {
				v := recover()
				if v == nil {
					return
				}
				if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
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
				trace.SpanFromContext(r.Context()).AddEvent(semconv.ExceptionEventName, trace.WithAttributes(
					semconv.ExceptionType(fmt.Sprintf("%T", v)),
					semconv.ExceptionMessage(fmt.Sprint(v)),
				))
				if rw.wroteHeader {
					return
				}
				SetErrorCode(r.Context(), errcode.Internal)
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
