package artifact

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"golang.org/x/net/html"

	"github.com/PeterBooker/kazari/internal/csp"
)

// The checks after a publish read at most checkFiles pages and stylesheets
// of up to checkFileBytes each, and report at most maxWarnings problems.
const (
	checkFiles     = 100
	checkFileBytes = 2 << 20
	maxWarnings    = 10
)

// What the checks parse out of a file depends only on its content and its
// path, so it is cached by both: a publish that carries files over
// unchanged does not read them again.
const (
	DefaultCheckCacheEntries = 4096
	DefaultCheckCacheBytes   = 8 << 20
)

// warnings lists what will stop a version from rendering as published: no
// root index.html, and in its pages and stylesheets, files that are not in
// the version and origins the page's Content-Security-Policy blocks. They
// are advice: the version is published either way.
func (s *Service) warnings(ctx context.Context, a *Artifact, files []File) []string {
	have := make(map[string]bool, len(files))
	for _, f := range files {
		have[f.Path] = true
	}
	var out []string
	if !have["index.html"] {
		out = append(out, missingIndex(files, have))
	}
	policy := s.opts.CSP
	if s.opts.AllowCSPOverride && a.Metadata[MetaCSP] != "" {
		policy = csp.Merge(policy, a.Metadata[MetaCSP])
	}
	c := checker{have: have, policy: csp.Parse(policy), checkCSP: policy != "", override: s.opts.AllowCSPOverride, seen: map[string]bool{}}
	read := 0
	for _, f := range files {
		kind := checkKind(f.Path)
		if kind == "" || f.Size > checkFileBytes || read >= checkFiles {
			continue
		}
		read++
		key := refKey{hash: f.Hash, path: f.Path}
		refs, ok := s.refs.Get(key)
		if !ok {
			data, err := s.readBlob(ctx, f.Hash)
			if err != nil {
				s.opts.Logger.WarnContext(ctx, "reading a file to check it", "slug", a.Slug, "path", f.Path, "err", err)
				continue
			}
			refs = parseRefs(kind, f.Path, data)
			s.refs.Add(key, refs, entrySize(key, refs))
		}
		c.check(f.Path, refs)
	}
	out = append(out, c.out...)
	if len(out) > maxWarnings {
		out = append(out[:maxWarnings-1], fmt.Sprintf("and %d more problems of these kinds", len(out)-maxWarnings+1))
	}
	return out
}

func missingIndex(files []File, have map[string]bool) string {
	if dir, ok := commonDir(files); ok && have[dir+"/index.html"] {
		return fmt.Sprintf("every path starts with %s/, so the page's address answers 404: publish the contents of %s, not the directory itself", dir, dir)
	}
	return "there is no index.html at the root, so the page's address answers 404: name the main page index.html"
}

// commonDir is the one top-level directory every path is in.
func commonDir(files []File) (string, bool) {
	dir := ""
	for _, f := range files {
		top, _, nested := strings.Cut(f.Path, "/")
		if !nested || (dir != "" && top != dir) {
			return "", false
		}
		dir = top
	}
	return dir, dir != ""
}

func (s *Service) readBlob(ctx context.Context, hash string) ([]byte, error) {
	rc, err := s.blobs.Open(ctx, hash)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, checkFileBytes))
}

func checkKind(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".html", ".htm":
		return "html"
	case ".css":
		return "css"
	}
	return ""
}

// reference is what a page or stylesheet loads: a path in the version, or
// a remote URL with its origin and the directive that governs it.
type reference struct {
	remote    bool
	target    string
	origin    string
	directive string
}

// parseRefs lists, in order and once each, what the file at file loads.
func parseRefs(kind, file string, data []byte) []reference {
	p := refParser{from: file, seen: map[reference]bool{}}
	if kind == "html" {
		p.html(data)
	} else {
		p.css(data)
	}
	return p.refs
}

type refParser struct {
	from string
	seen map[reference]bool
	refs []reference
}

func (p *refParser) add(r reference) {
	if p.seen[r] {
		return
	}
	p.seen[r] = true
	p.refs = append(p.refs, r)
}

func (p *refParser) html(data []byte) {
	z := html.NewTokenizer(bytes.NewReader(data))
	inStyle := false
	for {
		switch z.Next() {
		case html.ErrorToken:
			return
		case html.TextToken:
			if inStyle {
				p.css(z.Text())
			}
		case html.EndTagToken:
			if name, _ := z.TagName(); string(name) == "style" {
				inStyle = false
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			attrs := map[string]string{}
			for hasAttr {
				var k, v []byte
				k, v, hasAttr = z.TagAttr()
				attrs[string(k)] = string(v)
			}
			tag := string(name)
			if tag == "style" {
				inStyle = true
				continue
			}
			ref, directive := resource(tag, attrs)
			if ref != "" {
				p.ref(ref, directive)
			}
		}
	}
}

// resource is the URL a tag loads and the directive that governs it.
func resource(tag string, attrs map[string]string) (string, string) {
	switch tag {
	case "script":
		return attrs["src"], "script-src"
	case "img":
		return attrs["src"], "img-src"
	case "iframe":
		return attrs["src"], "frame-src"
	case "video", "audio", "source", "track":
		return attrs["src"], "media-src"
	case "link":
		rel := strings.Fields(strings.ToLower(attrs["rel"]))
		switch {
		case slices.Contains(rel, "stylesheet"):
			return attrs["href"], "style-src"
		case slices.Contains(rel, "icon"):
			return attrs["href"], "img-src"
		case slices.Contains(rel, "manifest"):
			return attrs["href"], "manifest-src"
		}
	}
	return "", ""
}

var (
	cssImport = regexp.MustCompile(`@import\s+(?:url\(\s*)?["']?([^"')\s;]+)`)
	cssURL    = regexp.MustCompile(`url\(\s*["']?([^"')]+?)["']?\s*\)`)
)

func (p *refParser) css(data []byte) {
	for _, m := range cssImport.FindAllSubmatch(data, -1) {
		p.ref(string(m[1]), "style-src")
	}
	for _, m := range cssURL.FindAllSubmatch(data, -1) {
		ref := string(m[1])
		directive := "img-src"
		switch strings.ToLower(path.Ext(strings.SplitN(ref, "?", 2)[0])) {
		case ".woff", ".woff2", ".ttf", ".otf", ".eot":
			directive = "font-src"
		case ".css":
			continue // an @import, checked above
		}
		p.ref(ref, directive)
	}
}

func (p *refParser) ref(ref, directive string) {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.HasPrefix(ref, "#") {
		return
	}
	u, err := url.Parse(ref)
	if err != nil {
		return
	}
	if strings.HasPrefix(ref, "//") {
		u.Scheme = "https"
	}
	switch {
	case u.Scheme == "http" || u.Scheme == "https":
		p.add(reference{remote: true, target: u.String(), origin: u.Scheme + "://" + u.Host, directive: directive})
	case u.Scheme == "" && u.Host == "" && u.Path != "":
		p.add(reference{target: p.local(u.Path)})
	}
}

// local is the path in the version that ref names, resolved against the
// file it is in.
func (p *refParser) local(ref string) string {
	target := ref
	if !strings.HasPrefix(target, "/") {
		target = path.Join(path.Dir("/"+p.from), target)
	}
	target = strings.TrimPrefix(path.Clean(target), "/")
	if strings.HasSuffix(ref, "/") || target == "" {
		target = strings.TrimPrefix(target+"/index.html", "/")
	}
	return target
}

type checker struct {
	have     map[string]bool
	policy   map[string][]string
	checkCSP bool
	override bool
	seen     map[string]bool
	out      []string
}

func (c *checker) warn(key, msg string) {
	if c.seen[key] {
		return
	}
	c.seen[key] = true
	c.out = append(c.out, msg)
}

// check holds what file loads against the version's files and the policy.
func (c *checker) check(file string, refs []reference) {
	for _, r := range refs {
		if r.remote {
			c.remote(file, r)
		} else if !c.have[r.target] {
			c.warn("missing "+r.target, fmt.Sprintf("%s refers to %s, which is not in this version", file, r.target))
		}
	}
}

func (c *checker) remote(file string, r reference) {
	if !c.checkCSP {
		return
	}
	sources, ok := csp.Sources(c.policy, r.directive)
	if !ok || csp.Allows(sources, r.target) {
		return
	}
	fix := "ship the file in the artifact instead"
	if c.override {
		fix += fmt.Sprintf(", or add %s to %s with kazari.csp metadata", r.origin, r.directive)
	}
	c.warn(r.directive+" "+r.origin, fmt.Sprintf("%s loads %s, which the page's Content-Security-Policy (%s) blocks: %s",
		file, r.target, r.directive, fix))
}

type refKey struct{ hash, path string }

// entrySize estimates the memory an entry holds: its strings and the
// headers around them. Directives are constants and cost nothing.
func entrySize(k refKey, refs []reference) int64 {
	const entryOverhead, refOverhead = 160, 64
	n := int64(entryOverhead + len(k.hash) + len(k.path))
	for _, r := range refs {
		n += refOverhead + int64(len(r.target)+len(r.origin))
	}
	return n
}

// pageTitle is the <title> of the root index.html, as a name for an
// artifact created without one; "" when there is none.
func (s *Service) pageTitle(ctx context.Context, files []File) string {
	i := slices.IndexFunc(files, func(f File) bool { return f.Path == "index.html" })
	if i < 0 || files[i].Size > checkFileBytes {
		return ""
	}
	data, err := s.readBlob(ctx, files[i].Hash)
	if err != nil {
		return ""
	}
	return PageTitle(data)
}

// PageTitle is the text of the first <title> in an HTML document, with its
// whitespace collapsed and cut to a valid name.
func PageTitle(data []byte) string {
	z := html.NewTokenizer(bytes.NewReader(data))
	for {
		switch z.Next() {
		case html.ErrorToken:
			return ""
		case html.StartTagToken:
			if name, _ := z.TagName(); string(name) != "title" {
				continue
			}
			if z.Next() != html.TextToken {
				return ""
			}
			title := strings.Map(func(r rune) rune {
				if unicode.IsControl(r) {
					return ' '
				}
				return r
			}, string(z.Text()))
			title = strings.Join(strings.Fields(title), " ")
			title = strings.TrimRight(truncate(title, NameMaxBytes), " ")
			if ValidateName(title) != nil {
				return ""
			}
			return title
		}
	}
}
