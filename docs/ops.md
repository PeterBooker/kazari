# Operating Kazari

Kazari keeps all its state in one directory and runs its own maintenance jobs. This page covers the data layout, backing up and restoring, upgrading, logs and metrics, the scheduled jobs and the page lifecycle, quotas, disabling users, responding to abuse, the audit log and troubleshooting. It assumes an instance set up as in the [installation guide](install.md).

## Data layout

Everything Kazari keeps lives under `data_dir`, which the systemd unit and the Docker image set to `/var/lib/kazari`. Under systemd's `DynamicUser`, the real path is `/var/lib/private/kazari`. The directory holds:

| Path                                          | Contents                                                                                 |
| --------------------------------------------- | ---------------------------------------------------------------------------------------- |
| `kazari.db`, `kazari.db-wal`, `kazari.db-shm` | SQLite in WAL mode: users, keys, artifacts, versions, file lists, OAuth state, audit log |
| `blobs/<aa>/<sha256>`                         | File contents, named by their SHA-256; `aa` is its first two hex digits                  |
| `uploads/<id>/<n>`                            | Chunked uploads in progress. Disposable                                                  |
| `certmagic/`                                  | ACME account, certificates and locks (`tls.mode: acme` only)                             |
| `admin.sock`                                  | The admin socket, mode `0600`; recreated at every start                                  |
| `tmp/`                                        | Multipart bundles spilled to disk while they are published, removed afterwards. Disposable |

Kazari writes each blob to a temporary `.put-*` file and renames it, so a partial blob is never visible.

## Backing up and restoring

The database and the blobs are the state. Back up the database first, with SQLite's online backup, which is consistent while Kazari is running, then copy the blobs straight after:

```sh
cd /var/lib/private/kazari
sqlite3 kazari.db ".backup /backup/kazari/kazari.db"
rsync -a --delete blobs/ /backup/kazari/blobs/
rsync -a certmagic/ /backup/kazari/certmagic/     # optional; saves a re-issue after a restore
```

`uploads/` and `tmp/` need no backup.

> [!WARNING]
> Do not copy `kazari.db` with `cp` or `rsync` while the server runs. The WAL makes that copy inconsistent; use SQLite's `.backup` as above.

The database first and the blobs second gives a complete backup. Blobs are immutable and deleted only once no version references them, so a later blob copy holds everything the database snapshot refers to. The only gap is blobs of artifacts purged in between, which the snapshot no longer needs once it is restored and swept.

### Restoring from a backup

Restore with the server stopped, the blobs first and the database second:

```sh
sudo systemctl stop kazari
cd /var/lib/private/kazari
rsync -a --delete /backup/kazari/blobs/ blobs/
rm -f kazari.db-wal kazari.db-shm
cp /backup/kazari/kazari.db kazari.db
rsync -a /backup/kazari/certmagic/ certmagic/
sudo systemctl start kazari
```

Restoring the blobs before the database means no restored row ever points at a missing file. Extra blobs are harmless: the `gc` job removes the ones nothing references.

## Upgrading

Kazari applies schema migrations at start. They are embedded in the binary, each runs in its own transaction, and they only go forward: to downgrade, restore the backup taken before the upgrade.

To upgrade an instance that runs under systemd, back up as in [backing up and restoring](#backing-up-and-restoring), then install the new release the way you installed the old one:

```sh
# from the package: the service restarts itself
sudo apt install ./kazari_<version>_amd64.deb

# from the install script, then restart
curl -fsSL https://kazari.io/install.sh | sh
sudo systemctl restart kazari
```

The package keeps `/etc/kazari/kazari.yaml` and `/etc/kazari/kazari.env` as they are. Installing by hand follows [installing the binary](install.md#installing-the-binary), with the checksums and signature verified, then a restart.

### Upgrading under Docker Compose

With `KAZARI_VERSION` pinned in `.env`, set it to the new version; with the default `latest`, nothing changes. Then pull and recreate:

```sh
docker compose pull
docker compose up -d
```

The container keeps its data in the volume `kazari_data`.

## Logs

Kazari logs to stderr through `log/slog`; under systemd, that is the journal. `log.format` sets the format, `text` by default or `json`, and `log.level` sets the level, `info` by default. Every request produces one line when it completes:

```text
time=2026-10-24T09:12:03Z level=INFO msg=request method=GET host=q3-report.art.example.com path=/ status=200 bytes=5120 duration_ms=1.8 request_id=6f1c0e9b2a7d4c33a1e0f5b6c7d8e9f0 ip=198.51.100.7 user_agent="Mozilla/5.0 ..."
```

Each line carries `method`, `host`, `path`, `status`, `bytes`, `duration_ms`, `request_id`, `ip` and `user_agent`. In proxy mode, `ip` comes from `X-Forwarded-For` or `X-Real-IP`. `user_agent` is cut to 200 bytes.

`5xx` responses log at `ERROR`, the rest at `INFO`. API `5xx` errors also log their cause on a separate line (`httpapi: request failed`); the client only ever sees `internal error`.

### Request ids

The request id comes from an incoming `X-Request-ID` header of 8 to 128 safe characters, or Kazari generates one. Either way, it is echoed in the `X-Request-ID` response header. Quote it when reporting a problem.

### Finding errors

To find server errors, or the certificate messages, filter the journal:

```sh
journalctl -u kazari -o cat | grep 'status=5'
journalctl -u kazari -o cat | grep -i certificate
```

## Metrics

Kazari serves Prometheus metrics at `https://admin.<base>/metrics`. When `admin.listen` is set, the admin host leaves the public listener, and the metrics are served only at `http://<admin.listen>/metrics`.

> [!WARNING]
> The metrics endpoint needs no credential. Set `admin.listen` to a private address and scrape that, or restrict `/metrics` at your proxy or firewall.

Kazari exports these metrics:

| Metric                                 | Type      | Labels                         |
| -------------------------------------- | --------- | ------------------------------ |
| `kazari_http_requests_total`           | counter   | `surface`, `method`, `status`  |
| `kazari_http_request_duration_seconds` | histogram | `surface`                      |
| `kazari_http_requests_in_flight`       | gauge     | `surface`                      |
| `kazari_job_runs_total`                | counter   | `job`, `result` (`ok`/`error`) |
| `kazari_job_duration_seconds`          | histogram | `job`                          |

`surface` is `site` (artifact pages), `api`, `mcp`, `admin` or `apex` (the base domain itself). The standard Go runtime (`go_*`) and process (`process_*`) collectors are included.

### Scraping

With `admin.listen` set to `"127.0.0.1:9443"`, the Prometheus scrape job is:

```yaml
scrape_configs:
  - job_name: kazari
    static_configs:
      - targets: ["127.0.0.1:9443"]      # admin.listen: "127.0.0.1:9443"
```

### Alerts

Two alerts are worth setting:

- `increase(kazari_job_runs_total{result="error"}[1h]) > 0`, for a failing job.
- A high rate of `kazari_http_requests_total{status=~"5.."}`, for server errors.

## Scheduled jobs

Kazari runs four maintenance jobs on its own:

| Job              | Interval                                           | What it does                                                                                         |
| ---------------- | -------------------------------------------------- | ---------------------------------------------------------------------------------------------------- |
| `sweep`          | `lifecycle.sweep_interval`, 15 minutes by default  | Moves artifacts along the [lifecycle](#lifecycle): soft-deletes expired stale ones, purges soft-deleted ones past retention and removes tombstones past the tombstone period |
| `gc`             | `lifecycle.gc_interval`, 1 hour by default         | Deletes blobs that no version references, in batches of 1000, sparing blobs written in the last hour |
| `uploads-expire` | 10 minutes                                         | Removes chunked uploads past `limits.upload_ttl` and staging directories left by a crash             |
| `oauth-cleanup`  | 1 hour                                             | Deletes expired OAuth authorization requests, codes and tokens, and expired browser sign-ins, sign-in codes and sessions |

`gc` spares recent blobs so that a publish in progress never loses one. It also removes temporary files a crash left behind: blob writes and spilled bundles older than an hour.

Each interval is jittered by ±10%, and the first run comes after a random delay of up to 10% of the interval. A job never runs twice at once.

### Checking the jobs

To see each job's interval, last run, next run and last error, list them:

```sh
kazari admin jobs list
```

A sweep or gc that changed something also logs a summary line, such as `sweep soft_deleted=1 purged=0 freed=0`.

### Running a job now

To run a job now, name it. The job runs synchronously, and the command returns when it finishes:

```sh
kazari admin jobs run sweep

# the same through the admin API
curl -X POST -H "Authorization: Bearer $KAZARI_ADMIN_TOKEN" https://admin.art.example.com/v1/jobs/gc/run
```

## Lifecycle

An artifact that is no longer needed moves through these states until its slug is free again:

```text
published ──stale──▶ stale ──grace──▶ soft-deleted ──retention──▶ purged ──tombstone──▶ slug free
    ▲                  │                   │
    └──── unstale ─────┘                   └── restore (PUT/PATCH {"restore": true})
```

Each state works as follows:

- **Stale**: `kazari stale <slug> [--grace 72h]` sets `stale_at`, and sets `expires_at` to now plus the grace period. The page keeps serving. Publishing a new version clears the mark unless `keep_stale` is set.
- **Soft-deleted**: by `rm`, or by the sweep after `expires_at`. The page answers `404` and the slug stays taken. Until `lifecycle.retention` has passed since `deleted_at`, the owner can still read the files and restore the artifact.
- **Purged**: by the sweep after retention, or at once by `rm --hard`. Versions and file lists are deleted, and the blobs go at the next `gc`. The page answers `410 Gone`, and the slug stays reserved for `lifecycle.tombstone`.
- **Freed**: the tombstone row is deleted, and the slug can be used again.

Each step lands in the [audit log](#audit-log).

### Timings

Timings run from the transition, and the sweep acts at its next run. With the default grace of 14 days, retention of 30 days and tombstone of 90 days, an artifact marked stale today is soft-deleted after 14 days, purged 30 days later, and its slug freed 90 days after that. That is 134 days in all, plus up to one sweep interval at each step.

### Tuning the lifecycle

Each period is a `lifecycle` setting in the [configuration](configuration.md#lifecycle). Restart after editing. What each one trades:

- Shorten `retention` to reclaim space sooner. It is the window in which a mistaken delete can be undone.
- Shorten `tombstone` to free slugs sooner. It is the window in which old links answer `410` rather than someone else's page.
- Change `default_grace` to set the grace a `stale` call gets when it names none.

### Unpublished pages

`unpublished` is separate from this chain. It comes from `kazari visibility <slug> unpublished` or `admin hide`. The page answers `404`, nothing is deleted, and nothing moves on until someone deletes it.

## Quotas

Each user has a quota, `quota_bytes`. A new user gets `limits.default_user_quota`, 2 GiB by default, and `0` means unlimited.

Usage is the sum of the sizes of every version of every artifact the user owns, counted logically. A 5 MiB site with ten versions uses 50 MiB of quota, even though unchanged files are stored once. Soft-deleted artifacts keep counting until they are purged; a hard delete frees the space at once.

Kazari checks the quota when the owner adds a version, and a version that would exceed it gets `403 quota_exceeded`. Under the `shared` write policy, a version that another user adds to someone else's artifact is not checked against any quota.

### Changing a quota

To see every user's usage and change one user's quota:

```sh
kazari admin users list                      # every user, with usage and quota
kazari admin users update ana --quota 5GiB
kazari --json admin users list | jq -r '.[].handle'   # handles only, for scripts
```

`kazari me` shows users their own usage. The per-version limits `max_version_bytes`, `max_file_bytes` and `max_files` apply to everyone, admins included.

## Disabling users and revoking access

To block a user, disable them:

```sh
kazari admin users update ana --disable
```

Disabling a user revokes every API key at once, makes every OAuth access and refresh token fail, and refuses sign-in with the "No access" page. `--enable` lets them sign in again, but revoked keys stay revoked.

### Revoking one API key

To revoke a single key, list the user's keys and revoke one by its prefix:

```sh
kazari admin keys list ana
kazari admin keys revoke ana abcdefgh
```

To revoke one OAuth grant instead, which also revokes every token rotated from it, use the admin API as in [Revoking one client's access](identity.md#revoking-one-clients-access).

### Handing over a disabled user's pages

A disabled user's artifacts keep serving. To hand one over to another user, transfer it:

```sh
kazari admin transfer q3-report peter
```

## Responding to abuse

To take a page down at once, hide it and disable its owner:

```sh
kazari admin hide q3-report                 # unpublishes: the page answers 404 immediately
kazari admin users update ana --disable
```

`hide` deletes nothing, and the audit log records it as `artifact.visibility`. It is not a lock: the owner, or under the `shared` policy any writer, can publish the page again. That is why disabling the owner usually goes with it.

### Removing the content for good

To remove the content for good, use an admin user's key, with the `admin` scope, on the user API, then collect the blobs:

```sh
KAZARI_TOKEN=kzk_admin_key kazari rm q3-report --hard
kazari admin jobs run gc
```

Blobs written in the last hour survive one more `gc` run.

> [!WARNING]
> Version hosts are served with `Cache-Control: public, max-age=<lifecycle.tombstone, at most a year>, immutable`, 90 days by default. Browsers and any CDN in front can keep copies of a pinned version for that long; purge them there if needed.

## Audit log

Kazari records every change with its time, actor, action, slug and details. The actor is a handle, `admin` for the admin token, or `scheduler` for jobs.

Artifact events also carry `credential`, the credential that acted: `api_key:<prefix>`, `oauth:<grant>` or `admin_token`. An `oauth:` credential has one id per grant, the same across refreshes.

The actions, with the details each records:

| Action                | When                                                                 |
| --------------------- | -------------------------------------------------------------------- |
| `artifact.create`     | Created (details: `version`, `size`)                                 |
| `artifact.update`     | Fields changed or version added (`version`, `size` when a version)   |
| `artifact.publish`    | Published version changed (`version`)                                |
| `artifact.stale`      | Marked stale (`expires_at`)                                          |
| `artifact.unstale`    | Stale mark cleared                                                   |
| `artifact.visibility` | Visibility set, including unpublish and `hide` (`visibility`)        |
| `artifact.delete`     | Soft-deleted, by a user or by the sweep (`reason: expired`)          |
| `artifact.restore`    | Restored from soft delete                                            |
| `artifact.purge`      | Purged, by `rm --hard` or the sweep (`reason: retention`)            |
| `artifact.free`       | Tombstone removed, slug free                                         |
| `artifact.transfer`   | Owner changed (`from`, `to`)                                         |
| `user.create`         | User created by an admin, or provisioned at sign-in (`via: identity`) |
| `user.update`         | User changed, identity linked or unlinked                            |
| `key.issue`           | API key issued (`prefix`, `label`, `scopes`)                         |
| `key.revoke`          | API key revoked                                                      |
| `oauth.grant`         | Consent approved (`client_id`, `scopes`, `via: oidc` or `api_key`, and `key_prefix` for `api_key`) |
| `oauth.revoke`        | OAuth grant revoked                                                  |
| `oauth.signin`        | A browser signed in to view `members` pages (`via: oidc` or `api_key`, and `key_prefix` for `api_key`) |
| `share_link.create`   | Share link created (`prefix`, `expires_at`)                          |
| `share_link.revoke`   | Share link revoked (`prefix`)                                        |

An event is written just after the change it describes, outside its transaction, so a crash in between loses that one row. The log is append-only and never pruned.

### Reading the log

To read the log, filter it by time, slug or actor:

```sh
kazari admin audit --since 24h
kazari admin audit --slug q3-report --limit 50
curl -H "Authorization: Bearer $KAZARI_ADMIN_TOKEN" "https://admin.art.example.com/v1/audit?actor=ana&since=2026-10-01T00:00:00Z"
```

## Troubleshooting

Each entry is named for the symptom. Certificate and DNS problems are in [DNS and certificates](certificates.md#troubleshooting), and `kazari check` reproduces most of them in one run.

### `401 Unauthorized`

Kazari did not accept the credential. Check that:

- the header is `Authorization: Bearer <token>`;
- the admin token is used only on `admin.<base>`, not on `api.` or `mcp.`;
- the key is not revoked or expired (`kazari admin keys list <handle>`), and its user is not disabled;
- an OAuth client can refresh its access token, which lives one hour. A client that cannot refresh must sign in again.

A `403` instead means the credential is valid but lacks a scope: `artifacts:delete` for `rm`, or the `admin` scope for the admin API.

### `413 Payload Too Large`

The `message` names the limit that was hit:

- a file over `limits.max_file_bytes`;
- a version over `limits.max_version_bytes`, which counts all files of the version, not only the ones sent;
- a chunk over `limits.max_chunk_bytes`;
- the request body.

Raise the limit and restart, or split the bundle across a chunked upload.

In proxy mode, the proxy has its own limit. nginx defaults to `client_max_body_size 1m`, which answers `413` with an HTML page before Kazari sees the request.

### `404` or `410` on a page

`404` with the instance page means there is no such slug, the artifact is unpublished or soft-deleted, or there is no such version on a `--v<N>` host. `410 Gone` means the artifact was purged and its slug is still in the tombstone period.

`kazari get <slug>` shows which timestamps are set.

### `401` or a sign-in redirect on a page

The artifact's visibility is `members`. A browser is sent to sign in and other clients get `401`; signed-in members and holders of a share link see the page.

To let in someone outside the instance, create a share link with `kazari share create <slug>`. To open the page to everyone, run `kazari visibility <slug> public`. If the redirect never ends, see [A members page keeps redirecting to sign-in](identity.md#a-members-page-keeps-redirecting-to-sign-in).

### A CDN serves a members page without sign-in

Responses of `members` pages are `Cache-Control: private, no-cache`, so a shared cache does not keep them. Copies it took while the page was still `public` stay, though, and pinned versions are cached for up to `lifecycle.tombstone`.

When you switch a page from public to `members`, purge `https://<slug>.<base>/` and `https://<slug>--v*.<base>/` at the CDN.
