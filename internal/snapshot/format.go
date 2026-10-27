package snapshot

import (
	"bytes"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"github.com/PeterBooker/kazari/internal/artifact"
)

// blockLike are the elements whose boundaries may take a newline when
// whitespace already stood there. New whitespace is created only inside
// <head> and between the direct children of <html> and <body>, where it
// cannot render; anywhere else it could open a gap between inline boxes.
var blockLike = map[atom.Atom]bool{
	atom.Html: true, atom.Head: true, atom.Body: true, atom.Div: true, atom.P: true,
	atom.Section: true, atom.Article: true, atom.Aside: true, atom.Header: true,
	atom.Footer: true, atom.Nav: true, atom.Main: true, atom.Ul: true, atom.Ol: true,
	atom.Li: true, atom.Table: true, atom.Thead: true, atom.Tbody: true, atom.Tfoot: true,
	atom.Tr: true, atom.Form: true, atom.Fieldset: true, atom.H1: true, atom.H2: true,
	atom.H3: true, atom.H4: true, atom.H5: true, atom.H6: true, atom.Hr: true,
	atom.Figure: true, atom.Figcaption: true, atom.Blockquote: true, atom.Dl: true,
	atom.Dt: true, atom.Dd: true, atom.Details: true, atom.Summary: true, atom.Pre: true,
	atom.Title: true, atom.Meta: true, atom.Link: true, atom.Style: true, atom.Script: true,
	atom.Noscript: true, atom.Template: true, atom.Svg: true, atom.Picture: true,
}

var preformatted = map[atom.Atom]bool{atom.Pre: true, atom.Textarea: true, atom.Script: true, atom.Style: true}

func isWhitespace(n *html.Node) bool {
	return n != nil && n.Type == html.TextNode && strings.TrimSpace(n.Data) == ""
}

func format(doc *html.Node) { formatNode(doc, 0) }

func formatNode(n *html.Node, depth int) {
	if n.Type == html.ElementNode && preformatted[n.DataAtom] {
		return
	}
	mayCreate := n.Type == html.DocumentNode || n.DataAtom == atom.Html || n.DataAtom == atom.Head || n.DataAtom == atom.Body
	indent := "\n" + strings.Repeat("  ", depth)
	hadBlock := false
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		switch c.Type {
		case html.ElementNode:
			if !blockLike[c.DataAtom] {
				continue
			}
		case html.DoctypeNode, html.CommentNode:
		default:
			continue
		}
		hadBlock = true
		switch {
		case isWhitespace(c.PrevSibling):
			c.PrevSibling.Data = indent
		case mayCreate:
			n.InsertBefore(&html.Node{Type: html.TextNode, Data: indent}, c)
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode {
			formatNode(c, depth+1)
		}
	}
	if !hadBlock || n.Type != html.ElementNode {
		return
	}
	closing := "\n" + strings.Repeat("  ", max(depth-1, 0))
	switch {
	case isWhitespace(n.LastChild):
		n.LastChild.Data = closing
	case mayCreate:
		n.AppendChild(&html.Node{Type: html.TextNode, Data: closing})
	}
}

var landmarks = map[atom.Atom]bool{
	atom.Header: true, atom.Nav: true, atom.Main: true, atom.Section: true, atom.Article: true,
	atom.Aside: true, atom.Footer: true, atom.Form: true, atom.H1: true, atom.H2: true,
}

var headings = map[atom.Atom]bool{atom.H1: true, atom.H2: true, atom.H3: true, atom.H4: true}

const (
	outlineDepth   = 4
	tagMaxLen      = 120
	headingMaxLen  = 80
	outlineMaxSize = 60
)

func startTag(n *html.Node) string {
	var b strings.Builder
	b.WriteString("<" + n.Data)
	for _, a := range n.Attr {
		b.WriteString(" " + a.Key + `="` + html.EscapeString(a.Val) + `"`)
	}
	b.WriteString(">")
	return b.String()
}

// outline lists the landmarks of the rendered page in document order. A
// tag is located by searching the rendering from the previous one on, so
// the line is right even where several landmarks share a line.
func outline(doc *html.Node, rendered []byte) []artifact.Landmark {
	body := find(doc, atom.Body)
	if body == nil {
		return nil
	}
	var out []artifact.Landmark
	offset := 0
	var visit func(n *html.Node, depth int)
	visit = func(n *html.Node, depth int) {
		for c := n.FirstChild; c != nil && len(out) < outlineMaxSize; c = c.NextSibling {
			if c.Type != html.ElementNode {
				continue
			}
			if landmarks[c.DataAtom] {
				tag := startTag(c)
				if i := bytes.Index(rendered[offset:], []byte(tag)); i >= 0 {
					offset += i
					out = append(out, artifact.Landmark{
						Line:    1 + bytes.Count(rendered[:offset], []byte("\n")),
						Tag:     clip(tag, tagMaxLen),
						Heading: clip(strings.Join(strings.Fields(heading(c)), " "), headingMaxLen),
					})
				}
			}
			if depth < outlineDepth {
				visit(c, depth+1)
			}
		}
	}
	visit(body, 1)
	return out
}

func heading(n *html.Node) string {
	if headings[n.DataAtom] {
		return text(n)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.ElementNode {
			continue
		}
		if headings[c.DataAtom] {
			return text(c)
		}
		if h := heading(c); h != "" {
			return h
		}
	}
	return ""
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + "…"
}
