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
		data, err := s.readBlob(ctx, f.Hash)
		if err != nil {
			s.opts.Logger.WarnContext(ctx, "reading a file to check it", "slug", a.Slug, "path", f.Path, "err", err)
			continue
		}
		if kind == "html" {
			c.html(f.Path, data)
		} else {
			c.css(f.Path, f.Path, data)
		}
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

func (c *checker) html(file string, data []byte) {
	z := html.NewTokenizer(bytes.NewReader(data))
	inStyle := false
	for {
		switch z.Next() {
		case html.ErrorToken:
			return
		case html.TextToken:
			if inStyle {
				c.css(file, file, z.Text())
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
				c.ref(file, file, ref, directive)
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

// css checks a stylesheet; from is the file references resolve against,
// file the one named in a warning.
func (c *checker) css(file, from string, data []byte) {
	for _, m := range cssImport.FindAllSubmatch(data, -1) {
		c.ref(file, from, string(m[1]), "style-src")
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
		c.ref(file, from, ref, directive)
	}
}

func (c *checker) ref(file, from, ref, directive string) {
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
		c.remote(file, u, directive)
	case u.Scheme == "" && u.Host == "":
		c.local(file, from, u)
	}
}

func (c *checker) remote(file string, u *url.URL, directive string) {
	if !c.checkCSP {
		return
	}
	sources, ok := csp.Sources(c.policy, directive)
	if !ok || csp.Allows(sources, u.String()) {
		return
	}
	origin := u.Scheme + "://" + u.Host
	fix := "ship the file in the artifact instead"
	if c.override {
		fix += fmt.Sprintf(", or add %s to %s with kazari.csp metadata", origin, directive)
	}
	c.warn(directive+" "+origin, fmt.Sprintf("%s loads %s, which the page's Content-Security-Policy (%s) blocks: %s",
		file, u.String(), directive, fix))
}

func (c *checker) local(file, from string, u *url.URL) {
	p := u.Path
	if p == "" {
		return
	}
	if !strings.HasPrefix(p, "/") {
		p = path.Join(path.Dir("/"+from), p)
	}
	p = strings.TrimPrefix(path.Clean(p), "/")
	if strings.HasSuffix(u.Path, "/") || p == "" {
		p = strings.TrimPrefix(p+"/index.html", "/")
	}
	if c.have[p] {
		return
	}
	c.warn("missing "+p, fmt.Sprintf("%s refers to %s, which is not in this version", file, p))
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
			title = truncate(title, NameMaxBytes)
			if ValidateName(title) != nil {
				return ""
			}
			return title
		}
	}
}
