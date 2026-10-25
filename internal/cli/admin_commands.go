package cli

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"kazari/internal/api"
)

const adminUsage = `usage: kazari admin <command>

Commands:
  users list
  users create <handle> [--email] [--role] [--quota]
  users update <handle> [--disable|--enable] [--role] [--quota] [--email]
  keys list <handle>
  keys issue <handle> [--label] [--scopes a,b] [--expires 720h]
  keys revoke <handle> <prefix>
  artifacts list [list flags]
  transfer <slug> <handle>
  hide <slug>
  settings
  jobs list
  jobs run <name>
  audit [--since RFC3339|duration] [--slug] [--limit]
  health
`

func cmdAdmin(r *runner, args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(r.stderr, adminUsage)
		if len(args) == 0 {
			return errUsagePrinted
		}
		return nil
	}
	type sub map[string]commandFunc
	groups := map[string]sub{
		"users":     {"list": cmdAdminUsersList, "create": cmdAdminUsersCreate, "update": cmdAdminUsersUpdate},
		"keys":      {"list": cmdAdminKeysList, "issue": cmdAdminKeysIssue, "revoke": cmdAdminKeysRevoke},
		"artifacts": {"list": cmdAdminArtifactsList},
		"jobs":      {"list": cmdAdminJobsList, "run": cmdAdminJobsRun},
	}
	single := map[string]commandFunc{
		"transfer": cmdAdminTransfer,
		"hide":     cmdAdminHide,
		"settings": cmdAdminSettings,
		"audit":    cmdAdminAudit,
		"health":   cmdAdminHealth,
	}
	if cmd, ok := single[args[0]]; ok {
		return cmd(r, args[1:])
	}
	g, ok := groups[args[0]]
	if !ok {
		return usagef("admin: unknown command %q (run kazari admin help)", args[0])
	}
	if len(args) < 2 {
		return usagef("admin %s: want one of %s", args[0], strings.Join(slices.Sorted(maps.Keys(g)), ", "))
	}
	cmd, ok := g[args[1]]
	if !ok {
		return usagef("admin %s: unknown command %q, want one of %s", args[0], args[1],
			strings.Join(slices.Sorted(maps.Keys(g)), ", "))
	}
	return cmd(r, args[2:])
}

func cmdAdminUsersList(r *runner, args []string) error {
	fs := r.newFlags("admin users list", "admin users list")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	c, err := r.adminClient()
	if err != nil {
		return err
	}
	users, err := c.ListUsers(r.ctx)
	if err != nil {
		return err
	}
	if r.g.json {
		return r.printJSON(users)
	}
	tw := newTable(r.stdout)
	fmt.Fprintln(tw, "HANDLE\tEMAIL\tROLE\tUSED\tQUOTA\tDISABLED\tCREATED")
	for _, u := range users {
		quota := "-"
		if u.QuotaBytes > 0 {
			quota = humanSize(u.QuotaBytes)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", u.Handle, dash(oneLine(u.Email)), u.Role, humanSize(u.UsedBytes),
			quota, fmtTimePtr(u.DisabledAt), fmtTime(u.CreatedAt))
	}
	return tw.Flush()
}

func (r *runner) printUser(u *api.User) error {
	if r.g.json {
		return r.printJSON(u)
	}
	tw := newTable(r.stdout)
	fmt.Fprintf(tw, "handle:\t%s\n", u.Handle)
	fmt.Fprintf(tw, "email:\t%s\n", dash(oneLine(u.Email)))
	fmt.Fprintf(tw, "role:\t%s\n", u.Role)
	fmt.Fprintf(tw, "storage:\t%s\n", fmtQuota(u.UsedBytes, u.QuotaBytes))
	if u.DisabledAt != nil {
		fmt.Fprintf(tw, "disabled:\t%s\n", fmtTime(*u.DisabledAt))
	}
	fmt.Fprintf(tw, "created:\t%s\n", fmtTime(u.CreatedAt))
	return tw.Flush()
}

func cmdAdminUsersCreate(r *runner, args []string) error {
	fs := r.newFlags("admin users create", "admin users create <handle> [flags]")
	email := fs.String("email", "", "email address")
	role := fs.String("role", "", "member (default) or admin")
	quota := fs.String("quota", "", "storage quota, e.g. 500MiB (server default if empty)")
	pos, err := parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	req := api.CreateUserRequest{Handle: pos[0], Email: *email, Role: *role}
	if *quota != "" {
		n, err := parseSize(*quota)
		if err != nil {
			return usagef("admin users create: --quota: %v", err)
		}
		req.QuotaBytes = &n
	}
	c, err := r.adminClient()
	if err != nil {
		return err
	}
	u, err := c.CreateUser(r.ctx, req)
	if err != nil {
		return err
	}
	return r.printUser(u)
}

func cmdAdminUsersUpdate(r *runner, args []string) error {
	fs := r.newFlags("admin users update", "admin users update <handle> [flags]")
	disable := fs.Bool("disable", false, "disable the user (revokes every key)")
	enable := fs.Bool("enable", false, "re-enable the user")
	role := fs.String("role", "", "member or admin")
	quota := fs.String("quota", "", "storage quota, e.g. 500MiB")
	email := fs.String("email", "", "email address")
	pos, err := parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	if *disable && *enable {
		return usagef("admin users update: --disable and --enable are exclusive")
	}
	var req api.UpdateUserRequest
	if *disable || *enable {
		req.Disabled = disable
	}
	if flagSet(fs, "role") {
		req.Role = role
	}
	if flagSet(fs, "email") {
		req.Email = email
	}
	if flagSet(fs, "quota") {
		n, err := parseSize(*quota)
		if err != nil {
			return usagef("admin users update: --quota: %v", err)
		}
		req.QuotaBytes = &n
	}
	if req == (api.UpdateUserRequest{}) {
		return usagef("admin users update: nothing to change")
	}
	c, err := r.adminClient()
	if err != nil {
		return err
	}
	u, err := c.UpdateUser(r.ctx, pos[0], req)
	if err != nil {
		return err
	}
	return r.printUser(u)
}

func cmdAdminKeysList(r *runner, args []string) error {
	fs := r.newFlags("admin keys list", "admin keys list <handle>")
	pos, err := parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	c, err := r.adminClient()
	if err != nil {
		return err
	}
	keys, err := c.ListKeys(r.ctx, pos[0])
	if err != nil {
		return err
	}
	return r.printKeys(keys)
}

func cmdAdminKeysIssue(r *runner, args []string) error {
	fs := r.newFlags("admin keys issue", "admin keys issue <handle> [flags]")
	label := fs.String("label", "", "key label")
	scopes := fs.String("scopes", "", "comma-separated scopes (server default: read, write, delete)")
	expires := fs.String("expires", "", "lifetime as a Go `duration`, e.g. 720h (no expiry if empty)")
	pos, err := parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	if *expires != "" {
		if _, err := time.ParseDuration(*expires); err != nil {
			return usagef("admin keys issue: --expires: %v", err)
		}
	}
	req := api.IssueKeyRequest{Label: *label, ExpiresIn: *expires}
	for s := range strings.SplitSeq(*scopes, ",") {
		if s = strings.TrimSpace(s); s != "" {
			req.Scopes = append(req.Scopes, s)
		}
	}
	c, err := r.adminClient()
	if err != nil {
		return err
	}
	k, err := c.IssueKey(r.ctx, pos[0], req)
	if err != nil {
		return err
	}
	if r.g.json {
		return r.printJSON(k)
	}
	fmt.Fprintln(r.stdout, k.Key)
	fmt.Fprintf(r.stderr, "issued key %s for %s; the key is shown only once\n", k.APIKey.Prefix, pos[0])
	return nil
}

func cmdAdminKeysRevoke(r *runner, args []string) error {
	fs := r.newFlags("admin keys revoke", "admin keys revoke <handle> <prefix>")
	pos, err := parse(fs, args, 2, 2)
	if err != nil {
		return err
	}
	c, err := r.adminClient()
	if err != nil {
		return err
	}
	if err := c.RevokeKey(r.ctx, pos[0], pos[1]); err != nil {
		return err
	}
	if r.g.json {
		return r.printJSON(api.OK{OK: true})
	}
	fmt.Fprintf(r.stdout, "revoked %s\n", pos[1])
	return nil
}

func cmdAdminArtifactsList(r *runner, args []string) error {
	o, parseArgs := addListFlags(r, "admin artifacts list", "admin artifacts list [flags]")
	if err := parseArgs(args); err != nil {
		return err
	}
	c, err := r.adminClient()
	if err != nil {
		return err
	}
	l, err := r.fetchList(o, func(p ListParams) (*api.ArtifactList, error) { return c.AdminListArtifacts(r.ctx, p) })
	if err != nil {
		return err
	}
	return r.printList(l, true)
}

func cmdAdminTransfer(r *runner, args []string) error {
	fs := r.newFlags("admin transfer", "admin transfer <slug> <handle>")
	pos, err := parse(fs, args, 2, 2)
	if err != nil {
		return err
	}
	c, err := r.adminClient()
	if err != nil {
		return err
	}
	a, err := c.Transfer(r.ctx, pos[0], pos[1])
	if err != nil {
		return err
	}
	if r.g.json {
		return r.printJSON(a)
	}
	fmt.Fprintf(r.stdout, "%s now belongs to %s\n", pos[0], pos[1])
	return nil
}

func cmdAdminHide(r *runner, args []string) error {
	fs := r.newFlags("admin hide", "admin hide <slug>")
	pos, err := parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	c, err := r.adminClient()
	if err != nil {
		return err
	}
	a, err := c.Hide(r.ctx, pos[0])
	if err != nil {
		return err
	}
	if r.g.json {
		return r.printJSON(a)
	}
	fmt.Fprintf(r.stdout, "%s is hidden (unpublished)\n", pos[0])
	return nil
}

func cmdAdminSettings(r *runner, args []string) error {
	fs := r.newFlags("admin settings", "admin settings")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	c, err := r.adminClient()
	if err != nil {
		return err
	}
	s, err := c.Settings(r.ctx)
	if err != nil {
		return err
	}
	if r.g.json {
		return r.printJSON(s)
	}
	lines := map[string]string{}
	flatten("", s, lines)
	tw := tabwriter.NewWriter(termSafe{r.stdout}, 0, 0, 1, ' ', 0)
	for _, k := range slices.Sorted(maps.Keys(lines)) {
		fmt.Fprintf(tw, "%s\t= %s\n", k, lines[k])
	}
	return tw.Flush()
}

func flatten(prefix string, v any, out map[string]string) {
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 && prefix != "" {
			out[prefix] = "{}"
		}
		for k, child := range t {
			key := k
			if prefix != "" {
				key = prefix + "." + k
			}
			flatten(key, child, out)
		}
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = fmt.Sprint(scalar(e))
		}
		out[prefix] = "[" + strings.Join(parts, ", ") + "]"
	default:
		out[prefix] = fmt.Sprint(scalar(v))
	}
}

func scalar(v any) any {
	switch t := v.(type) {
	case string:
		return fmt.Sprintf("%q", t)
	case nil:
		return "null"
	default:
		return t
	}
}

func cmdAdminJobsList(r *runner, args []string) error {
	fs := r.newFlags("admin jobs list", "admin jobs list")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	c, err := r.adminClient()
	if err != nil {
		return err
	}
	jobs, err := c.Jobs(r.ctx)
	if err != nil {
		return err
	}
	if r.g.json {
		return r.printJSON(jobs)
	}
	tw := newTable(r.stdout)
	fmt.Fprintln(tw, "NAME\tEVERY\tRUNNING\tLAST RUN\tNEXT RUN\tLAST ERROR")
	for _, j := range jobs {
		fmt.Fprintf(tw, "%s\t%s\t%t\t%s\t%s\t%s\n", j.Name, j.Every, j.Running,
			fmtTimePtr(j.LastRun), fmtTimePtr(j.NextRun), dash(j.LastError))
	}
	return tw.Flush()
}

func cmdAdminJobsRun(r *runner, args []string) error {
	fs := r.newFlags("admin jobs run", "admin jobs run <name>")
	pos, err := parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	c, err := r.adminClient()
	if err != nil {
		return err
	}
	if err := c.RunJob(r.ctx, pos[0]); err != nil {
		return err
	}
	if r.g.json {
		return r.printJSON(api.OK{OK: true})
	}
	fmt.Fprintf(r.stdout, "triggered %s\n", pos[0])
	return nil
}

func cmdAdminAudit(r *runner, args []string) error {
	fs := r.newFlags("admin audit", "admin audit [flags]")
	since := fs.String("since", "", "only events after this RFC 3339 time, or this Go duration ago (e.g. 24h)")
	slug := fs.String("slug", "", "only events for this artifact")
	limit := fs.Int("limit", 0, "maximum events")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	sinceArg := ""
	if *since != "" {
		if t, err := time.Parse(time.RFC3339, *since); err == nil {
			sinceArg = t.Format(time.RFC3339)
		} else if d, derr := time.ParseDuration(*since); derr == nil {
			sinceArg = r.now().Add(-d).UTC().Format(time.RFC3339)
		} else {
			return usagef("admin audit: --since: want an RFC 3339 time or a duration, got %q", *since)
		}
	}
	c, err := r.adminClient()
	if err != nil {
		return err
	}
	events, err := c.Audit(r.ctx, sinceArg, *slug, *limit)
	if err != nil {
		return err
	}
	if r.g.json {
		return r.printJSON(events)
	}
	tw := newTable(r.stdout)
	fmt.Fprintln(tw, "AT\tACTOR\tACTION\tSLUG\tDETAILS")
	for _, e := range events {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", fmtTime(e.At), e.Actor, e.Action, dash(e.Slug), dash(oneLine(fmtMap(e.Details))))
	}
	return tw.Flush()
}

func cmdAdminHealth(r *runner, args []string) error {
	fs := r.newFlags("admin health", "admin health")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	c, err := r.adminClient()
	if err != nil {
		return err
	}
	h, err := c.Health(r.ctx)
	if err != nil {
		return err
	}
	if r.g.json {
		return r.printJSON(h)
	}
	fmt.Fprintf(r.stdout, "%s (server %s)\n", h.Status, dash(h.Version))
	return nil
}
