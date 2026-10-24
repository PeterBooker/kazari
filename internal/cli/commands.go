package cli

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"agalma/internal/api"
	"agalma/internal/buildinfo"
)

// commandFunc runs one subcommand with the arguments after its name.
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
		"rollback":   cmdRollback,
		"files":      cmdFiles,
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
	vis := fs.String("visibility", "", "public or unlisted")
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
	if *vis != "" && *vis != "public" && *vis != "unlisted" {
		return usagef("publish: --visibility must be public or unlisted")
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

// removedFiles lists files of the latest version that are not in local.
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

// printResult prints a publish result: the URL, or the JSON.
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
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", num, dash(v.Label), dash(v.CreatedBy),
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

// printArtifact prints one artifact as aligned key: value lines.
func (r *runner) printArtifact(a *api.Artifact) {
	tw := newTable(r.stdout)
	kv := func(k, v string) {
		if v != "" {
			fmt.Fprintf(tw, "%s:\t%s\n", k, v)
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

// listOptions registers the list filter flags shared by list and admin
// artifacts list.
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

// fetchList pages through list when --all is set.
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
			humanSize(a.SizeBytes), fmtTime(a.UpdatedAt), a.Name)
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
	fs := r.newFlags("visibility", "visibility <slug> <public|unlisted|unpublished>")
	pos, err := parse(fs, args, 2, 2)
	if err != nil {
		return err
	}
	switch pos[1] {
	case "public", "unlisted", "unpublished":
	default:
		return usagef("visibility: want public, unlisted or unpublished, got %q", pos[1])
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

func cmdFiles(r *runner, args []string) error {
	if len(args) == 0 || args[0] != "cat" {
		if len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
			fmt.Fprintln(r.stderr, "usage: agalma files cat <slug> <path> [--version N]")
			return nil
		}
		return usagef("usage: agalma files cat <slug> <path> [--version N]")
	}
	fs := r.newFlags("files cat", "files cat <slug> <path> [--version N]")
	version := fs.Int("version", 0, "read from version `N` instead of the published one")
	pos, err := parse(fs, args[1:], 2, 2)
	if err != nil {
		return err
	}
	c, err := r.userClient()
	if err != nil {
		return err
	}
	body, _, err := c.ReadFile(r.ctx, pos[0], pos[1], *version)
	if err != nil {
		return err
	}
	defer body.Close()
	if _, err := io.Copy(r.stdout, body); err != nil {
		return fmt.Errorf("read %s: %w", pos[1], err)
	}
	return nil
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
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", k.Prefix, dash(k.Label), dash(strings.Join(k.Scopes, ",")),
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
	fmt.Fprintf(r.stdout, "agalma %s\n", info)
	return nil
}

// fmtMap renders a map as sorted k=v pairs.
func fmtMap(m map[string]string) string {
	keys := slices.Sorted(maps.Keys(m))
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + m[k]
	}
	return strings.Join(parts, ", ")
}
