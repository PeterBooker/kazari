// Package middleware holds the net/http middleware every server shares.
// Chain(h, RequestID(), RealIP(proxy), Logger(l), Recover(l)) is the order
// that lets the access log see the request id, the client IP and the 500
// Recover writes after a panic.
package middleware

import "net/http"

// Chain wraps h in mws, the first outermost.
func Chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

type responseWriter struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func wrap(w http.ResponseWriter) *responseWriter {
	return &responseWriter{ResponseWriter: w}
}

func (w *responseWriter) WriteHeader(code int) {
	// 1xx other than 101 is informational; the real status follows.
	if !w.wroteHeader && (code >= 200 || code == http.StatusSwitchingProtocols) {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *responseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.status = http.StatusOK
		w.wroteHeader = true
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *responseWriter) Flush() {
	if !w.wroteHeader {
		w.status = http.StatusOK
		w.wroteHeader = true
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer's other
// optional interfaces.
func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Status is 200 if the handler wrote nothing, as net/http sends.
func (w *responseWriter) Status() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}
