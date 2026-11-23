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
	if err := pullTarget(dir, c.Force); err != nil {
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
		if errors.Is(err, errUnsafeEntry) {
			return fmt.Errorf("pull: %w; the server sent an archive pull does not write", err)
		}
		return fmt.Errorf("pull: %w; what was written before it stays in %s", err, dir)
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
	fmt.Fprintf(r.stdout, "pulled version %d of %s to %s: %d %s, %s\n", version, c.Slug, dir, len(pulled), noun, humanSize(size))
	return nil
}

// stream writes the archive as the server sent it, for a pipeline.
func (c *PullCmd) stream(r *runner) error {
	if f, ok := r.stdout.(*os.File); ok {
		if fi, err := f.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
			return usagef("pull -: standard output is a terminal; redirect it to a file, such as > %s.tar.gz", c.Slug)
		}
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

// pullTarget reports why dir cannot take a pull: without force it must be
// empty or not exist yet.
func pullTarget(dir string, force bool) error {
	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("pull: %w", err)
	case !info.IsDir():
		return usagef("pull: %s is a file; name a directory", dir)
	case force:
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("pull: %w", err)
	}
	if len(entries) > 0 {
		return usagef("pull: %s is not empty; name an empty directory or one that does not exist yet, or add --force to replace the files the version holds", dir)
	}
	return nil
}

var errUnsafeEntry = errors.New("unsafe archive entry")

// extractBundle writes a gzip-compressed tar of regular files into dir,
// replacing files of the same name, and returns the paths it wrote, sorted,
// and their total size. An entry that is not a regular file or directory,
// leads outside dir or comes twice fails with errUnsafeEntry.
func extractBundle(r io.Reader, dir string) ([]string, int64, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, 0, fmt.Errorf("read archive: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, 0, err
	}
	// os.Root refuses names that leave dir, through a symlink already in it
	// too, which --force may meet.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, 0, err
	}
	defer root.Close()

	var (
		pulled []string
		size   int64
		seen   = map[string]bool{}
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
		name := filepath.FromSlash(hdr.Name)
		if !filepath.IsLocal(name) {
			return pulled, size, fmt.Errorf("%w: %q leads outside the directory", errUnsafeEntry, hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o755); err != nil {
				return pulled, size, fmt.Errorf("write %s: %w", hdr.Name, err)
			}
		case tar.TypeReg:
			if seen[name] {
				return pulled, size, fmt.Errorf("%w: %q comes twice", errUnsafeEntry, hdr.Name)
			}
			seen[name] = true
			n, err := writePulled(root, name, tr)
			if err != nil {
				return pulled, size, fmt.Errorf("write %s: %w", hdr.Name, err)
			}
			pulled = append(pulled, filepath.ToSlash(name))
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
