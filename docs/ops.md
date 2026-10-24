# Operating Kazari

## Data layout

Everything Kazari keeps lives under `data_dir` (`/var/lib/kazari`; under systemd's `DynamicUser` the real path is `/var/lib/private/kazari`):

| Path                         | Contents                                                                                       |
| ---------------------------- | ---------------------------------------------------------------------------------------------- |
| `kazari.db`, `kazari.db-wal`, `kazari.db-shm` | SQLite in WAL mode: users, keys, artifacts, versions, file lists, OAuth state, audit log |
| `blobs/<aa>/<sha256>`        | File contents, content-addressed by SHA-256 (`aa` is the first two hex digits). Written to a temporary `.put-*` file and renamed, so a partial blob is never visible |
| `uploads/<id>/<n>`           | Chunked uploads in progress. Disposable                                                        |
| `certmagic/`                 | ACME account, certificates and locks (`tls.mode: acme` only)                                   |
| `tmp/`                       | Multipart bundles spilled to disk while they are published; removed afterwards. Disposable     |

## Backup

The database and the blobs are the state. Back up the database with SQLite's online backup, which is consistent while Kazari is running, then copy the blobs:

```sh
cd /var/lib/private/kazari
sqlite3 kazari.db ".backup /backup/kazari/kazari.db"
rsync -a --delete blobs/ /backup/kazari/blobs/
rsync -a certmagic/ /backup/kazari/certmagic/     # optional; saves a re-issue after a restore
```

Do the database first and the blobs straight after. Blobs are immutable and only deleted once no version references them, so a database snapshot plus a later blob copy is complete, apart from blobs of artifacts purged in between, which the snapshot no longer needs once it is restored and swept. Do not copy `kazari.db` with `cp` or `rsync` while the server runs; the WAL makes that copy inconsistent. `uploads/` and `tmp/` need not be backed up.

**Restore**, in this order:

```sh
sudo systemctl stop kazari
cd /var/lib/private/kazari
rsync -a --delete /backup/kazari/blobs/ blobs/
rm -f kazari.db-wal kazari.db-shm
cp /backup/kazari/kazari.db kazari.db
rsync -a /backup/kazari/certmagic/ certmagic/
sudo systemctl start kazari
```

Blobs before the database means no restored row ever points at a missing file. Extra blobs are harmless: the `gc` job removes the ones nothing references.

## Upgrades

1. Back up (above).
2. Install the new binary ([install.md](install.md#install-the-release-binary)), verifying the checksums and signature.
3. `sudo systemctl restart kazari`.

Schema migrations are embedded in the binary and applied at start, each in its own transaction. They only go forward: to downgrade, restore the backup taken before the upgrade. With Docker, `docker pull ghcr.io/peterbooker/kazari:<version>` and recreate the container on the same volume.

## Logs

Kazari logs to stderr (the journal under systemd) through `log/slog`, as `text` (default) or `json` (`log.format`), at `log.level`. Every request produces one line when it completes:

```text
time=2026-10-24T09:12:03Z level=INFO msg=request method=GET host=q3-report.art.example.com path=/ status=200 bytes=5120 duration_ms=1.8 request_id=6f1c0e9b2a7d4c33a1e0f5b6c7d8e9f0 ip=198.51.100.7 user_agent="Mozilla/5.0 ..."
```

Fields: `method`, `host`, `path`, `status`, `bytes`, `duration_ms`, `request_id`, `ip` (from `X-Forwarded-For`/`X-Real-IP` in proxy mode), `user_agent` (truncated to 200 bytes). `5xx` responses log at `ERROR`, the rest at `INFO`. The request id comes from an incoming `X-Request-ID` (8 to 128 safe characters) or is generated, and is echoed in the `X-Request-ID` response header; quote it when reporting a problem. API `5xx` errors log their cause separately (`httpapi: request failed`); the client only ever sees `internal error`.

```sh
journalctl -u kazari -o cat | grep 'status=5'
journalctl -u kazari -o cat | grep -i certmagic
```

## Metrics

Prometheus metrics are served at `https://admin.<base>/metrics`, or, when `admin.listen` is set, only at `http://<admin.listen>/metrics`: the admin host then leaves the public listener. **The endpoint needs no credential**, so set `admin.listen` to a private address (or restrict `/metrics` at your proxy or firewall) and scrape that.

| Metric                                  | Type      | Labels                       |
| --------------------------------------- | --------- | ---------------------------- |
| `kazari_http_requests_total`            | counter   | `surface`, `method`, `status` |
| `kazari_http_request_duration_seconds`  | histogram | `surface`                    |
| `kazari_http_requests_in_flight`        | gauge     | `surface`                    |
| `kazari_job_runs_total`                 | counter   | `job`, `result` (`ok`/`error`) |
| `kazari_job_duration_seconds`           | histogram | `job`                        |

`surface` is `site` (artifact pages), `api`, `mcp`, `admin` or `apex` (the base domain itself). The standard Go runtime (`go_*`) and process (`process_*`) collectors are included.

```yaml
scrape_configs:
  - job_name: kazari
    static_configs:
      - targets: ["127.0.0.1:9443"]      # admin.listen: "127.0.0.1:9443"
```

Useful alerts: `increase(kazari_job_runs_total{result="error"}[1h]) > 0`, and a high rate of `kazari_http_requests_total{status=~"5.."}`.

## Scheduled jobs

| Job              | Interval                                  | Does                                                                       |
| ---------------- | ----------------------------------------- | -------------------------------------------------------------------------- |
| `sweep`          | `lifecycle.sweep_interval` (15 min)       | Advances the lifecycle: expired stale artifacts are soft-deleted, soft-deleted ones past retention are purged, tombstones past the tombstone period are removed |
| `gc`             | `lifecycle.gc_interval` (1 h)             | Deletes blobs no version references, in batches of 1000; blobs written in the last hour are left alone so a publish in progress never loses one |
| `uploads-expire` | 10 min                                    | Removes chunked uploads past `limits.upload_ttl` and staging directories left by a crash |
| `oauth-cleanup`  | 1 h                                       | Deletes expired OAuth authorization requests, codes and tokens            |

`kazari admin jobs list` shows each job's interval, last run, next run and last error. A sweep or gc that changed something logs a summary line (`sweep soft_deleted=1 purged=0 freed=0`). Each interval is jittered by ±10% and the first run comes after a random delay of up to 10% of the interval. A job never runs twice at once.

Run a job now (it runs synchronously, and the command returns when it finishes):

```sh
kazari admin jobs run sweep
curl -X POST -H "Authorization: Bearer $KAZARI_ADMIN_TOKEN" https://admin.art.example.com/v1/jobs/gc/run
```

## Lifecycle

```text
published ──stale──▶ stale ──grace──▶ soft-deleted ──retention──▶ purged ──tombstone──▶ slug free
    ▲                  │                   │
    └──── unstale ─────┘                   └── restore (PUT/PATCH {"restore": true})
```

- **Stale**: `kazari stale <slug> [--grace 72h]` sets `stale_at` and `expires_at = now + grace`. The page keeps serving. Publishing a new version clears the mark unless `keep_stale` is set.
- **Soft-deleted**: by `rm` or by the sweep after `expires_at`. The page answers `404`, the slug stays taken, the owner can still read files and restore it until `lifecycle.retention` has passed since `deleted_at`.
- **Purged**: by the sweep after retention, or at once by `rm --hard`. Versions and file lists are deleted; the page answers `410 Gone` and the slug stays reserved for `lifecycle.tombstone`; blobs go at the next `gc`.
- **Freed**: the tombstone row is deleted and the slug can be used again.

Each step lands in the audit log. Timings run from the transition, and the sweep acts at its next run, so with the defaults an artifact marked stale today is soft-deleted after 14 days, purged 30 days later and its slug freed 90 days after that (134 days, plus up to one sweep interval each step).

To tune: shorten `retention` to reclaim space sooner (it is the window in which a mistaken delete can be undone), shorten `tombstone` to free slugs sooner (it is the window in which old links answer `410` rather than someone else's page), and change `default_grace` for the grace a `stale` call gets without one. Restart after editing.

`unpublished` (from `kazari visibility <slug> unpublished` or `admin hide`) is separate from this chain: the page answers `404`, nothing is deleted, and nothing moves on until someone deletes it.

## Quotas

Each user has `quota_bytes`, defaulting to `limits.default_user_quota` (2 GiB) when the user is created, and `0` meaning unlimited. Usage is the sum of the sizes of every version of every artifact the user owns, counted logically: a 5 MiB site with ten versions uses 50 MiB of quota even though unchanged files are stored once. Soft-deleted artifacts keep counting until they are purged; a hard delete frees the space at once.

The quota is checked when the owner adds a version (`403 quota_exceeded`). Under the `shared` write policy, a version another user adds to someone else's artifact is not checked against any quota.

```sh
kazari admin users list
kazari admin users update ana --quota 5GiB
kazari --json admin users list | jq -r '.[].handle'
```

`kazari me` shows a user their own usage. The per-version limits (`max_version_bytes`, `max_file_bytes`, `max_files`) apply to everyone, admins included.

## Disabling users and revoking access

```sh
kazari admin users update ana --disable       # blocks the user
kazari admin keys list ana
kazari admin keys revoke ana abcdefgh          # one key, by prefix
```

Disabling a user revokes every API key at once, makes every OAuth access and refresh token fail and refuses sign-in ("No access" page). `--enable` lets them sign in again; revoked keys stay revoked. OAuth grants are revoked individually through the admin API, which also revokes every token rotated from that grant:

```sh
curl -H "Authorization: Bearer $KAZARI_ADMIN_TOKEN" https://admin.art.example.com/v1/users/ana/grants
curl -X DELETE -H "Authorization: Bearer $KAZARI_ADMIN_TOKEN" https://admin.art.example.com/v1/grants/42
```

A disabled user's artifacts keep serving. Hand them over with `kazari admin transfer <slug> <handle>`.

## Abuse response

To take a page down at once:

```sh
kazari admin hide <slug>          # unpublishes: the page answers 404 immediately
kazari admin users update <owner> --disable
```

`hide` deletes nothing and is recorded in the audit log as `artifact.visibility`. It is not a lock: the owner (or, under the `shared` policy, any writer) can publish the page again, which is why disabling the owner usually goes with it. To remove the content for good, use an admin user's key (with the `admin` scope) on the user API:

```sh
KAZARI_TOKEN=kzk_admin_key kazari rm <slug> --hard
kazari admin jobs run gc
```

Blobs written in the last hour survive one more `gc` run. Version hosts are served with `Cache-Control: public, max-age=<lifecycle.tombstone, at most a year>, immutable` (90 days by default), so browsers and any CDN in front may keep copies of a pinned version for that long; purge them there if needed. The same `gc` run also removes temporary files a crash left behind (blob writes and spilled bundles older than an hour).

## Audit log

Every change is recorded with time, actor (a handle, `admin` for the admin token, `scheduler` for jobs), action, slug and details. Artifact events also carry `credential`, the credential that acted: `api_key:<prefix>`, `oauth:<grant>` (one id per grant, the same across refreshes) or `admin_token`.

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

```sh
kazari admin audit --since 24h
kazari admin audit --slug q3-report --limit 50
curl -H "Authorization: Bearer $KAZARI_ADMIN_TOKEN" "https://admin.art.example.com/v1/audit?actor=ana&since=2026-10-01T00:00:00Z"
```

An event is written just after the change it describes, outside its transaction; a crash in between loses that one row. The log is append-only and never pruned.

## Troubleshooting

### Certificate issuance

With `tls.mode: acme`, the certificate is obtained in the background after start; until then TLS handshakes fail. Look for CertMagic's lines in the log (`journalctl -u kazari | grep -i certmagic`).

- **DNS provider errors** (`401`, `403`, "zone not found"): the token lacks permission on the zone. A Cloudflare token needs `Zone.DNS:Edit` for the zone that contains `base_domain`.
- **Challenge not seen**: DNS-01 writes a `_acme-challenge.art.example.com` TXT record and waits for it to propagate. Check `dig TXT _acme-challenge.art.example.com @1.1.1.1` while it runs.
- **CAA records** on the domain must allow `letsencrypt.org`.
- **Rate limits**: repeated failures against production Let's Encrypt hit its limits. Debug with `tls.ca: letsencrypt-staging`, then switch back and delete `certmagic/` to drop the staging certificate.
- **`files` mode**: a reload failure keeps the old certificate and logs `certs: reload failed, keeping previous certificate`; the files are checked once a minute.

### DNS propagation

Every host must resolve to the server: `dig +short art.example.com`, `dig +short api.art.example.com`, `dig +short anything.art.example.com`. A wildcard record does not cover the apex, and an explicit record for a subdomain (say `www`) overrides the wildcard for that name. An artifact host that resolves but shows the instance 404 page means DNS and TLS work and the slug is unknown, unpublished or deleted.

### 401 Unauthorized

- The header must be `Authorization: Bearer <token>`.
- The admin token works only on `admin.<base>`, not on `api.` or `mcp.`.
- The key may be revoked or expired (`kazari admin keys list <handle>`), or its user disabled.
- OAuth access tokens live one hour; a client that cannot refresh must sign in again.

A `403` instead means the credential is valid but lacks a scope: `artifacts:delete` for `rm`, or the `admin` scope for the admin API.

### 413 Payload Too Large

The `message` names the limit that was hit: a file over `limits.max_file_bytes`, a version over `limits.max_version_bytes` (all files of the version, not only the ones sent), a chunk over `limits.max_chunk_bytes`, or the request body. Raise the limit and restart, or split the bundle across a chunked upload. In proxy mode the proxy has its own limit: nginx defaults to `client_max_body_size 1m`, which answers `413` with an HTML page before Kazari sees the request.

### 404 or 410 on a page

`404` with the instance page: no such slug, unpublished, soft-deleted, or no such version on a `--v<N>` host. `410 Gone`: the artifact was purged and its slug is still in the tombstone period. `kazari get <slug>` shows which timestamps are set.
