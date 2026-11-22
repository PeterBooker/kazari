package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/PeterBooker/kazari/internal/api"
	"github.com/PeterBooker/kazari/internal/backup"
	"github.com/PeterBooker/kazari/internal/dirlock"
	"github.com/PeterBooker/kazari/internal/preflight"
)

// printLink prints a new link's address alone on stdout, so it can be piped,
// and what the person handing it over needs to know on stderr.
func (r *runner) printLink(v any, l *api.CreatedLink, what string) error {
	if r.g.JSON {
		return r.printJSON(v)
	}
	fmt.Fprintln(r.stdout, l.URL)
	fmt.Fprintf(termSafe{r.stderr}, "%s: it works once, until %s; send it over a channel you trust\n", what, fmtTime(l.ExpiresAt))
	return nil
}

// AdminUsersShowCmd shows one user.
type AdminUsersShowCmd struct {
	Handle string `arg:"" help:"The user's handle."`
}

func (c *AdminUsersShowCmd) Run(r *runner) error {
	cl, err := r.adminClient(true)
	if err != nil {
		return err
	}
	u, err := cl.GetUser(r.ctx, c.Handle)
	if err != nil {
		return err
	}
	return r.printUser(u)
}

// AdminUsersInviteCmd invites a person.
type AdminUsersInviteCmd struct {
	Handle  string `arg:"" help:"The user's handle."`
	Email   string `help:"Email address; they can sign in with it or with the handle." placeholder:"EMAIL"`
	Name    string `help:"Display name." placeholder:"NAME"`
	Role    string `help:"member (default) or admin." placeholder:"ROLE"`
	Quota   string `help:"Storage quota, such as 500MiB (default: the server's)." placeholder:"SIZE"`
	Replace bool   `help:"Make a new invite link for a user who exists and has not set a password yet; the link before it stops working. A user who has a password gets a reset link instead (admin users reset-password)."`
}

func (c *AdminUsersInviteCmd) Run(r *runner) error {
	if c.Replace {
		if c.Email != "" || c.Name != "" || c.Role != "" || c.Quota != "" {
			return usagef("admin users invite: --replace makes a link for an existing user and takes no other flag; change the user with admin users update")
		}
		cl, err := r.adminClient(true)
		if err != nil {
			return err
		}
		l, err := cl.IssueLink(r.ctx, c.Handle, api.LinkRequest{Purpose: "invite"})
		if err != nil {
			return err
		}
		return r.printLink(l, l, "new invite link for "+c.Handle)
	}
	req := api.InviteRequest{Handle: c.Handle, Email: c.Email, DisplayName: c.Name, Role: c.Role}
	if c.Quota != "" {
		n, err := parseSize(c.Quota)
		if err != nil {
			return usagef("admin users invite: --quota: %v", err)
		}
		req.QuotaBytes = &n
	}
	cl, err := r.adminClient(true)
	if err != nil {
		return err
	}
	out, err := cl.Invite(r.ctx, req)
	if err != nil {
		return err
	}
	return r.printLink(out, &out.Link, fmt.Sprintf("invited %s as %s", out.User.Handle, out.User.Role))
}

// AdminUsersResetPasswordCmd makes a reset link.
type AdminUsersResetPasswordCmd struct {
	Handle    string `arg:"" help:"The user's handle."`
	RevokeAll bool   `name:"revoke-all" help:"Using the link also revokes the user's API keys and disconnects their apps."`
}

func (c *AdminUsersResetPasswordCmd) Run(r *runner) error {
	cl, err := r.adminClient(true)
	if err != nil {
		return err
	}
	l, err := cl.IssueLink(r.ctx, c.Handle, api.LinkRequest{Purpose: "reset", RevokeAll: c.RevokeAll})
	if err != nil {
		return err
	}
	what := "password reset link for " + c.Handle
	if l.RevokeAll {
		what += ", which also revokes their keys and connected apps"
	}
	return r.printLink(l, l, what)
}

// AdminUsersSignOutCmd ends a user's browser sessions.
type AdminUsersSignOutCmd struct {
	Handle string `arg:"" help:"The user's handle."`
}

func (c *AdminUsersSignOutCmd) Run(r *runner) error {
	cl, err := r.adminClient(true)
	if err != nil {
		return err
	}
	if err := cl.EndSessions(r.ctx, c.Handle); err != nil {
		return err
	}
	if r.g.JSON {
		return r.printJSON(api.OK{OK: true})
	}
	fmt.Fprintf(r.stdout, "signed %s out of every browser\n", c.Handle)
	return nil
}

// AdminInvitesCmd manages the links that have not been used.
type AdminInvitesCmd struct {
	List   AdminInvitesListCmd   `cmd:"" help:"List the invite, reset and setup links that still work."`
	Revoke AdminInvitesRevokeCmd `cmd:"" help:"End a link by its id."`
}

// AdminInvitesListCmd lists the live links.
type AdminInvitesListCmd struct{}

func (c *AdminInvitesListCmd) Run(r *runner) error {
	cl, err := r.adminClient(true)
	if err != nil {
		return err
	}
	links, err := cl.ListLinks(r.ctx)
	if err != nil {
		return err
	}
	if r.g.JSON {
		return r.printJSON(links)
	}
	tw := newTable(r.stdout)
	fmt.Fprintln(tw, "ID\tUSER\tPURPOSE\tBY\tCREATED\tEXPIRES")
	for _, l := range links {
		purpose := l.Purpose
		if l.RevokeAll {
			purpose += " (revokes keys and apps)"
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", l.ID, dash(l.User), purpose, dash(l.CreatedBy), fmtTime(l.CreatedAt), fmtTime(l.ExpiresAt))
	}
	return tw.Flush()
}

// AdminInvitesRevokeCmd ends a link.
type AdminInvitesRevokeCmd struct {
	ID int64 `arg:"" help:"The link's id, from invites list."`
}

func (c *AdminInvitesRevokeCmd) Run(r *runner) error {
	cl, err := r.adminClient(true)
	if err != nil {
		return err
	}
	if err := cl.RevokeLink(r.ctx, c.ID); err != nil {
		return err
	}
	if r.g.JSON {
		return r.printJSON(api.OK{OK: true})
	}
	fmt.Fprintf(r.stdout, "revoked link %d\n", c.ID)
	return nil
}

// AdminSetupLinkCmd makes the first-run setup link.
type AdminSetupLinkCmd struct{}

func (c *AdminSetupLinkCmd) Run(r *runner) error {
	cl, err := r.adminClient(true)
	if err != nil {
		return err
	}
	l, err := cl.SetupLink(r.ctx)
	if err != nil {
		return err
	}
	return r.printLink(l, l, "setup link: whoever opens it becomes the first admin")
}

// AdminBackupCmd writes a backup into a directory, or a tar stream to
// standard output.
type AdminBackupCmd struct {
	Dir     string `arg:"" help:"Directory to write into; it must be empty or not exist yet. - writes the archive to standard output as tar."`
	NoCerts bool   `name:"no-certs" help:"Leave out certmagic/, the certificate and its private key, which is included over the admin socket."`
}

func (c *AdminBackupCmd) Run(r *runner) error {
	if c.Dir == "-" {
		return c.stream(r)
	}
	// Before the server is asked: a backup started for a directory that
	// cannot take it copies the whole database for nothing.
	if err := backupTarget(c.Dir); err != nil {
		return err
	}
	cl, err := r.adminClient(true)
	if err != nil {
		return err
	}
	// The server hands the private key to the socket alone, so over a URL
	// asking would only be refused.
	certs := cl.local && !c.NoCerts
	body, err := cl.Backup(r.ctx, certs)
	if err != nil {
		return err
	}
	defer body.Close()
	if err := backup.Extract(body, c.Dir); err != nil {
		switch {
		case errors.Is(err, backup.ErrNotEmpty):
			return usagef("admin backup: %s is not empty; name an empty directory or one that does not exist yet", c.Dir)
		case errors.Is(err, backup.ErrUnsafeEntry):
			return fmt.Errorf("admin backup: the server sent an archive this command will not unpack: %w", err)
		}
		return fmt.Errorf("admin backup: the backup is incomplete, remove %s and run it again: %w", c.Dir, err)
	}
	files, size, err := treeStats(c.Dir)
	if err != nil {
		return fmt.Errorf("admin backup: %w", err)
	}
	if !cl.local {
		fmt.Fprintln(r.stderr, "certmagic/ is not included: the certificate's private key leaves the server only over the admin socket; "+
			"an instance restored from this backup asks for a new certificate")
	}
	if r.g.JSON {
		// What the archive held, not what was asked for: a server behind a
		// proxy has no certificate to give.
		_, err := os.Stat(filepath.Join(c.Dir, "certmagic"))
		return r.printJSON(struct {
			Dir          string `json:"dir"`
			Files        int    `json:"files"`
			Bytes        int64  `json:"bytes"`
			Certificates bool   `json:"certificates"`
		}{c.Dir, files, size, err == nil})
	}
	fmt.Fprintf(r.stdout, "wrote %d files, %s, to %s\n", files, humanSize(size), c.Dir)
	return nil
}

// stream writes the archive to standard output, for a backup that leaves a
// container through docker exec or goes straight to other storage.
func (c *AdminBackupCmd) stream(r *runner) error {
	if f, ok := r.stdout.(*os.File); ok {
		if fi, err := f.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
			return usagef("admin backup -: standard output is a terminal; redirect it to a file, such as > kazari-backup.tar")
		}
	}
	cl, err := r.adminClient(true)
	if err != nil {
		return err
	}
	body, err := cl.Backup(r.ctx, cl.local && !c.NoCerts)
	if err != nil {
		return err
	}
	defer body.Close()
	if _, err := io.Copy(r.stdout, body); err != nil {
		return fmt.Errorf("admin backup: the archive is incomplete: %w", err)
	}
	if !cl.local {
		fmt.Fprintln(r.stderr, "certmagic/ is not included: the certificate's private key leaves the server only over the admin socket")
	}
	return nil
}

// backupTarget reports why dir cannot take a backup: it must be a directory
// that is empty, or not exist yet.
func backupTarget(dir string) error {
	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("admin backup: %w", err)
	case !info.IsDir():
		return usagef("admin backup: %s is a file; name an empty directory or one that does not exist yet", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("admin backup: %w", err)
	}
	if len(entries) > 0 {
		return usagef("admin backup: %s is not empty; name an empty directory or one that does not exist yet", dir)
	}
	return nil
}

// treeStats counts the regular files under dir and sums their sizes.
func treeStats(dir string) (files int, size int64, err error) {
	err = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		files++
		size += info.Size()
		return nil
	})
	return files, size, err
}

// AdminRestoreBackupCmd restores a backup directory into the data
// directory of a stopped server on this host.
type AdminRestoreBackupCmd struct {
	Dir     string `arg:"" help:"Backup directory that kazari admin backup wrote."`
	DataDir string `name:"data-dir" help:"Data directory to restore into (default: data_dir of the server's configuration on this host, /var/lib/kazari unless it says otherwise)." placeholder:"PATH"`
	Force   bool   `help:"Replace the database, blobs and staged uploads the data directory holds, and its certificates when the backup has some."`
}

func (c *AdminRestoreBackupCmd) Run(r *runner) error {
	dataDir := c.DataDir
	if dataDir == "" {
		cfg, err := r.serverConfig()
		if err != nil {
			return usagef("admin restore-backup: %v; name the data directory with --data-dir", err)
		}
		dataDir = cfg.DataDir
	}
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return fmt.Errorf("admin restore-backup: %w", err)
	}
	lock, err := dirlock.Acquire(dataDir)
	var locked *dirlock.LockedError
	switch {
	case errors.As(err, &locked):
		return usagef("admin restore-backup: %v; stop the server first (sudo systemctl stop kazari, or docker compose stop kazari)", err)
	case errors.Is(err, fs.ErrPermission):
		return usagef("admin restore-backup: %v; run it as root, for example with sudo", err)
	case err != nil:
		return fmt.Errorf("admin restore-backup: %w", err)
	}
	defer lock.Release()
	sum, err := backup.Restore(c.Dir, dataDir, backup.RestoreOptions{Replace: c.Force})
	switch {
	case errors.Is(err, backup.ErrNotBackup):
		return usagef("admin restore-backup: %s holds no kazari.db; name a directory that kazari admin backup wrote", c.Dir)
	case errors.Is(err, backup.ErrNotEmpty):
		return usagef("admin restore-backup: %s already holds an instance; add --force to replace it with the backup", dataDir)
	case errors.Is(err, fs.ErrPermission):
		return usagef("admin restore-backup: %v; run it as root, for example with sudo", err)
	case err != nil:
		return err
	}
	if r.g.JSON {
		return r.printJSON(struct {
			DataDir string `json:"data_dir"`
			Files   int    `json:"files"`
			Bytes   int64  `json:"bytes"`
		}{dataDir, sum.Files, sum.Bytes})
	}
	fmt.Fprintf(r.stdout, "restored %d files, %s, into %s; start the server, then run kazari admin ready\n", sum.Files, humanSize(sum.Bytes), dataDir)
	return nil
}

// AdminReadyCmd runs the readiness checks.
type AdminReadyCmd struct {
	Fresh bool `help:"Run the checks now; without it the server answers from its last run when that is recent."`
}

func (c *AdminReadyCmd) Run(r *runner) error {
	cl, err := r.adminClient(true)
	if err != nil {
		return err
	}
	out, err := cl.Ready(r.ctx, c.Fresh)
	if err != nil {
		return err
	}
	failed := errors.New("readiness checks failed")
	if r.g.JSON {
		if err := r.printJSON(out); err != nil {
			return err
		}
		if !out.Ready {
			return failed
		}
		return nil
	}
	findings := make([]preflight.Finding, 0, len(out.Checks))
	for _, ch := range out.Checks {
		findings = append(findings, preflight.Finding{Check: ch.Check, Status: preflight.Status(ch.Status), Detail: ch.Detail})
	}
	preflight.Write(termSafe{r.stdout}, findings)
	fmt.Fprintf(r.stdout, "checked %s\n", fmtTime(out.CheckedAt))
	if u := out.Update; u != nil && u.Newer {
		fmt.Fprintf(termSafe{r.stdout}, "a newer release is available: %s (checked %s)\n", u.Latest, fmtTime(u.CheckedAt))
	}
	if !out.Ready {
		return failed
	}
	return nil
}
