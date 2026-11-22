# Dashboard

The dashboard is a set of web pages on the admin host for running an instance day to day: the people, their pages, who can open what, what happened, and whether anything needs attention. This page covers reaching it and signing in, then each of its five pages, task by task. It assumes a running instance ([installation guide](install.md)).

The dashboard does nothing the command line cannot. Every task below names the `kazari admin` command that does the same, so a script and a person at a browser can share the work.

## How the dashboard works

The dashboard is served at the root of the admin host, `https://admin.<base>/`, beside the [admin API](api.md#admin-api) under `/v1`. Only admins can open it.

Some work stays with whoever operates the host, and the dashboard reports its state without doing it:

- installing and upgrading Kazari;
- restoring a backup, and scheduling backups;
- changing the configuration, which means editing the file and restarting;
- reading the logs, which stay in the journal or the container's output.

The pages use a few words where the rest of the documentation uses others. A *page* is an artifact. The *bin* holds soft-deleted pages. A *connected app* is an OAuth grant, the connection of an MCP client. A *sign-in method* is a password, an identity at the provider or an API key.

## Reaching the dashboard

By default, the dashboard is at `https://admin.<base_domain>/` and needs no configuration.

### With a separate admin listener

If [`admin.listen`](configuration.md#the-separate-listener) moves the admin host to its own listener, Kazari cannot know how a browser reaches that address, and the dashboard is off until [`admin.public_url`](configuration.md#the-dashboards-address) says. For a listener on the loopback address, reached through an SSH tunnel, the settings are:

```yaml
admin:
  listen: "127.0.0.1:9443"
  public_url: http://localhost:9443
```

Restart Kazari, then open the tunnel from your own machine and the dashboard in a browser:

```sh
ssh -N -L 9443:127.0.0.1:9443 peter@art.example.com
```

The dashboard is then at `http://localhost:9443/`. Open it at exactly the address in `admin.public_url`: sign-in returns there, and a page opened as `http://127.0.0.1:9443/` does not accept the dashboard's forms.

Plain `http` is accepted in `admin.public_url` for `localhost`, a name under it or a loopback address only. To reach the listener at any other address, such as a name on a VPN, put a TLS proxy in front of it and set `admin.public_url` to its `https` address.

The default address follows the same rule. On an instance whose public URLs are plain `http` on a name other than `localhost`, the dashboard is off until the admin host is served over `https`.

### Turning the dashboard off

To serve the admin API without the dashboard, set `admin.dashboard` to `false`:

```yaml
admin:
  dashboard: false
```

## Signing in

Opening the dashboard sends the browser to the instance's own sign-in page, on `mcp.<base>`, and back. You sign in as you do anywhere else on the instance:

- at the identity provider, when the instance has one;
- with the handle or email and the password of your [built-in account](identity.md#built-in-accounts);
- with **Use an API key instead**, with one of your keys that holds the `admin` scope.

Your user must have the `admin` role. A member who signs in sees a page saying so, with a link to their [account page](identity.md#the-account-page).

The admin token (`auth.admin_token`) does not open the dashboard: it belongs to no person. It keeps working on the admin API.

A session lasts 12 hours. Kazari checks the role on every request, so an admin who is demoted or disabled loses the dashboard at once. **Sign out**, at the foot of the navigation, ends your sign-in on every host of the instance in that browser.

## Overview

The Overview is the first page. It lists what needs attention, then counts and recent activity.

### Needs attention

Each item is one sentence with its next step or a link to it. The dashboard raises an item for:

- a job whose last run failed;
- a certificate that has expired or expires within 14 days;
- less than 10% free space on the disk that holds the data directory;
- a person who has used more than 90% of their quota;
- an instance with a single admin, since nobody else can let them back in without the server;
- a health check that failed or warns;
- a newer release of Kazari.

When nothing needs attention, the section says so.

### Counts and activity

Below, the page shows the people by status, the pages by status and visibility, the storage in use, the certificate's mode and expiry, each job's last and next run, and the last ten events of the [activity log](#activity).

Storage has two numbers. The first is what quotas count: every version in full. The second is what the blobs take on disk, where a file that several versions share is stored once.

### Getting started

On a new instance the Overview shows three steps in place of empty tables: invite a person, connect an agent, publish a first page. The second step has the connector address to copy, with the command that adds it to an agent and the lines that set up the `kazari` command. Each step is marked done once it is.

## People

The People page lists everyone with an account: name and handle, email, role, status, sign-in methods, usage against quota, number of pages and when they were last active. A person's status is *active*, *invited* (an invite was made and no password is set yet) or *disabled*.

Search by name, handle or email, and filter by role and status. The filters are kept in the address, so a filtered list can be bookmarked. The command line has the same list:

```sh
kazari admin users list
```

### Inviting a person

On an instance with built-in accounts, **Invite a person** asks for a handle, a name, an optional email, a role and a quota. The quota is a size as the configuration writes them, such as `500MiB` or `2GiB`; `0` is no limit, and an empty field takes the instance's default. The next page shows the invite link, once. Copy it and send it to the person over a channel you trust: Kazari sends no email.

The link works once and for 7 days by default. Opening it changes nothing; setting a password uses it up. [Inviting a person](identity.md#inviting-a-person) has the rules in full. The command is:

```sh
kazari admin users invite ana --email ana@example.com --name "Ana Lima"
```

Links that have not been used are listed under the people, with their expiry. **Replace** makes a new link and ends the old one, and **Revoke** ends a link without a new one (`kazari admin users invite ana --replace`, `kazari admin invites revoke <id>`). A link leaves the list once it no longer works.

An invite is for a person who has no password yet. A person who has one gets a [reset link](#resetting-a-password) instead.

On an instance with an identity provider the button reads **Add a person**. It creates the user ahead of their first sign-in, with no link (`kazari admin users create`).

### A person's page

Opening a person shows everything about them in one place. From top to bottom:

- **Profile, role and quota**, with a form to change the name, email, role and quota (`kazari admin users update`).
- **Sign-in methods**: whether a password is set, the identities linked at the provider, and how many live keys and browser sessions they have.
- **API keys**, with issue, rotate and revoke.
- **Connected apps**, each with **Disconnect**.
- **Their pages**, with a form to hand all of them to someone else.
- **Recent activity**: what they did lately.

#### Issuing and revoking keys

**Issue a key** takes a label, what the key may do, optional slug patterns and an optional expiry in days. The key is shown once on the next page, with the lines that set it up in an agent and in the `kazari` command. This is where a key with the `admin` scope comes from (`kazari admin keys issue`).

**Rotate** replaces a key and keeps the old one working for the time you choose (`kazari admin keys rotate`). **Revoke** asks for confirmation on a page that lists what ends: the key, every key issued with it, and every app approved with one of them (`kazari admin keys revoke`).

#### Disconnecting an app

**Disconnect** revokes one connected app after a confirmation. The app's next request is refused, and the person connects it again by signing in from it (`kazari admin grants list`, `kazari admin grants revoke`).

#### Resetting a password

**Create a reset link** is for a person who lost their password and has no [passkey](identity.md#passkeys) or [recovery code](identity.md#recovery-codes) left, or who has none yet because they used API keys until now. The confirmation page has one option, off by default: **Also disconnect their apps and revoke their keys**, for an account someone else may have got into. It removes their passkeys and recovery codes too.

The link is shown once on the next page and works for 24 hours by default. Using it signs the person out of every browser (`kazari admin users reset-password ana`, with `--revoke-all` for the option).

#### Signing a person out everywhere

**Sign out everywhere** ends every browser session of the person. Their keys and connected apps keep working (`kazari admin users sign-out ana`).

#### Linking a sign-in at the provider

On an instance with an identity provider, **Link a sign-in at the provider** pins the person to one account there by its issuer and subject, and **Unlink** removes one, after a page that shows the issuer and subject you would need to link it again (`kazari admin identities link`, `kazari admin identities unlink`). [Admin-created users](identity.md#admin-created-users) explains when this is needed.

### Disabling a person and handing over their pages

**Disable** opens a confirmation page that lists what ends for good: the person's keys, their connected apps and sessions, and every share link they created. An invite or reset link that was waiting for them stops working as well. Enabling them later restores none of these.

The same page offers to hand all their pages to another person, which keeps the pages online under an owner who can still sign in. **Enable** lets a disabled person sign in again. The commands are:

```sh
kazari admin users transfer ana peter
kazari admin users update ana --disable
```

The dashboard has no "delete person". A person who leaves is disabled and their pages handed over, so the activity log and the versions they published keep their name.

### What the dashboard refuses

Kazari refuses two changes, in the dashboard and everywhere else:

- The last active admin cannot be disabled or demoted. Make another person an admin first.
- Nobody disables or demotes their own account. Another admin does.

A person's own page therefore has no **Disable** button for them.

## Pages

The Pages page lists every page on the instance, whoever owns it: slug, name, owner, who it is open to, status, versions, size, when it was updated and the date of its next lifecycle step. A page's status is *live*, *stale*, *unpublished* or *in the bin*.

Search by slug or name, and filter by status, visibility and owner. The list shows 50 pages at a time. The command is:

```sh
kazari admin artifacts list --search report --visibility members
```

### A page's own page

Opening a page shows its address, owner and visibility, its lifecycle in dates, its versions, its share links and its activity, with every action that changes it.

#### Changing who can open a page

**Who can open it** sets the visibility: public, unlisted or members only. A page that was hidden is published again with the same control (`kazari visibility <slug> members`).

#### Keeping or restoring a page

A page marked stale shows the date it moves to the bin, and **Keep the page** clears the mark (`kazari unstale <slug>`). A page in the bin shows the date it is removed for good, and **Restore the page** puts it back as it was (`kazari restore <slug>`).

#### Publishing an earlier version

The version list shows each version's number, date, author and size, marks the published one, and links to each version's own address. **Publish this version** asks for confirmation, then makes that version the one at the page's address (`kazari rollback <slug> <version>`).

Nothing is deleted, and the next publish from an agent goes live as usual.

#### Share links

The share links of a members-only page are listed with their label, creator, expiry and how often they were opened. **Create a share link** shows the new link once, and **Revoke link** ends one after a confirmation (`kazari share create`, `kazari share revoke`).

#### Handing a page over

**Hand over** gives the page to another person. Its size then counts against their quota (`kazari admin transfer <slug> <handle>`).

### The bin

The bin lists the pages that were deleted, or went stale and were not kept, each with the date it is removed for good. Nobody can open a page in the bin. **Restore** brings one back, and **Move to the bin** on a page's own page sends one there (`kazari rm <slug>`, `kazari restore <slug>`).

### Taking a page down

**Take down** is for a page that must go offline and stay offline. It hides the page, so its address answers `404` at once, and as a ticked option disables its owner. Hiding alone is not a lock: the owner, or under the `shared` write policy any member who can write, can publish the page again.

The commands are those of the [abuse response](ops.md#responding-to-abuse):

```sh
kazari admin hide q3-report
kazari admin users update ana --disable
```

**Hide the page** alone takes a page offline without disabling anyone, and the visibility control brings it back.

### Removing a page for good

**Remove for good** deletes every version of a page, and nothing brings it back. The confirmation page asks you to type the page's slug, and says what stays for a while:

- copies that browsers and CDNs already hold, for up to `lifecycle.tombstone`, 90 days by default;
- the slug, which no new page can take for that long;
- the files on the server's disk, until the `gc` job next runs.

The command is `kazari rm <slug> --hard`, with an admin's key.

## Activity

The Activity page is the [audit log](ops.md#audit-log), newest first. Each row reads as a sentence, such as "ana published version 4 of q3-report", with the recorded action beside it, and links to the person and the page.

Filter by person, page, kind of action and a range of dates. Dates are whole days in UTC, and the last day is included. The command is:

```sh
kazari admin audit --actor ana --action artifact. --since 2026-11-01T00:00:00Z
```

Nothing prunes the log: events are kept for as long as the instance's database, and a backup includes them.

### Exporting the log

**Export as CSV** downloads the events the current filter matches, for a spreadsheet or an archive. The export is itself recorded, as `audit.export`. From the command line, `kazari --json admin audit` prints the same events as JSON.

## System

The System page says how the instance is doing and what it runs with.

### Checking health

The health section shows the result of the readiness checks: the configuration, the DNS records, the DNS token, the certificate and the disk. Each line that warns or fails says what to do next.

The server runs the checks once when it starts and keeps a result for 15 minutes. **Run the checks now** runs them again, which can take a minute. [Checking readiness](ops.md#checking-readiness) lists the checks, and the command is:

```sh
kazari admin ready
```

### Running a job

The jobs section lists the [scheduled jobs](ops.md#scheduled-jobs) with their interval, last run, next run and last error. **Run now** runs one and waits for it (`kazari admin jobs run sweep`).

### Downloading a backup

The backup section shows the size to expect, and when the last backup was taken and by whom. **Download a backup** sends the browser an archive named `kazari-<base_domain>-<date>.tar`, with the database and every blob.

The download leaves out the certificate's private key, which `kazari admin backup` on the server includes. An archive holds every page and the hash of every password and key, so keep it as a secret.

Restoring an archive, and backing up on a schedule, are done on the host ([backing up and restoring](ops.md#backing-up-and-restoring)).

### About and the update check

The about section shows the version and commit, when the server started, the data directory and the connector address, with links to the documentation and the release notes.

It also says whether a newer release exists. Kazari asks GitHub once a day, and the request carries nothing about the instance ([the update check](ops.md#the-update-check)). Upgrading is done on the host ([upgrading](ops.md#upgrading)).

### Reading the settings

The settings section shows the configuration the server runs with, secrets hidden. It is read only: to change a setting, edit the configuration file on the host and restart ([configuration](configuration.md)). The command is `kazari admin settings`.

### Copying diagnostics

**Copy diagnostics** copies the version, the settings with their secrets hidden and the results of the health checks as text, to paste for whoever you ask for help.

## Troubleshooting

Each entry is named for what the browser or the log shows.

### The admin host answers `404` at `/`

The dashboard is off, for one of three reasons:

- `admin.dashboard` is `false`.
- `admin.listen` is set and `admin.public_url` is not. Set [`admin.public_url`](#with-a-separate-admin-listener) and restart.
- The dashboard's address is plain `http` on a host that is not this machine, for example with `external_scheme: http` on a public name. Serve the admin host over `https`, or set `admin.public_url` to an `https` address, and restart.

In the last two cases the log says so at start, with a line that begins `the dashboard is off`, and the `dashboard` line of `kazari check` and `kazari admin ready` says which.

### "Not an admin"

You signed in, and your user is a member, or you signed in with an API key that does not hold the `admin` scope. Sign out, then sign in with your password or with an admin key. To make a person an admin, another admin changes their role, or on the server:

```sh
sudo kazari admin users update ana --role admin
```

### "This form has expired"

The form was sent from a page that is not the dashboard's own, or from a page loaded before your last sign-in. With `admin.listen`, it also appears when the dashboard is open at another address than `admin.public_url`. Go back, reload the page and send the form again.

A form that makes a key or a link also expires 12 hours after its page was loaded, and when the server restarts. Reload the page and send it again.

### "This form was already sent"

A form that makes a key, an invite, a reset link or a share link was sent a second time, usually by reloading the page that showed what it made. Nothing new was made, and the key or link from the first time still works. To make another, open the form again.

### The only admin cannot sign in

The admin socket on the server is the way back in: it needs no credential. For a lost password, make a reset link there and open it:

```sh
sudo kazari admin users reset-password peter
```

If no admin is left who can sign in at all, promote a person on the server with `sudo kazari admin users update ana --role admin`. `kazari admin setup-link` is for a fresh instance only and fails once any user exists.

### A link no longer works

An invite, reset or share link is shown once, and Kazari keeps no copy. A lost or lapsed one is replaced: **Replace** under People, **Create a reset link** on the person's page, or **Create a share link** on the page's own page.

## Next steps

- [Sign-in and identity](identity.md): built-in accounts, the account page and connecting an identity provider.
- [Operations](ops.md): backups, upgrades, readiness, jobs and the audit log.
- [CLI](cli.md#admin-commands): every `kazari admin` command.
- [Configuration](configuration.md#admin): the `admin` keys.
