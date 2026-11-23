package site

import (
	"net/http"
	"strconv"
	"strings"
)

// minCompressSize leaves out files whose framing would outweigh what an
// encoding saves.
const minCompressSize = 1024

// maxCompressSize bounds the work of one compression and the memory of one
// cached entry: a larger file would take tens of milliseconds to encode.
const maxCompressSize = 4 << 20

// compressible reports whether a file of this type and size is sent
// encoded to clients that accept an encoding. Formats that are compressed
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

// knownCodings are the content codings an Accept-Encoding field is read
// for, whichever of them the site sends.
var knownCodings = [...]string{"br", "zstd", "gzip"}

// acceptance holds the weights an Accept-Encoding field gives: one per known
// coding, identity's and that of "*", each -1 when the field does not name it.
type acceptance struct {
	q        [len(knownCodings)]float64
	identity float64
	star     float64
}

// parseAcceptEncoding reads Accept-Encoding field values. A coding named
// more than once takes its highest weight, x-gzip is gzip, and an item with
// an unreadable weight is ignored.
func parseAcceptEncoding(values []string) acceptance {
	a := acceptance{identity: -1, star: -1}
	for i := range a.q {
		a.q[i] = -1
	}
	for _, v := range values {
		for item := range strings.SplitSeq(v, ",") {
			token, params, _ := strings.Cut(item, ";")
			token = strings.ToLower(strings.TrimSpace(token))
			var slot *float64
			switch token {
			case "*":
				slot = &a.star
			case "identity":
				slot = &a.identity
			case "x-gzip":
				slot = &a.q[codingIndex("gzip")]
			default:
				i := codingIndex(token)
				if i < 0 {
					continue
				}
				slot = &a.q[i]
			}
			if q, ok := weight(params); ok {
				*slot = max(*slot, q)
			}
		}
	}
	return a
}

func codingIndex(name string) int {
	for i, c := range knownCodings {
		if c == name {
			return i
		}
	}
	return -1
}

// of is the weight a gives the coding name: its own, else that of "*", else
// 0, as RFC 9110 section 12.5.3 reads a field. An absent field therefore
// gets the stored bytes, which every client can read.
func (a acceptance) of(name string) float64 {
	i := codingIndex(name)
	switch {
	case i < 0:
		return 0
	case a.q[i] >= 0:
		return a.q[i]
	case a.star >= 0:
		return a.star
	}
	return 0
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

// choose picks the coding a file of this hash is sent in, nil for the
// stored bytes. An If-Range or If-Match pins the encoded representation it
// names, so a client holding part of one goes on with it; one that names
// none of them gets the stored bytes, as a client holding their ETag
// expects. Otherwise, among the codings of the highest weight, one cached
// already comes first, then the order of codings. An identity weighted
// above them all keeps the stored bytes.
func choose(h http.Header, acc acceptance, hash string, cached func(*coding) bool) (c *coding, pinned bool) {
	ir, im := h.Get("If-Range"), h.Values("If-Match")
	if ir != "" || len(im) > 0 {
		for _, c := range codings {
			etag := codingETag(hash, c)
			if acc.of(c.name) > 0 && (ir == "" || ir == etag) && (len(im) == 0 || match(im, etag)) {
				return c, true
			}
		}
		return nil, false
	}
	best := 0.0
	for _, c := range codings {
		best = max(best, acc.of(c.name))
	}
	if best == 0 || acc.identity > best {
		return nil, false
	}
	var first *coding
	for _, c := range codings {
		if acc.of(c.name) != best {
			continue
		}
		if cached(c) {
			return c, false
		}
		if first == nil {
			first = c
		}
	}
	return first, false
}

// codingETag is the strong validator of a file's representation in c,
// which differs from the stored bytes that the bare hash names.
func codingETag(hash string, c *coding) string {
	return `"` + hash + "-" + c.name + `"`
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
