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

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/csp"
	"github.com/PeterBooker/kazari/internal/fetch"
)

const (
	assetDir = "assets/"
	cssDepth = 3
	// maxTextBytes caps what is held in memory to parse or rewrite: the
	// page and each stylesheet. Other assets stream to disk.
	maxTextBytes = 4 << 20
	// maxConcurrent bounds the imports in flight, and with them the
	// documents in memory at once.
	maxConcurrent = 4
	// maxSkipped bounds the report, since a page can reference any number
	// of resources that fail.
	maxSkipped = 100
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
	slots  chan struct{}
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
	return &Taker{client: client, opts: opts, policy: csp.Parse(opts.CSP), slots: make(chan struct{}, maxConcurrent)}
}

var _ artifact.PageSource = (*Taker)(nil)

// Take fetches the page at rawURL and its subresources within the client's
// budget; what does not fit is left remote and reported.
func (t *Taker) Take(ctx context.Context, rawURL string, keepScripts bool) (*artifact.Snapshot, error) {
	select {
	case t.slots <- struct{}{}:
		defer func() { <-t.slots }()
	default:
		return nil, &artifact.Error{Code: api.CodeRateLimited, Message: fmt.Sprintf("%d imports are already running; retry in a few seconds", maxConcurrent)}
	}
	ctx, cancel := context.WithTimeout(fetch.WithPurpose(ctx, fetch.PurposeImport), t.client.Budget())
	defer cancel()
	data, res, err := t.client.Get(ctx, rawURL, min(t.opts.MaxFileBytes, maxTextBytes))
	if err != nil {
		return nil, err
	}
	mt, _, _ := mime.ParseMediaType(res.ContentType)
	if mt != "text/html" && mt != "application/xhtml+xml" {
		if mt == "" {
			mt = "of unknown type"
		}
		return nil, artifact.Errorf(artifact.ErrFetchFailed, "%s is %s, not an HTML page", rawURL, mt)
	}
	r, err := charset.NewReader(bytes.NewReader(data), res.ContentType)
	if err != nil {
		return nil, artifact.Errorf(artifact.ErrFetchFailed, "%s: %v", rawURL, err)
	}
	doc, err := html.Parse(r)
	if err != nil {
		return nil, artifact.Errorf(artifact.ErrFetchFailed, "%s: %v", rawURL, err)
	}

	// The rendered page grows a little with indentation and absolute links,
	// so the budget for assets leaves it room.
	w := &walker{ctx: ctx, t: t, base: res.URL, keepScripts: keepScripts,
		local: map[string]string{}, failed: map[string]string{}, total: int64(len(data)) * 5 / 4}
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
	if w.more > 0 {
		w.report.Notes = append(w.report.Notes, fmt.Sprintf("%d more resources were left remote", w.more))
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
	failed  map[string]string // absolute URL -> why it stayed remote
	more    int               // skips beyond maxSkipped
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

var directives = map[string]string{
	"style": "style-src", "font": "font-src", "script": "script-src", "image": "img-src", "media": "media-src",
}

// ref resolves one reference for an attribute or a stylesheet; prefix is
// where the reference lives relative to assets/.
func (w *walker) ref(base *url.URL, ref, kind string, depth int, prefix string) string {
	name, local := w.localize(base, ref, kind, depth)
	if local {
		return prefix + name
	}
	return name
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
	if _, ok := w.failed[abs]; ok {
		return abs, false
	}
	kind = kindOf(u, kind)
	if src, ok := csp.Sources(w.t.policy, directives[kind]); ok && csp.HostAllowed(src, u.Scheme+"://"+u.Host) {
		return abs, false
	}
	switch {
	case w.ctx.Err() != nil:
		return w.skip(abs, "import budget exhausted")
	case w.report.Fetched+1 >= w.t.opts.MaxFiles:
		return w.skip(abs, "file limit reached")
	}
	limit := min(w.t.opts.MaxFileBytes, w.t.opts.MaxTotalBytes-w.total)
	if kind == "style" {
		limit = min(limit, maxTextBytes)
	}
	if limit <= 0 {
		return w.skip(abs, "version size limit reached")
	}
	f, err := w.t.client.Download(w.ctx, abs, limit)
	if err != nil {
		return w.skip(abs, reason(err, abs))
	}
	w.closers = append(w.closers, f)
	w.total += f.Size
	name := localName(u, f.ContentType)
	// Named before its stylesheet is rewritten, so an @import cycle
	// resolves to this file instead of fetching it again.
	w.local[abs] = name
	if kind == "style" && depth < cssDepth {
		before := f.Size
		if err := w.rewriteStylesheet(f, depth+1); err != nil {
			delete(w.local, abs)
			return w.skip(abs, "could not rewrite the stylesheet")
		}
		w.total += f.Size - before
	}
	in := artifact.FileInput{Path: assetDir + name, Content: f}
	if artifact.ValidateContentType(f.ContentType) == nil {
		in.ContentType = f.ContentType
	}
	w.files = append(w.files, in)
	w.report.Fetched++
	return name, true
}

func (w *walker) skip(abs, reason string) (string, bool) {
	w.failed[abs] = reason
	if len(w.report.Skipped) < maxSkipped {
		w.report.Skipped = append(w.report.Skipped, artifact.SkippedFetch{URL: abs, Reason: reason})
	} else {
		w.more++
	}
	return abs, false
}

func (w *walker) rewriteStylesheet(f *fetch.File, depth int) error {
	css, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	return f.Replace(w.rewriteCSS(css, f.URL, depth, ""))
}

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
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil
	}
	return u
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
