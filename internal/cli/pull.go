package cli

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/PeterBooker/kazari/internal/artifact"
)

// PullCmd writes a version's files to a directory.
type PullCmd struct {
	Slug    string `arg:"" help:"Artifact slug."`
	Dir     string `arg:"" optional:"" help:"Directory to write into (default: ./<slug>); it must be empty or not exist yet unless --force. - writes the archive to standard output as tar.gz."`
	Version int    `help:"Pull version N instead of the published one." placeholder:"N"`
	Force   bool   `help:"Write into a directory that is not empty, replacing the files the version holds and keeping the others."`
}

func (c *PullCmd) Run(r *runner) error {
	if c.Version < 0 {
		return usagef("pull: --version must be a positive integer")
	}
	if c.Dir == "-" {
		return c.stream(r)
	}
	dir := c.Dir
	if dir == "" {
		dir = c.Slug
	}
	// Before the download, which may be large.
	if err := emptyDir("pull", dir, c.Force, ", or add --force to replace the files the version holds"); err != nil {
		return err
	}
	cl, err := r.userClient()
	if err != nil {
		return err
	}
	body, version, err := cl.Bundle(r.ctx, c.Slug, c.Version)
	if err != nil {
		return r.nearSlug(err, cl, c.Slug)
	}
	defer body.Close()
	pulled, size, err := extractBundle(body, dir)
	if err != nil {
		return fmt.Errorf("pull: %w; what was written before it stays in %s, so pull again with --force", err, dir)
	}
	left, err := notDeployed(dir, pulled)
	if err != nil {
		return err
	}
	if len(left) > 0 {
		fmt.Fprintf(r.stderr, "kazari deploy would leave out, and delete from %s, %s; list them with ! in %s to send them\n",
			c.Slug, shortList(left), ignoreFile)
	}
	if r.g.JSON {
		return r.printJSON(struct {
			Slug        string   `json:"slug"`
			Version     int      `json:"version"`
			Dir         string   `json:"dir"`
			Files       int      `json:"files"`
			Bytes       int64    `json:"bytes"`
			NotDeployed []string `json:"not_deployed,omitempty"`
		}{c.Slug, version, dir, len(pulled), size, left})
	}
	noun := "files"
	if len(pulled) == 1 {
		noun = "file"
	}
	fmt.Fprintf(r.stdout, "pulled version %d of %s to %s: %d %s, %s\n", version, c.Slug, dir, len(pulled), noun, artifact.FormatBytes(size))
	return nil
}

// stream writes the archive as the server sent it, for a pipeline.
func (c *PullCmd) stream(r *runner) error {
	if err := r.notTerminal("pull -", c.Slug+".tar.gz"); err != nil {
		return err
	}
	cl, err := r.userClient()
	if err != nil {
		return err
	}
	body, _, err := cl.Bundle(r.ctx, c.Slug, c.Version)
	if err != nil {
		return r.nearSlug(err, cl, c.Slug)
	}
	defer body.Close()
	if _, err := io.Copy(r.stdout, body); err != nil {
		return fmt.Errorf("pull: the archive is incomplete: %w", err)
	}
	return nil
}

// emptyDir reports why dir cannot take what cmd writes: it must be an empty
// directory or not exist yet, or with force any directory.
func emptyDir(cmd, dir string, force bool, forceHint string) error {
	const want = "name an empty directory or one that does not exist yet"
	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("%s: %w", cmd, err)
	case !info.IsDir():
		return usagef("%s: %s is a file; %s", cmd, dir, want)
	case force:
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("%s: %w", cmd, err)
	}
	if len(entries) > 0 {
		return usagef("%s: %s is not empty; %s%s", cmd, dir, want, forceHint)
	}
	return nil
}

// notTerminal stops cmd writing an archive to a terminal.
func (r *runner) notTerminal(cmd, file string) error {
	if r.isTerminal(r.stdout) {
		return usagef("%s: standard output is a terminal; redirect it to a file, such as > %s", cmd, file)
	}
	return nil
}

var errUnsafeEntry = errors.New("unsafe archive entry")

// extractBundle writes a gzip-compressed tar of regular files into dir,
// replacing files of the same name, and returns the paths it wrote, sorted,
// and their total size. An entry that is not a regular file or directory,
// whose name is not a clean relative path, or that lands on a file an
// earlier entry wrote fails with errUnsafeEntry. A symlink already in dir
// on an entry's path fails too, so --force replaces files and never writes
// through a link to another one.
func extractBundle(r io.Reader, dir string) ([]string, int64, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, 0, fmt.Errorf("read archive: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, 0, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, 0, err
	}
	defer root.Close()

	type written struct {
		name string
		info os.FileInfo
	}
	var (
		pulled []string
		size   int64
		files  []written
	)
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return pulled, size, fmt.Errorf("read archive: %w", err)
		}
		name := hdr.Name
		if hdr.Typeflag == tar.TypeDir {
			name = strings.TrimSuffix(name, "/")
		}
		local := filepath.FromSlash(name)
		if !fs.ValidPath(name) || name == "." || !filepath.IsLocal(local) {
			return pulled, size, fmt.Errorf("%w: %q is not a clean path inside the directory", errUnsafeEntry, hdr.Name)
		}
		if err := noSymlink(root, local); err != nil {
			return pulled, size, err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(local, 0o755); err != nil {
				return pulled, size, fmt.Errorf("write %s: %w", hdr.Name, err)
			}
		case tar.TypeReg:
			// On a file system that folds case or normalises names, two
			// paths of the version can be one file.
			if fi, err := root.Lstat(local); err == nil {
				for _, w := range files {
					if os.SameFile(fi, w.info) {
						return pulled, size, fmt.Errorf("%w: %q and %q are the same file in %s", errUnsafeEntry, w.name, hdr.Name, dir)
					}
				}
			}
			n, err := writePulled(root, local, tr)
			if err != nil {
				return pulled, size, fmt.Errorf("write %s: %w", hdr.Name, err)
			}
			fi, err := root.Lstat(local)
			if err != nil {
				return pulled, size, fmt.Errorf("write %s: %w", hdr.Name, err)
			}
			files = append(files, written{hdr.Name, fi})
			pulled = append(pulled, name)
			size += n
		default:
			return pulled, size, fmt.Errorf("%w: %q is neither a file nor a directory", errUnsafeEntry, hdr.Name)
		}
	}
	// To the end of the gzip stream, whose checksum is verified there: an
	// archive cut short after its last entry is still an error.
	if _, err := io.Copy(io.Discard, zr); err != nil {
		return pulled, size, fmt.Errorf("read archive: %w", err)
	}
	slices.Sort(pulled)
	return pulled, size, nil
}

// noSymlink fails if name, or a directory on its way, is a symlink in root.
func noSymlink(root *os.Root, name string) error {
	p := ""
	for part := range strings.SplitSeq(name, string(filepath.Separator)) {
		p = filepath.Join(p, part)
		fi, err := root.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink, which pull does not write through; remove it or pull into an empty directory", filepath.Join(root.Name(), p))
		}
	}
	return nil
}

func writePulled(root *os.Root, name string, r io.Reader) (int64, error) {
	if parent := filepath.Dir(name); parent != "." {
		if err := root.MkdirAll(parent, 0o755); err != nil {
			return 0, err
		}
	}
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, r)
	if err != nil {
		f.Close()
		return n, err
	}
	return n, f.Close()
}

// notDeployed returns the pulled paths that a deploy of dir would not send,
// by asking the planner deploy uses, so the two cannot disagree.
func notDeployed(dir string, pulled []string) ([]string, error) {
	plan, err := planBundle(dir)
	if err != nil {
		return nil, err
	}
	plan.close()
	sent := make(map[string]bool, len(plan.Files))
	for _, f := range plan.Files {
		sent[f.Path] = true
	}
	var left []string
	for _, p := range pulled {
		if !sent[p] {
			left = append(left, p)
		}
	}
	return left, nil
}
