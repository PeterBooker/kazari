package snapshot

import (
	"bytes"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// The rendering can be far larger than the page, so the outline counts bytes
// scanned as well as searches: each miss here scans the whole rendering, and
// the last tag, found at its end, is out of reach once they have spent the
// limit. A landmark found before that keeps its entry.
func TestOutlineStopsAfterScanningTheLimit(t *testing.T) {
	doc, err := html.Parse(strings.NewReader(`<!doctype html><body><nav></nav><svg>` +
		strings.Repeat("<section xlink:href=x />", 3) + `</svg><p>padding</p><section id=after></section>`))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := html.Render(&buf, doc); err != nil {
		t.Fatal(err)
	}
	page := buf.Bytes()
	if o := outline(doc, page, 4*len(page)); len(o) != 2 {
		t.Fatalf("with room for every search: %+v", o)
	}
	o := outline(doc, page, 2*len(page))
	if len(o) != 1 || o[0].Tag != "<nav>" {
		t.Errorf("outline searched on past the scan limit: %+v", o)
	}
}
