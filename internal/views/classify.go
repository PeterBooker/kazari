package views

import (
	"bytes"
	"net/http"
	"strings"
)

// Reason is why a request for a page is not a view; Counted when it is.
type Reason string

const (
	Counted Reason = ""
	// NotPage is a request that does not load a page into a browser tab:
	// a HEAD, a range, a frame, a fetch, a client such as curl.
	NotPage  Reason = "not_page"
	Prefetch Reason = "prefetch"
	Bot      Reason = "bot"
)

// botNames are matched against the lower-cased user agent. A browser that
// a mail scanner drives looks like any other and is counted.
var botNames = func() [][]byte {
	names := []string{
		"bot", "crawl", "spider", "slurp", "preview", "facebookexternalhit", "embedly",
		"headless", "lighthouse", "whatsapp", "skypeuripreview", "outlook-ios", "bingpreview",
		"python-requests", "go-http-client", "curl/", "wget/",
	}
	out := make([][]byte, len(names))
	for i, n := range names {
		out[i] = []byte(n)
	}
	return out
}()

// maxAgent is how much of a user agent is matched; every bot names itself
// well before it.
const maxAgent = 512

// Classify reports whether r, a request answered with an HTML file of an
// artifact, is a page view, or why not.
func Classify(r *http.Request) Reason {
	if r.Method != http.MethodGet || r.Header.Get("Range") != "" || !navigation(r) {
		return NotPage
	}
	if prefetch(r) {
		return Prefetch
	}
	if bot(r.Header.Get("User-Agent")) {
		return Bot
	}
	return Counted
}

// navigation is a top-level document load. A frame is left out so a page
// shown inside another, such as an MCP host's preview card, is not counted
// each time it renders. Browsers that send no Fetch Metadata are judged by
// their Accept header.
func navigation(r *http.Request) bool {
	if dest := r.Header.Get("Sec-Fetch-Dest"); dest != "" {
		return dest == "document"
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

func prefetch(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Sec-Purpose"), "prefetch") ||
		strings.EqualFold(r.Header.Get("Purpose"), "prefetch") ||
		strings.EqualFold(r.Header.Get("X-Moz"), "prefetch")
}

func bot(ua string) bool {
	if ua == "" {
		return true
	}
	var buf [maxAgent]byte
	n := copy(buf[:], ua)
	lower := buf[:n]
	for i, c := range lower {
		if 'A' <= c && c <= 'Z' {
			lower[i] = c + 'a' - 'A'
		}
	}
	for _, name := range botNames {
		if bytes.Contains(lower, name) {
			return true
		}
	}
	return false
}
