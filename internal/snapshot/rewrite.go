package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"mime"
	"net/url"
	"path"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

func (w *walker) walk(n *html.Node) {
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling
		if c.Type == html.ElementNode {
			if w.element(c) {
				n.RemoveChild(c)
			} else {
				w.walk(c)
			}
		}
		c = next
	}
}

// element rewrites one element's references and reports whether to drop it.
func (w *walker) element(n *html.Node) (remove bool) {
	if n.Data == "use" { // SVG, so it has no atom
		w.localRef(n, "href", "image")
		w.localRef(n, "xlink:href", "image")
	}
	switch n.DataAtom {
	case atom.Script:
		if !w.keepScripts {
			w.report.ScriptsRemoved++
			return true
		}
		if attr(n, "src") != "" {
			w.localRef(n, "src", "script")
		}
	case atom.Base:
		return true
	case atom.Meta:
		if attr(n, "charset") != "" {
			return true
		}
		switch strings.ToLower(attr(n, "http-equiv")) {
		case "refresh", "content-security-policy", "content-type":
			return true
		}
		if strings.EqualFold(attr(n, "name"), "description") {
			w.description = attr(n, "content")
		}
	case atom.Link:
		return w.link(n)
	case atom.Img:
		w.localRef(n, "src", "image")
		w.srcset(n)
	case atom.Source:
		w.localRef(n, "src", "media")
		w.srcset(n)
	case atom.Video, atom.Audio:
		w.localRef(n, "src", "media")
		w.localRef(n, "poster", "image")
	case atom.Track:
		w.localRef(n, "src", "media")
	case atom.A, atom.Area:
		w.absolute(n, "href")
	case atom.Form:
		w.absolute(n, "action")
	case atom.Iframe:
		w.absolute(n, "src")
	}
	if st := attr(n, "style"); strings.Contains(st, "url(") {
		setAttr(n, "style", string(w.rewriteCSS([]byte(st), w.base, 1, assetDir)))
	}
	if n.DataAtom == atom.Style && n.FirstChild != nil && n.FirstChild.Type == html.TextNode {
		n.FirstChild.Data = string(w.rewriteCSS([]byte(n.FirstChild.Data), w.base, 1, assetDir))
	}
	if !w.keepScripts {
		kept := n.Attr[:0]
		for _, a := range n.Attr {
			if !strings.HasPrefix(strings.ToLower(a.Key), "on") {
				kept = append(kept, a)
			}
		}
		n.Attr = kept
	}
	return false
}

// link handles <link>: resources are localised, hints and metadata about
// the source site are dropped.
func (w *walker) link(n *html.Node) (remove bool) {
	rel := " " + strings.ToLower(attr(n, "rel")) + " "
	has := func(r string) bool { return strings.Contains(rel, " "+r+" ") }
	switch {
	case has("stylesheet"):
		w.localRef(n, "href", "style")
	case has("icon"), has("apple-touch-icon"), has("mask-icon"):
		w.localRef(n, "href", "image")
	case has("modulepreload"):
		if !w.keepScripts {
			return true
		}
		w.localRef(n, "href", "script")
	case has("preload"):
		as := strings.ToLower(attr(n, "as"))
		if as == "script" && !w.keepScripts {
			return true
		}
		switch as {
		case "style", "font", "image", "script":
			w.localRef(n, "href", as)
		case "video", "audio", "track":
			w.localRef(n, "href", "media")
		default:
			return true
		}
	case has("manifest"), has("canonical"), has("alternate"), has("dns-prefetch"), has("preconnect"), has("prefetch"), has("prerender"):
		return true
	}
	return false
}

// localRef rewrites one URL attribute; a localised file drops integrity and
// crossorigin, which belonged to the bytes as the source served them.
func (w *walker) localRef(n *html.Node, key, kind string) {
	v := attr(n, key)
	if v == "" {
		return
	}
	name, local := w.localize(w.base, v, kind, 1)
	if local {
		setAttr(n, key, assetDir+name)
		delAttr(n, "integrity")
		delAttr(n, "crossorigin")
		return
	}
	setAttr(n, key, name)
}

func (w *walker) absolute(n *html.Node, key string) {
	if u := resolve(w.base, attr(n, key)); u != nil {
		setAttr(n, key, u.String())
	}
}

func (w *walker) srcset(n *html.Node) {
	v := attr(n, "srcset")
	if v == "" {
		return
	}
	var out []string
	for _, c := range parseSrcset(v) {
		name, local := w.localize(w.base, c.url, "image", 1)
		if local {
			name = assetDir + name
		}
		out = append(out, strings.TrimSpace(name+" "+c.descriptor))
	}
	setAttr(n, "srcset", strings.Join(out, ", "))
}

type candidate struct{ url, descriptor string }

// parseSrcset follows the HTML algorithm: a URL runs to whitespace, trailing
// commas belong to the separator, and descriptors run to the next comma.
func parseSrcset(s string) []candidate {
	var out []candidate
	i := 0
	for i < len(s) {
		for i < len(s) && (unicode.IsSpace(rune(s[i])) || s[i] == ',') {
			i++
		}
		if i >= len(s) {
			break
		}
		start := i
		for i < len(s) && !unicode.IsSpace(rune(s[i])) {
			i++
		}
		u := strings.TrimRight(s[start:i], ",")
		var desc string
		if len(s[start:i]) == len(u) {
			dstart := i
			for i < len(s) && s[i] != ',' {
				i++
			}
			desc = strings.TrimSpace(s[dstart:i])
		}
		out = append(out, candidate{u, desc})
	}
	return out
}

func kindOf(u *url.URL, fallback string) string {
	switch strings.ToLower(path.Ext(u.Path)) {
	case ".css":
		return "style"
	case ".woff", ".woff2", ".ttf", ".otf", ".eot":
		return "font"
	case ".js", ".mjs":
		return "script"
	case ".mp4", ".webm", ".ogv", ".mp3", ".ogg", ".wav", ".m4a", ".vtt":
		return "media"
	}
	return fallback
}

var unsafeName = regexp.MustCompile(`[^a-z0-9._-]+`)

// extensions covers the types whose extension the mime package lacks or
// gets wrong, such as .jpe for image/jpeg.
var extensions = map[string]string{
	"text/css": ".css", "text/javascript": ".js", "application/javascript": ".js", "image/svg+xml": ".svg",
	"image/jpeg": ".jpg", "image/png": ".png", "image/gif": ".gif", "image/webp": ".webp", "image/avif": ".avif",
	"image/x-icon": ".ico", "image/vnd.microsoft.icon": ".ico", "font/woff": ".woff", "font/woff2": ".woff2",
	"font/ttf": ".ttf", "font/otf": ".otf", "application/font-woff": ".woff", "application/font-woff2": ".woff2",
	"video/mp4": ".mp4", "video/webm": ".webm", "audio/mpeg": ".mp3", "application/json": ".json",
}

// localName is a short hash of the URL and its base name, so two files with
// one name from different paths do not clash.
func localName(u *url.URL, contentType string) string {
	b := strings.ToLower(path.Base(u.Path))
	if b == "/" || b == "." || b == "" {
		b = "file"
	}
	b = unsafeName.ReplaceAllString(b, "-")
	if len(b) > 60 {
		b = b[len(b)-60:]
	}
	if path.Ext(b) == "" {
		if mt, _, err := mime.ParseMediaType(contentType); err == nil {
			if ext, ok := extensions[mt]; ok {
				b += ext
			} else if exts, _ := mime.ExtensionsByType(mt); len(exts) > 0 {
				b += exts[0]
			}
		}
	}
	sum := sha256.Sum256([]byte(u.String()))
	return hex.EncodeToString(sum[:4]) + "-" + b
}

var (
	cssURL    = regexp.MustCompile(`url\(\s*(?:"([^"]*)"|'([^']*)'|([^'")\s]+))\s*\)`)
	cssImport = regexp.MustCompile(`@import\s+(?:"([^"]+)"|'([^']+)')`)
)

// rewriteCSS localises url() and @import references. prefix is what a local
// name is prefixed with in this context: "" inside assets/, assetDir in HTML.
func (w *walker) rewriteCSS(css []byte, base *url.URL, depth int, prefix string) []byte {
	css = cssImport.ReplaceAllFunc(css, func(m []byte) []byte {
		name, local := w.localize(base, firstGroup(cssImport, m), "style", depth)
		if local {
			name = prefix + name
		}
		return []byte(`@import "` + name + `"`)
	})
	return cssURL.ReplaceAllFunc(css, func(m []byte) []byte {
		name, local := w.localize(base, firstGroup(cssURL, m), "image", depth)
		if local {
			name = prefix + name
		}
		return []byte(`url("` + name + `")`)
	})
}

func firstGroup(re *regexp.Regexp, m []byte) string {
	for _, g := range re.FindSubmatch(m)[1:] {
		if len(g) > 0 {
			return string(g)
		}
	}
	return ""
}

// declareUTF8 adds the charset the page now has; the source's declaration
// was dropped with the other metas.
func declareUTF8(doc *html.Node) {
	head := find(doc, atom.Head)
	if head == nil {
		return
	}
	meta := &html.Node{Type: html.ElementNode, Data: "meta", DataAtom: atom.Meta, Attr: []html.Attribute{{Key: "charset", Val: "utf-8"}}}
	head.InsertBefore(meta, head.FirstChild)
}
