# Changelog

What changed in each release of Kazari, newest first. Each release lists what is new, what behaves differently and what was fixed; the changes that need something from you before or after upgrading are under **Upgrading**. [Upgrading](docs/ops.md#upgrading) describes the upgrade itself.

Versions follow [semantic versioning](https://semver.org/). Until 1.0.0, a minor release can change behaviour, and the upgrade notes say how.

## 0.8.0

Released 2026-11-22.

Pages can now be reviewed before they go live, and old versions can be deleted to free quota. Publishing says when a page will not render as intended, members manage their own pages from the account page, and the CLI gains `kazari deploy`, `kazari login` and exit codes for scripts. Running an instance gets stricter checks at start and a server that no longer waits on its identity provider.

### Upgrading

Most of what changes for an instance is in [Upgrading to 0.8](docs/ops.md#upgrading-to-08):

- **Listener addresses are `host:port`.** A bare port such as `8080` in `listen.http`, `listen.https` or `admin.listen` fails validation; write `127.0.0.1:8080` or `:8080`.
- **Placeholder domains stop the server.** A `base_domain` or `tls.email` under `example.com`, `example.org` or `example.net` fails validation, and `tls.email` is optional.
- **One server per data directory.** A second server on the same `data_dir` stops with `another kazari (pid N) is using <data_dir>`.
- **The identity provider no longer stops the start.** Sign-in answers "temporarily unavailable" until the new `oidc-discovery` job reaches it, and `admin_emails` makes its addresses admins whatever `allowed_domains` says ([provisioning policy](docs/identity.md#provisioning-policy)).
- **A change after a rollback names its base.** While an earlier version than the latest is published, `update_artifact`, `edit_file`, `PUT /v1/artifacts/{slug}` and the edit route fail with `published_not_latest` unless they set `base_version`, and `kazari update`, `deploy` and `edit` need `--base` ([after a rollback](docs/api.md#after-a-rollback)). The same holds while a version is staged.
- **`kazari.csp` adds sources to the instance policy** instead of replacing it, directive by directive, and refuses `*` and bare schemes such as `https:` ([per-artifact CSP](docs/configuration.md#per-artifact-csp)). Check the pages that set it.
- **CLI exit codes tell failures apart**: `2` for usage, `3` not found, `4` conflict, `5` rate limited, `6` not signed in or not allowed, and `1` for anything else ([exit codes](docs/cli.md#exit-codes)).
- **The first start runs four migrations.** One records the uploader of every stored file, so on an instance with many versions it takes longer than usual.

### New

- **Staged versions.** `stage: true` on `update_artifact`, `edit_file` and `PUT /v1/artifacts/{slug}`, or `--stage` on `kazari update`, `deploy` and `edit`, adds a version without publishing it. Its version address opens only for people signed in to the instance, never through a share link, until `publish_version` or `kazari rollback` makes it live ([staging a version](docs/api.md#staging-a-version)).
- **Deleting versions.** `DELETE /v1/artifacts/{slug}/versions/{n}`, `POST /v1/artifacts/{slug}/versions/prune` with `keep` and `dry_run`, `kazari versions rm|prune` and **Delete this version** on the dashboard free the quota old versions take. The published and the latest version always stay, and a prune keeps staged versions newer than the published one. A page's `kazari.keep_versions` metadata has every sweep prune it ([versions](docs/cli.md#versions)). Deleting versions of another member's page, or setting the key on it, needs `artifacts:delete`.
- **Warnings after a publish**: a missing root `index.html`, files a page or stylesheet refers to that are not in the version, and origins the page's Content-Security-Policy blocks.
- **`kazari deploy`** makes an artifact match a directory, creating it the first time and deleting files gone from the directory after that, with `--dry-run` and a `.kazariignore`. `kazari publish` takes a single file, and bundles may be zip or tar.gz.
- **`kazari login` and `kazari logout`** keep the API address and a key in the client configuration.
- **Edits to several files in one version**: `update_artifact` takes `edits` per file, as `edit_file` does for one.
- **Members manage their own pages** at `/account/pages/{slug}` on the apex: visibility, share links, versions and taking a page offline. The account page shows the address to connect an agent.
- **`kazari admin restore-backup`** restores a backup into a stopped instance, and `kazari admin backup -` streams one as tar.
- **Configuration sources**: `kazari admin settings`, `GET /v1/settings/sources` and `kazari check` say where each value that is not a default was set.
- **`kazari.env` beside the configuration file** is read by `serve`, `check` and `admin`, as `/etc/kazari/kazari.env` beside `/etc/kazari/kazari.yaml`.
- **One file through an archive URL**: `archive_path` in `POST /v1/uploads` and `begin_upload` sends a single file to `archive_url` without zipping it.
- **Creating on the first deploy**: `PUT /v1/artifacts/{slug}?create=true` creates a missing artifact, and `mirror` deletes the files the request does not carry. `GET /v1/artifacts/{slug}` sends an `ETag` with the latest version number.
- OAuth responses to a client's redirect URI carry `iss`, and the server metadata advertises `authorization_response_iss_parameter_supported`.
- The changelog is published at `/docs/changelog/`, ships in the archives and packages, and is linked from the dashboard.

### Changed

- **A publish whose files match the latest version adds no version**, and says `unchanged`. A version added without a label gets one listing what changed, and a page created without a name takes its `<title>`.
- **The default `serve.csp` allows stylesheets and fonts from cdnjs, jsDelivr and unpkg**, as it already allowed their scripts.
- **The MCP tool list follows the credential's scopes**: a read-only key sees no write tools. The tool descriptions are shorter, `read_file` names the version it read, and arguments in common wrong shapes are read as meant.
- **MCP lists, discovery and resources are cached privately**: `cacheScope` is `private`, with `ttlMs` of five minutes, and zero for an artifact's record and files.
- **Durations accept weeks and days** (`2w`, `3d`) everywhere, including `kazari admin audit --since` and `--until`, and sizes in errors are written in units such as `25 MiB`. A file over the size limit is named with its own size when it is known up front.
- **A change to a page in the bin** says how to restore it, to the people who could restore it.
- **The CLI retries** after `429` and `503` as `Retry-After` says, up to twice, and `--timeout` limits a request.
- **The systemd unit keeps restarting** through a long outage, ten seconds apart.
- `kazari admin … --help` leaves out the flags of the user API.

### Fixed

- An upload given to a publishing call that fails stays open, so the call can be sent again with the same `upload_id`.
- The restore and upgrade steps in the operations guide work as written, including a backup without certificates.
- A forgotten password set with a recovery code returns to the sign-in that asked for it.

## 0.7.0

Released 2026-11-22.

Kazari now runs day to day from a browser and without an identity provider: a dashboard on the admin host, built-in accounts with passwords and passkeys, and page views. Keys can be limited to a few slugs for agents that publish on a schedule, and the MCP server returns smaller results.

### Upgrading

- **Sign-in is a password on instances without `auth.oidc`.** The sign-in page asks for a handle and a password, with **Use an API key instead** under the form. Existing keys keep working. [Upgrading to 0.7](docs/ops.md#upgrading-to-07) says how to give people a password, or how to keep sign-in by API key alone with `auth.local.enabled: false`.
- **The dashboard needs `admin.public_url` when `admin.listen` is set.** Without it, the dashboard stays off ([the dashboard's address](docs/configuration.md#the-dashboards-address)).
- **`GET /v1/audit` answers an object**, `{"events": [...], "next_cursor": "..."}`, instead of an array. The same goes for `kazari --json admin audit`.
- **Client commands read a dotenv file only when `KAZARI_ENV_FILE` names it.** They no longer read `./.env`. For `serve` and `check` with a configuration file, `./.env` fills `${NAME}` references only; `KAZARI_*` overrides come from the process environment or a named env file.
- **Usage counts 4 KiB for each file of each version**, on top of the file sizes, so `used_bytes` rises on upgrade. Check the quotas of people who are close to theirs ([quotas](docs/ops.md#quotas)).
- **Users named `admin` or `scheduler` are renamed** to the first free suffix, such as `admin-2`, at the first start ([users named admin or scheduler](docs/ops.md#users-named-admin-or-scheduler)).
- **The MCP endpoint refuses JSON-RPC batches** with `400 invalid_argument`. Clients on the 2025-03-26 and earlier revisions that batch calls send one per request.
- **The release image has no per-architecture tags.** `<version>-amd64` and `<version>-arm64` are no longer pushed; `<version>` and `latest` cover both platforms.

### New

- **A dashboard** at the root of the admin host: Overview, People, Pages, Activity and System. It does nothing the admin API and the CLI cannot ([dashboard](docs/dashboard.md)).
- **Built-in accounts**: passwords, single-use invite and reset links that need no email server, a setup link for the first admin on a fresh instance, and account pages at `/account/` on the apex. On by default when `auth.oidc` is absent ([built-in accounts](docs/identity.md#built-in-accounts)).
- **Passkeys and recovery codes** for built-in accounts. A person signs in with a passkey, and sets a new password with a recovery code when they forget it, without an admin.
- **Page views**: daily totals per page and per file, daily visitors, views through share links, and which members opened a members page, on the dashboard and with `kazari admin views`. No request log, IP address or user agent is stored. The `analytics` section turns it off or changes the retention, 366 days by default.
- **Keys for agents that run unattended.** A key can be limited to slugs (`--slugs 'weekly-*'`), people issue, rotate and revoke their own keys with `kazari keys`, and a key can sign in an MCP connection with its own scopes and expiry. Token lifetimes are set in `auth.tokens`, and `kazari admin grants` lists and revokes OAuth grants ([scheduled and unattended agents](docs/mcp.md#scheduled-and-unattended-agents)).
- **A built-in backup**, `kazari admin backup`, which needs no `sqlite3` binary.
- **Readiness**: `kazari admin ready` and `GET /v1/ready` run the checks of `kazari check` against the running server.
- **An update check**, a daily job that asks GitHub for the newest release and shows it on the dashboard. `admin.update_check: false` turns it off.
- **New commands**: `kazari restore`, `kazari admin identities link|unlink`, and `kazari admin users show|transfer|invite|reset-password|sign-out`. The artifact list takes a search text and a visibility filter, and the audit log takes action and time filters and a cursor.
- **Anchor inserts**: `after` and `before` in `edit_file`, the API and `kazari edit --after|--before` insert lines next to an anchor.
- **Release provenance**: every released file has a build provenance attestation, and the image carries an SBOM. Archives and packages are reproducible from the commit.

### Changed

- **MCP write tools return a summary** (URL, version, visibility and state) instead of the whole artifact record, and edit excerpts no longer repeat the text the agent sent. The skill and the tool descriptions tell agents to edit from the last result instead of reading the file first.
- **Marking another member's artifact stale needs `artifacts:delete`.** Owners and admins are unaffected. Restoring an artifact without files now clears its stale mark unless `keep_stale` is set.
- **Visibility and share links need the new `artifacts:share` scope.** Every stored credential with `artifacts:write` gets it at upgrade.
- **Refresh tokens last 90 days** by default (`auth.tokens.refresh_ttl`).
- **Signing out moved to the MCP host.** `/oauth/signout` on `mcp.<base>` ends the sign-in on every host of that browser; signing out on an artifact host ends only that host's view.
- **Disabling a user revokes the share links they created.** Enabling them again does not restore the links.
- **File lists come in pages** of whole versions, newest first, up to 5,000 files: `next_files_before` in the response goes back as `files_before`.
- **OAuth limits**: redirect URIs and URL client IDs are limited to 2 KiB and `state` to 1 KiB, and clients that never complete a grant are removed after one day.
- **Imports have a size budget** for what rewriting a page adds, 4 MiB or `limits.max_file_bytes` if lower, and fail with `too_large` past it.
- **Members pages send `Cross-Origin-Opener-Policy: same-origin`** and `Cross-Origin-Resource-Policy: same-origin` on every response.
- **Exported OTLP logs redact identities** in paths and quoted error text, as spans already did. Stderr keeps the full values.
- **`serve --local` runs on `kazari.localhost`**, where passkeys work over plain HTTP.
- Text in MCP results no longer escapes `<`, `>` and `&`.

### Fixed

- A publish that fails partway deletes the files it stored, instead of leaving them for the garbage collector.
- A service worker registered by an artifact is replaced once its author can no longer change the artifact.
- Signing out ends every sign-in of the browser, also those finished in another tab.
- Edits that are refused, reads with anchors and `read_file` windows hold a small, fixed amount of memory, whatever the file size.
- Zip archives whose entries add up to more than the archive are refused.
- Errors from the DuckDNS provider no longer contain the account token.
- The CLI replaces control characters in server text in errors and in the import report.
- The operator's admin token and socket no longer make a user involved in the artifacts they hide or transfer.
- Edit excerpts keep an empty last line, two cases of the import rewriter are fixed, and an aborted response is no longer logged as a panic.
- The dashboard's meters and the website's search, menu, tables and code buttons work with a keyboard and a screen reader.

## 0.6.1

Released 2026-11-02.

### Changed

- The MCP server no longer advertises `list_changed`, so clients do not hold a long-lived event stream open waiting for changes that never come.

## 0.6.0

Released 2026-10-31.

Kazari now reports what it does over OpenTelemetry, so an operator can follow an agent's request through the tracing and metrics tools they already run ([metrics](docs/ops.md#metrics)).

### Upgrading

- **Every metric is renamed** to the OpenTelemetry semantic conventions. [Upgrading from before 0.6](docs/ops.md#upgrading-from-before-06) maps the old names to the new.
- **`/metrics` on the admin host needs a token**, `telemetry.metrics.token`. `admin.listen` and the admin socket stay open.

### New

- **Traces** of requests, MCP tool calls, the store, blobs, outbound fetches, jobs and startup, exported over OTLP with HTTP or gRPC. The control plane is sampled in full and artifact hosts at 1%.
- **Metrics** for HTTP and MCP, sign-ins, rate limits, API errors and artifact actions, with gauges for the census, the connection pool, jobs and the certificate.
- **Optional OTLP log export** (`telemetry.logs.otlp`), without identity fields.
- The `telemetry` keys win; unset ones fall back to the standard `OTEL_*` variables. `kazari check` tests the OTLP endpoint, and the repository has an example collector configuration and a Compose profile.

## 0.5.0

Released 2026-10-30.

Agents in a sandbox can now send a whole site from their shell in one request, and this release carries the fixes from the first review of the code.

### Upgrading

- **An admin signs in through a listed email or a linked identity.** An existing admin links by email only when `auth.admin_emails` names the address; any other admin needs an identity linked through the admin API ([provisioning policy](docs/identity.md#provisioning-policy)).
- **Only an admin transfers an artifact**, and a version counts against the owner's quota whoever adds it.
- **Reserved slugs stop serving.** An artifact whose slug is later added to `serve.reserved_slugs` answers with the instance's 404 page.
- **The handles `admin` and `scheduler` are reserved** for new users.
- **Agents in a sandbox with an egress allow-list** need `api.<base>` on it to use archive uploads.

### New

- **Archive uploads to a signed URL.** `begin_upload` with `archive_sha256` and `archive_bytes` returns a single-use URL that takes a zip or tar.gz archive with `curl`, which the agent then publishes with `upload_id` as before.

### Changed

- Changing an artifact that is offline needs read access to it first, under either write policy.
- An invalid authorization request shows an error page instead of redirecting to the client.
- Reusing an authorization code revokes the tokens it was exchanged for.
- OAuth clients that never complete a grant are removed after seven days.

### Fixed

- The sweep no longer purges an artifact restored after it was listed.
- A chunked upload stores exactly the bytes it recorded.
- A tar entry's declared size counts against the bundle limit before it is read.

## 0.4.0

Released 2026-10-28.

Agents can now copy an existing web page and change it, and work with large files over MCP.

### New

- **`import_page`** copies a page with its stylesheets, fonts, images and media under `assets/`, and returns an outline of its sections for the edits that follow. Over REST it is `source_url` on `POST /v1/artifacts`; on the command line, `kazari import`.
- **Files by URL**: `files[].url` on create and update downloads the file before the publish. The `fetch` section of the configuration controls both.
- **`read_file` windows**: `start_line` and `end_line` read part of a text file of any size, with anchors that `edit_file` accepts.
- **`discard_upload`** removes an upload the agent no longer needs.
- **Restore over MCP**: `update_artifact` takes `restore: true`, and `list_artifacts` takes `deleted`.

## 0.3.0

Released 2026-10-27.

Setting up an instance takes a few copy-and-paste steps: packages, an install script, a preflight check and admin commands that need no token on the server itself.

### Upgrading

- **The CLI is rebuilt on one grammar** for `serve`, `check`, the client and the admin commands. The client's `--config` is now `--client-config`.
- **The module path is `github.com/PeterBooker/kazari`**, so `go install` works.
- **`serve --local` replaces `deploy/dev.yaml`.**

### New

- **`kazari check`** tests the configuration, the data directory, the DNS records, the DNS token and the ports before the first start.
- **An admin socket** in the data directory serves the admin API to `kazari admin` on the same host without a URL or a token.
- **Packages and an install script**: `.deb` and `.rpm` packages, and `install.sh`, which verifies the checksums.
- **More DNS providers**: DigitalOcean, Gandi, OVH, Namecheap and DuckDNS.
- **Secrets from files**: any secret key also reads `KAZARI_<KEY>_FILE`.
- `deploy/kazari.yaml` is the short file an instance starts from, `deploy/kazari.reference.yaml` lists every key, and `deploy/compose.yml` runs the published image.

### Changed

- Edits return the changed content, so agents that read only the text of a result see it.

## 0.2.1

Released 2026-10-25.

### Fixed

- claude.ai connects as a custom connector. Kazari refused clients that list grant types it does not offer; it now requires only the ones it uses.

## 0.2.0

Released 2026-10-25.

Pages can now be limited to the people of the organisation, and agents can change a file without sending all of it.

### New

- **Members-only pages**: the `members` visibility serves a page only to people who sign in to the instance in their browser, and to holders of one of its share links.
- **Share links** with an expiry, for people who cannot sign in.
- **`edit_file`** changes part of a text file by matching text or by line anchors from `read_file`, and publishes the result as a new version.
- **A served skill**: the instance's own guide for agents, generated from its configuration.
- **Confirmation before a purge** over MCP, and a **preview card** of a published page in MCP clients that show apps.

## 0.1.0

Released 2026-10-25.

The first release: one Go binary that serves each artifact as its own site at `<slug>.<base>` under a wildcard certificate.

- Immutable versions stored once by SHA-256, with roll back and forward, and `expected_version` and `If-Match` against lost updates.
- A REST API, an MCP server with twelve tools and the `kazari` CLI over one service layer.
- Inline files, tar.gz bundles and chunked uploads.
- A stale, soft-delete and purge lifecycle that can be undone until the purge, recorded in an audit log.
- An OAuth 2.1 server for MCP clients federated to one OpenID Connect provider, and API keys.
- ACME DNS-01 for Cloudflare, Route 53 and Hetzner, certificate files or a proxy in front; SQLite and a blob directory as the only state; Prometheus metrics, quotas and rate limits.
