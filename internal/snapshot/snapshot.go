// Package snapshot copies one web page into the files of an artifact: the
// HTML with its subresources localised under assets/, links made absolute
// to the source, scripts removed unless asked for, and an outline of the
// result for the agent that edits it next.
package snapshot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/url"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
	"golang.org/x/net/html/charset"

	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/csp"
	"github.com/PeterBooker/kazari/internal/fetch"
)

const (
	assetDir = "assets/"
	cssDepth = 3
)

// Options bounds a snapshot. The limits are the artifact service's, so a
// snapshot that fits here publishes.
type Options struct {
	// CSP is the instance policy; a resource stays remote only when the
	// policy names its host.
	CSP           string
	MaxFileBytes  int64
	MaxTotalBytes int64
	MaxFiles      int
}

// Taker takes snapshots with one fetch client.
type Taker struct {
	client *fetch.Client
	opts   Options
	policy map[string][]string
}

// New returns a Taker; zero limits take the artifact service's defaults.
func New(client *fetch.Client, opts Options) *Taker {
	if opts.MaxFileBytes <= 0 {
		opts.MaxFileBytes = artifact.DefaultMaxFileBytes
	}
	if opts.MaxTotalBytes <= 0 {
		opts.MaxTotalBytes = artifact.DefaultMaxVersionBytes
	}
	if opts.MaxFiles <= 0 {
		opts.MaxFiles = artifact.DefaultMaxFiles
	}
	return &Taker{client: client, opts: opts, policy: csp.Parse(opts.CSP)}
}

var _ artifact.PageSource = (*Taker)(nil)

// Take fetches the page at rawURL and its subresources within the client's
// budget; what does not fit is left remote and reported.
func (t *Taker) Take(ctx context.Context, rawURL string, keepScripts bool) (*artifact.Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, t.client.Budget())
	defer cancel()
	data, res, err := t.client.Get(ctx, rawURL, t.opts.MaxFileBytes)
	if err != nil {
		return nil, err
	}
	mt, _, _ := mime.ParseMediaType(res.ContentType)
	if mt != "text/html" && mt != "application/xhtml+xml" {
		return nil, artifact.Errorf(artifact.ErrFetchFailed, "%s is %s, not an HTML page", rawURL, firstNonEmpty(mt, "of unknown type"))
	}
	r, err := charset.NewReader(bytes.NewReader(data), res.ContentType)
	if err != nil {
		return nil, artifact.Errorf(artifact.ErrFetchFailed, "%s: %v", rawURL, err)
	}
	doc, err := html.Parse(r)
	if err != nil {
		return nil, artifact.Errorf(artifact.ErrFetchFailed, "%s: %v", rawURL, err)
	}

	w := &walker{ctx: ctx, t: t, base: res.URL, keepScripts: keepScripts, local: map[string]string{}}
	w.report.Source = res.URL.String()
	if b := find(doc, atom.Base); b != nil {
		if href := attr(b, "href"); href != "" {
			if bu, err := w.base.Parse(href); err == nil {
				w.base = bu
			}
		}
	}
	w.walk(doc)
	declareUTF8(doc)
	format(doc)
	var buf bytes.Buffer
	if err := html.Render(&buf, doc); err != nil {
		w.close()
		return nil, fmt.Errorf("snapshot: render: %w", err)
	}
	page := buf.Bytes()
	w.report.Outline = outline(doc, page)
	if w.report.ScriptsRemoved >= 2 && len(strings.Fields(text(find(doc, atom.Body)))) < 40 {
		w.report.Notes = append(w.report.Notes, "the page holds little text and relied on scripts that were removed; "+
			"the site probably renders with JavaScript, so this copy may be a shell")
	}
	files := append([]artifact.FileInput{{Path: "index.html", Content: bytes.NewReader(page), ContentType: "text/html; charset=utf-8"}}, w.files...)
	return &artifact.Snapshot{
		Title:       text(find(doc, atom.Title)),
		Description: w.description,
		Files:       files,
		Report:      w.report,
		Close:       w.close,
	}, nil
}

// walker rewrites one document, fetching as it goes.
type walker struct {
	ctx         context.Context
	t           *Taker
	base        *url.URL
	keepScripts bool
	description string

	local   map[string]string // absolute URL -> file name under assets/
	files   []artifact.FileInput
	closers []io.Closer
	total   int64
	report  artifact.ImportReport
}

func (w *walker) close() error {
	var errs []error
	for _, c := range w.closers {
		errs = append(errs, c.Close())
	}
	w.closers = nil
	return errors.Join(errs...)
}

// directives map a resource kind to the CSP directive that governs it.
var directives = map[string]string{
	"style": "style-src", "font": "font-src", "script": "script-src", "image": "img-src", "media": "media-src",
}

// localize fetches ref, resolved against base, into assets/ and returns
// its file name; when the resource stays remote it returns the absolute
// URL and false.
func (w *walker) localize(base *url.URL, ref, kind string, depth int) (string, bool) {
	u := resolve(base, ref)
	if u == nil {
		return ref, false
	}
	u.Fragment = ""
	abs := u.String()
	if name, ok := w.local[abs]; ok {
		return name, true
	}
	kind = kindOf(u, kind)
	if src, ok := csp.Sources(w.t.policy, directives[kind]); ok && csp.HostAllowed(src, u.Scheme+"://"+u.Host) {
		return abs, false
	}
	skip := func(reason string) (string, bool) {
		w.report.Skipped = append(w.report.Skipped, artifact.SkippedFetch{URL: abs, Reason: reason})
		return abs, false
	}
	switch {
	case w.ctx.Err() != nil:
		return skip("import budget exhausted")
	case w.report.Fetched+1 >= w.t.opts.MaxFiles:
		return skip("file limit reached")
	}
	limit := min(w.t.opts.MaxFileBytes, w.t.opts.MaxTotalBytes-w.total)
	if limit <= 0 {
		return skip("version size limit reached")
	}
	var (
		in   artifact.FileInput
		size int64
		res  fetch.Result
	)
	if kind == "style" {
		data, r, err := w.t.client.Get(w.ctx, abs, limit)
		if err != nil {
			return skip(reason(err, abs))
		}
		if depth < cssDepth {
			data = w.rewriteCSS(data, u, depth+1, "")
		}
		in, size, res = artifact.FileInput{Content: bytes.NewReader(data)}, int64(len(data)), r
	} else {
		f, err := w.t.client.Download(w.ctx, abs, limit)
		if err != nil {
			return skip(reason(err, abs))
		}
		w.closers = append(w.closers, f)
		in, size, res = artifact.FileInput{Content: f}, f.Size, f.Result
	}
	name := localName(u, res.ContentType)
	in.Path = assetDir + name
	if artifact.ValidateContentType(res.ContentType) == nil {
		in.ContentType = res.ContentType
	}
	w.local[abs] = name
	w.files = append(w.files, in)
	w.total += size
	w.report.Fetched++
	return name, true
}

// reason strips the URL the fetch error starts with.
func reason(err error, u string) string {
	msg := err.Error()
	var ae *artifact.Error
	if errors.As(err, &ae) {
		msg = ae.Message
	}
	return strings.TrimLeft(strings.TrimPrefix(msg, u), ": ")
}

func resolve(base *url.URL, ref string) *url.URL {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil
	}
	lower := strings.ToLower(ref)
	for _, p := range []string{"#", "data:", "blob:", "javascript:", "mailto:", "tel:", "about:"} {
		if strings.HasPrefix(lower, p) {
			return nil
		}
	}
	u, err := base.Parse(ref)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil
	}
	return u
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ---- DOM helpers ----

func find(n *html.Node, a atom.Atom) *html.Node {
	if n == nil {
		return nil
	}
	if n.Type == html.ElementNode && n.DataAtom == a {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if r := find(c, a); r != nil {
			return r
		}
	}
	return nil
}

func text(n *html.Node) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	var visit func(*html.Node)
	visit = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			visit(c)
		}
	}
	visit(n)
	return b.String()
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func setAttr(n *html.Node, key, val string) {
	for i := range n.Attr {
		if n.Attr[i].Key == key {
			n.Attr[i].Val = val
			return
		}
	}
	n.Attr = append(n.Attr, html.Attribute{Key: key, Val: val})
}

func delAttr(n *html.Node, key string) {
	kept := n.Attr[:0]
	for _, a := range n.Attr {
		if a.Key != key {
			kept = append(kept, a)
		}
	}
	n.Attr = kept
}
