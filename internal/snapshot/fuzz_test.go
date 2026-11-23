package snapshot_test

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/fetch"
	"github.com/PeterBooker/kazari/internal/snapshot"
)

// web answers every request from memory: the page at /, the stylesheet at
// any path ending in .css, nothing under /missing and a small image elsewhere.
type web struct{ page, css string }

func (w web) RoundTrip(r *http.Request) (*http.Response, error) {
	status, body, ct := http.StatusOK, "PNG", "image/png"
	switch {
	case r.URL.Path == "/" || r.URL.Path == "":
		body, ct = w.page, "text/html; charset=utf-8"
	case strings.HasSuffix(r.URL.Path, ".css"):
		body, ct = w.css, "text/css"
	case strings.HasPrefix(r.URL.Path, "/missing"):
		status, body, ct = http.StatusNotFound, "", "text/plain"
	}
	return &http.Response{
		StatusCode: status, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header:        http.Header{"Content-Type": {ct}},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       r,
	}, nil
}

// scriptURL reports whether a browser runs v as a script when it is
// followed: its URL parser drops leading controls and spaces, and tabs and
// line breaks anywhere, before it reads the scheme.
func scriptURL(v string) bool {
	v = strings.TrimLeftFunc(v, func(r rune) bool { return r <= ' ' })
	v = strings.NewReplacer("\t", "", "\n", "", "\r", "").Replace(v)
	const scheme = "javascript:"
	return len(v) >= len(scheme) && strings.EqualFold(v[:len(scheme)], scheme)
}

// navigates lists the attributes whose URL the element follows or loads as a document.
var navigates = map[atom.Atom]string{
	atom.A: "href", atom.Area: "href", atom.Form: "action", atom.Iframe: "src", atom.Object: "data", atom.Embed: "src",
}

// scriptsLeft lists what would still run a script in a page copied without them.
func scriptsLeft(n *html.Node) []string {
	var out []string
	if n.Type == html.ElementNode {
		switch {
		case n.DataAtom == atom.Script:
			out = append(out, "a script element")
		case n.DataAtom == atom.Base:
			out = append(out, "a base element")
		case n.DataAtom == atom.Iframe && hasAttr(n, "srcdoc"):
			out = append(out, "an iframe with srcdoc")
		}
		for _, a := range n.Attr {
			if strings.HasPrefix(strings.ToLower(a.Key), "on") {
				out = append(out, fmt.Sprintf("the attribute %s on <%s>", a.Key, n.Data))
			}
			if a.Key == navigates[n.DataAtom] && scriptURL(a.Val) {
				out = append(out, fmt.Sprintf("%s=%q on <%s>", a.Key, a.Val, n.Data))
			}
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		out = append(out, scriptsLeft(c)...)
	}
	return out
}

func hasAttr(n *html.Node, key string) bool {
	for _, a := range n.Attr {
		if a.Key == key {
			return true
		}
	}
	return false
}

func FuzzTake(f *testing.F) {
	const css = `@import "more.css"; body{background:url('bg.png')} @font-face{src:url(https://fonts.gstatic.com/f.woff2)}`
	for _, page := range []string{
		`<!doctype html><html><head><meta charset="iso-8859-1"><title> My  Page </title><meta name="description" content="About it">` +
			`<base href="/sub/"><link rel="stylesheet" href="../s.css" integrity="sha256-x" crossorigin="anonymous">` +
			`<link rel="canonical" href="/"><script src="https://cdnjs.cloudflare.com/x.js"></script></head>` +
			`<body onload="go()"><header><h1>Hello</h1><nav><a href="../about">About</a></nav></header>` +
			`<main><img src="../a.png" srcset="../a.png 1x, ../b.png 2x"><p style="background:url(../bg.png)">x</p>` +
			`<form action="post"><input></form><script>track()</script><iframe src="../f"></iframe></main>` +
			`<footer><picture><source srcset="../missing.png"><img src="data:image/gif;base64,R0lG"></picture></footer></body></html>`,
		`<html><body><a href="javascript:alert(1)">x</a><a href="/ok">y</a><iframe srcdoc="<script>1</script>"></iframe>` +
			`<form action=" JavaScript:go()"></form><object data="javascript:1"></object><embed src="JAVASCRIPT:1"></body></html>`,
		`<svg><use href="#a"/><a href="x"><script>1</script></a><style>a{fill:url(p.png)}</style></svg><math><mtext><table><mglyph><style><img src=x onerror=1>`,
		`<noscript><script>1</script></noscript><template><script>2</script><p onclick=1></template><xmp><script>3</script></xmp>`,
		`<link rel=preload as=script href=a.js><link rel=modulepreload href=b.js><link rel=icon href=/missing/i.ico><video src=v.mp4 poster=p.png><track src=t.vtt></video>`,
		`<section><h2>One</h2></section><article><h3>Two &amp; three</h3></article><aside>` + strings.Repeat("<p>x</p>", 30) + `</aside>`,
		`<plaintext><script>x</script>`, `<frameset><frame src="a">`, `<p>` + strings.Repeat("&amp;", 200), ``,
	} {
		f.Add(page, css, false)
		f.Add(page, css, true)
	}
	dir := f.TempDir()

	f.Fuzz(func(t *testing.T, page, css string, keepScripts bool) {
		opts := snapshot.Options{CSP: policy, MaxFileBytes: 64 << 10, MaxVersionBytes: 1 << 20, MaxFiles: 8}
		taker := snapshot.New(fetch.New(fetch.Options{Transport: web{page, css}, Dir: dir}), opts)
		snap, err := taker.Take(t.Context(), "http://page.test/", keepScripts)
		if err != nil {
			if artifact.CodeOf(err) == "" {
				t.Errorf("Take(%q, %q) = %v, want an error with a code", page, css, err)
			}
		} else {
			checkSnapshot(t, snap, opts, keepScripts)
			if err := snap.Close(); err != nil {
				t.Errorf("Take(%q, %q): Close = %v", page, css, err)
			}
		}
		if left, err := os.ReadDir(dir); err != nil || len(left) > 0 {
			t.Errorf("Take(%q, %q) left %v in its directory (%v), want nothing", page, css, left, err)
		}
	})
}

func checkSnapshot(t *testing.T, snap *artifact.Snapshot, opts snapshot.Options, keepScripts bool) {
	t.Helper()
	if len(snap.Files) == 0 || snap.Files[0].Path != "index.html" || len(snap.Files) > opts.MaxFiles {
		t.Fatalf("Take gave %d files, want index.html first and at most %d", len(snap.Files), opts.MaxFiles)
	}
	seen := map[string]bool{}
	var out []byte
	for i, file := range snap.Files {
		if clean, err := artifact.ValidatePath(file.Path); err != nil || clean != file.Path || seen[clean] {
			t.Errorf("Take gave the file %q (%v), want a valid path used once", file.Path, err)
		}
		seen[file.Path] = true
		data, err := io.ReadAll(file.Content)
		if err != nil || int64(len(data)) > opts.MaxFileBytes {
			t.Errorf("Take gave %q with %d bytes (%v), want at most %d", file.Path, len(data), err, opts.MaxFileBytes)
		}
		if i == 0 {
			out = data
		}
	}
	if len(snap.Report.Skipped) > 100 {
		t.Errorf("Take reported %d skipped resources, want at most 100", len(snap.Report.Skipped))
	}
	rows, last := bytes.Split(out, []byte("\n")), 1
	for _, l := range snap.Report.Outline {
		if l.Line < last || l.Line > len(rows) {
			t.Errorf("Take outlined %q at line %d of %d, after line %d", l.Tag, l.Line, len(rows), last)
			continue
		}
		last = l.Line
		if tag := strings.TrimSuffix(l.Tag, "…"); !bytes.Contains(bytes.Join(rows[l.Line-1:], []byte("\n")), []byte(tag)) {
			t.Errorf("Take outlined %q at line %d, which does not hold it", l.Tag, l.Line)
		}
	}
	if keepScripts {
		return
	}
	doc, err := html.Parse(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("Take gave a page that does not parse: %v", err)
	}
	if left := scriptsLeft(doc); len(left) > 0 {
		t.Errorf("Take without scripts left %s in the page:\n%s", strings.Join(left, ", "), out)
	}
}
