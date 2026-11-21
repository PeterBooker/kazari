package middleware

import (
	"io"
	"net/http"
	"time"
)

// Streaming returns a writer for a response of any length, such as a
// backup: the server's write timeout, which bounds a whole response, is
// replaced by one that each write renews. The response then lasts as long
// as its client keeps reading, and ends when the client stalls for longer
// than stall. Lifting the timeout instead would let a client that stopped
// reading hold the handler, and whatever it has locked, for ever.
func Streaming(w http.ResponseWriter, stall time.Duration) io.Writer {
	return &streamWriter{w: w, rc: http.NewResponseController(w), stall: stall}
}

type streamWriter struct {
	w     http.ResponseWriter
	rc    *http.ResponseController
	stall time.Duration
}

func (s *streamWriter) Write(p []byte) (int, error) {
	// A writer that takes no deadline, as in a test, is written to as it is.
	_ = s.rc.SetWriteDeadline(time.Now().Add(s.stall))
	return s.w.Write(p)
}
