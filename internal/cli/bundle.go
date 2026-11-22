package cli

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/PeterBooker/kazari/internal/user"
)

// ignoreFile, at the root of a published directory, lists what to leave out.
const ignoreFile = ".kazariignore"

// bundleFile is one file a publish sends; Path is relative and
// slash-separated.
type bundleFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	abs  string
}

// skippedFile is a file a publish leaves out, with the reason.
type skippedFile struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// bundlePlan is what a publish of a directory or a file sends, sorted by
// path. Removed is filled in by a mirror: the files the new version drops.
type bundlePlan struct {
	Files   []bundleFile  `json:"files"`
	Skipped []skippedFile `json:"skipped,omitempty"`
	Removed []string      `json:"removed,omitempty"`
	Bytes   int64         `json:"bytes"`
}

func (p *bundlePlan) paths() []string {
	out := make([]string, len(p.Files))
	for i, f := range p.Files {
		out[i] = f.Path
	}
	return out
}

// summary is "14 files, 312.0 KiB; skipped: .env (dotfile); removed: a.txt".
func (p *bundlePlan) summary() string {
	noun := "files"
	if len(p.Files) == 1 {
		noun = "file"
	}
	s := fmt.Sprintf("%d %s, %s", len(p.Files), noun, humanSize(p.Bytes))
	if len(p.Skipped) > 0 {
		items := make([]string, len(p.Skipped))
		for i, sk := range p.Skipped {
			items[i] = sk.Path + " (" + sk.Reason + ")"
		}
		s += "; skipped: " + shortList(items)
	}
	if len(p.Removed) > 0 {
		s += "; removed: " + shortList(p.Removed)
	}
	return s
}

func shortList(items []string) string {
	const most = 10
	if len(items) <= most {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:most], ", ") + fmt.Sprintf(" and %d more", len(items)-most)
}

// planBundle lists what a publish of src sends. A directory sends its
// regular files, less dotfiles (but .well-known), symlinks, files that look
// like secrets and what its .kazariignore excludes; a "!pattern" there sends
// a dotfile or a secret-looking file after all. A file is sent alone, as
// index.html when it is HTML. A symlink to either is followed.
func planBundle(src string) (*bundlePlan, error) {
	root, err := filepath.EvalSymlinks(src)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, usagef("no such directory: %s", src)
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", src, err)
	}
	st, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", src, err)
	}
	if st.Mode().IsRegular() {
		return planFile(root, st.Size())
	}
	if !st.IsDir() {
		return nil, usagef("%s is neither a directory nor a file", src)
	}
	rules, err := readIgnore(filepath.Join(root, ignoreFile))
	if err != nil {
		return nil, err
	}
	p := &bundlePlan{}
	skip := func(rel, reason string) { p.Skipped = append(p.Skipped, skippedFile{Path: rel, Reason: reason}) }
	err = filepath.WalkDir(root, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if abs == root {
			return nil
		}
		rel, err := filepath.Rel(root, abs)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		matched, include := rules.match(rel, d.IsDir())
		if matched && !include {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			skip(rel, "symlink")
			return nil
		}
		reason := ""
		switch {
		case hidden(d.Name()):
			reason = "dotfile"
		case !d.IsDir() && secretName(d.Name()):
			reason = "looks like a secret"
		}
		if reason != "" && !include {
			if rel != ignoreFile {
				skip(rel, reason)
			}
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			skip(rel, "not a regular file")
			return nil
		}
		if !include {
			found, err := holdsKey(abs)
			if err != nil {
				return err
			}
			if found {
				skip(rel, "looks like a secret")
				return nil
			}
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		p.Files = append(p.Files, bundleFile{Path: rel, Size: info.Size(), abs: abs})
		p.Bytes += info.Size()
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", src, err)
	}
	slices.SortFunc(p.Files, func(a, b bundleFile) int { return strings.Compare(a.Path, b.Path) })
	return p, nil
}

func planFile(abs string, size int64) (*bundlePlan, error) {
	name := filepath.Base(abs)
	switch strings.ToLower(filepath.Ext(name)) {
	case ".html", ".htm":
		name = "index.html"
	}
	p := &bundlePlan{}
	found, err := holdsKey(abs)
	if err != nil {
		return nil, err
	}
	if secretName(filepath.Base(abs)) || found {
		p.Skipped = []skippedFile{{Path: filepath.Base(abs), Reason: "looks like a secret"}}
		return p, nil
	}
	p.Files = []bundleFile{{Path: name, Size: size, abs: abs}}
	p.Bytes = size
	return p, nil
}

func hidden(name string) bool {
	return strings.HasPrefix(name, ".") && name != ".well-known"
}

var secretPatterns = []string{"*.pem", "*.key", "*.p12", "*.pfx", "*.kdbx", "id_rsa*", "id_ed25519*", "id_ecdsa*", "credentials*"}

func secretName(name string) bool {
	name = strings.ToLower(name)
	for _, p := range secretPatterns {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

var keyPattern = regexp.MustCompile(fmt.Sprintf(`%s[a-z2-7]{%d}_[a-z2-7]{%d}`, user.KeyPrefix, user.PrefixLen, user.SecretLen))

// holdsKey reads the file in blocks that overlap by a key's length, so a
// key across two blocks is found.
func holdsKey(abs string) (bool, error) {
	f, err := os.Open(abs)
	if err != nil {
		return false, err
	}
	defer f.Close()
	const block = 64 << 10
	buf := make([]byte, 0, block+user.KeyLen)
	r := bufio.NewReaderSize(f, block)
	chunk := make([]byte, block)
	for {
		n, err := io.ReadFull(r, chunk)
		buf = append(buf, chunk[:n]...)
		if keyPattern.Match(buf) {
			return true, nil
		}
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return false, nil
			}
			return false, err
		}
		if keep := user.KeyLen - 1; len(buf) > keep {
			buf = append(buf[:0], buf[len(buf)-keep:]...)
		}
	}
}

// ignoreRule is one line of .kazariignore, in the subset of gitignore's
// syntax it takes: globs, ** for any directories, a leading / or an inner /
// to anchor at the root, a trailing / for directories only, ! to send what
// an earlier line left out, and # comments.
type ignoreRule struct {
	parts    []string
	negate   bool
	dirOnly  bool
	anchored bool
}

type ignoreRules []ignoreRule

func readIgnore(name string) (ignoreRules, error) {
	b, err := os.ReadFile(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", ignoreFile, err)
	}
	return parseIgnore(string(b)), nil
}

func parseIgnore(text string) ignoreRules {
	var rules ignoreRules
	for line := range strings.Lines(text) {
		line = strings.TrimRight(line, "\r\n")
		line = strings.TrimRight(line, " \t")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var r ignoreRule
		if rest, ok := strings.CutPrefix(line, "!"); ok {
			r.negate, line = true, rest
		}
		line = strings.TrimPrefix(line, `\`)
		if rest, ok := strings.CutSuffix(line, "/"); ok {
			r.dirOnly, line = true, rest
		}
		if rest, ok := strings.CutPrefix(line, "/"); ok {
			r.anchored, line = true, rest
		}
		if strings.Contains(line, "/") {
			r.anchored = true
		}
		if line == "" {
			continue
		}
		r.parts = strings.Split(line, "/")
		rules = append(rules, r)
	}
	return rules
}

// match reports whether a rule names rel and, if so, whether the last one
// that does sends it (a ! rule) or leaves it out.
func (rules ignoreRules) match(rel string, dir bool) (matched, include bool) {
	segs := strings.Split(rel, "/")
	for _, r := range rules {
		if r.dirOnly && !dir {
			continue
		}
		var ok bool
		if r.anchored {
			ok = matchSegments(r.parts, segs)
		} else {
			ok = len(r.parts) == 1 && matchSegments(r.parts, segs[len(segs)-1:])
		}
		if ok {
			matched, include = true, r.negate
		}
	}
	return matched, include
}

func matchSegments(pat, segs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			pat = pat[1:]
			if len(pat) == 0 {
				return true
			}
			for i := range len(segs) + 1 {
				if matchSegments(pat, segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		if ok, _ := path.Match(pat[0], segs[0]); !ok {
			return false
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0
}

// writeTarGz writes the plan's files to w as a gzip-compressed tar. Entries
// are sorted, with zero ownership, mode 0644 and epoch mtime, so the same
// tree always yields the same bytes.
func (p *bundlePlan) writeTarGz(w io.Writer) error {
	zw := gzip.NewWriter(w)
	tw := tar.NewWriter(zw)
	for _, f := range p.Files {
		if err := addFile(tw, f); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("close tar: %w", err)
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("close gzip: %w", err)
	}
	return nil
}

func addFile(tw *tar.Writer, bf bundleFile) error {
	f, err := os.Open(bf.abs)
	if err != nil {
		return fmt.Errorf("open %s: %w", bf.Path, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", bf.Path, err)
	}
	hdr := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     bf.Path,
		Size:     st.Size(),
		Mode:     0o644,
		ModTime:  time.Unix(0, 0),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("tar header %s: %w", bf.Path, err)
	}
	n, err := io.Copy(tw, f)
	if err != nil {
		return fmt.Errorf("tar %s: %w", bf.Path, err)
	}
	if n != st.Size() {
		return fmt.Errorf("tar %s: %w", bf.Path, errors.New("file changed while reading"))
	}
	return nil
}

// reader streams writeTarGz without holding it in memory. Closing the
// reader early stops the writer; read errors surface from Read.
func (p *bundlePlan) reader() io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(p.writeTarGz(pw))
	}()
	return pr
}

// TarGz writes what a publish of src sends to w as a gzip-compressed tar;
// see planBundle for what that is.
func TarGz(src string, w io.Writer) error {
	p, err := planBundle(src)
	if err != nil {
		return err
	}
	return p.writeTarGz(w)
}

// TarGzReader streams TarGz(src) without holding it in memory. Closing the
// reader early stops the writer; walk and read errors surface from Read.
func TarGzReader(src string) io.ReadCloser {
	p, err := planBundle(src)
	if err != nil {
		pr, pw := io.Pipe()
		pw.CloseWithError(err)
		return pr
	}
	return p.reader()
}
