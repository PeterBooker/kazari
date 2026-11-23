package site

import (
	"compress/gzip"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// minCompressSize leaves out files whose gzip framing would outweigh what it
// saves.
const minCompressSize = 1024

// maxCompressSize bounds the work one request can ask of the server: at
// gzipLevel a larger file costs tens of milliseconds of CPU every time.
const maxCompressSize = 4 << 20

// gzipLevel is near the default ratio at a fraction of the cost of the best.
const gzipLevel = 5

var gzipWriters sync.Pool

// compressible reports whether a file of this type and size is sent
// gzip-encoded to clients that accept it. Formats that are compressed
// already gain nothing.
func compressible(contentType string, size int64) bool {
	if size < minCompressSize || size > maxCompressSize {
		return false
	}
	base, _, _ := strings.Cut(contentType, ";")
	base = strings.ToLower(strings.TrimSpace(base))
	if strings.HasPrefix(base, "text/") || strings.HasSuffix(base, "+json") || strings.HasSuffix(base, "+xml") {
		return true
	}
	switch base {
	case "application/javascript", "application/x-javascript", "application/ecmascript",
		"application/json", "application/xml", "application/wasm",
		"font/ttf", "font/otf", "font/sfnt", "font/collection",
		"application/x-font-ttf", "application/x-font-otf", "application/font-sfnt", "application/x-font-truetype",
		"application/x-font-opentype", "application/vnd.ms-opentype":
		return true
	}
	return false
}

// acceptsGzip reports whether the Accept-Encoding field values give gzip, or
// its alias x-gzip, a non-zero weight, directly or through "*". An absent
// field gets an uncompressed answer, which every client can read.
func acceptsGzip(values []string) bool {
	gzipQ, starQ := -1.0, -1.0
	for _, v := range values {
		for item := range strings.SplitSeq(v, ",") {
			coding, params, _ := strings.Cut(item, ";")
			coding = strings.ToLower(strings.TrimSpace(coding))
			if coding != "gzip" && coding != "x-gzip" && coding != "*" {
				continue
			}
			q, ok := weight(params)
			if !ok {
				continue
			}
			if coding == "*" {
				starQ = max(starQ, q)
			} else {
				gzipQ = max(gzipQ, q)
			}
		}
	}
	if gzipQ >= 0 {
		return gzipQ > 0
	}
	return starQ > 0
}

// weight reads the q parameter of an Accept-Encoding item, 1 when absent.
// An item with an unreadable weight is ignored.
func weight(params string) (float64, bool) {
	q := 1.0
	for p := range strings.SplitSeq(params, ";") {
		name, value, found := strings.Cut(p, "=")
		if !found {
			if strings.TrimSpace(p) == "" {
				continue
			}
			return 0, false
		}
		if !strings.EqualFold(strings.TrimSpace(name), "q") {
			continue
		}
		value = strings.TrimSpace(value)
		if value == "" || len(value) > 5 || (value[0] != '0' && value[0] != '1') {
			return 0, false
		}
		f, err := strconv.ParseFloat(value, 64)
		if err != nil || f < 0 || f > 1 {
			return 0, false
		}
		q = f
	}
	return q, true
}

// wantsGzip reports whether r is answered with the gzip representation,
// whose ETag is etag. Range requests stay with http.ServeContent on the
// stored bytes, and so does an If-Match that does not name etag, so a
// client holding the stored bytes' ETag is answered as before.
func wantsGzip(r *http.Request, etag string) bool {
	h := r.Header
	if h.Get("Range") != "" || h.Get("If-Range") != "" {
		return false
	}
	if im := h.Values("If-Match"); len(im) > 0 && !match(im, etag) {
		return false
	}
	return acceptsGzip(h.Values("Accept-Encoding"))
}

// gzipETag is the strong validator of a file's gzip representation, which
// differs from the stored bytes that the bare hash names.
func gzipETag(hash string) string {
	return `"` + hash + `-gzip"`
}

// noneMatch reports whether an If-None-Match field value matches etag under
// the weak comparison: "*", or any listed tag with the same opaque value.
func noneMatch(values []string, etag string) bool {
	return eachTag(values, func(tag string, _ bool) bool { return tag == etag })
}

// match reports whether an If-Match field value matches etag under the
// strong comparison, which a weak tag never passes.
func match(values []string, etag string) bool {
	return eachTag(values, func(tag string, weak bool) bool { return !weak && tag == etag })
}

// eachTag reports whether the field values hold "*" or a tag for which
// matches is true.
func eachTag(values []string, matches func(tag string, weak bool) bool) bool {
	for _, v := range values {
		for {
			v = strings.TrimLeft(v, " \t,")
			if v == "" {
				break
			}
			if v[0] == '*' {
				return true
			}
			weak := strings.HasPrefix(v, "W/")
			v = strings.TrimPrefix(v, "W/")
			if v == "" || v[0] != '"' {
				break
			}
			end := strings.IndexByte(v[1:], '"')
			if end < 0 {
				break
			}
			if matches(v[:end+2], weak) {
				return true
			}
			v = v[end+2:]
		}
	}
	return false
}

// writeGzip sends rc gzip-encoded with status. The caller has set every
// other header. An error after the status has gone out ends the response
// with http.ErrAbortHandler: returning would end the body cleanly, and a
// client or a cache of a pinned version would keep a cut page as a whole one.
func writeGzip(w http.ResponseWriter, r *http.Request, status int, rc io.Reader) error {
	hdr := w.Header()
	hdr.Set("Content-Encoding", "gzip")
	hdr.Del("Content-Length")
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return nil
	}
	gz, _ := gzipWriters.Get().(*gzip.Writer)
	if gz == nil {
		// The level is a valid constant, so this cannot fail.
		gz, _ = gzip.NewWriterLevel(w, gzipLevel)
	} else {
		gz.Reset(w)
	}
	defer func() {
		gz.Reset(io.Discard)
		gzipWriters.Put(gz)
	}()
	if _, err := io.Copy(gz, rc); err != nil {
		return err
	}
	return gz.Close()
}
