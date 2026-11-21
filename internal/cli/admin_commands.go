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
	Users      AdminUsersCmd      `cmd:"" help:"List, show, create, invite and update users, reset a password, end sessions and hand a user's artifacts over."`
	Invites    AdminInvitesCmd    `cmd:"" help:"List and revoke the invite and reset links that have not been used."`
	SetupLink  AdminSetupLinkCmd  `cmd:"" name:"setup-link" help:"Print a link that creates the first admin of a fresh instance."`
	Keys       AdminKeysCmd       `cmd:"" help:"List, issue, rotate and revoke a user's API keys."`
	Identities AdminIdentitiesCmd `cmd:"" help:"List, link and unlink the identities a user signs in with at the identity provider."`
	Grants     AdminGrantsCmd     `cmd:"" help:"List and revoke a user's OAuth grants, the connections of MCP clients."`
	Artifacts  AdminArtifactsCmd  `cmd:"" help:"Artifacts of every owner."`
	Jobs       AdminJobsCmd       `cmd:"" help:"The maintenance jobs."`
	Transfer   AdminTransferCmd   `cmd:"" help:"Give an artifact to another user."`
	Hide       AdminHideCmd       `cmd:"" help:"Unpublish an artifact at once."`
	Settings   AdminSettingsCmd   `cmd:"" help:"Show the server's effective configuration, secrets redacted."`
	Audit      AdminAuditCmd      `cmd:"" help:"Read the audit log."`
	Backup     AdminBackupCmd     `cmd:"" help:"Write a backup of the database and every blob into a directory."`
	Ready      AdminReadyCmd      `cmd:"" help:"Check the running server is set up correctly: configuration, DNS, certificate, disk."`
	Health     AdminHealthCmd     `cmd:"" help:"Check the server is up; needs no token."`
}

// AdminUsersCmd manages users.
type AdminUsersCmd struct {
	List          AdminUsersListCmd          `cmd:"" help:"List every user with usage, quota, state and last activity."`
	Show          AdminUsersShowCmd          `cmd:"" help:"Show one user: how they sign in, their keys, sessions and last activity."`
	Create        AdminUsersCreateCmd        `cmd:"" help:"Create a user."`
	Invite        AdminUsersInviteCmd        `cmd:"" help:"Create a user and print the link that lets them set a password."`
	ResetPassword AdminUsersResetPasswordCmd `cmd:"" name:"reset-password" help:"Print a link that lets a user set a new password."`
	SignOut       AdminUsersSignOutCmd       `cmd:"" name:"sign-out" help:"End every browser session of a user; keys and connected apps stay."`
	Update        AdminUsersUpdateCmd        `cmd:"" help:"Change a user's role, quota, email or disabled state."`
	Transfer      AdminUsersTransferCmd      `cmd:"" help:"Give every artifact a user owns to another user."`
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
	fmt.Fprintln(tw, "HANDLE\tEMAIL\tROLE\tSTATE\tUSED\tQUOTA\tLAST ACTIVE\tCREATED")
	for _, u := range users {
		quota := "-"
		if u.QuotaBytes > 0 {
			quota = humanSize(u.QuotaBytes)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", u.Handle, dash(oneLine(u.Email)), u.Role, userState(&u),
			humanSize(u.UsedBytes), quota, fmtTimePtr(u.LastActiveAt), fmtTime(u.CreatedAt))
	}
	return tw.Flush()
}

// userState is disabled, invited (an invite waits for the person to set a
// password) or active.
func userState(u *api.User) string {
	switch {
	case u.DisabledAt != nil:
		return "disabled"
	case u.Invited:
		return "invited"
	}
	return "active"
}

func (r *runner) printUser(u *api.User) error {
	if r.g.JSON {
		return r.printJSON(u)
	}
	tw := newTable(r.stdout)
	fmt.Fprintf(tw, "handle:\t%s\n", u.Handle)
	if u.DisplayName != "" {
		fmt.Fprintf(tw, "name:\t%s\n", oneLine(u.DisplayName))
	}
	fmt.Fprintf(tw, "email:\t%s\n", dash(oneLine(u.Email)))
	fmt.Fprintf(tw, "role:\t%s\n", u.Role)
	fmt.Fprintf(tw, "storage:\t%s\n", fmtQuota(u.UsedBytes, u.QuotaBytes))
	if u.DisabledAt != nil {
		fmt.Fprintf(tw, "disabled:\t%s\n", fmtTime(*u.DisabledAt))
	}
	switch {
	case u.Password:
		fmt.Fprintf(tw, "password:\tset\n")
	case u.Invited:
		fmt.Fprintf(tw, "password:\tnot set, invited\n")
	}
	if u.Keys > 0 {
		fmt.Fprintf(tw, "keys:\t%d\n", u.Keys)
	}
	if u.LastActiveAt != nil {
		fmt.Fprintf(tw, "last active:\t%s\n", fmtTime(*u.LastActiveAt))
	}
	if u.Sessions != nil {
		fmt.Fprintf(tw, "sessions:\t%d\n", *u.Sessions)
	}
	fmt.Fprintf(tw, "created:\t%s\n", fmtTime(u.CreatedAt))
	return tw.Flush()
}

// AdminUsersCreateCmd creates a user.
type AdminUsersCreateCmd struct {
	Handle string `arg:"" help:"The user's handle."`
	Email  string `help:"Email address." placeholder:"EMAIL"`
	Name   string `help:"Display name." placeholder:"NAME"`
	Role   string `help:"member (default) or admin." placeholder:"ROLE"`
	Quota  string `help:"Storage quota, such as 500MiB (default: the server's)." placeholder:"SIZE"`
}

func (c *AdminUsersCreateCmd) Run(r *runner) error {
	req := api.CreateUserRequest{Handle: c.Handle, Email: c.Email, DisplayName: c.Name, Role: c.Role}
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
	Disable bool    `help:"Disable the user, which revokes every key, OAuth grant and share link they created."`
	Enable  bool    `help:"Re-enable the user."`
	Role    *string `help:"member or admin." placeholder:"ROLE"`
	Quota   *string `help:"Storage quota, such as 500MiB; 0 is unlimited." placeholder:"SIZE"`
	Email   *string `help:"Email address." placeholder:"EMAIL"`
	Name    *string `help:"Display name; an empty one clears it." placeholder:"NAME"`
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
	req.DisplayName = c.Name
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

// AdminUsersTransferCmd hands a user's artifacts over.
type AdminUsersTransferCmd struct {
	Handle string `arg:"" help:"The handle of the user who owns the artifacts."`
	Owner  string `arg:"" help:"The new owner's handle."`
}

func (c *AdminUsersTransferCmd) Run(r *runner) error {
	cl, err := r.adminClient(true)
	if err != nil {
		return err
	}
	out, err := cl.TransferAll(r.ctx, c.Handle, c.Owner)
	if err != nil {
		return err
	}
	if r.g.JSON {
		return r.printJSON(out)
	}
	if len(out.Transferred) == 0 {
		fmt.Fprintf(r.stdout, "%s owns no artifacts\n", c.Handle)
		return nil
	}
	noun := "artifacts"
	if len(out.Transferred) == 1 {
		noun = "artifact"
	}
	fmt.Fprintf(r.stdout, "%d %s of %s now belong to %s\n", len(out.Transferred), noun, c.Handle, c.Owner)
	for _, slug := range out.Transferred {
		fmt.Fprintln(r.stdout, slug)
	}
	return nil
}

// AdminIdentitiesCmd manages the links between users and the identity
// provider's accounts.
type AdminIdentitiesCmd struct {
	List   AdminIdentitiesListCmd   `cmd:"" help:"List a user's identities with their last sign-in."`
	Link   AdminIdentitiesLinkCmd   `cmd:"" help:"Link an identity to a user, moving it from any other user."`
	Unlink AdminIdentitiesUnlinkCmd `cmd:"" help:"Remove one of a user's identities."`
}

// AdminIdentitiesListCmd lists a user's identities.
type AdminIdentitiesListCmd struct {
	Handle string `arg:"" help:"The user's handle."`
}

func (c *AdminIdentitiesListCmd) Run(r *runner) error {
	cl, err := r.adminClient(true)
	if err != nil {
		return err
	}
	u, err := cl.GetUser(r.ctx, c.Handle)
	if err != nil {
		return err
	}
	if r.g.JSON {
		ids := u.Identities
		if ids == nil {
			ids = []api.Identity{}
		}
		return r.printJSON(ids)
	}
	tw := newTable(r.stdout)
	fmt.Fprintln(tw, "ISSUER\tSUBJECT\tEMAIL\tLAST SIGN-IN")
	for _, id := range u.Identities {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", oneLine(id.Issuer), oneLine(id.Subject), dash(oneLine(id.Email)), fmtTimePtr(id.LastLoginAt))
	}
	return tw.Flush()
}

// AdminIdentitiesLinkCmd links an identity to a user.
type AdminIdentitiesLinkCmd struct {
	Handle  string `arg:"" help:"The user's handle."`
	Issuer  string `required:"" help:"The identity provider's issuer URL, as in auth.oidc.issuer." placeholder:"URL"`
	Subject string `required:"" help:"The person's subject at the provider, the sub claim." placeholder:"SUB"`
	Email   string `help:"The email address to show with the identity." placeholder:"EMAIL"`
}

func (c *AdminIdentitiesLinkCmd) Run(r *runner) error {
	cl, err := r.adminClient(true)
	if err != nil {
		return err
	}
	req := api.LinkIdentityRequest{Issuer: c.Issuer, Subject: c.Subject, Email: c.Email}
	if err := cl.LinkIdentity(r.ctx, c.Handle, req); err != nil {
		return err
	}
	if r.g.JSON {
		return r.printJSON(api.OK{OK: true})
	}
	fmt.Fprintf(r.stdout, "linked %s to %s\n", oneLine(c.Subject), c.Handle)
	return nil
}

// AdminIdentitiesUnlinkCmd removes an identity from a user.
type AdminIdentitiesUnlinkCmd struct {
	Handle  string `arg:"" help:"The user's handle."`
	Issuer  string `required:"" help:"The identity's issuer, from identities list." placeholder:"URL"`
	Subject string `required:"" help:"The identity's subject, from identities list." placeholder:"SUB"`
}

func (c *AdminIdentitiesUnlinkCmd) Run(r *runner) error {
	cl, err := r.adminClient(true)
	if err != nil {
		return err
	}
	if err := cl.UnlinkIdentity(r.ctx, c.Handle, c.Issuer, c.Subject); err != nil {
		return err
	}
	if r.g.JSON {
		return r.printJSON(api.OK{OK: true})
	}
	fmt.Fprintf(r.stdout, "unlinked %s from %s\n", oneLine(c.Subject), c.Handle)
	return nil
}

// AdminKeysCmd manages API keys.
type AdminKeysCmd struct {
	List   AdminKeysListCmd   `cmd:"" help:"List a user's keys."`
	Issue  AdminKeysIssueCmd  `cmd:"" help:"Issue a key; the secret is printed once."`
	Rotate AdminKeysRotateCmd `cmd:"" help:"Replace a key with a new one; the secret is printed once."`
	Revoke AdminKeysRevokeCmd `cmd:"" help:"Revoke a key by its prefix, with the keys issued with it."`
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
	Handle string `arg:"" help:"The user's handle."`
	keyFlags
}

func (c *AdminKeysIssueCmd) Run(r *runner) error {
	cl, err := r.adminClient(true)
	if err != nil {
		return err
	}
	k, err := cl.IssueKey(r.ctx, c.Handle, c.request())
	if err != nil {
		return err
	}
	return r.printIssued(k, fmt.Sprintf("issued key %s for %s", k.APIKey.Prefix, c.Handle))
}

// AdminKeysRotateCmd replaces a key.
type AdminKeysRotateCmd struct {
	Handle  string `arg:"" help:"The user's handle."`
	Prefix  string `arg:"" help:"The key's prefix, from keys list."`
	Overlap string `help:"How long the old key keeps working, such as 24h (the default), 7d (the most) or 0." placeholder:"DURATION"`
}

func (c *AdminKeysRotateCmd) Run(r *runner) error {
	cl, err := r.adminClient(true)
	if err != nil {
		return err
	}
	k, err := cl.RotateKey(r.ctx, c.Handle, c.Prefix, c.Overlap)
	if err != nil {
		return err
	}
	return r.printIssued(k, rotatedNote(k)+" for "+c.Handle)
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

// AdminGrantsCmd manages OAuth grants: the connections of MCP clients.
type AdminGrantsCmd struct {
	List   AdminGrantsListCmd   `cmd:"" help:"List a user's connections, with the last refresh and the expiry."`
	Revoke AdminGrantsRevokeCmd `cmd:"" help:"Revoke a connection by its id."`
}

// AdminGrantsListCmd lists a user's grants.
type AdminGrantsListCmd struct {
	Handle string `arg:"" help:"The user's handle."`
}

func (c *AdminGrantsListCmd) Run(r *runner) error {
	cl, err := r.adminClient(true)
	if err != nil {
		return err
	}
	grants, err := cl.ListGrants(r.ctx, c.Handle)
	if err != nil {
		return err
	}
	if r.g.JSON {
		return r.printJSON(grants)
	}
	tw := newTable(r.stdout)
	fmt.Fprintln(tw, "ID\tCLIENT\tSCOPES\tKEY\tLAST REFRESH\tEXPIRES")
	for _, g := range grants {
		client := g.Client
		if client == "" {
			client = g.ClientID
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", g.ID, dash(oneLine(client)), dash(strings.Join(g.Scopes, ",")),
			dash(g.KeyPrefix), fmtTime(g.CreatedAt), fmtTime(g.ExpiresAt))
	}
	return tw.Flush()
}

// AdminGrantsRevokeCmd revokes a grant.
type AdminGrantsRevokeCmd struct {
	ID int64 `arg:"" help:"The grant's id, from grants list or an oauth:<id> credential in the audit log."`
}

func (c *AdminGrantsRevokeCmd) Run(r *runner) error {
	cl, err := r.adminClient(true)
	if err != nil {
		return err
	}
	if err := cl.RevokeGrant(r.ctx, c.ID); err != nil {
		return err
	}
	if r.g.JSON {
		return r.printJSON(api.OK{OK: true})
	}
	fmt.Fprintf(r.stdout, "revoked grant %d\n", c.ID)
	return nil
}

// AdminArtifactsCmd lists artifacts across owners.
type AdminArtifactsCmd struct {
	List AdminArtifactsListCmd `cmd:"" help:"List every artifact, with its owner."`
}

// AdminArtifactsListCmd lists every artifact.
type AdminArtifactsListCmd struct {
	listFlags
	Owner string `help:"Only artifacts this user owns." placeholder:"HANDLE"`
}

func (c *AdminArtifactsListCmd) Run(r *runner) error {
	cl, err := r.adminClient(true)
	if err != nil {
		return err
	}
	l, err := r.fetchList(&c.listFlags, func(p ListParams) (*api.ArtifactList, error) {
		p.Owner = c.Owner
		return cl.AdminListArtifacts(r.ctx, p)
	})
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
	Name string `arg:"" help:"sweep, gc, uploads-expire, oauth-cleanup or update-check."`
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
	Since  string   `help:"Only events from this RFC 3339 time on, or from this Go duration ago (such as 24h)." placeholder:"WHEN"`
	Until  string   `help:"Only events before this RFC 3339 time, or before this Go duration ago." placeholder:"WHEN"`
	Slug   string   `help:"Only events for this artifact." placeholder:"SLUG"`
	Actor  string   `help:"Only events by this handle, or by admin or scheduler." placeholder:"HANDLE"`
	Action []string `help:"Only events with this action, such as key.issue, or this prefix, such as artifact. (repeatable)." sep:"none" placeholder:"ACTION"`
	Cursor string   `help:"Page cursor from a previous audit." placeholder:"CURSOR"`
	Limit  int      `help:"Maximum events." placeholder:"N"`
}

// when reads an RFC 3339 time, or a duration counted back from now.
func (r *runner) when(flag, v string) (string, error) {
	if v == "" {
		return "", nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.Format(time.RFC3339), nil
	}
	if d, err := time.ParseDuration(v); err == nil {
		return r.now().Add(-d).UTC().Format(time.RFC3339), nil
	}
	return "", usagef("admin audit: %s: want an RFC 3339 time or a duration, got %q", flag, v)
}

func (c *AdminAuditCmd) Run(r *runner) error {
	since, err := r.when("--since", c.Since)
	if err != nil {
		return err
	}
	until, err := r.when("--until", c.Until)
	if err != nil {
		return err
	}
	cl, err := r.adminClient(true)
	if err != nil {
		return err
	}
	l, err := cl.Audit(r.ctx, AuditParams{Since: since, Until: until, Slug: c.Slug, Actor: c.Actor,
		Actions: c.Action, Cursor: c.Cursor, Limit: c.Limit})
	if err != nil {
		return err
	}
	if r.g.JSON {
		return r.printJSON(l)
	}
	tw := newTable(r.stdout)
	fmt.Fprintln(tw, "AT\tACTOR\tACTION\tSLUG\tDETAILS")
	for _, e := range l.Events {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", fmtTime(e.At), e.Actor, e.Action, dash(e.Slug), dash(oneLine(fmtMap(e.Details))))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if l.NextCursor != "" {
		fmt.Fprintf(r.stderr, "more results: --cursor %s\n", l.NextCursor)
	}
	return nil
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
