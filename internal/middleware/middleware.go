// Package middleware holds the net/http middleware shared by every server:
// request ids, client IP resolution, access logging, panic recovery and the
// control-plane security headers.
//
// A typical stack, outermost first, is
//
//	Chain(h, RequestID(), RealIP(proxy), Logger(l), Recover(l))
//
// so the access log sees the request id, the client IP and the 500 that
// Recover writes after a panic.
package middleware

import "net/http"

// Chain wraps h in mws. The first middleware is the outermost: it sees the
// request first and the response last.
func Chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// responseWriter records the status and body size of a response. It
// forwards Flush and exposes Unwrap so http.ResponseController can reach
// the other optional interfaces of the underlying writer.
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
	// 1xx responses other than 101 are informational and may be followed
	// by the real status.
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

// Flush implements http.Flusher.
func (w *responseWriter) Flush() {
	if !w.wroteHeader {
		w.status = http.StatusOK
		w.wroteHeader = true
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap returns the underlying writer for http.ResponseController.
func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Status returns the recorded status, 200 if the handler wrote nothing
// (which is what net/http sends).
func (w *responseWriter) Status() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}
