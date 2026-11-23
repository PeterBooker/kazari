package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/apiclient"
	"github.com/PeterBooker/kazari/internal/artifact"
	"github.com/PeterBooker/kazari/internal/config"
)

// ImportCmd copies a web page into a new artifact.
type ImportCmd struct {
	URL         string            `arg:"" help:"Address of the page to copy."`
	Slug        string            `help:"Artifact slug (required)." placeholder:"SLUG"`
	Name        *string           `help:"Display name (default: the page title)." placeholder:"NAME"`
	Description *string           `help:"Description (default: the page's meta description)." placeholder:"TEXT"`
	Label       string            `help:"Version label." placeholder:"LABEL"`
	Visibility  string            `help:"public, unlisted or members (default: the instance's)." enum:"public,unlisted,members," default:"" placeholder:"VISIBILITY"`
	Meta        map[string]string `help:"Metadata key=value (repeatable)." mapsep:"none" placeholder:"KEY=VALUE"`
	KeepScripts bool              `name:"keep-scripts" help:"Keep the page's scripts and event handlers."`
}

func (c *ImportCmd) Run(r *runner) error {
	if c.Slug == "" {
		return usagef("import: --slug is required")
	}
	cl, err := r.userClient()
	if err != nil {
		return err
	}
	res, err := cl.Create(r.ctx, api.CreateArtifactRequest{
		Slug: c.Slug, Name: deref(c.Name), Description: deref(c.Description), Visibility: c.Visibility,
		Metadata: nilIfEmpty(c.Meta), Label: c.Label, SourceURL: c.URL, KeepScripts: c.KeepScripts,
	})
	if err != nil {
		return err
	}
	if r.g.JSON {
		return r.printJSON(res)
	}
	fmt.Fprintln(r.stdout, res.URL)
	if imp := res.Import; imp != nil {
		out := termSafe{r.stdout}
		fmt.Fprintf(out, "%d files from %s; %d scripts removed\n", imp.Fetched+1, imp.Source, imp.ScriptsRemoved)
		for _, s := range imp.Skipped {
			fmt.Fprintf(out, "left remote: %s (%s)\n", s.URL, s.Reason)
		}
		for _, n := range imp.Notes {
			fmt.Fprintf(out, "note: %s\n", n)
		}
	}
	return nil
}

// GetCmd shows an artifact and its versions.
type GetCmd struct {
	Slug  string `arg:"" help:"Artifact slug."`
	Files bool   `help:"Include each version's files."`
}

func (c *GetCmd) Run(r *runner) error {
	cl, err := r.userClient()
	if err != nil {
		return err
	}
	d, err := cl.Get(r.ctx, c.Slug, c.Files)
	if err != nil {
		return r.nearSlug(err, cl, c.Slug)
	}
	if r.g.JSON {
		return r.printJSON(d)
	}
	r.printArtifact(&d.Artifact)
	if len(d.Versions) == 0 {
		return nil
	}
	fmt.Fprintln(r.stdout)
	tw := newTable(r.stdout)
	tw.row("VERSION", "LABEL", "CREATED BY", "CREATED", "SIZE", "URL")
	for _, v := range d.Versions {
		num := strconv.Itoa(v.Number)
		switch {
		case v.Number == d.Artifact.PublishedVersion:
			num += " *"
		case v.Staged:
			num += " staged"
		}
		tw.row(num, dash(v.Label), dash(v.CreatedBy), fmtTime(v.CreatedAt), artifact.FormatBytes(v.SizeBytes), v.URL)
	}
	tw.flush()
	if !c.Files {
		return nil
	}
	for _, v := range d.Versions {
		if d.NextFilesBefore != 0 && v.Number < d.NextFilesBefore {
			fmt.Fprintf(r.stdout, "\nversions below %d not listed; ask the API with files_before=%d\n", d.NextFilesBefore, d.NextFilesBefore)
			break
		}
		fmt.Fprintf(r.stdout, "\nversion %d files:\n", v.Number)
		tw := newTable(r.stdout)
		tw.row("PATH", "SIZE", "TYPE", "HASH")
		for _, f := range v.Files {
			tw.row(f.Path, artifact.FormatBytes(f.Size), f.ContentType, shortHash(f.Hash))
		}
		tw.flush()
	}
	return nil
}

func (r *runner) printArtifact(a *api.Artifact) {
	tw := newTable(r.stdout)
	kv := func(k, v string) {
		if v != "" {
			tw.row(k+":", v)
		}
	}
	kv("slug", a.Slug)
	kv("name", a.Name)
	kv("description", a.Description)
	kv("owner", a.Owner)
	kv("url", a.URL)
	kv("visibility", a.Visibility)
	kv("state", artifactState(a))
	kv("version", fmt.Sprintf("%d published, %d latest", a.PublishedVersion, a.LatestVersion))
	kv("size", artifact.FormatBytes(a.SizeBytes))
	kv("metadata", fmtMap(a.Metadata))
	kv("created", fmtTime(a.CreatedAt))
	kv("updated", fmtTime(a.UpdatedAt))
	if a.StaleAt != nil {
		kv("stale at", fmtTime(*a.StaleAt))
	}
	if a.ExpiresAt != nil {
		kv("expires at", fmtTime(*a.ExpiresAt))
	}
	if a.UnpublishedAt != nil {
		kv("unpublished at", fmtTime(*a.UnpublishedAt))
	}
	if a.DeletedAt != nil {
		kv("deleted at", fmtTime(*a.DeletedAt))
	}
	if a.PurgedAt != nil {
		kv("purged at", fmtTime(*a.PurgedAt))
	}
	tw.flush()
}

// listFlags are the filters of list and admin artifacts list.
type listFlags struct {
	Mine       string            `help:"created, updated or any." placeholder:"WHICH"`
	Meta       map[string]string `help:"Metadata filter key=value (repeatable)." mapsep:"none" placeholder:"KEY=VALUE"`
	Stale      string            `help:"include, exclude or only." placeholder:"WHICH"`
	Deleted    string            `help:"include, exclude or only." placeholder:"WHICH"`
	Search     string            `help:"Only artifacts with this text in their slug or name." placeholder:"TEXT"`
	Visibility string            `help:"public, unlisted, members or unpublished." placeholder:"WHICH"`
	Cursor     string            `help:"Page cursor from a previous list." placeholder:"CURSOR"`
	Limit      int               `help:"Page size." placeholder:"N"`
	All        bool              `help:"Follow cursors and print every page."`
}

func (f *listFlags) params() apiclient.ListParams {
	return apiclient.ListParams{Mine: f.Mine, Metadata: nilIfEmpty(f.Meta), Stale: f.Stale, Deleted: f.Deleted,
		Search: f.Search, Visibility: f.Visibility, Cursor: f.Cursor, Limit: f.Limit}
}

func (r *runner) fetchList(f *listFlags, list func(apiclient.ListParams) (*api.ArtifactList, error)) (*api.ArtifactList, error) {
	p := f.params()
	out, err := list(p)
	if err != nil || !f.All {
		return out, err
	}
	for out.NextCursor != "" && out.NextCursor != p.Cursor {
		p.Cursor = out.NextCursor
		page, err := list(p)
		if err != nil {
			return nil, err
		}
		out.Items = append(out.Items, page.Items...)
		out.NextCursor = page.NextCursor
	}
	out.NextCursor = ""
	return out, nil
}

// ListCmd lists artifacts.
type ListCmd struct {
	listFlags
}

func (c *ListCmd) Run(r *runner) error {
	cl, err := r.userClient()
	if err != nil {
		return err
	}
	l, err := r.fetchList(&c.listFlags, func(p apiclient.ListParams) (*api.ArtifactList, error) { return cl.List(r.ctx, p) })
	if err != nil {
		return err
	}
	return r.printList(l, false)
}

func (r *runner) printList(l *api.ArtifactList, owner bool) error {
	if r.g.JSON {
		return r.printJSON(l)
	}
	tw := newTable(r.stdout)
	head := []any{"SLUG", "VERSION", "VISIBILITY", "STATE", "SIZE", "UPDATED", "NAME"}
	if owner {
		head = append([]any{"OWNER"}, head...)
	}
	tw.row(head...)
	for _, a := range l.Items {
		ver := strconv.Itoa(a.PublishedVersion)
		if a.LatestVersion != a.PublishedVersion {
			ver += "/" + strconv.Itoa(a.LatestVersion)
		}
		cells := []any{a.Slug, ver, a.Visibility, artifactState(&a), artifact.FormatBytes(a.SizeBytes), fmtTime(a.UpdatedAt), a.Name}
		if owner {
			cells = append([]any{a.Owner}, cells...)
		}
		tw.row(cells...)
	}
	if err := tw.flush(); err != nil {
		return err
	}
	if l.NextCursor != "" {
		fmt.Fprintf(r.stderr, "more results: --cursor %s (or --all)\n", l.NextCursor)
	}
	return nil
}

// nearSlug adds the closest slug the caller can see to not_found, when
// one is close enough to be a typo.
func (r *runner) nearSlug(err error, cl *apiclient.Client, slug string) error {
	if !apiclient.IsCode(err, api.CodeNotFound) {
		return err
	}
	l, lerr := cl.List(r.ctx, apiclient.ListParams{Mine: "any", Deleted: "include", Limit: 200})
	if lerr != nil {
		return err
	}
	best, bestDist := "", 3
	for _, a := range l.Items {
		if d := editDistance(slug, a.Slug); d < bestDist {
			best, bestDist = a.Slug, d
		}
	}
	if best == "" {
		return err
	}
	return withHints(err, fmt.Sprintf("did you mean %s?", best))
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// RmCmd deletes an artifact.
type RmCmd struct {
	Slug string `arg:"" help:"Artifact slug."`
	Hard bool   `help:"Purge now instead of a soft delete (needs artifacts:delete); asks for the slug on a terminal."`
	Yes  bool   `help:"Purge without asking; needed for --hard when stdin is not a terminal."`
}

func (c *RmCmd) Run(r *runner) error {
	if c.Hard && !c.Yes {
		if err := r.confirmPurge(c.Slug); err != nil {
			return err
		}
	}
	cl, err := r.userClient()
	if err != nil {
		return err
	}
	if err := cl.Delete(r.ctx, c.Slug, c.Hard); err != nil {
		return err
	}
	if r.g.JSON {
		return r.printJSON(api.OK{OK: true})
	}
	verb := "deleted"
	if c.Hard {
		verb = "purged"
	}
	fmt.Fprintf(r.stdout, "%s %s\n", verb, c.Slug)
	return nil
}

// confirmPurge asks for the slug typed out, since a purge cannot be undone.
func (r *runner) confirmPurge(slug string) error {
	if !r.terminal() {
		return usagef("rm: --hard purges %s and every version for good; add --yes to do it without a terminal to confirm on", slug)
	}
	fmt.Fprintf(r.stderr, "This purges %s and every version for good. Type the slug to confirm: ", slug)
	line, err := bufio.NewReader(r.stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("rm: read the confirmation: %w", err)
	}
	if strings.TrimSpace(line) != slug {
		return usagef("rm: not purged: the slug did not match")
	}
	return nil
}

// StaleCmd marks an artifact stale.
type StaleCmd struct {
	Slug  string `arg:"" help:"Artifact slug."`
	Grace string `help:"Time until expiry, such as 72h, 3d or 2w (default: the server's)." placeholder:"DURATION"`
}

func (c *StaleCmd) Run(r *runner) error {
	if c.Grace != "" {
		if _, err := config.ParseDuration(c.Grace); err != nil {
			return usagef("stale: --grace: %v", err)
		}
	}
	cl, err := r.userClient()
	if err != nil {
		return err
	}
	res, err := cl.MarkStale(r.ctx, c.Slug, c.Grace)
	if err != nil {
		return err
	}
	if r.g.JSON {
		return r.printJSON(res)
	}
	fmt.Fprintf(r.stdout, "%s is stale since %s and expires at %s\n", c.Slug, fmtTime(res.StaleAt), fmtTime(res.ExpiresAt))
	return nil
}

// UnstaleCmd clears the stale mark.
type UnstaleCmd struct {
	Slug string `arg:"" help:"Artifact slug."`
}

func (c *UnstaleCmd) Run(r *runner) error {
	cl, err := r.userClient()
	if err != nil {
		return err
	}
	a, err := cl.UnmarkStale(r.ctx, c.Slug)
	if err != nil {
		return err
	}
	if r.g.JSON {
		return r.printJSON(a)
	}
	fmt.Fprintf(r.stdout, "%s is no longer stale\n", c.Slug)
	return nil
}

// VisibilityCmd sets an artifact's visibility.
type VisibilityCmd struct {
	Slug       string `arg:"" help:"Artifact slug."`
	Visibility string `arg:"" help:"public, unlisted, members or unpublished." enum:"public,unlisted,members,unpublished"`
}

func (c *VisibilityCmd) Run(r *runner) error {
	cl, err := r.userClient()
	if err != nil {
		return err
	}
	res, err := cl.SetVisibility(r.ctx, c.Slug, c.Visibility)
	if err != nil {
		return err
	}
	if r.g.JSON {
		return r.printJSON(res)
	}
	fmt.Fprintf(r.stdout, "%s is now %s\n", c.Slug, c.Visibility)
	return nil
}

// ShareCmd manages share links.
type ShareCmd struct {
	Create ShareCreateCmd `cmd:"" help:"Create a share link that opens a members artifact."`
	List   ShareListCmd   `cmd:"" help:"List an artifact's share links."`
	Revoke ShareRevokeCmd `cmd:"" help:"Revoke a share link."`
}

// ShareCreateCmd creates a share link.
type ShareCreateCmd struct {
	Slug    string `arg:"" help:"Artifact slug."`
	Label   string `help:"Note on who the link is for." placeholder:"LABEL"`
	Expires string `help:"How long the link works, such as 72h or 14d (default: 7d)." placeholder:"DURATION"`
	// A flag, not a value: a password on the command line would be in the
	// shell's history and the process list.
	Password bool `help:"Ask for a password the link needs before it opens the page: typed twice on a terminal, else the first line of stdin."`
}

func (c *ShareCreateCmd) Run(r *runner) error {
	var password string
	if c.Password {
		var err error
		if password, err = r.readLinkPassword(); err != nil {
			return err
		}
	}
	cl, err := r.userClient()
	if err != nil {
		return err
	}
	res, err := cl.CreateShareLink(r.ctx, c.Slug, c.Label, c.Expires, password)
	if err != nil {
		return err
	}
	if r.g.JSON {
		return r.printJSON(res)
	}
	fmt.Fprintln(r.stdout, res.URL)
	who := "anyone with the URL"
	if res.ShareLink.Password {
		who = "anyone with the URL and the password; send the password apart from it"
	}
	fmt.Fprintf(r.stderr, "share link %s opens %s until %s; %s can open it\n",
		res.ShareLink.Prefix, c.Slug, fmtTime(res.ShareLink.ExpiresAt), who)
	if res.Artifact.Visibility != "members" {
		fmt.Fprintf(r.stderr, "note: %s is %s, so it opens without the link too (kazari visibility %s members)\n",
			c.Slug, res.Artifact.Visibility, c.Slug)
	}
	return nil
}

// readLinkPassword reads a share link's password: typed twice with the
// echo off on a terminal, else the first line of stdin.
func (r *runner) readLinkPassword() (string, error) {
	in := bufio.NewReader(r.stdin)
	line := func() (string, error) {
		s, err := in.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("share create: read the password: %w", err)
		}
		return strings.TrimRight(s, "\r\n"), nil
	}
	if !r.terminal() {
		p, err := line()
		if err == nil && p == "" {
			err = usagef("share create: --password read an empty line from stdin")
		}
		return p, err
	}
	var first, second string
	ask := func(prompt string, into *string) error {
		fmt.Fprint(r.stderr, prompt)
		err := withoutEcho(r.stdin, func() error {
			var err error
			*into, err = line()
			return err
		})
		fmt.Fprintln(r.stderr)
		return err
	}
	if err := ask("Password for the link: ", &first); err != nil {
		return "", err
	}
	if err := ask("The same password again: ", &second); err != nil {
		return "", err
	}
	switch {
	case first == "":
		return "", usagef("share create: no password was typed")
	case first != second:
		return "", usagef("share create: the two passwords differ")
	}
	return first, nil
}

// ShareListCmd lists share links.
type ShareListCmd struct {
	Slug string `arg:"" help:"Artifact slug."`
}

func (c *ShareListCmd) Run(r *runner) error {
	cl, err := r.userClient()
	if err != nil {
		return err
	}
	list, err := cl.ListShareLinks(r.ctx, c.Slug)
	if err != nil {
		return err
	}
	if r.g.JSON {
		return r.printJSON(list)
	}
	tw := newTable(r.stdout)
	tw.row("PREFIX", "LABEL", "CREATED BY", "CREATED", "EXPIRES", "USES", "LAST USED", "PASSWORD", "STATE")
	for _, l := range list.Items {
		state := "active"
		switch {
		case l.RevokedAt != nil:
			state = "revoked"
		case !l.Active:
			state = "expired"
		}
		password := "no"
		if l.Password {
			password = "yes"
		}
		tw.row(l.Prefix, dash(l.Label), l.CreatedBy, fmtTime(l.CreatedAt), fmtTime(l.ExpiresAt), l.Uses, fmtTimePtr(l.LastUsedAt), password, state)
	}
	return tw.flush()
}

// ShareRevokeCmd revokes a share link.
type ShareRevokeCmd struct {
	Slug   string `arg:"" help:"Artifact slug."`
	Prefix string `arg:"" help:"The link's prefix, from share list."`
}

func (c *ShareRevokeCmd) Run(r *runner) error {
	cl, err := r.userClient()
	if err != nil {
		return err
	}
	if err := cl.RevokeShareLink(r.ctx, c.Slug, c.Prefix); err != nil {
		return err
	}
	if r.g.JSON {
		return r.printJSON(api.OK{OK: true})
	}
	fmt.Fprintf(r.stdout, "revoked share link %s of %s\n", c.Prefix, c.Slug)
	return nil
}

// VersionsCmd deletes versions.
type VersionsCmd struct {
	Rm    VersionsRmCmd    `cmd:"" help:"Delete versions that are neither published nor the latest."`
	Prune VersionsPruneCmd `cmd:"" help:"Delete every version but the newest, the published one and staged ones newer than it."`
}

// VersionsRmCmd deletes versions by number.
type VersionsRmCmd struct {
	Slug     string   `arg:"" help:"Artifact slug."`
	Versions []string `arg:"" help:"Version numbers to delete."`
}

func (c *VersionsRmCmd) Run(r *runner) error {
	numbers := make([]int, len(c.Versions))
	for i, s := range c.Versions {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			return usagef("versions rm: version must be a positive integer, got %q", s)
		}
		numbers[i] = n
	}
	cl, err := r.userClient()
	if err != nil {
		return err
	}
	var last *api.DeletedVersions
	for _, n := range numbers {
		if last, err = cl.DeleteVersion(r.ctx, c.Slug, n); err != nil {
			return err
		}
		if !r.g.JSON {
			fmt.Fprintf(r.stdout, "deleted version %d of %s\n", n, c.Slug)
		}
	}
	if r.g.JSON {
		return r.printJSON(api.DeletedVersions{Deleted: slices.Sorted(slices.Values(numbers)), Artifact: last.Artifact})
	}
	return nil
}

// VersionsPruneCmd deletes all but the newest versions.
type VersionsPruneCmd struct {
	Slug   string `arg:"" help:"Artifact slug."`
	Keep   int    `required:"" help:"How many of the newest versions to keep; the published one and staged ones newer than it are kept too." placeholder:"N"`
	DryRun bool   `name:"dry-run" help:"List the versions it would delete, and delete nothing."`
}

func (c *VersionsPruneCmd) Run(r *runner) error {
	if c.Keep < 1 {
		return usagef("versions prune: --keep must be at least 1")
	}
	cl, err := r.userClient()
	if err != nil {
		return err
	}
	res, err := cl.PruneVersions(r.ctx, c.Slug, c.Keep, c.DryRun)
	if err != nil {
		return err
	}
	if r.g.JSON {
		return r.printJSON(res)
	}
	parts := make([]string, len(res.Deleted))
	for i, n := range res.Deleted {
		parts[i] = strconv.Itoa(n)
	}
	switch {
	case len(parts) == 0:
		fmt.Fprintf(r.stdout, "nothing to delete: %s keeps every version it has\n", c.Slug)
	case c.DryRun:
		fmt.Fprintf(r.stdout, "would delete versions %s of %s\n", strings.Join(parts, ", "), c.Slug)
	default:
		fmt.Fprintf(r.stdout, "deleted versions %s of %s\n", strings.Join(parts, ", "), c.Slug)
	}
	return nil
}

// RestoreCmd takes a soft-deleted artifact out of the bin.
type RestoreCmd struct {
	Slug string `arg:"" help:"Artifact slug."`
}

func (c *RestoreCmd) Run(r *runner) error {
	cl, err := r.userClient()
	if err != nil {
		return err
	}
	res, err := cl.Patch(r.ctx, c.Slug, api.UpdateArtifactRequest{Restore: true})
	if err != nil {
		return err
	}
	if r.g.JSON {
		return r.printJSON(res)
	}
	fmt.Fprintf(r.stdout, "restored %s: %s\n", c.Slug, res.URL)
	return nil
}

// RollbackCmd publishes an earlier version.
type RollbackCmd struct {
	Slug    string `arg:"" help:"Artifact slug."`
	Version string `arg:"" help:"Version number to publish."`
}

func (c *RollbackCmd) Run(r *runner) error {
	v, err := strconv.Atoi(c.Version)
	if err != nil || v < 1 {
		return usagef("rollback: version must be a positive integer, got %q", c.Version)
	}
	cl, err := r.userClient()
	if err != nil {
		return err
	}
	res, err := cl.Publish(r.ctx, c.Slug, v)
	if err != nil {
		return err
	}
	if r.g.JSON {
		return r.printJSON(res)
	}
	fmt.Fprintf(r.stdout, "%s now serves version %d: %s\n", c.Slug, res.Version, res.URL)
	if res.Artifact.LatestVersion > res.Version {
		fmt.Fprintf(r.stderr, "the next change needs --base: %d to build on this version, %d to build on the latest\n",
			res.Version, res.Artifact.LatestVersion)
	}
	return nil
}

// EditCmd changes text in one file.
type EditCmd struct {
	Slug      string  `arg:"" help:"Artifact slug."`
	Path      string  `arg:"" help:"File to edit."`
	Old       string  `help:"Exact text to replace." placeholder:"TEXT"`
	New       *string `help:"Replacement text; empty deletes." placeholder:"TEXT"`
	All       bool    `help:"Replace every match of --old."`
	Start     string  `help:"Anchor of the first line to replace, from files cat --anchors." placeholder:"ANCHOR"`
	End       string  `help:"Anchor of the last line to replace (default: --start)." placeholder:"ANCHOR"`
	After     string  `help:"Insert --new as lines after the line with this anchor." placeholder:"ANCHOR"`
	Before    string  `help:"Insert --new as lines before the line with this anchor." placeholder:"ANCHOR"`
	Edits     string  `help:"Read a JSON array of edits from FILE (- for stdin) instead of the flags above." placeholder:"FILE"`
	Label     string  `help:"Version label." placeholder:"LABEL"`
	Expected  int     `help:"Fail unless the latest version is N." placeholder:"N"`
	Base      int     `help:"The version whose file to edit; needed when an earlier version is published." placeholder:"N"`
	KeepStale bool    `name:"keep-stale" help:"Keep the stale mark."`
	Stage     bool    `help:"Add the version without publishing it, for review at its version URL; kazari rollback SLUG N publishes it."`
}

func (c *EditCmd) Run(r *runner) error {
	var edits []api.Edit
	var err error
	single := c.Old != "" || c.New != nil || c.Start != "" || c.End != "" || c.After != "" || c.Before != "" || c.All
	switch {
	case c.Edits != "" && single:
		return usagef("edit: --edits replaces --old, --new, --all, --start, --end, --after and --before")
	case c.Edits != "":
		if edits, err = r.readEdits(c.Edits); err != nil {
			return err
		}
	case c.Old == "" && c.Start == "" && c.After == "" && c.Before == "":
		return usagef("edit: give --old, --start, --after or --before, with --new, or --edits FILE")
	case c.New == nil:
		return usagef("edit: --new is required; --new \"\" deletes")
	default:
		edits = []api.Edit{{OldText: c.Old, NewText: *c.New, ReplaceAll: c.All, Start: c.Start, End: c.End,
			After: c.After, Before: c.Before}}
	}
	cl, err := r.userClient()
	if err != nil {
		return err
	}
	res, err := cl.Edit(r.ctx, c.Slug, api.EditRequest{
		Path: c.Path, Edits: edits, Label: c.Label, ExpectedVersion: c.Expected, BaseVersion: c.Base, KeepStale: c.KeepStale,
		Stage: c.Stage,
	})
	if err != nil {
		return r.baseHint(err, cl, c.Slug)
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(termSafe{r.stderr}, "warning: %s\n", w)
	}
	if r.g.JSON {
		return r.printJSON(res)
	}
	fmt.Fprintln(termSafe{r.stdout}, versionLine(c.Slug, res.Version, res.URL, res.VersionURL, res.Staged,
		res.Artifact.PublishedVersion))
	for i, e := range res.Edits {
		where := fmt.Sprintf("lines %d-%d", e.StartLine, e.EndLine)
		if e.EndLine < e.StartLine {
			where = fmt.Sprintf("removed at line %d", e.StartLine)
		}
		fmt.Fprintf(r.stdout, "edit %d: %s (%s", i+1, where, e.Match)
		if e.Replacements > 1 {
			fmt.Fprintf(r.stdout, ", %d places", e.Replacements)
		}
		fmt.Fprintln(r.stdout, ")")
	}
	return nil
}

func (r *runner) readEdits(name string) ([]api.Edit, error) {
	var (
		data []byte
		err  error
	)
	if name == "-" {
		data, err = io.ReadAll(r.stdin)
	} else {
		data, err = os.ReadFile(name)
	}
	if err != nil {
		return nil, fmt.Errorf("edit: read edits: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var edits []api.Edit
	if err := dec.Decode(&edits); err != nil {
		return nil, usagef("edit: --edits must be a JSON array of {old_text, new_text, replace_all} or {start, end, new_text}: %v", err)
	}
	if len(edits) == 0 {
		return nil, usagef("edit: --edits holds no edits")
	}
	return edits, nil
}

// FilesCmd lists and reads files.
type FilesCmd struct {
	Ls  FilesLsCmd  `cmd:"" help:"List the files of a version."`
	Cat FilesCatCmd `cmd:"" help:"Write a file to stdout."`
}

// FilesLsCmd lists one version's files.
type FilesLsCmd struct {
	Slug    string `arg:"" help:"Artifact slug."`
	Version int    `help:"List version N instead of the published one." placeholder:"N"`
}

func (c *FilesLsCmd) Run(r *runner) error {
	if c.Version < 0 {
		return usagef("files ls: --version must be a positive integer")
	}
	cl, err := r.userClient()
	if err != nil {
		return err
	}
	_, v, err := cl.VersionFiles(r.ctx, c.Slug, c.Version)
	if err != nil {
		return r.nearSlug(err, cl, c.Slug)
	}
	if r.g.JSON {
		return r.printJSON(v.Files)
	}
	tw := newTable(r.stdout)
	tw.row("PATH", "SIZE", "TYPE", "HASH")
	for _, f := range v.Files {
		tw.row(f.Path, artifact.FormatBytes(f.Size), f.ContentType, shortHash(f.Hash))
	}
	return tw.flush()
}

// FilesCatCmd writes one file to stdout.
type FilesCatCmd struct {
	Slug    string `arg:"" help:"Artifact slug."`
	Path    string `arg:"" help:"File path within the artifact."`
	Version int    `help:"Read from version N instead of the published one." placeholder:"N"`
	Anchors bool   `help:"Prefix each line with its anchor, for kazari edit --start and --end."`
}

func (c *FilesCatCmd) Run(r *runner) error {
	cl, err := r.userClient()
	if err != nil {
		return err
	}
	body, _, err := cl.ReadFile(r.ctx, c.Slug, c.Path, c.Version, c.Anchors)
	if err != nil {
		return err
	}
	defer body.Close()
	if _, err := io.Copy(r.stdout, body); err != nil {
		return fmt.Errorf("read %s: %w", c.Path, err)
	}
	return nil
}

// SkillCmd prints the instance's skill.
type SkillCmd struct{}

func (c *SkillCmd) Run(r *runner) error {
	cl, err := r.userClient()
	if err != nil {
		return err
	}
	md, err := cl.Skill(r.ctx)
	if err != nil {
		return err
	}
	_, err = io.WriteString(r.stdout, md)
	return err
}

// MeCmd shows the caller.
type MeCmd struct{}

func (c *MeCmd) Run(r *runner) error {
	cl, err := r.userClient()
	if err != nil {
		return err
	}
	me, err := cl.Me(r.ctx)
	if err != nil {
		return err
	}
	if r.g.JSON {
		return r.printJSON(me)
	}
	tw := newTable(r.stdout)
	tw.row("handle:", me.Handle)
	switch {
	case me.Role == "":
		tw.row("admin:", me.Admin)
	case me.Role == "admin" && !me.Admin:
		tw.row("role:", "admin (this key has no admin scope)")
	default:
		tw.row("role:", me.Role)
	}
	tw.row("scopes:", dash(strings.Join(me.Scopes, ", ")))
	tw.row("source:", dash(me.Source))
	if len(me.Slugs) > 0 {
		tw.row("slugs:", strings.Join(me.Slugs, ", "))
	}
	if me.ExpiresAt != nil {
		tw.row("expires:", fmtTime(*me.ExpiresAt))
	}
	tw.row("storage:", fmtQuota(me.UsedBytes, me.QuotaBytes))
	return tw.flush()
}

// KeysCmd manages the caller's own keys. Without a subcommand it lists them.
type KeysCmd struct {
	List   KeysListCmd   `cmd:"" default:"1" help:"List the caller's keys."`
	Issue  KeysIssueCmd  `cmd:"" help:"Issue a key no wider than the one in use; the secret is printed once."`
	Rotate KeysRotateCmd `cmd:"" help:"Replace a key with a new one; the secret is printed once."`
	Revoke KeysRevokeCmd `cmd:"" help:"Revoke a key by its prefix, with the keys issued with it."`
}

// KeysListCmd lists the caller's keys.
type KeysListCmd struct{}

func (c *KeysListCmd) Run(r *runner) error {
	cl, err := r.userClient()
	if err != nil {
		return err
	}
	keys, err := cl.MyKeys(r.ctx)
	if err != nil {
		return err
	}
	return r.printKeys(keys)
}

// keyFlags describe a new key, for keys issue and admin keys issue.
type keyFlags struct {
	Label   string `help:"Key label." placeholder:"LABEL"`
	Scopes  string `help:"Comma-separated scopes (default: artifacts:read,artifacts:write,artifacts:share,artifacts:delete)." placeholder:"A,B"`
	Slugs   string `help:"Comma-separated slug patterns the key is restricted to: an exact slug, or a prefix ending in *, such as 'reports-*' (default: no restriction)." placeholder:"A,B"`
	Expires string `help:"Lifetime, such as 720h or 90d (default: no expiry, or the server's limit)." placeholder:"DURATION"`
}

func (f *keyFlags) request() api.IssueKeyRequest {
	return api.IssueKeyRequest{Label: f.Label, Scopes: splitList(f.Scopes), Slugs: splitList(f.Slugs), ExpiresIn: f.Expires}
}

func splitList(s string) []string {
	var out []string
	for item := range strings.SplitSeq(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// printIssued prints the key string alone on stdout, so it can be piped
// into a secret store, and what it is on stderr.
func (r *runner) printIssued(k *api.IssuedKey, note string) error {
	if r.g.JSON {
		return r.printJSON(k)
	}
	fmt.Fprintln(r.stdout, k.Key)
	fmt.Fprintf(termSafe{r.stderr}, "%s; the key is shown only once\n", note)
	return nil
}

func keyNote(k api.APIKey) string {
	note := "scopes " + strings.Join(k.Scopes, ",")
	if len(k.Slugs) > 0 {
		note += ", restricted to " + strings.Join(k.Slugs, ",")
	}
	if k.ExpiresAt != nil {
		note += ", expires " + fmtTime(*k.ExpiresAt)
	}
	return note
}

func rotatedNote(k *api.IssuedKey) string {
	note := fmt.Sprintf("issued key %s (%s)", k.APIKey.Prefix, keyNote(k.APIKey))
	if old := k.Replaces; old != nil && old.ExpiresAt != nil {
		note += fmt.Sprintf(" in place of %s, which works until %s", old.Prefix, fmtTime(*old.ExpiresAt))
	}
	return note
}

// KeysIssueCmd issues a key for the caller.
type KeysIssueCmd struct {
	keyFlags
}

func (c *KeysIssueCmd) Run(r *runner) error {
	cl, err := r.userClient()
	if err != nil {
		return err
	}
	k, err := cl.IssueMyKey(r.ctx, c.request())
	if err != nil {
		return err
	}
	return r.printIssued(k, fmt.Sprintf("issued key %s (%s)", k.APIKey.Prefix, keyNote(k.APIKey)))
}

// KeysRotateCmd replaces one of the caller's keys.
type KeysRotateCmd struct {
	Prefix  string `arg:"" help:"The key's prefix, from keys list."`
	Overlap string `help:"How long the old key keeps working, such as 24h (the default), 7d (the most) or 0." placeholder:"DURATION"`
}

func (c *KeysRotateCmd) Run(r *runner) error {
	cl, err := r.userClient()
	if err != nil {
		return err
	}
	k, err := cl.RotateMyKey(r.ctx, c.Prefix, c.Overlap)
	if err != nil {
		return err
	}
	return r.printIssued(k, rotatedNote(k))
}

// KeysRevokeCmd revokes one of the caller's keys.
type KeysRevokeCmd struct {
	Prefix string `arg:"" help:"The key's prefix, from keys list."`
}

func (c *KeysRevokeCmd) Run(r *runner) error {
	cl, err := r.userClient()
	if err != nil {
		return err
	}
	if err := cl.RevokeMyKey(r.ctx, c.Prefix); err != nil {
		return err
	}
	if r.g.JSON {
		return r.printJSON(api.OK{OK: true})
	}
	fmt.Fprintf(r.stdout, "revoked %s\n", c.Prefix)
	return nil
}

func (r *runner) printKeys(keys []api.APIKey) error {
	if r.g.JSON {
		return r.printJSON(keys)
	}
	tw := newTable(r.stdout)
	tw.row("PREFIX", "LABEL", "SCOPES", "SLUGS", "ISSUED BY", "CREATED", "LAST USED", "EXPIRES", "REVOKED")
	for _, k := range keys {
		tw.row(k.Prefix, dash(k.Label), dash(strings.Join(k.Scopes, ",")), dash(strings.Join(k.Slugs, ",")), dash(k.IssuedBy), fmtTime(k.CreatedAt), fmtTimePtr(k.LastUsedAt), fmtTimePtr(k.ExpiresAt), fmtTimePtr(k.RevokedAt))
	}
	return tw.flush()
}

func fmtMap(m map[string]string) string {
	keys := slices.Sorted(maps.Keys(m))
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + m[k]
	}
	return strings.Join(parts, ", ")
}
