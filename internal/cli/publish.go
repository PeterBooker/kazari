package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/artifact"
)

// artifactFlags are the fields publish and deploy set on the artifact.
type artifactFlags struct {
	Name        *string           `help:"Display name (default: the <title> of index.html)." placeholder:"NAME"`
	Description *string           `help:"Description." placeholder:"TEXT"`
	Label       string            `help:"Version label." placeholder:"LABEL"`
	Visibility  string            `help:"public, unlisted or members (default: the instance's)." placeholder:"VISIBILITY"`
	Meta        map[string]string `help:"Metadata key=value (repeatable)." mapsep:"none" placeholder:"KEY=VALUE"`
}

// PublishCmd creates an artifact from a directory or a file.
type PublishCmd struct {
	Dir  string `arg:"" help:"Directory to publish, or one file (an HTML file becomes index.html)."`
	Slug string `help:"Artifact slug (default: the directory's or file's name)." placeholder:"SLUG"`
	artifactFlags
	Update bool `help:"The same as kazari deploy: if the slug exists, add a version that mirrors the directory."`
	Base   int  `help:"With --update, the version to build on; needed when an earlier version is published." placeholder:"N"`
	DryRun bool `name:"dry-run" help:"List what would be sent, skipped and removed, and send nothing."`
}

func (c *PublishCmd) Run(r *runner) error {
	if c.Base != 0 && !c.Update {
		return usagef("publish: --base needs --update; a new artifact has no versions")
	}
	return r.publish("publish", c.Dir, c.Slug, &c.artifactFlags, c.Update, c.Base, false, c.DryRun)
}

// DeployCmd makes an artifact match a directory.
type DeployCmd struct {
	Dir  string `arg:"" help:"Directory to deploy, or one file (an HTML file becomes index.html)."`
	Slug string `help:"Artifact slug (default: the directory's or file's name)." placeholder:"SLUG"`
	artifactFlags
	Base   int  `help:"The version to build on and delete from; needed when an earlier version is published." placeholder:"N"`
	Stage  bool `help:"Add the version without publishing it, for review at its version URL; kazari rollback SLUG N publishes it."`
	DryRun bool `name:"dry-run" help:"List what would be sent, skipped and removed, and send nothing."`
}

func (c *DeployCmd) Run(r *runner) error {
	return r.publish("deploy", c.Dir, c.Slug, &c.artifactFlags, true, c.Base, c.Stage, c.DryRun)
}

// publish creates the artifact, or with mirror adds a version holding
// exactly the plan's files when the slug exists.
func (r *runner) publish(cmd, src, slug string, f *artifactFlags, mirror bool, base int, stage, dryRun bool) error {
	switch f.Visibility {
	case "", "public", "unlisted", "members":
	default:
		return usagef("%s: --visibility must be public, unlisted or members", cmd)
	}
	slug, err := slugFor(cmd, slug, src)
	if err != nil {
		return err
	}
	plan, err := planBundle(src)
	if err != nil {
		return err
	}
	if len(plan.Files) == 0 {
		return fmt.Errorf("%s: nothing to publish in %s (%s)", cmd, src, plan.summary())
	}
	if dryRun && !mirror {
		return r.printPlan(plan)
	}
	cl, err := r.userClient()
	if err != nil {
		return err
	}

	create := func() (*api.PublishResult, error) {
		bundle := plan.reader()
		defer bundle.Close()
		return cl.CreateBundle(r.ctx, api.CreateManifest{
			Slug: slug, Name: deref(f.Name), Description: deref(f.Description), Visibility: f.Visibility,
			Metadata: nilIfEmpty(f.Meta), Label: f.Label,
		}, bundle)
	}
	// update mirrors onto the base version: what it has and the plan does
	// not is deleted.
	update := func() (*api.PublishResult, bool, error) {
		want := LatestVersion
		if base > 0 {
			want = base
		}
		cur, v, err := cl.VersionFiles(r.ctx, slug, want)
		if err != nil {
			return nil, false, err
		}
		plan.Removed = removedFiles(v.Files, plan.paths())
		if dryRun {
			return nil, true, r.printPlan(plan)
		}
		m := api.UpdateArtifactRequest{Label: f.Label, ExpectedVersion: cur.Artifact.LatestVersion, BaseVersion: base,
			Stage: stage, Name: f.Name, Description: f.Description, Metadata: metaPatch(f.Meta)}
		if f.Visibility != "" {
			m.Visibility = &f.Visibility
		}
		for _, p := range plan.Removed {
			m.Files = append(m.Files, api.FileInput{Path: p, Delete: true})
		}
		bundle := plan.reader()
		defer bundle.Close()
		res, err := cl.UpdateBundle(r.ctx, slug, m, bundle)
		return res, false, r.baseHint(err, cl, slug)
	}

	var res *api.PublishResult
	if !mirror {
		res, err = create()
		if IsCode(err, api.CodeSlugTaken) && strings.Contains(err.Error(), "already taken") {
			err = withHints(err, fmt.Sprintf("to make it match %s: kazari deploy %s --slug %s", src, shellQuote(src), slug))
		}
	} else {
		var done bool
		res, done, err = update()
		switch {
		case done:
			return err
		case IsCode(err, api.CodeNotFound) && stage:
			return usagef("%s: --stage needs an existing artifact; %s does not exist, and a new one is published as version 1", cmd, slug)
		case IsCode(err, api.CodeNotFound):
			if dryRun {
				fmt.Fprintf(r.stderr, "%s does not exist yet and would be created\n", slug)
				return r.printPlan(plan)
			}
			res, err = create()
			if IsCode(err, api.CodeSlugTaken) {
				// Created meanwhile, unless it is a slug the caller cannot
				// see, whose create error says why.
				if res2, _, err2 := update(); !IsCode(err2, api.CodeNotFound) {
					res, err = res2, err2
				}
			}
		}
	}
	if err != nil {
		return err
	}
	return r.printPublished(slug, res, plan)
}

// slugFor defaults the slug to the name of the directory or file, lower-
// cased with other characters as hyphens.
func slugFor(cmd, slug, src string) (string, error) {
	if slug != "" {
		return slug, nil
	}
	abs, err := filepath.Abs(src)
	if err != nil {
		return "", usagef("%s: --slug is required: %v", cmd, err)
	}
	name := filepath.Base(abs)
	if ext := filepath.Ext(name); ext != "" && ext != name {
		if st, err := os.Stat(abs); err == nil && st.Mode().IsRegular() {
			name = strings.TrimSuffix(name, ext)
		}
	}
	guess := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if err := artifact.ValidateSlug(guess, nil); err != nil {
		return "", usagef("%s: %q does not make a slug; give --slug", cmd, name)
	}
	return guess, nil
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// UpdateCmd adds a version, changes fields, or both.
type UpdateCmd struct {
	Slug        string            `arg:"" help:"Artifact slug."`
	Dir         string            `arg:"" optional:"" help:"Directory whose files are added, replacing those at the same paths; files not in it are kept (kazari deploy deletes them)."`
	Label       string            `help:"Version label." placeholder:"LABEL"`
	Expected    int               `help:"Fail unless the latest version is N." placeholder:"N"`
	Base        int               `help:"The version to build on; needed when an earlier version is published." placeholder:"N"`
	KeepStale   bool              `name:"keep-stale" help:"Keep the stale mark."`
	Stage       bool              `help:"Add the version without publishing it, for review at its version URL; kazari rollback SLUG N publishes it."`
	Name        *string           `help:"Display name." placeholder:"NAME"`
	Description *string           `help:"Description." placeholder:"TEXT"`
	Meta        map[string]string `help:"Metadata key=value (repeatable)." mapsep:"none" placeholder:"KEY=VALUE"`
	Delete      []string          `help:"Delete the file at PATH (repeatable)." sep:"none" placeholder:"PATH"`
	DryRun      bool              `name:"dry-run" help:"List what would be sent and deleted, and send nothing."`
}

func (c *UpdateCmd) Run(r *runner) error {
	m := api.UpdateArtifactRequest{Label: c.Label, ExpectedVersion: c.Expected, BaseVersion: c.Base, KeepStale: c.KeepStale,
		Stage: c.Stage, Name: c.Name, Description: c.Description}
	m.Metadata = metaPatch(c.Meta)
	for _, p := range c.Delete {
		m.Files = append(m.Files, api.FileInput{Path: p, Delete: true})
	}
	if c.Dir == "" && m.Name == nil && m.Description == nil && m.Metadata == nil && len(m.Files) == 0 {
		return usagef("update: nothing to update: give a directory, --delete, --name, --description or --meta")
	}
	var plan *bundlePlan
	if c.Dir != "" {
		var err error
		if plan, err = planBundle(c.Dir); err != nil {
			return err
		}
		plan.Removed = c.Delete
	}
	if c.DryRun {
		if plan == nil {
			plan = &bundlePlan{Removed: c.Delete}
		}
		return r.printPlan(plan)
	}
	cl, err := r.userClient()
	if err != nil {
		return err
	}
	var res *api.PublishResult
	if plan != nil {
		bundle := plan.reader()
		defer bundle.Close()
		res, err = cl.UpdateBundle(r.ctx, c.Slug, m, bundle)
	} else {
		res, err = cl.Update(r.ctx, c.Slug, m)
	}
	if err != nil {
		return r.baseHint(err, cl, c.Slug)
	}
	return r.printPublished(c.Slug, res, plan)
}

// baseHint follows published_not_latest with the command run again with
// either --base it needs.
func (r *runner) baseHint(err error, cl *Client, slug string) error {
	if !IsCode(err, api.CodePublishedNotLatest) {
		return err
	}
	d, gerr := cl.Get(r.ctx, slug, false)
	if gerr != nil {
		return err
	}
	a := d.Artifact
	cmd := r.commandLine()
	return withHints(err,
		fmt.Sprintf("to change the published version %d: %s --base %d", a.PublishedVersion, cmd, a.PublishedVersion),
		fmt.Sprintf("to build on the latest version %d and publish it: %s --base %d", a.LatestVersion, cmd, a.LatestVersion),
		fmt.Sprintf("to add to the latest version %d without publishing: %s --stage", a.LatestVersion, cmd))
}

// commandLine is the command as typed, without the key.
func (r *runner) commandLine() string {
	parts := []string{"kazari"}
	for i := 0; i < len(r.args); i++ {
		a := r.args[i]
		name, _, hasValue := strings.Cut(a, "=")
		if name == "--token" || name == "--admin-token" {
			if !hasValue {
				i++
			}
			continue
		}
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

func shellQuote(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// printPlan is --dry-run's output: one line per file sent, deleted or
// skipped, and the summary on stderr.
func (r *runner) printPlan(p *bundlePlan) error {
	if r.g.JSON {
		return r.printJSON(p)
	}
	tw := newTable(r.stdout)
	for _, f := range p.Files {
		fmt.Fprintf(tw, "send\t%s\t%s\n", f.Path, humanSize(f.Size))
	}
	for _, path := range p.Removed {
		fmt.Fprintf(tw, "delete\t%s\t\n", path)
	}
	for _, s := range p.Skipped {
		fmt.Fprintf(tw, "skip\t%s\t%s\n", s.Path, s.Reason)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(termSafe{r.stderr}, "dry run, nothing sent: %s\n", p.summary())
	return nil
}

// printPublished puts what was sent and what the server noticed on stderr,
// and the result on stdout: "hello v5 → URL (pinned: URL)".
func (r *runner) printPublished(slug string, res *api.PublishResult, plan *bundlePlan) error {
	errOut := termSafe{r.stderr}
	if plan != nil {
		fmt.Fprintln(errOut, plan.summary())
	}
	if res.Unchanged {
		fmt.Fprintf(errOut, "no changes: version %d is already this content\n", res.Version)
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(errOut, "warning: %s\n", w)
	}
	if r.g.JSON {
		return r.printJSON(res)
	}
	out := termSafe{r.stdout}
	if res.Version == 0 {
		fmt.Fprintf(out, "%s → %s\n", slug, res.URL)
		return nil
	}
	fmt.Fprintln(out, versionLine(slug, res.Version, res.URL, res.VersionURL, res.Staged, res.Artifact.PublishedVersion))
	return nil
}

// versionLine names the version a change made and where it opens; a staged
// one opens only at its pinned URL.
func versionLine(slug string, version int, url, versionURL string, staged bool, published int) string {
	if staged {
		return fmt.Sprintf("%s v%d staged → %s (members only; %s serves v%d until kazari rollback %s %d)",
			slug, version, versionURL, url, published, slug, version)
	}
	line := fmt.Sprintf("%s v%d → %s", slug, version, url)
	if versionURL != "" {
		line += " (pinned: " + versionURL + ")"
	}
	return line
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// metaPatch turns --meta pairs into the update request's patch; nil when
// there are none.
func metaPatch(meta map[string]string) map[string]*string {
	if len(meta) == 0 {
		return nil
	}
	out := make(map[string]*string, len(meta))
	for k, v := range meta {
		out[k] = &v
	}
	return out
}

// removedFiles are the paths of have that local, sorted, lacks.
func removedFiles(have []api.File, local []string) []string {
	var out []string
	for _, f := range have {
		if _, found := slices.BinarySearch(local, f.Path); !found {
			out = append(out, f.Path)
		}
	}
	return out
}

func nilIfEmpty(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	return m
}
