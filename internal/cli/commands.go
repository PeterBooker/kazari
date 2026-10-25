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

	"kazari/internal/api"
	"kazari/internal/buildinfo"
)

type commandFunc func(r *runner, args []string) error

func commands() map[string]commandFunc {
	return map[string]commandFunc{
		"publish":    cmdPublish,
		"update":     cmdUpdate,
		"get":        cmdGet,
		"list":       cmdList,
		"rm":         cmdRm,
		"stale":      cmdStale,
		"unstale":    cmdUnstale,
		"visibility": cmdVisibility,
		"share":      cmdShare,
		"rollback":   cmdRollback,
		"edit":       cmdEdit,
		"files":      cmdFiles,
		"skill":      cmdSkill,
		"me":         cmdMe,
		"keys":       cmdKeys,
		"version":    cmdVersion,
		"admin":      cmdAdmin,
	}
}

func cmdPublish(r *runner, args []string) error {
	fs := r.newFlags("publish", "publish <dir> --slug <slug> [flags]")
	slug := fs.String("slug", "", "artifact slug (required)")
	name := fs.String("name", "", "display name")
	desc := fs.String("description", "", "description")
	label := fs.String("label", "", "version label")
	vis := fs.String("visibility", "", "public, unlisted or members (default: the instance's)")
	meta := metaFlag{}
	fs.Var(meta, "meta", "metadata `key=value` (repeatable)")
	update := fs.Bool("update", false, "if the slug exists, add a version that mirrors dir (files missing from dir are deleted)")
	pos, err := parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	if *slug == "" {
		return usagef("publish: --slug is required")
	}
	switch *vis {
	case "", "public", "unlisted", "members":
	default:
		return usagef("publish: --visibility must be public, unlisted or members")
	}
	dir := pos[0]
	files, err := bundleFiles(dir)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("publish: no files to publish in %s", dir)
	}
	c, err := r.userClient()
	if err != nil {
		return err
	}

	create := func() (*api.PublishResult, error) {
		bundle := TarGzReader(dir)
		defer bundle.Close()
		return c.CreateBundle(r.ctx, api.CreateManifest{
			Slug: *slug, Name: *name, Description: *desc, Visibility: *vis,
			Metadata: nilIfEmpty(meta), Label: *label,
		}, bundle)
	}
	updateExisting := func(cur *api.ArtifactDetail) (*api.PublishResult, error) {
		m := api.UpdateArtifactRequest{Label: *label, ExpectedVersion: cur.Artifact.LatestVersion}
		if flagSet(fs, "name") {
			m.Name = name
		}
		if flagSet(fs, "description") {
			m.Description = desc
		}
		if *vis != "" {
			m.Visibility = vis
		}
		if len(meta) > 0 {
			m.Metadata = map[string]*string{}
			for k, v := range meta {
				m.Metadata[k] = &v
			}
		}
		for _, p := range removedFiles(cur, files) {
			m.Files = append(m.Files, api.FileInput{Path: p, Delete: true})
		}
		bundle := TarGzReader(dir)
		defer bundle.Close()
		return c.UpdateBundle(r.ctx, *slug, m, bundle)
	}

	var res *api.PublishResult
	if !*update {
		res, err = create()
	} else {
		var cur *api.ArtifactDetail
		cur, err = c.Get(r.ctx, *slug, true)
		switch {
		case IsCode(err, api.CodeNotFound):
			res, err = create()
			if IsCode(err, api.CodeSlugTaken) { // created concurrently
				if cur, err = c.Get(r.ctx, *slug, true); err == nil {
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
	if r.g.json {
		return r.printJSON(res)
	}
	fmt.Fprintln(r.stdout, res.URL)
	return nil
}

func cmdUpdate(r *runner, args []string) error {
	fs := r.newFlags("update", "update <slug> [dir] [flags]")
	label := fs.String("label", "", "version label")
	expected := fs.Int("expected", 0, "fail unless the latest version is `N`")
	keepStale := fs.Bool("keep-stale", false, "keep the stale mark")
	name := fs.String("name", "", "display name")
	desc := fs.String("description", "", "description")
	meta := metaFlag{}
	fs.Var(meta, "meta", "metadata `key=value` (repeatable)")
	var deletes listFlag
	fs.Var(&deletes, "delete", "delete file `path` (repeatable)")
	pos, err := parse(fs, args, 1, 2)
	if err != nil {
		return err
	}
	m := api.UpdateArtifactRequest{Label: *label, ExpectedVersion: *expected, KeepStale: *keepStale}
	if flagSet(fs, "name") {
		m.Name = name
	}
	if flagSet(fs, "description") {
		m.Description = desc
	}
	if len(meta) > 0 {
		m.Metadata = map[string]*string{}
		for k, v := range meta {
			m.Metadata[k] = &v
		}
	}
	for _, p := range deletes {
		m.Files = append(m.Files, api.FileInput{Path: p, Delete: true})
	}
	if len(pos) == 1 && m.Name == nil && m.Description == nil && m.Metadata == nil && len(m.Files) == 0 {
		return usagef("update: nothing to update: give a directory, --delete, --name, --description or --meta")
	}
	slug := pos[0]
	if len(pos) == 2 {
		if _, err := bundleFiles(pos[1]); err != nil {
			return err
		}
	}
	c, err := r.userClient()
	if err != nil {
		return err
	}
	var res *api.PublishResult
	if len(pos) == 2 {
		bundle := TarGzReader(pos[1])
		defer bundle.Close()
		res, err = c.UpdateBundle(r.ctx, slug, m, bundle)
	} else {
		res, err = c.Update(r.ctx, slug, m)
	}
	if err != nil {
		return err
	}
	return r.printResult(res)
}

func cmdGet(r *runner, args []string) error {
	fs := r.newFlags("get", "get <slug> [--files]")
	files := fs.Bool("files", false, "include each version's files")
	pos, err := parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	c, err := r.userClient()
	if err != nil {
		return err
	}
	d, err := c.Get(r.ctx, pos[0], *files)
	if err != nil {
		return err
	}
	if r.g.json {
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
	if !*files {
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

type listOptions struct {
	params ListParams
	meta   metaFlag
	all    *bool
}

func addListFlags(r *runner, name, usage string) (*listOptions, func([]string) error) {
	fs := r.newFlags(name, usage)
	o := &listOptions{meta: metaFlag{}}
	fs.StringVar(&o.params.Mine, "mine", "", "created, updated or any")
	fs.Var(o.meta, "meta", "metadata filter `key=value` (repeatable)")
	fs.StringVar(&o.params.Stale, "stale", "", "include, exclude or only")
	fs.StringVar(&o.params.Deleted, "deleted", "", "include, exclude or only")
	fs.StringVar(&o.params.Cursor, "cursor", "", "page cursor from a previous list")
	fs.IntVar(&o.params.Limit, "limit", 0, "page size")
	o.all = fs.Bool("all", false, "follow cursors and print every page")
	return o, func(args []string) error {
		if _, err := parse(fs, args, 0, 0); err != nil {
			return err
		}
		o.params.Metadata = nilIfEmpty(o.meta)
		return nil
	}
}

func (r *runner) fetchList(o *listOptions, list func(ListParams) (*api.ArtifactList, error)) (*api.ArtifactList, error) {
	p := o.params
	out, err := list(p)
	if err != nil || !*o.all {
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

func cmdList(r *runner, args []string) error {
	o, parseArgs := addListFlags(r, "list", "list [flags]")
	if err := parseArgs(args); err != nil {
		return err
	}
	c, err := r.userClient()
	if err != nil {
		return err
	}
	l, err := r.fetchList(o, func(p ListParams) (*api.ArtifactList, error) { return c.List(r.ctx, p) })
	if err != nil {
		return err
	}
	return r.printList(l, false)
}

func (r *runner) printList(l *api.ArtifactList, owner bool) error {
	if r.g.json {
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

func cmdRm(r *runner, args []string) error {
	fs := r.newFlags("rm", "rm <slug> [--hard]")
	hard := fs.Bool("hard", false, "purge now instead of a soft delete (needs artifacts:delete)")
	pos, err := parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	c, err := r.userClient()
	if err != nil {
		return err
	}
	if err := c.Delete(r.ctx, pos[0], *hard); err != nil {
		return err
	}
	if r.g.json {
		return r.printJSON(api.OK{OK: true})
	}
	verb := "deleted"
	if *hard {
		verb = "purged"
	}
	fmt.Fprintf(r.stdout, "%s %s\n", verb, pos[0])
	return nil
}

func cmdStale(r *runner, args []string) error {
	fs := r.newFlags("stale", "stale <slug> [--grace 336h]")
	grace := fs.String("grace", "", "time until expiry, a Go `duration` (server default if empty)")
	pos, err := parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	if *grace != "" {
		if _, err := time.ParseDuration(*grace); err != nil {
			return usagef("stale: --grace: %v", err)
		}
	}
	c, err := r.userClient()
	if err != nil {
		return err
	}
	res, err := c.MarkStale(r.ctx, pos[0], *grace)
	if err != nil {
		return err
	}
	if r.g.json {
		return r.printJSON(res)
	}
	fmt.Fprintf(r.stdout, "%s is stale since %s and expires at %s\n", pos[0], fmtTime(res.StaleAt), fmtTime(res.ExpiresAt))
	return nil
}

func cmdUnstale(r *runner, args []string) error {
	fs := r.newFlags("unstale", "unstale <slug>")
	pos, err := parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	c, err := r.userClient()
	if err != nil {
		return err
	}
	a, err := c.UnmarkStale(r.ctx, pos[0])
	if err != nil {
		return err
	}
	if r.g.json {
		return r.printJSON(a)
	}
	fmt.Fprintf(r.stdout, "%s is no longer stale\n", pos[0])
	return nil
}

func cmdVisibility(r *runner, args []string) error {
	fs := r.newFlags("visibility", "visibility <slug> <public|unlisted|members|unpublished>")
	pos, err := parse(fs, args, 2, 2)
	if err != nil {
		return err
	}
	switch pos[1] {
	case "public", "unlisted", "members", "unpublished":
	default:
		return usagef("visibility: want public, unlisted, members or unpublished, got %q", pos[1])
	}
	c, err := r.userClient()
	if err != nil {
		return err
	}
	res, err := c.SetVisibility(r.ctx, pos[0], pos[1])
	if err != nil {
		return err
	}
	if r.g.json {
		return r.printJSON(res)
	}
	fmt.Fprintf(r.stdout, "%s is now %s\n", pos[0], pos[1])
	return nil
}

const shareUsage = "share create <slug> [--label L] [--expires 7d] | share list <slug> | share revoke <slug> <prefix>"

func cmdShare(r *runner, args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprintln(r.stderr, "usage: kazari "+shareUsage)
		if len(args) == 0 {
			return errUsagePrinted
		}
		return nil
	}
	switch args[0] {
	case "create":
		return cmdShareCreate(r, args[1:])
	case "list":
		return cmdShareList(r, args[1:])
	case "revoke":
		return cmdShareRevoke(r, args[1:])
	}
	return usagef("share: unknown command %q, want create, list or revoke", args[0])
}

func cmdShareCreate(r *runner, args []string) error {
	fs := r.newFlags("share create", "share create <slug> [--label L] [--expires 7d]")
	label := fs.String("label", "", "note on who the link is for")
	expires := fs.String("expires", "", "how long the link works, e.g. 72h or 14d (default 7d)")
	pos, err := parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	c, err := r.userClient()
	if err != nil {
		return err
	}
	res, err := c.CreateShareLink(r.ctx, pos[0], *label, *expires)
	if err != nil {
		return err
	}
	if r.g.json {
		return r.printJSON(res)
	}
	fmt.Fprintln(r.stdout, res.URL)
	fmt.Fprintf(r.stderr, "share link %s opens %s until %s; anyone with the URL can open it\n",
		res.ShareLink.Prefix, pos[0], fmtTime(res.ShareLink.ExpiresAt))
	if res.Artifact.Visibility != "members" {
		fmt.Fprintf(r.stderr, "note: %s is %s, so it opens without the link too (kazari visibility %s members)\n",
			pos[0], res.Artifact.Visibility, pos[0])
	}
	return nil
}

func cmdShareList(r *runner, args []string) error {
	fs := r.newFlags("share list", "share list <slug>")
	pos, err := parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	c, err := r.userClient()
	if err != nil {
		return err
	}
	list, err := c.ListShareLinks(r.ctx, pos[0])
	if err != nil {
		return err
	}
	if r.g.json {
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

func cmdShareRevoke(r *runner, args []string) error {
	fs := r.newFlags("share revoke", "share revoke <slug> <prefix>")
	pos, err := parse(fs, args, 2, 2)
	if err != nil {
		return err
	}
	c, err := r.userClient()
	if err != nil {
		return err
	}
	if err := c.RevokeShareLink(r.ctx, pos[0], pos[1]); err != nil {
		return err
	}
	if r.g.json {
		return r.printJSON(api.OK{OK: true})
	}
	fmt.Fprintf(r.stdout, "revoked share link %s of %s\n", pos[1], pos[0])
	return nil
}

func cmdRollback(r *runner, args []string) error {
	fs := r.newFlags("rollback", "rollback <slug> <version>")
	pos, err := parse(fs, args, 2, 2)
	if err != nil {
		return err
	}
	v, err := strconv.Atoi(pos[1])
	if err != nil || v < 1 {
		return usagef("rollback: version must be a positive integer, got %q", pos[1])
	}
	c, err := r.userClient()
	if err != nil {
		return err
	}
	res, err := c.Publish(r.ctx, pos[0], v)
	if err != nil {
		return err
	}
	if r.g.json {
		return r.printJSON(res)
	}
	fmt.Fprintf(r.stdout, "%s now serves version %d: %s\n", pos[0], res.Version, res.URL)
	return nil
}

func cmdEdit(r *runner, args []string) error {
	fs := r.newFlags("edit", "edit <slug> <path> (--old TEXT | --start ANCHOR [--end ANCHOR]) --new TEXT [flags]\n"+
		"       kazari edit <slug> <path> --edits FILE [flags]")
	old := fs.String("old", "", "exact `text` to replace")
	newText := fs.String("new", "", "replacement `text`; empty deletes")
	all := fs.Bool("all", false, "replace every match of --old")
	start := fs.String("start", "", "`anchor` of the first line to replace (from files cat --anchors)")
	end := fs.String("end", "", "`anchor` of the last line to replace (default --start)")
	editsFile := fs.String("edits", "", "read a JSON array of edits from `file` (- for stdin)")
	label := fs.String("label", "", "version label")
	expected := fs.Int("expected", 0, "fail unless the latest version is `N`")
	keepStale := fs.Bool("keep-stale", false, "keep the stale mark")
	pos, err := parse(fs, args, 2, 2)
	if err != nil {
		return err
	}
	var edits []api.Edit
	single := flagSet(fs, "old") || flagSet(fs, "new") || flagSet(fs, "start") || flagSet(fs, "end") || *all
	switch {
	case *editsFile != "" && single:
		return usagef("edit: --edits replaces --old, --new, --all, --start and --end")
	case *editsFile != "":
		if edits, err = r.readEdits(*editsFile); err != nil {
			return err
		}
	case *old == "" && *start == "":
		return usagef("edit: give --old or --start, with --new, or --edits FILE")
	case !flagSet(fs, "new"):
		return usagef("edit: --new is required; --new \"\" deletes")
	default:
		edits = []api.Edit{{OldText: *old, NewText: *newText, ReplaceAll: *all, Start: *start, End: *end}}
	}
	c, err := r.userClient()
	if err != nil {
		return err
	}
	res, err := c.Edit(r.ctx, pos[0], api.EditRequest{
		Path: pos[1], Edits: edits, Label: *label, ExpectedVersion: *expected, KeepStale: *keepStale,
	})
	if err != nil {
		return err
	}
	if r.g.json {
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

func cmdFiles(r *runner, args []string) error {
	const usage = "files cat <slug> <path> [--version N] [--anchors]"
	if len(args) == 0 || args[0] != "cat" {
		if len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
			fmt.Fprintln(r.stderr, "usage: kazari "+usage)
			return nil
		}
		return usagef("usage: kazari " + usage)
	}
	fs := r.newFlags("files cat", usage)
	version := fs.Int("version", 0, "read from version `N` instead of the published one")
	anchors := fs.Bool("anchors", false, "prefix each line with its anchor, for kazari edit --start and --end")
	pos, err := parse(fs, args[1:], 2, 2)
	if err != nil {
		return err
	}
	c, err := r.userClient()
	if err != nil {
		return err
	}
	body, _, err := c.ReadFile(r.ctx, pos[0], pos[1], *version, *anchors)
	if err != nil {
		return err
	}
	defer body.Close()
	if _, err := io.Copy(r.stdout, body); err != nil {
		return fmt.Errorf("read %s: %w", pos[1], err)
	}
	return nil
}

func cmdSkill(r *runner, args []string) error {
	fs := r.newFlags("skill", "skill")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	c, err := r.userClient()
	if err != nil {
		return err
	}
	md, err := c.Skill(r.ctx)
	if err != nil {
		return err
	}
	_, err = io.WriteString(r.stdout, md)
	return err
}

func cmdMe(r *runner, args []string) error {
	fs := r.newFlags("me", "me")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	c, err := r.userClient()
	if err != nil {
		return err
	}
	me, err := c.Me(r.ctx)
	if err != nil {
		return err
	}
	if r.g.json {
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

func cmdKeys(r *runner, args []string) error {
	fs := r.newFlags("keys", "keys")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	c, err := r.userClient()
	if err != nil {
		return err
	}
	keys, err := c.MyKeys(r.ctx)
	if err != nil {
		return err
	}
	return r.printKeys(keys)
}

func (r *runner) printKeys(keys []api.APIKey) error {
	if r.g.json {
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

func cmdVersion(r *runner, args []string) error {
	fs := r.newFlags("version", "version")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	info := buildinfo.Get()
	if r.g.json {
		return r.printJSON(info)
	}
	fmt.Fprintf(r.stdout, "kazari %s\n", info)
	return nil
}

func fmtMap(m map[string]string) string {
	keys := slices.Sorted(maps.Keys(m))
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + m[k]
	}
	return strings.Join(parts, ", ")
}
