package cli

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/apiclient"
	"github.com/PeterBooker/kazari/internal/bytesize"
	"github.com/PeterBooker/kazari/internal/config"
)

// AdminCmd groups the admin API commands. On the server's host they use the
// admin socket and need no token.
type AdminCmd struct {
	Users      AdminUsersCmd         `cmd:"" help:"List, show, create, invite and update users, reset a password, end sessions and hand a user's artifacts over."`
	Invites    AdminInvitesCmd       `cmd:"" help:"List and revoke the invite and reset links that have not been used."`
	SetupLink  AdminSetupLinkCmd     `cmd:"" name:"setup-link" help:"Print a link that creates the first admin of a fresh instance."`
	Keys       AdminKeysCmd          `cmd:"" help:"List, issue, rotate and revoke a user's API keys."`
	Identities AdminIdentitiesCmd    `cmd:"" help:"List, link and unlink the identities a user signs in with at the identity provider."`
	Grants     AdminGrantsCmd        `cmd:"" help:"List and revoke a user's OAuth grants, the connections of MCP clients."`
	Artifacts  AdminArtifactsCmd     `cmd:"" help:"Artifacts of every owner."`
	Jobs       AdminJobsCmd          `cmd:"" help:"The maintenance jobs."`
	Transfer   AdminTransferCmd      `cmd:"" help:"Give an artifact to another user."`
	Hide       AdminHideCmd          `cmd:"" help:"Unpublish an artifact at once."`
	Settings   AdminSettingsCmd      `cmd:"" help:"Show the server's effective configuration, secrets redacted."`
	Audit      AdminAuditCmd         `cmd:"" help:"Read the audit log."`
	Views      AdminViewsCmd         `cmd:"" help:"Page views of the instance, or of one artifact with the members who opened it."`
	Backup     AdminBackupCmd        `cmd:"" help:"Write a backup of the database and every blob into a directory, or as tar to standard output."`
	Restore    AdminRestoreBackupCmd `cmd:"" name:"restore-backup" help:"Restore a backup directory into the data directory of a stopped server on this host; needs no server."`
	Ready      AdminReadyCmd         `cmd:"" help:"Check the running server is set up correctly: configuration, DNS, certificate, disk."`
	Health     AdminHealthCmd        `cmd:"" help:"Check the server is up; needs no token."`
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

func (c *AdminUsersListCmd) Run(r *runner, cl adminAPI) error {
	users, err := cl.ListUsers(r.ctx)
	if err != nil {
		return err
	}
	return r.emit(users, func(w io.Writer) error {
		tw := newTable(w)
		tw.row("HANDLE", "EMAIL", "ROLE", "STATE", "USED", "QUOTA", "LAST ACTIVE", "CREATED")
		for _, u := range users {
			quota := "-"
			if u.QuotaBytes > 0 {
				quota = bytesize.Format(u.QuotaBytes)
			}
			tw.row(u.Handle, dash(u.Email), u.Role, userState(&u), bytesize.Format(u.UsedBytes), quota, fmtTimePtr(u.LastActiveAt), fmtTime(u.CreatedAt))
		}
		return tw.flush()
	})
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
	return r.emit(u, func(w io.Writer) error {
		tw := newTable(w)
		tw.row("handle:", u.Handle)
		if u.DisplayName != "" {
			tw.row("name:", u.DisplayName)
		}
		tw.row("email:", dash(u.Email))
		tw.row("role:", u.Role)
		tw.row("storage:", fmtQuota(u.UsedBytes, u.QuotaBytes))
		if u.DisabledAt != nil {
			tw.row("disabled:", fmtTime(*u.DisabledAt))
		}
		switch {
		case u.Password:
			tw.row("password:", "set")
		case u.Invited:
			tw.row("password:", "not set, invited")
		}
		if u.Passkeys > 0 {
			tw.row("passkeys:", u.Passkeys)
		}
		if u.RecoveryCodes > 0 {
			tw.row("recovery codes:", fmt.Sprintf("%d unused", u.RecoveryCodes))
		}
		if u.Keys > 0 {
			tw.row("keys:", u.Keys)
		}
		if u.LastActiveAt != nil {
			tw.row("last active:", fmtTime(*u.LastActiveAt))
		}
		if u.Sessions != nil {
			tw.row("sessions:", *u.Sessions)
		}
		tw.row("created:", fmtTime(u.CreatedAt))
		return tw.flush()
	})
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

func (c *AdminUsersTransferCmd) Run(r *runner, cl adminAPI) error {
	out, err := cl.TransferAll(r.ctx, c.Handle, c.Owner)
	if err != nil {
		return err
	}
	return r.emit(out, func(w io.Writer) error {
		if len(out.Transferred) == 0 {
			fmt.Fprintf(w, "%s owns no artifacts\n", c.Handle)
			return nil
		}
		fmt.Fprintf(w, "%s of %s now belong to %s\n", count(len(out.Transferred), "artifact"), c.Handle, c.Owner)
		for _, slug := range out.Transferred {
			fmt.Fprintln(w, slug)
		}
		return nil
	})
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

func (c *AdminIdentitiesListCmd) Run(r *runner, cl adminAPI) error {
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
	tw.row("ISSUER", "SUBJECT", "EMAIL", "LAST SIGN-IN")
	for _, id := range u.Identities {
		tw.row(id.Issuer, id.Subject, dash(id.Email), fmtTimePtr(id.LastLoginAt))
	}
	return tw.flush()
}

// AdminIdentitiesLinkCmd links an identity to a user.
type AdminIdentitiesLinkCmd struct {
	Handle  string `arg:"" help:"The user's handle."`
	Issuer  string `required:"" help:"The identity provider's issuer URL, as in auth.oidc.issuer." placeholder:"URL"`
	Subject string `required:"" help:"The person's subject at the provider, the sub claim." placeholder:"SUB"`
	Email   string `help:"The email address to show with the identity." placeholder:"EMAIL"`
}

func (c *AdminIdentitiesLinkCmd) Run(r *runner, cl adminAPI) error {
	req := api.LinkIdentityRequest{Issuer: c.Issuer, Subject: c.Subject, Email: c.Email}
	if err := cl.LinkIdentity(r.ctx, c.Handle, req); err != nil {
		return err
	}
	return r.emit(api.OK{OK: true}, func(w io.Writer) error {
		fmt.Fprintf(w, "linked %s to %s\n", oneLine(c.Subject), c.Handle)
		return nil
	})
}

// AdminIdentitiesUnlinkCmd removes an identity from a user.
type AdminIdentitiesUnlinkCmd struct {
	Handle  string `arg:"" help:"The user's handle."`
	Issuer  string `required:"" help:"The identity's issuer, from identities list." placeholder:"URL"`
	Subject string `required:"" help:"The identity's subject, from identities list." placeholder:"SUB"`
}

func (c *AdminIdentitiesUnlinkCmd) Run(r *runner, cl adminAPI) error {
	if err := cl.UnlinkIdentity(r.ctx, c.Handle, c.Issuer, c.Subject); err != nil {
		return err
	}
	return r.emit(api.OK{OK: true}, func(w io.Writer) error {
		fmt.Fprintf(w, "unlinked %s from %s\n", oneLine(c.Subject), c.Handle)
		return nil
	})
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

func (c *AdminKeysListCmd) Run(r *runner, cl adminAPI) error {
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

func (c *AdminKeysIssueCmd) Run(r *runner, cl adminAPI) error {
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

func (c *AdminKeysRotateCmd) Run(r *runner, cl adminAPI) error {
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

func (c *AdminKeysRevokeCmd) Run(r *runner, cl adminAPI) error {
	if err := cl.RevokeKey(r.ctx, c.Handle, c.Prefix); err != nil {
		return err
	}
	return r.emit(api.OK{OK: true}, func(w io.Writer) error {
		fmt.Fprintf(w, "revoked %s\n", c.Prefix)
		return nil
	})
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

func (c *AdminGrantsListCmd) Run(r *runner, cl adminAPI) error {
	grants, err := cl.ListGrants(r.ctx, c.Handle)
	if err != nil {
		return err
	}
	return r.emit(grants, func(w io.Writer) error {
		tw := newTable(w)
		tw.row("ID", "CLIENT", "SCOPES", "KEY", "LAST REFRESH", "EXPIRES")
		for _, g := range grants {
			client := g.Client
			if client == "" {
				client = g.ClientID
			}
			tw.row(g.ID, dash(client), dash(strings.Join(g.Scopes, ",")), dash(g.KeyPrefix), fmtTime(g.CreatedAt), fmtTime(g.ExpiresAt))
		}
		return tw.flush()
	})
}

// AdminGrantsRevokeCmd revokes a grant.
type AdminGrantsRevokeCmd struct {
	ID int64 `arg:"" help:"The grant's id, from grants list or an oauth:<id> credential in the audit log."`
}

func (c *AdminGrantsRevokeCmd) Run(r *runner, cl adminAPI) error {
	if err := cl.RevokeGrant(r.ctx, c.ID); err != nil {
		return err
	}
	return r.emit(api.OK{OK: true}, func(w io.Writer) error {
		fmt.Fprintf(w, "revoked grant %d\n", c.ID)
		return nil
	})
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

func (c *AdminArtifactsListCmd) Run(r *runner, cl adminAPI) error {
	l, err := r.fetchList(&c.listFlags, func(p apiclient.ListParams) (*api.ArtifactList, error) {
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

func (c *AdminTransferCmd) Run(r *runner, cl adminAPI) error {
	a, err := cl.Transfer(r.ctx, c.Slug, c.Handle)
	if err != nil {
		return err
	}
	return r.emit(a, func(w io.Writer) error {
		fmt.Fprintf(w, "%s now belongs to %s\n", c.Slug, c.Handle)
		return nil
	})
}

// AdminHideCmd unpublishes an artifact.
type AdminHideCmd struct {
	Slug string `arg:"" help:"Artifact slug."`
}

func (c *AdminHideCmd) Run(r *runner, cl adminAPI) error {
	a, err := cl.Hide(r.ctx, c.Slug)
	if err != nil {
		return err
	}
	return r.emit(a, func(w io.Writer) error {
		fmt.Fprintf(w, "%s is hidden (unpublished)\n", c.Slug)
		return nil
	})
}

// AdminSettingsCmd prints the effective configuration.
type AdminSettingsCmd struct{}

func (c *AdminSettingsCmd) Run(r *runner, cl adminAPI) error {
	s, err := cl.Settings(r.ctx)
	if err != nil {
		return err
	}
	return r.emit(s, func(w io.Writer) error {
		// A server older than the sources route answers 404: the values alone.
		sources, err := cl.SettingSources(r.ctx)
		if he, ok := errors.AsType[*apiclient.HTTPError](err); ok && he.Status == http.StatusNotFound {
			err = nil
		}
		if err != nil {
			return err
		}
		lines := map[string]string{}
		flatten("", s, lines)
		tw := newTablePadded(w, 1)
		for _, k := range slices.Sorted(maps.Keys(lines)) {
			if src := sourceOf(sources, k); src != "" {
				tw.row(k, "= "+lines[k], "("+src+")")
				continue
			}
			tw.row(k, "= "+lines[k])
		}
		return tw.flush()
	})
}

// sourceOf finds where key, or the mapping holding it, was set: a header
// in telemetry.otlp.headers is set where the mapping is.
func sourceOf(sources map[string]string, key string) string {
	for k := key; k != ""; {
		if src, ok := sources[k]; ok {
			return src
		}
		i := strings.LastIndexByte(k, '.')
		if i < 0 {
			break
		}
		k = k[:i]
	}
	return ""
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

func (c *AdminJobsListCmd) Run(r *runner, cl adminAPI) error {
	jobs, err := cl.Jobs(r.ctx)
	if err != nil {
		return err
	}
	return r.emit(jobs, func(w io.Writer) error {
		tw := newTable(w)
		tw.row("NAME", "EVERY", "RUNNING", "LAST RUN", "NEXT RUN", "LAST ERROR")
		for _, j := range jobs {
			tw.row(j.Name, j.Every, j.Running, fmtTimePtr(j.LastRun), fmtTimePtr(j.NextRun), dash(j.LastError))
		}
		return tw.flush()
	})
}

// AdminJobsRunCmd runs one job.
type AdminJobsRunCmd struct {
	Name string `arg:"" help:"sweep, gc, uploads-expire, oauth-cleanup, oidc-discovery, update-check, views-flush or views-prune."`
}

func (c *AdminJobsRunCmd) Run(r *runner, cl adminAPI) error {
	if err := cl.RunJob(r.ctx, c.Name); err != nil {
		return err
	}
	return r.emit(api.OK{OK: true}, func(w io.Writer) error {
		fmt.Fprintf(w, "triggered %s\n", c.Name)
		return nil
	})
}

// AdminAuditCmd reads the audit log.
type AdminAuditCmd struct {
	Since   string   `help:"Only events from this RFC 3339 time on, or from this long ago (such as 24h, 3d or 2w)." placeholder:"WHEN"`
	Until   string   `help:"Only events before this RFC 3339 time, or before this long ago." placeholder:"WHEN"`
	Slug    string   `help:"Only events for this artifact." placeholder:"SLUG"`
	Actor   string   `help:"Only events by this handle, or by admin or scheduler." placeholder:"HANDLE"`
	Subject string   `help:"Only events about the account of this handle, whoever acted." placeholder:"HANDLE"`
	Action  []string `help:"Only events with this action, such as key.issue, or this prefix, such as artifact. (repeatable)." sep:"none" placeholder:"ACTION"`
	Cursor  string   `help:"Page cursor from a previous audit." placeholder:"CURSOR"`
	Limit   int      `help:"Maximum events." placeholder:"N"`
}

// when reads an RFC 3339 time, or a duration counted back from now.
func (r *runner) when(flag, v string) (string, error) {
	if v == "" {
		return "", nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.Format(time.RFC3339), nil
	}
	if d, err := config.ParseDuration(v); err == nil {
		return r.now().Add(-time.Duration(d)).UTC().Format(time.RFC3339), nil
	}
	return "", usagef("admin audit: %s: want an RFC 3339 time or a duration such as 24h or 3d, got %q", flag, v)
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
	l, err := cl.Audit(r.ctx, apiclient.AuditParams{Since: since, Until: until, Slug: c.Slug, Actor: c.Actor, Subject: c.Subject,
		Actions: c.Action, Cursor: c.Cursor, Limit: c.Limit})
	if err != nil {
		return err
	}
	return r.emit(l, func(w io.Writer) error {
		tw := newTable(w)
		tw.row("AT", "ACTOR", "ACTION", "SLUG", "SUBJECT", "DETAILS")
		for _, e := range l.Events {
			tw.row(fmtTime(e.At), e.Actor, e.Action, dash(e.Slug), dash(e.Subject), dash(fmtMap(e.Details)))
		}
		if err := tw.flush(); err != nil {
			return err
		}
		if l.NextCursor != "" {
			fmt.Fprintf(r.stderr, "more results: --cursor %s\n", l.NextCursor)
		}
		return nil
	})
}

// AdminViewsCmd shows page views.
type AdminViewsCmd struct {
	Slug string `arg:"" optional:"" help:"An artifact; without one, the whole instance."`
	Days int    `default:"30" help:"How many days back, today included." placeholder:"N"`
}

func (c *AdminViewsCmd) Run(r *runner, cl adminAPI) error {
	if c.Slug == "" {
		s, err := cl.ViewSummary(r.ctx, c.Days)
		if err != nil {
			return err
		}
		if r.g.JSON {
			return r.printJSON(s)
		}
		fmt.Fprintf(r.stdout, "%s to %s: %s\n", s.From, s.To, viewLine(s.Totals))
		if len(s.Top) > 0 {
			fmt.Fprintln(r.stdout)
			tw := newTable(r.stdout)
			tw.row("SLUG", "VIEWS", "VISITORS", "MEMBERS", "SHARE LINKS")
			for _, a := range s.Top {
				tw.row(a.Slug, a.Views, a.Visitors, a.MemberViews, a.LinkViews)
			}
			if err := tw.flush(); err != nil {
				return err
			}
		}
		return printViewDays(r, s.Days)
	}

	a, err := cl.ArtifactViews(r.ctx, c.Slug, c.Days)
	if err != nil {
		return err
	}
	return r.emit(a, func(w io.Writer) error {
		fmt.Fprintf(w, "%s, %s to %s: %s\n", a.Slug, a.From, a.To, viewLine(a.Totals))
		if len(a.Files) > 0 {
			fmt.Fprintln(w)
			tw := newTable(w)
			tw.row("FILE", "VIEWS")
			for _, f := range a.Files {
				tw.row(f.Path, f.Views)
			}
			if err := tw.flush(); err != nil {
				return err
			}
		}
		if err := printViewDays(r, a.Days); err != nil {
			return err
		}
		if len(a.Readers) > 0 {
			fmt.Fprintln(w)
			tw := newTable(w)
			tw.row("READER", "NAME", "VIEWS", "FIRST OPENED", "LAST OPENED")
			for _, rd := range a.Readers {
				tw.row(rd.Handle, dash(rd.Name), rd.Views, fmtTime(rd.FirstAt), fmtTime(rd.LastAt))
			}
			return tw.flush()
		}
		return nil
	})
}

func viewLine(t api.ViewTotals) string {
	return fmt.Sprintf("%d views, %d daily visitors, %d by members, %d by share link", t.Views, t.Visitors, t.MemberViews, t.LinkViews)
}

// printViewDays lists the days with views; a month of empty days says
// nothing.
func printViewDays(r *runner, days []api.ViewDay) error {
	if !slices.ContainsFunc(days, func(d api.ViewDay) bool { return d.Views > 0 }) {
		return nil
	}
	fmt.Fprintln(r.stdout)
	tw := newTable(r.stdout)
	tw.row("DAY", "VIEWS", "VISITORS")
	for _, d := range days {
		if d.Views > 0 {
			tw.row(d.Day, d.Views, d.Visitors)
		}
	}
	return tw.flush()
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
	return r.emit(h, func(w io.Writer) error {
		fmt.Fprintf(w, "%s (server %s)\n", h.Status, dash(h.Version))
		return nil
	})
}
