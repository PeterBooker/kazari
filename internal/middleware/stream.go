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

// DownloadStall is how long the reader of a Download may take over one
// write before the server gives the download up. A backup holds the one
// backup slot until it ends.
const DownloadStall = time.Minute

// Download is a file sent as an attachment, written through Streaming. It
// sends its headers with the first byte, so that until then the response
// can still be an error.
type Download struct {
	w           http.ResponseWriter
	body        io.Writer
	name        string
	contentType string
	started     bool
}

// NewDownload returns a Download of a file called name, which must need no
// quoting, of contentType.
func NewDownload(w http.ResponseWriter, name, contentType string) *Download {
	return &Download{w: w, body: Streaming(w, DownloadStall), name: name, contentType: contentType}
}

// Start sends the headers if no write has, as an empty file needs.
func (d *Download) Start() {
	if d.started {
		return
	}
	d.started = true
	h := d.w.Header()
	h.Set("Content-Type", d.contentType)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Disposition", `attachment; filename="`+d.name+`"`)
	h.Set("Cache-Control", "no-store")
	h.Del("Content-Length")
	d.w.WriteHeader(http.StatusOK)
}

// Started reports whether the status is sent, after which a failure can
// only cut the connection.
func (d *Download) Started() bool { return d.started }

func (d *Download) Write(p []byte) (int, error) {
	d.Start()
	return d.body.Write(p)
}
