package cli

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/PeterBooker/kazari/internal/api"
)

// AdminCmd groups the admin API commands. On the server's host they use the
// admin socket and need no token.
type AdminCmd struct {
	Users     AdminUsersCmd     `cmd:"" help:"List, create and update users."`
	Keys      AdminKeysCmd      `cmd:"" help:"List, issue and revoke a user's API keys."`
	Artifacts AdminArtifactsCmd `cmd:"" help:"Artifacts of every owner."`
	Jobs      AdminJobsCmd      `cmd:"" help:"The maintenance jobs."`
	Transfer  AdminTransferCmd  `cmd:"" help:"Give an artifact to another user."`
	Hide      AdminHideCmd      `cmd:"" help:"Unpublish an artifact at once."`
	Settings  AdminSettingsCmd  `cmd:"" help:"Show the server's effective configuration, secrets redacted."`
	Audit     AdminAuditCmd     `cmd:"" help:"Read the audit log."`
	Health    AdminHealthCmd    `cmd:"" help:"Check the server is up; needs no token."`
}

// AdminUsersCmd manages users.
type AdminUsersCmd struct {
	List   AdminUsersListCmd   `cmd:"" help:"List every user with usage and quota."`
	Create AdminUsersCreateCmd `cmd:"" help:"Create a user."`
	Update AdminUsersUpdateCmd `cmd:"" help:"Change a user's role, quota, email or disabled state."`
}

// AdminUsersListCmd lists users.
type AdminUsersListCmd struct{}

func (c *AdminUsersListCmd) Run(r *runner) error {
	cl, err := r.adminClient(true)
	if err != nil {
		return err
	}
	users, err := cl.ListUsers(r.ctx)
	if err != nil {
		return err
	}
	if r.g.JSON {
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
	if r.g.JSON {
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

// AdminUsersCreateCmd creates a user.
type AdminUsersCreateCmd struct {
	Handle string `arg:"" help:"The user's handle."`
	Email  string `help:"Email address." placeholder:"EMAIL"`
	Role   string `help:"member (default) or admin." placeholder:"ROLE"`
	Quota  string `help:"Storage quota, such as 500MiB (default: the server's)." placeholder:"SIZE"`
}

func (c *AdminUsersCreateCmd) Run(r *runner) error {
	req := api.CreateUserRequest{Handle: c.Handle, Email: c.Email, Role: c.Role}
	if c.Quota != "" {
		n, err := parseSize(c.Quota)
		if err != nil {
			return usagef("admin users create: --quota: %v", err)
		}
		req.QuotaBytes = &n
	}
	cl, err := r.adminClient(true)
	if err != nil {
		return err
	}
	u, err := cl.CreateUser(r.ctx, req)
	if err != nil {
		return err
	}
	return r.printUser(u)
}

// AdminUsersUpdateCmd changes a user.
type AdminUsersUpdateCmd struct {
	Handle  string  `arg:"" help:"The user's handle."`
	Disable bool    `help:"Disable the user, which revokes every key."`
	Enable  bool    `help:"Re-enable the user."`
	Role    *string `help:"member or admin." placeholder:"ROLE"`
	Quota   *string `help:"Storage quota, such as 500MiB; 0 is unlimited." placeholder:"SIZE"`
	Email   *string `help:"Email address." placeholder:"EMAIL"`
}

func (c *AdminUsersUpdateCmd) Run(r *runner) error {
	if c.Disable && c.Enable {
		return usagef("admin users update: --disable and --enable are exclusive")
	}
	var req api.UpdateUserRequest
	if c.Disable || c.Enable {
		req.Disabled = &c.Disable
	}
	req.Role = c.Role
	req.Email = c.Email
	if c.Quota != nil {
		n, err := parseSize(*c.Quota)
		if err != nil {
			return usagef("admin users update: --quota: %v", err)
		}
		req.QuotaBytes = &n
	}
	if req == (api.UpdateUserRequest{}) {
		return usagef("admin users update: nothing to change")
	}
	cl, err := r.adminClient(true)
	if err != nil {
		return err
	}
	u, err := cl.UpdateUser(r.ctx, c.Handle, req)
	if err != nil {
		return err
	}
	return r.printUser(u)
}

// AdminKeysCmd manages API keys.
type AdminKeysCmd struct {
	List   AdminKeysListCmd   `cmd:"" help:"List a user's keys."`
	Issue  AdminKeysIssueCmd  `cmd:"" help:"Issue a key; the secret is printed once."`
	Revoke AdminKeysRevokeCmd `cmd:"" help:"Revoke a key by its prefix."`
}

// AdminKeysListCmd lists a user's keys.
type AdminKeysListCmd struct {
	Handle string `arg:"" help:"The user's handle."`
}

func (c *AdminKeysListCmd) Run(r *runner) error {
	cl, err := r.adminClient(true)
	if err != nil {
		return err
	}
	keys, err := cl.ListKeys(r.ctx, c.Handle)
	if err != nil {
		return err
	}
	return r.printKeys(keys)
}

// AdminKeysIssueCmd issues a key.
type AdminKeysIssueCmd struct {
	Handle  string `arg:"" help:"The user's handle."`
	Label   string `help:"Key label." placeholder:"LABEL"`
	Scopes  string `help:"Comma-separated scopes (default: artifacts:read,artifacts:write,artifacts:delete)." placeholder:"A,B"`
	Expires string `help:"Lifetime as a Go duration, such as 720h (default: no expiry)." placeholder:"DURATION"`
}

func (c *AdminKeysIssueCmd) Run(r *runner) error {
	if c.Expires != "" {
		if _, err := time.ParseDuration(c.Expires); err != nil {
			return usagef("admin keys issue: --expires: %v", err)
		}
	}
	req := api.IssueKeyRequest{Label: c.Label, ExpiresIn: c.Expires}
	for s := range strings.SplitSeq(c.Scopes, ",") {
		if s = strings.TrimSpace(s); s != "" {
			req.Scopes = append(req.Scopes, s)
		}
	}
	cl, err := r.adminClient(true)
	if err != nil {
		return err
	}
	k, err := cl.IssueKey(r.ctx, c.Handle, req)
	if err != nil {
		return err
	}
	if r.g.JSON {
		return r.printJSON(k)
	}
	fmt.Fprintln(r.stdout, k.Key)
	fmt.Fprintf(r.stderr, "issued key %s for %s; the key is shown only once\n", k.APIKey.Prefix, c.Handle)
	return nil
}

// AdminKeysRevokeCmd revokes a key.
type AdminKeysRevokeCmd struct {
	Handle string `arg:"" help:"The user's handle."`
	Prefix string `arg:"" help:"The key's prefix, from keys list."`
}

func (c *AdminKeysRevokeCmd) Run(r *runner) error {
	cl, err := r.adminClient(true)
	if err != nil {
		return err
	}
	if err := cl.RevokeKey(r.ctx, c.Handle, c.Prefix); err != nil {
		return err
	}
	if r.g.JSON {
		return r.printJSON(api.OK{OK: true})
	}
	fmt.Fprintf(r.stdout, "revoked %s\n", c.Prefix)
	return nil
}

// AdminArtifactsCmd lists artifacts across owners.
type AdminArtifactsCmd struct {
	List AdminArtifactsListCmd `cmd:"" help:"List every artifact, with its owner."`
}

// AdminArtifactsListCmd lists every artifact.
type AdminArtifactsListCmd struct {
	listFlags
}

func (c *AdminArtifactsListCmd) Run(r *runner) error {
	cl, err := r.adminClient(true)
	if err != nil {
		return err
	}
	l, err := r.fetchList(&c.listFlags, func(p ListParams) (*api.ArtifactList, error) { return cl.AdminListArtifacts(r.ctx, p) })
	if err != nil {
		return err
	}
	return r.printList(l, true)
}

// AdminTransferCmd changes an artifact's owner.
type AdminTransferCmd struct {
	Slug   string `arg:"" help:"Artifact slug."`
	Handle string `arg:"" help:"The new owner's handle."`
}

func (c *AdminTransferCmd) Run(r *runner) error {
	cl, err := r.adminClient(true)
	if err != nil {
		return err
	}
	a, err := cl.Transfer(r.ctx, c.Slug, c.Handle)
	if err != nil {
		return err
	}
	if r.g.JSON {
		return r.printJSON(a)
	}
	fmt.Fprintf(r.stdout, "%s now belongs to %s\n", c.Slug, c.Handle)
	return nil
}

// AdminHideCmd unpublishes an artifact.
type AdminHideCmd struct {
	Slug string `arg:"" help:"Artifact slug."`
}

func (c *AdminHideCmd) Run(r *runner) error {
	cl, err := r.adminClient(true)
	if err != nil {
		return err
	}
	a, err := cl.Hide(r.ctx, c.Slug)
	if err != nil {
		return err
	}
	if r.g.JSON {
		return r.printJSON(a)
	}
	fmt.Fprintf(r.stdout, "%s is hidden (unpublished)\n", c.Slug)
	return nil
}

// AdminSettingsCmd prints the effective configuration.
type AdminSettingsCmd struct{}

func (c *AdminSettingsCmd) Run(r *runner) error {
	cl, err := r.adminClient(true)
	if err != nil {
		return err
	}
	s, err := cl.Settings(r.ctx)
	if err != nil {
		return err
	}
	if r.g.JSON {
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

// AdminJobsCmd manages the maintenance jobs.
type AdminJobsCmd struct {
	List AdminJobsListCmd `cmd:"" help:"Show each job's interval, last run, next run and last error."`
	Run  AdminJobsRunCmd  `cmd:"" help:"Run a job now and wait for it."`
}

// AdminJobsListCmd lists the jobs.
type AdminJobsListCmd struct{}

func (c *AdminJobsListCmd) Run(r *runner) error {
	cl, err := r.adminClient(true)
	if err != nil {
		return err
	}
	jobs, err := cl.Jobs(r.ctx)
	if err != nil {
		return err
	}
	if r.g.JSON {
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

// AdminJobsRunCmd runs one job.
type AdminJobsRunCmd struct {
	Name string `arg:"" help:"sweep, gc, uploads-expire or oauth-cleanup."`
}

func (c *AdminJobsRunCmd) Run(r *runner) error {
	cl, err := r.adminClient(true)
	if err != nil {
		return err
	}
	if err := cl.RunJob(r.ctx, c.Name); err != nil {
		return err
	}
	if r.g.JSON {
		return r.printJSON(api.OK{OK: true})
	}
	fmt.Fprintf(r.stdout, "triggered %s\n", c.Name)
	return nil
}

// AdminAuditCmd reads the audit log.
type AdminAuditCmd struct {
	Since string `help:"Only events after this RFC 3339 time, or this Go duration ago (such as 24h)." placeholder:"WHEN"`
	Slug  string `help:"Only events for this artifact." placeholder:"SLUG"`
	Limit int    `help:"Maximum events." placeholder:"N"`
}

func (c *AdminAuditCmd) Run(r *runner) error {
	since := ""
	if c.Since != "" {
		if t, err := time.Parse(time.RFC3339, c.Since); err == nil {
			since = t.Format(time.RFC3339)
		} else if d, derr := time.ParseDuration(c.Since); derr == nil {
			since = r.now().Add(-d).UTC().Format(time.RFC3339)
		} else {
			return usagef("admin audit: --since: want an RFC 3339 time or a duration, got %q", c.Since)
		}
	}
	cl, err := r.adminClient(true)
	if err != nil {
		return err
	}
	events, err := cl.Audit(r.ctx, since, c.Slug, c.Limit)
	if err != nil {
		return err
	}
	if r.g.JSON {
		return r.printJSON(events)
	}
	tw := newTable(r.stdout)
	fmt.Fprintln(tw, "AT\tACTOR\tACTION\tSLUG\tDETAILS")
	for _, e := range events {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", fmtTime(e.At), e.Actor, e.Action, dash(e.Slug), dash(oneLine(fmtMap(e.Details))))
	}
	return tw.Flush()
}

// AdminHealthCmd checks liveness.
type AdminHealthCmd struct{}

func (c *AdminHealthCmd) Run(r *runner) error {
	cl, err := r.adminClient(false)
	if err != nil {
		return err
	}
	h, err := cl.Health(r.ctx)
	if err != nil {
		return err
	}
	if r.g.JSON {
		return r.printJSON(h)
	}
	fmt.Fprintf(r.stdout, "%s (server %s)\n", h.Status, dash(h.Version))
	return nil
}
