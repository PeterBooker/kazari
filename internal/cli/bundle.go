package cli

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/user"
)

// ignoreFile, at the root of a published directory, lists what to leave out.
const ignoreFile = ".kazariignore"

// bundleFile is one file a publish sends; Path is relative and
// slash-separated.
type bundleFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	// name is the file's name in the plan's root, and sum the hex sha256 of
	// the bytes the plan read.
	name string
	sum  string
}

// skippedFile is a file a publish leaves out, with the reason.
type skippedFile struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// bundlePlan is what a publish of a directory or a file sends, sorted by
// path. A deploy onto an existing artifact fills in Unchanged, the files the
// base version already holds as they are, which it does not send, and
// Removed, the files the new version drops.
type bundlePlan struct {
	Files     []bundleFile  `json:"files"`
	Unchanged []bundleFile  `json:"unchanged,omitempty"`
	Skipped   []skippedFile `json:"skipped,omitempty"`
	Removed   []string      `json:"removed,omitempty"`
	Bytes     int64         `json:"bytes"`
	// root is where the files were read, and are read again to be sent.
	root *os.Root
}

func (p *bundlePlan) close() {
	if p != nil && p.root != nil {
		p.root.Close()
	}
}

// paths are every local file the plan holds, sent or not, sorted.
func (p *bundlePlan) paths() []string {
	out := make([]string, 0, len(p.Files)+len(p.Unchanged))
	for _, f := range p.Files {
		out = append(out, f.Path)
	}
	for _, f := range p.Unchanged {
		out = append(out, f.Path)
	}
	slices.Sort(out)
	return out
}

// mirror is the plan as the change from the base version's files: it sends
// only the files whose path or content differ from base's, and deletes the
// files base holds and the directory does not. With nothing to send or
// delete, the smallest file is sent all the same, so the server answers as
// it does for any publish that changes nothing.
func (p *bundlePlan) mirror(base []api.File) *bundlePlan {
	m := &bundlePlan{Skipped: p.Skipped, Removed: removedFiles(base, p.paths()), root: p.root}
	sums := make(map[string]string, len(base))
	for _, f := range base {
		sums[f.Path] = strings.ToLower(strings.TrimPrefix(f.Hash, "sha256:"))
	}
	for _, f := range p.Files {
		if sum, ok := sums[f.Path]; ok && sum == f.sum {
			m.Unchanged = append(m.Unchanged, f)
			continue
		}
		m.add(f)
	}
	if len(m.Files) == 0 && len(m.Removed) == 0 && len(m.Unchanged) > 0 {
		i := 0
		for j, f := range m.Unchanged {
			if f.Size < m.Unchanged[i].Size {
				i = j
			}
		}
		m.add(m.Unchanged[i])
		m.Unchanged = slices.Delete(m.Unchanged, i, i+1)
	}
	return m
}

// summary is "3 files, 12 KiB; 11 unchanged; skipped: .env (dotfile);
// removed: a.txt".
func (p *bundlePlan) summary() string {
	s := fmt.Sprintf("%s, %s", count(len(p.Files), "file"), artifact.FormatBytes(p.Bytes))
	if len(p.Unchanged) > 0 {
		s += fmt.Sprintf("; %d unchanged", len(p.Unchanged))
	}
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

// count is "1 file" or "3 files".
func count(n int, noun string) string {
	if n != 1 {
		noun += "s"
	}
	return fmt.Sprintf("%d %s", n, noun)
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
// index.html when it is HTML. A symlink to either is followed. Each file is
// read once here, for its checksum and the secret scan; the caller closes
// the plan.
func planBundle(src string) (*bundlePlan, error) {
	abs, err := filepath.EvalSymlinks(src)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, usagef("no such directory: %s", src)
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", src, err)
	}
	st, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", src, err)
	}
	dir := abs
	switch {
	case st.Mode().IsRegular():
		dir = filepath.Dir(abs)
	case !st.IsDir():
		return nil, usagef("%s is neither a directory nor a file", src)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", src, err)
	}
	p := &bundlePlan{root: root}
	if st.Mode().IsRegular() {
		err = p.planFile(filepath.Base(abs))
	} else {
		err = p.planDir()
	}
	if err != nil {
		p.close()
		return nil, fmt.Errorf("read %s: %w", src, err)
	}
	return p, nil
}

func (p *bundlePlan) planDir() error {
	rules, err := readIgnore(p.root)
	if err != nil {
		return err
	}
	skip := func(rel, reason string) { p.Skipped = append(p.Skipped, skippedFile{Path: rel, Reason: reason}) }
	err = fs.WalkDir(p.root.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		matched, include := rules.match(rel, d.IsDir())
		if matched && !include {
			if d.IsDir() {
				return fs.SkipDir
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
				return fs.SkipDir
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
		f, key, err := p.read(rel, filepath.FromSlash(rel), !include)
		if err != nil {
			return err
		}
		if key {
			skip(rel, "looks like a secret")
			return nil
		}
		p.add(f)
		return nil
	})
	if err != nil {
		return err
	}
	slices.SortFunc(p.Files, func(a, b bundleFile) int { return strings.Compare(a.Path, b.Path) })
	return nil
}

func (p *bundlePlan) planFile(name string) error {
	sent := name
	switch strings.ToLower(filepath.Ext(name)) {
	case ".html", ".htm":
		sent = "index.html"
	}
	f, key, err := p.read(sent, name, true)
	if err != nil {
		return err
	}
	if key || secretName(name) {
		p.Skipped = []skippedFile{{Path: name, Reason: "looks like a secret"}}
		return nil
	}
	p.add(f)
	return nil
}

func (p *bundlePlan) add(f bundleFile) {
	p.Files = append(p.Files, f)
	p.Bytes += f.Size
}

// read reads the file at name in the root once: its size, its checksum and,
// with scan, whether it holds an API key.
func (p *bundlePlan) read(sent, name string, scan bool) (bundleFile, bool, error) {
	fh, err := p.root.Open(name)
	if err != nil {
		return bundleFile{}, false, err
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		return bundleFile{}, false, err
	}
	if !st.Mode().IsRegular() {
		return bundleFile{}, false, fmt.Errorf("%s is not a regular file", name)
	}
	h := sha256.New()
	var w io.Writer = h
	var keys keyScanner
	if scan {
		w = io.MultiWriter(h, &keys)
	}
	n, err := io.Copy(w, fh)
	if err != nil {
		return bundleFile{}, false, err
	}
	return bundleFile{Path: sent, Size: n, name: name, sum: hex.EncodeToString(h.Sum(nil))}, keys.found, nil
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

// keyScanner reports whether what was written to it holds an API key, also
// one split across two writes.
type keyScanner struct {
	tail  []byte
	found bool
}

func (k *keyScanner) Write(b []byte) (int, error) {
	if k.found {
		return len(b), nil
	}
	buf := append(k.tail, b...)
	if keyPattern.Match(buf) {
		k.found = true
		return len(b), nil
	}
	keep := min(len(buf), user.KeyLen-1)
	k.tail = append(k.tail[:0:0], buf[len(buf)-keep:]...)
	return len(b), nil
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

func readIgnore(root *os.Root) (ignoreRules, error) {
	b, err := root.ReadFile(ignoreFile)
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

// errChanged is a file that no longer holds the bytes the plan read.
var errChanged = errors.New("the file changed after it was checked; run the command again")

// writeTarGz writes the plan's files to w as a gzip-compressed tar. Entries
// are sorted, with zero ownership, mode 0644 and epoch mtime, so the same
// tree always yields the same bytes.
func (p *bundlePlan) writeTarGz(w io.Writer) error {
	zw := gzip.NewWriter(w)
	tw := tar.NewWriter(zw)
	for _, f := range p.Files {
		if err := p.addFile(tw, f); err != nil {
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

// addFile sends the bytes the plan checked, or fails before the archive
// ends.
func (p *bundlePlan) addFile(tw *tar.Writer, bf bundleFile) error {
	f, err := p.root.Open(bf.name)
	if err != nil {
		return fmt.Errorf("open %s: %w", bf.Path, err)
	}
	defer f.Close()
	hdr := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     bf.Path,
		Size:     bf.Size,
		Mode:     0o644,
		ModTime:  time.Unix(0, 0),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("tar header %s: %w", bf.Path, err)
	}
	h := sha256.New()
	if _, err := io.CopyN(tw, io.TeeReader(f, h), bf.Size); err != nil {
		if errors.Is(err, io.EOF) {
			err = errChanged
		}
		return fmt.Errorf("tar %s: %w", bf.Path, err)
	}
	if !unchangedAfter(f, h, bf.sum) {
		return fmt.Errorf("tar %s: %w", bf.Path, errChanged)
	}
	return nil
}

// unchangedAfter reports whether r is at its end and h holds sum.
func unchangedAfter(r io.Reader, h hash.Hash, sum string) bool {
	n, _ := r.Read(make([]byte, 1))
	return n == 0 && hex.EncodeToString(h.Sum(nil)) == sum
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
