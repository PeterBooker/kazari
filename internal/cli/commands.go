package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/PeterBooker/kazari/internal/api"
)

// PublishCmd creates an artifact from a directory.
type PublishCmd struct {
	Dir         string            `arg:"" help:"Directory to publish."`
	Slug        string            `help:"Artifact slug (required)." placeholder:"SLUG"`
	Name        *string           `help:"Display name." placeholder:"NAME"`
	Description *string           `help:"Description." placeholder:"TEXT"`
	Label       string            `help:"Version label." placeholder:"LABEL"`
	Visibility  string            `help:"public, unlisted or members (default: the instance's)." placeholder:"VISIBILITY"`
	Meta        map[string]string `help:"Metadata key=value (repeatable)." mapsep:"none" placeholder:"KEY=VALUE"`
	Update      bool              `help:"If the slug exists, add a version that mirrors the directory (files missing from it are deleted)."`
}

func (c *PublishCmd) Run(r *runner) error {
	if c.Slug == "" {
		return usagef("publish: --slug is required")
	}
	switch c.Visibility {
	case "", "public", "unlisted", "members":
	default:
		return usagef("publish: --visibility must be public, unlisted or members")
	}
	files, err := bundleFiles(c.Dir)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("publish: no files to publish in %s", c.Dir)
	}
	cl, err := r.userClient()
	if err != nil {
		return err
	}

	create := func() (*api.PublishResult, error) {
		bundle := TarGzReader(c.Dir)
		defer bundle.Close()
		return cl.CreateBundle(r.ctx, api.CreateManifest{
			Slug: c.Slug, Name: deref(c.Name), Description: deref(c.Description), Visibility: c.Visibility,
			Metadata: nilIfEmpty(c.Meta), Label: c.Label,
		}, bundle)
	}
	updateExisting := func(cur *api.ArtifactDetail) (*api.PublishResult, error) {
		m := api.UpdateArtifactRequest{Label: c.Label, ExpectedVersion: cur.Artifact.LatestVersion, Name: c.Name, Description: c.Description}
		if c.Visibility != "" {
			m.Visibility = &c.Visibility
		}
		m.Metadata = metaPatch(c.Meta)
		for _, p := range removedFiles(cur, files) {
			m.Files = append(m.Files, api.FileInput{Path: p, Delete: true})
		}
		bundle := TarGzReader(c.Dir)
		defer bundle.Close()
		return cl.UpdateBundle(r.ctx, c.Slug, m, bundle)
	}

	var res *api.PublishResult
	if !c.Update {
		res, err = create()
	} else {
		var cur *api.ArtifactDetail
		cur, err = cl.Get(r.ctx, c.Slug, true)
		switch {
		case IsCode(err, api.CodeNotFound):
			res, err = create()
			if IsCode(err, api.CodeSlugTaken) { // created concurrently
				if cur, err = cl.Get(r.ctx, c.Slug, true); err == nil {
					res, err = updateExisting(cur)
				}
			}
		case err == nil:
			res, err = updateExisting(cur)
		}
	}
	if err != nil {
		return err
	}
	return r.printResult(res)
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

func removedFiles(cur *api.ArtifactDetail, local []string) []string {
	var out []string
	for _, v := range cur.Versions {
		if v.Number != cur.Artifact.LatestVersion {
			continue
		}
		for _, f := range v.Files {
			if _, found := slices.BinarySearch(local, f.Path); !found {
				out = append(out, f.Path)
			}
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

func (r *runner) printResult(res *api.PublishResult) error {
	if r.g.JSON {
		return r.printJSON(res)
	}
	fmt.Fprintln(r.stdout, res.URL)
	return nil
}

// UpdateCmd adds a version, changes fields, or both.
type UpdateCmd struct {
	Slug        string            `arg:"" help:"Artifact slug."`
	Dir         string            `arg:"" optional:"" help:"Directory whose files are added, replacing those at the same paths."`
	Label       string            `help:"Version label." placeholder:"LABEL"`
	Expected    int               `help:"Fail unless the latest version is N." placeholder:"N"`
	KeepStale   bool              `name:"keep-stale" help:"Keep the stale mark."`
	Name        *string           `help:"Display name." placeholder:"NAME"`
	Description *string           `help:"Description." placeholder:"TEXT"`
	Meta        map[string]string `help:"Metadata key=value (repeatable)." mapsep:"none" placeholder:"KEY=VALUE"`
	Delete      []string          `help:"Delete the file at PATH (repeatable)." sep:"none" placeholder:"PATH"`
}

func (c *UpdateCmd) Run(r *runner) error {
	m := api.UpdateArtifactRequest{Label: c.Label, ExpectedVersion: c.Expected, KeepStale: c.KeepStale, Name: c.Name, Description: c.Description}
	m.Metadata = metaPatch(c.Meta)
	for _, p := range c.Delete {
		m.Files = append(m.Files, api.FileInput{Path: p, Delete: true})
	}
	if c.Dir == "" && m.Name == nil && m.Description == nil && m.Metadata == nil && len(m.Files) == 0 {
		return usagef("update: nothing to update: give a directory, --delete, --name, --description or --meta")
	}
	if c.Dir != "" {
		if _, err := bundleFiles(c.Dir); err != nil {
			return err
		}
	}
	cl, err := r.userClient()
	if err != nil {
		return err
	}
	var res *api.PublishResult
	if c.Dir != "" {
		bundle := TarGzReader(c.Dir)
		defer bundle.Close()
		res, err = cl.UpdateBundle(r.ctx, c.Slug, m, bundle)
	} else {
		res, err = cl.Update(r.ctx, c.Slug, m)
	}
	if err != nil {
		return err
	}
	return r.printResult(res)
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
		return err
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
	fmt.Fprintln(tw, "VERSION\tLABEL\tCREATED BY\tCREATED\tSIZE\tURL")
	for _, v := range d.Versions {
		num := strconv.Itoa(v.Number)
		if v.Number == d.Artifact.PublishedVersion {
			num += " *"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", num, dash(oneLine(v.Label)), dash(v.CreatedBy),
			fmtTime(v.CreatedAt), humanSize(v.SizeBytes), v.URL)
	}
	tw.Flush()
	if !c.Files {
		return nil
	}
	for _, v := range d.Versions {
		fmt.Fprintf(r.stdout, "\nversion %d files:\n", v.Number)
		tw := newTable(r.stdout)
		fmt.Fprintln(tw, "PATH\tSIZE\tTYPE\tHASH")
		for _, f := range v.Files {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", f.Path, humanSize(f.Size), f.ContentType, shortHash(f.Hash))
		}
		tw.Flush()
	}
	return nil
}

func (r *runner) printArtifact(a *api.Artifact) {
	tw := newTable(r.stdout)
	kv := func(k, v string) {
		if v != "" {
			fmt.Fprintf(tw, "%s:\t%s\n", k, oneLine(v))
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
	kv("size", humanSize(a.SizeBytes))
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
	tw.Flush()
}

// listFlags are the filters of list and admin artifacts list.
type listFlags struct {
	Mine    string            `help:"created, updated or any." placeholder:"WHICH"`
	Meta    map[string]string `help:"Metadata filter key=value (repeatable)." mapsep:"none" placeholder:"KEY=VALUE"`
	Stale   string            `help:"include, exclude or only." placeholder:"WHICH"`
	Deleted string            `help:"include, exclude or only." placeholder:"WHICH"`
	Cursor  string            `help:"Page cursor from a previous list." placeholder:"CURSOR"`
	Limit   int               `help:"Page size." placeholder:"N"`
	All     bool              `help:"Follow cursors and print every page."`
}

func (f *listFlags) params() ListParams {
	return ListParams{Mine: f.Mine, Metadata: nilIfEmpty(f.Meta), Stale: f.Stale, Deleted: f.Deleted, Cursor: f.Cursor, Limit: f.Limit}
}

func (r *runner) fetchList(f *listFlags, list func(ListParams) (*api.ArtifactList, error)) (*api.ArtifactList, error) {
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
	l, err := r.fetchList(&c.listFlags, func(p ListParams) (*api.ArtifactList, error) { return cl.List(r.ctx, p) })
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
	if owner {
		fmt.Fprint(tw, "OWNER\t")
	}
	fmt.Fprintln(tw, "SLUG\tVERSION\tVISIBILITY\tSTATE\tSIZE\tUPDATED\tNAME")
	for _, a := range l.Items {
		if owner {
			fmt.Fprintf(tw, "%s\t", a.Owner)
		}
		ver := strconv.Itoa(a.PublishedVersion)
		if a.LatestVersion != a.PublishedVersion {
			ver += "/" + strconv.Itoa(a.LatestVersion)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", a.Slug, ver, a.Visibility, artifactState(&a),
			humanSize(a.SizeBytes), fmtTime(a.UpdatedAt), oneLine(a.Name))
	}
	tw.Flush()
	if l.NextCursor != "" {
		fmt.Fprintf(r.stderr, "more results: --cursor %s (or --all)\n", l.NextCursor)
	}
	return nil
}

// RmCmd deletes an artifact.
type RmCmd struct {
	Slug string `arg:"" help:"Artifact slug."`
	Hard bool   `help:"Purge now instead of a soft delete (needs artifacts:delete)."`
}

func (c *RmCmd) Run(r *runner) error {
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

// StaleCmd marks an artifact stale.
type StaleCmd struct {
	Slug  string `arg:"" help:"Artifact slug."`
	Grace string `help:"Time until expiry, a Go duration such as 72h (default: the server's)." placeholder:"DURATION"`
}

func (c *StaleCmd) Run(r *runner) error {
	if c.Grace != "" {
		if _, err := time.ParseDuration(c.Grace); err != nil {
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
	Visibility string `arg:"" help:"public, unlisted, members or unpublished."`
}

func (c *VisibilityCmd) Run(r *runner) error {
	switch c.Visibility {
	case "public", "unlisted", "members", "unpublished":
	default:
		return usagef("visibility: want public, unlisted, members or unpublished, got %q", c.Visibility)
	}
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
}

func (c *ShareCreateCmd) Run(r *runner) error {
	cl, err := r.userClient()
	if err != nil {
		return err
	}
	res, err := cl.CreateShareLink(r.ctx, c.Slug, c.Label, c.Expires)
	if err != nil {
		return err
	}
	if r.g.JSON {
		return r.printJSON(res)
	}
	fmt.Fprintln(r.stdout, res.URL)
	fmt.Fprintf(r.stderr, "share link %s opens %s until %s; anyone with the URL can open it\n",
		res.ShareLink.Prefix, c.Slug, fmtTime(res.ShareLink.ExpiresAt))
	if res.Artifact.Visibility != "members" {
		fmt.Fprintf(r.stderr, "note: %s is %s, so it opens without the link too (kazari visibility %s members)\n",
			c.Slug, res.Artifact.Visibility, c.Slug)
	}
	return nil
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
	fmt.Fprintln(tw, "PREFIX\tLABEL\tCREATED BY\tCREATED\tEXPIRES\tUSES\tLAST USED\tSTATE")
	for _, l := range list.Items {
		state := "active"
		switch {
		case l.RevokedAt != nil:
			state = "revoked"
		case !l.Active:
			state = "expired"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n", l.Prefix, dash(oneLine(l.Label)), l.CreatedBy,
			fmtTime(l.CreatedAt), fmtTime(l.ExpiresAt), l.Uses, fmtTimePtr(l.LastUsedAt), state)
	}
	return tw.Flush()
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
	Edits     string  `help:"Read a JSON array of edits from FILE (- for stdin) instead of the flags above." placeholder:"FILE"`
	Label     string  `help:"Version label." placeholder:"LABEL"`
	Expected  int     `help:"Fail unless the latest version is N." placeholder:"N"`
	KeepStale bool    `name:"keep-stale" help:"Keep the stale mark."`
}

func (c *EditCmd) Run(r *runner) error {
	var edits []api.Edit
	var err error
	single := c.Old != "" || c.New != nil || c.Start != "" || c.End != "" || c.All
	switch {
	case c.Edits != "" && single:
		return usagef("edit: --edits replaces --old, --new, --all, --start and --end")
	case c.Edits != "":
		if edits, err = r.readEdits(c.Edits); err != nil {
			return err
		}
	case c.Old == "" && c.Start == "":
		return usagef("edit: give --old or --start, with --new, or --edits FILE")
	case c.New == nil:
		return usagef("edit: --new is required; --new \"\" deletes")
	default:
		edits = []api.Edit{{OldText: c.Old, NewText: *c.New, ReplaceAll: c.All, Start: c.Start, End: c.End}}
	}
	cl, err := r.userClient()
	if err != nil {
		return err
	}
	res, err := cl.Edit(r.ctx, c.Slug, api.EditRequest{
		Path: c.Path, Edits: edits, Label: c.Label, ExpectedVersion: c.Expected, KeepStale: c.KeepStale,
	})
	if err != nil {
		return err
	}
	if r.g.JSON {
		return r.printJSON(res)
	}
	fmt.Fprintln(r.stdout, res.URL)
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

// FilesCmd reads files.
type FilesCmd struct {
	Cat FilesCatCmd `cmd:"" help:"Write a file to stdout."`
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
	fmt.Fprintf(tw, "handle:\t%s\n", me.Handle)
	fmt.Fprintf(tw, "admin:\t%t\n", me.Admin)
	fmt.Fprintf(tw, "scopes:\t%s\n", dash(strings.Join(me.Scopes, ", ")))
	fmt.Fprintf(tw, "source:\t%s\n", dash(me.Source))
	fmt.Fprintf(tw, "storage:\t%s\n", fmtQuota(me.UsedBytes, me.QuotaBytes))
	return tw.Flush()
}

// KeysCmd lists the caller's keys.
type KeysCmd struct{}

func (c *KeysCmd) Run(r *runner) error {
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

func (r *runner) printKeys(keys []api.APIKey) error {
	if r.g.JSON {
		return r.printJSON(keys)
	}
	tw := newTable(r.stdout)
	fmt.Fprintln(tw, "PREFIX\tLABEL\tSCOPES\tCREATED\tLAST USED\tEXPIRES\tREVOKED")
	for _, k := range keys {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", k.Prefix, dash(oneLine(k.Label)), dash(strings.Join(k.Scopes, ",")),
			fmtTime(k.CreatedAt), fmtTimePtr(k.LastUsedAt), fmtTimePtr(k.ExpiresAt), fmtTimePtr(k.RevokedAt))
	}
	return tw.Flush()
}

func fmtMap(m map[string]string) string {
	keys := slices.Sorted(maps.Keys(m))
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + m[k]
	}
	return strings.Join(parts, ", ")
}
