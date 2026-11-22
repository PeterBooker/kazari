# Operating Kazari

Kazari keeps all its state in one directory and runs its own maintenance jobs. This page covers the data layout, backing up and restoring, upgrading, checking readiness, logs, metrics and tracing, the scheduled jobs and the page lifecycle, quotas, disabling users, responding to abuse, the audit log and troubleshooting. It assumes an instance set up as in the [installation guide](install.md).

Most of the everyday tasks here, such as disabling a user, taking a page down or downloading a backup, can also be done in the [dashboard](dashboard.md). What only the host's operator does stays on this page: restoring, upgrading and scheduling.

## Data layout

Everything Kazari keeps lives under `data_dir`, `/var/lib/kazari` by default, which the systemd unit and the Docker image provide. Under systemd's `DynamicUser`, the real path is `/var/lib/private/kazari`. The directory holds:

| Path                                          | Contents                                                                                 |
| --------------------------------------------- | ---------------------------------------------------------------------------------------- |
| `kazari.db`, `kazari.db-wal`, `kazari.db-shm` | SQLite in WAL mode: users, keys, artifacts, versions, file lists, OAuth state, audit log |
| `blobs/<aa>/<sha256>`                         | File contents, named by their SHA-256; `aa` is its first two hex digits                  |
| `uploads/<id>/<n>`                            | Chunked uploads in progress. Disposable                                                  |
| `certmagic/`                                  | ACME account, certificates and locks (`tls.mode: acme` only)                             |
| `admin.sock`                                  | The admin socket, mode `0600`; recreated at every start                                  |
| `kazari.lock`                                 | Held by the running server, with its process id, so a second server on the directory stops |
| `tmp/`                                        | Multipart bundles spilled to disk while they are published and the database copy of a running backup, removed afterwards. Disposable |

Kazari writes each blob to a temporary `.put-*` file and renames it, so a partial blob is never visible.

## Backing up and restoring

The database and the blobs are the state. Kazari writes both as one archive, which is consistent while the server runs and needs no other tool.

### Taking a backup

To take a backup on the server, name a directory that is empty or does not exist yet:

```sh
sudo kazari admin backup /backup/kazari/2026-11-21
```

The command prints the number of files, their size and the directory, which then holds what a data directory holds:

| Path         | Contents                                                                     |
| ------------ | ---------------------------------------------------------------------------- |
| `kazari.db`  | A complete copy of the database, taken first                                 |
| `blobs/`     | Every blob, copied after the database                                        |
| `certmagic/` | The ACME account and the certificate with its private key (`tls.mode: acme`) |

`uploads/` and `tmp/` are left out, as they need no backup. `--no-certs` leaves `certmagic/` out too; an instance restored without it asks for a new certificate.

Under Compose, nothing inside the container survives its next recreation, so send the backup to the host. The simplest way is `-` in place of the directory, which writes the archive as tar to standard output:

```sh
docker compose exec -T kazari kazari admin backup - > kazari-2026-11-21.tar
```

To write into a directory instead, uncomment the `./backups:/backup` mount in `compose.yml`. The server runs as UID 65532, which must own the directory on the host:

```sh
mkdir backups && sudo chown 65532:65532 backups
docker compose up -d
docker compose exec kazari kazari admin backup /backup/2026-11-21
```

`kazari admin backup -` works anywhere, for example to pipe the archive to other storage. It refuses to write to a terminal.

#### What a backup needs

A backup depends on three things:

- **Free space.** Kazari first copies the database into `tmp/` under the data directory and removes the copy afterwards. With less free space there than the database's size, the backup is refused with `no_space`.
- **One at a time.** A second backup started while one runs is refused with `backup_running`.
- **The garbage collector waits.** The `gc` job skips its run while a backup is written, and a backup waits for a run in progress, so every blob the database copy refers to is in the archive.

Every backup is recorded in the [audit log](#audit-log) as `backup.create`, with who took it.

#### From another machine

From another machine, the same command works over the admin URL with an admin credential. The archive then never holds `certmagic/`, and the command says so: the certificate's private key leaves the server only over the admin socket.

The route behind the command is `POST /v1/backup`, which answers the archive as a tar stream ([admin API](api.md#backup)).

A backup lasts as long as its reader keeps reading. When `kazari admin backup` or a download reads nothing for a minute, the server ends it, and the next backup can start.

#### From the dashboard

**Download a backup** on the dashboard's [System page](dashboard.md#downloading-a-backup) sends the same archive to the browser as `kazari-<base_domain>-<date>.tar`, without `certmagic/`.

> [!WARNING]
> An archive holds every page, private ones included, and the hash of every password and API key. Keep it where only the instance's operators can read it.

### Scheduling backups

Kazari does not schedule backups itself. Run the command from a timer, for example with systemd. The two units are:

```ini
# /etc/systemd/system/kazari-backup.service
[Unit]
Description=Back up Kazari

[Service]
Type=oneshot
ExecStart=/bin/sh -c 'exec kazari admin backup /backup/kazari/$$(date +%%F)'
```

```ini
# /etc/systemd/system/kazari-backup.timer
[Unit]
Description=Back up Kazari every night

[Timer]
OnCalendar=*-*-* 03:00:00
Persistent=true

[Install]
WantedBy=timers.target
```

Enable the timer:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now kazari-backup.timer
```

Each night's backup lands in a directory named for its date. Removing old ones, and copying them off the host, is left to your own tooling.

### Restoring from a backup

`kazari admin restore-backup` copies a directory written by `kazari admin backup` into the data directory of a stopped server. It needs no running server and finds the data directory from the configuration, as `kazari admin` does. Stop the server, restore, then start it again:

```sh
sudo systemctl stop kazari
sudo kazari admin restore-backup /backup/kazari/2026-11-21 --force
sudo systemctl start kazari
sudo kazari admin ready
```

`--force` replaces the database, the blobs and the staged uploads the directory holds, and its certificates when the backup has some; without it, the command refuses a data directory that already holds an instance. The restored files are readable by their owner alone and belong to the data directory's owner, the user the server runs as.

The command refuses while a server holds the directory, naming its process id. `--data-dir` names another directory, which is created when it does not exist.

At its next start Kazari applies any migrations the backup's database lacks, and the `gc` job later removes blobs that nothing in it references.

#### Restoring on a new host

Install Kazari as on the old host, with the same `kazari.yaml` and dotenv file. Under systemd, start the service once, so systemd creates the data directory for its user, then stop it and restore:

```sh
sudo systemctl start kazari
sudo systemctl stop kazari
sudo kazari admin restore-backup /backup/kazari/2026-11-21 --force
sudo systemctl start kazari
```

#### Restoring an archive

To restore an archive downloaded from the dashboard or written by `kazari admin backup -`, unpack it into an empty directory and restore that:

```sh
mkdir /tmp/restore
tar -xf kazari-art.example.com-20261121-030000.tar -C /tmp/restore
sudo kazari admin restore-backup /tmp/restore --force
```

#### Restoring under Docker Compose

The data lives in the volume `kazari_data`, owned by UID 65532. Stop the container, copy the backup into the volume with a throwaway container, give the files to that user, and start again:

```sh
docker compose stop kazari
mkdir restore && tar -xf kazari-2026-11-21.tar -C restore
docker run --rm -v kazari_data:/data -v "$PWD/restore:/b:ro" busybox \
  sh -c 'rm -rf /data/kazari.db /data/kazari.db-wal /data/kazari.db-shm /data/blobs /data/uploads && cp -a /b/. /data && chown -R 65532:65532 /data'
docker compose start kazari
```

The volume is called `kazari_data` when `compose.yml` sits in a directory named `kazari`; `docker volume ls` shows its name otherwise.

### Backing up by hand

The archive is the recommended way. To back up with your own tools instead, for example to keep the blobs in step with `rsync`, copy the database first, with SQLite's online backup, then the blobs straight after:

```sh
sudo sqlite3 /var/lib/private/kazari/kazari.db ".backup /backup/kazari/kazari.db"
sudo rsync -a --delete /var/lib/private/kazari/blobs/ /backup/kazari/blobs/
# optional; saves a re-issue after a restore
sudo rsync -a /var/lib/private/kazari/certmagic/ /backup/kazari/certmagic/
```

This needs the `sqlite3` command, which the Docker image does not have.

> [!WARNING]
> Do not copy `kazari.db` with `cp` or `rsync` while the server runs. The WAL makes that copy inconsistent; use SQLite's `.backup` as above, or `kazari admin backup`.

The database first and the blobs second gives a complete backup. Blobs are immutable and deleted only once no version references them, so a later blob copy holds everything the database snapshot refers to. The only gap is blobs of artifacts purged in between, which the snapshot no longer needs once it is restored and swept.

To restore such a copy, stop the server and put the blobs back first and the database second, then give the files to the data directory's owner:

```sh
sudo systemctl stop kazari
sudo rsync -a --delete /backup/kazari/blobs/ /var/lib/private/kazari/blobs/
sudo rm -f /var/lib/private/kazari/kazari.db-wal /var/lib/private/kazari/kazari.db-shm
sudo cp /backup/kazari/kazari.db /var/lib/private/kazari/kazari.db
sudo rsync -a /backup/kazari/certmagic/ /var/lib/private/kazari/certmagic/
sudo chown -R --reference=/var/lib/private/kazari /var/lib/private/kazari
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

After any upgrade, check the running server:

```sh
sudo kazari admin ready --fresh
```

It runs the [readiness checks](#checking-readiness) now, against the new release's rules; a line that fails says what to change.

### Skipping releases

An upgrade can skip releases: the new binary applies every migration the database lacks, in order, and each release is tested as a starting point. Read the notes below for every release you skip, oldest first, since their changes add up.

### Upgrading under Docker Compose

With `KAZARI_VERSION` pinned in `.env`, set it to the new version; with the default `latest`, nothing changes. Then pull and recreate:

```sh
docker compose pull
docker compose up -d
```

The container keeps its data in the volume `kazari_data`.

### Upgrading to 0.8

Four things change in how an instance is run:

- **`tls.email` is optional, and placeholders fail.** A `base_domain` or `tls.email` under `example.com`, `example.org` or `example.net` stops the server, so a copied example cannot start. An empty `tls.email` registers the ACME account without a contact.
- **Listener addresses are `host:port`.** A bare port such as `8080` in `listen.http`, `listen.https` or `admin.listen` fails validation; write `127.0.0.1:8080` or `:8080`.
- **One server per data directory.** A second server started on the same `data_dir` stops with `another kazari (pid N) is using <data_dir>`.
- **The identity provider no longer stops the start.** An unreachable provider makes sign-in answer "temporarily unavailable" until the new `oidc-discovery` job reaches it, and `admin_emails` provisions its addresses as admins whatever `allowed_domains` says ([provisioning policy](identity.md#provisioning-policy)).

### Upgrading to 0.7

Kazari 0.7 adds [built-in accounts](identity.md#built-in-accounts) and the [dashboard](dashboard.md). An instance that signs people in at an identity provider keeps doing so. Four things change for the others, and for scripts:

- **Sign-in is a password.** Without an `auth.oidc` section, the sign-in page asks for a handle and a password, with **Use an API key instead** under the form. People who signed in with an API key have no password yet, and their keys keep working. To give a person a password, make them a reset link with `kazari admin users reset-password <handle>` and send it to them ([resetting a password](identity.md#resetting-a-password)). To keep sign-in by API key alone, set `auth.local.enabled` to `false`.
- **The dashboard needs an address with `admin.listen`.** The admin host serves the dashboard at `/`. On an instance that sets `admin.listen`, the dashboard stays off until `admin.public_url` says where a browser reaches the listener ([the dashboard's address](configuration.md#the-dashboards-address)).
- **The audit log answers an object.** `GET /v1/audit` and `kazari --json admin audit` answered an array of events. They now answer `{"events": [...], "next_cursor": "..."}`, so a script that read the array reads `events` ([audit log](api.md#audit-log)).
- **A fifth job runs.** `update-check` asks GitHub for the newest release once a day ([the update check](#the-update-check)). `kazari admin jobs list` shows it, and `admin.update_check: false` turns it off.

#### Users named admin or scheduler

Kazari 0.5 reserved the handles `admin` and `scheduler` for new users only. The first start of 0.7 renames any user who still holds one to the first free suffix, such as `admin-2`, and records a `user.update` event with `handle` and `renamed_from`. API keys, sign-ins, OAuth grants and artifacts stay with the user; versions and share links show the new handle. Scripts and links that name the user need the new handle.

Events from before the rename keep the actor they had, so `admin` there can be the operator or that user: `credential` tells them apart, `admin_token` or `local` for the operator. Earlier releases also made a user called `admin` involved in any artifact the operator hid or transferred, which opens it to that user while it is offline. The migration removes that involvement where the audit log shows only operator events for the artifact from that actor. It stays where the log also shows the user acting, or shows an event with no `credential`, because those cannot be attributed; review them with `kazari admin audit --slug <slug>`.

## Checking readiness

Two commands ask a running server how it is. `kazari admin health` answers whether it is up: it needs no credential, costs nothing and is what the Compose file's health check runs. `kazari admin ready` answers whether it is set up correctly:

```sh
sudo kazari admin ready
```

It prints one line per check, as [`kazari check`](install.md#checking-the-setup) does, and when the checks ran:

```text
config       ok    the configuration is valid
dns          ok    art.example.com -> 203.0.113.10 (this host)
dns          ok    api.art.example.com -> 203.0.113.10 (this host)
dns          ok    check-7f3a9c01.art.example.com -> 203.0.113.10 (this host)
dns token    ok    cloudflare wrote and removed a TXT record in zone example.com.
certificate  ok    valid until 2027-02-12 (83 days)
disk         ok    31.2 GiB free of 49.0 GiB on the disk holding /var/lib/kazari (64%)
database     ok    18.4 MiB in /var/lib/kazari/kazari.db
blobs        ok    1.2 GiB in /var/lib/kazari/blobs
dashboard    ok    https://admin.art.example.com

all checks passed
checked 2026-11-21T14:05:00Z
```

The command exits with status 1 when a check fails, so a monitor can run it. Every `warn` and `fail` line ends with what to do next.

These are the checks:

| Check         | Warns                                   | Fails                                   |
| ------------- | --------------------------------------- | --------------------------------------- |
| `config`      |                                         | The configuration is not valid          |
| `dns`         | As in `kazari check`                    | As in `kazari check`                    |
| `dns token`   | As in `kazari check`                    | As in `kazari check`                    |
| `certificate` | It expires within 14 days, or there is none five minutes after start | It has expired, or an order failed, quoting the error |
| `oidc`        |                                         | The identity provider's discovery fails or names another issuer |
| `disk`        | Under 10% of the data directory's disk is free | Under 2% is free                 |
| `database`, `blobs` | The blob directory cannot be read | Never: they report sizes                |
| `dashboard`   | `admin.listen` is set without `admin.public_url` | The dashboard's address is plain `http` on a host other than this machine |

Unlike `kazari check`, it leaves the ports and the admin socket alone, since the running server holds them. In proxy mode the certificate check is skipped: the proxy holds the certificate.

The server runs the checks once when it starts, in the background, so the result of a restart is there without anyone asking. After that it runs them on demand: it keeps the result of a run for 15 minutes and answers from it. To run the checks now, add `--fresh`:

```sh
sudo kazari admin ready --fresh
```

A fresh run looks the DNS records up and writes and removes a TXT record through the DNS provider, so it can take a minute. The same result is on the dashboard's [System page](dashboard.md#checking-health) and at `GET /v1/ready` on the [admin API](api.md#readiness), which needs an admin credential.

When [the update check](#the-update-check) knows of a newer release, the command ends with one more line:

```text
a newer release is available: 0.8.0 (checked 2026-11-21T03:12:00Z)
```

## Logs

Kazari logs to stderr through `log/slog`; under systemd, that is the journal. `log.format` sets the format, `text` by default or `json`, and `log.level` sets the level, `info` by default. Every request produces one line when it completes:

```text
time=2026-10-24T09:12:03Z level=INFO msg=request method=GET host=q3-report.art.example.com path=/ status=200 bytes=5120 duration_ms=1.8 request_id=6f1c0e9b2a7d4c33a1e0f5b6c7d8e9f0 ip=198.51.100.7 user_agent="Mozilla/5.0 ..."
```

Each line carries `method`, `host`, `path`, `status`, `bytes`, `duration_ms`, `request_id`, `ip` and `user_agent`. In proxy mode, `ip` comes from `X-Forwarded-For` or `X-Real-IP`. `user_agent` is cut to 200 bytes.

`5xx` responses log at `ERROR`, the rest at `INFO`. API `5xx` errors also log their cause on a separate line (`httpapi: request failed`); the client only ever sees `internal error`.

A successful request for `/health`, `/v1/health` or `/metrics` logs at `DEBUG` instead, so health checks and scrapes every few seconds do not bury the other requests. A failed one logs as usual, and on artifact hosts these paths are pages like any other.

### Request ids

The request id comes from an incoming `X-Request-ID` header of 8 to 128 safe characters, or Kazari generates one. Either way, it is echoed in the `X-Request-ID` response header. Quote it when reporting a problem.

### Trace ids

When tracing is on ([tracing](#tracing)), every line logged while a sampled request or job runs ends with its `trace_id` and `span_id`, so a log line leads to its trace and a trace to its lines:

```text
time=2026-11-04T09:12:03Z level=INFO msg=request method=POST host=mcp.art.example.com path=/ status=200 ... trace_id=4bf92f3577b34da6a3ce929d0e0e4736 span_id=00f067aa0ba902b7
```

Lines of requests that were not sampled carry neither.

### Exporting logs over OTLP

To send a copy of the log to the OpenTelemetry endpoint as well, set `telemetry.logs.otlp: true`. Records at `log.level` and above are exported, or at `telemetry.logs.level` when it is set; stderr is unchanged. Each record carries its trace and span ids as fields, so the backend links it to its trace.

Exported records leave out the keys that name a person or a credential: `ip`, `email`, `subject`, `handle`, `user`, `actor` and `family`. On the admin API, the handle and credential id in `path` read `REDACTED`, as does text quoted in an error message, which is where errors name a user or a key. The journal keeps all of them. User agents are still exported in the access log's `user_agent`; drop the attribute in your collector if it should not reach a hosted backend.

### Messages from libraries

The OpenTelemetry SDK and gRPC, which export telemetry over OTLP, log through the same handler in the same format, marked `logger=otel` or `logger=grpc`. Their errors log at `WARN`, like Kazari's own telemetry problems. Their warnings, such as each failed attempt to reach a gRPC collector, log at `DEBUG`.

### Finding errors

Under systemd, each line starts with its syslog priority, which the journal records and removes, so `journalctl -p` filters by level. To list the errors, every `5xx` response among them, then the warnings as well, run:

```sh
journalctl -u kazari -p err
journalctl -u kazari -p warning
```

To find the certificate messages, filter by text:

```sh
journalctl -u kazari -o cat | grep -i certificate
```

Kazari adds the priority only when stderr is the journal stream that systemd names in `JOURNAL_STREAM`. Elsewhere, such as under Docker, filter on the level instead:

```sh
docker compose logs kazari | grep -E 'level=(WARN|ERROR)'
```

## Metrics

Kazari serves Prometheus metrics at `/metrics` on the admin host. The names follow the OpenTelemetry semantic conventions, translated the Prometheus way: dots become underscores, the unit becomes a `_seconds` or `_bytes` suffix, and counters end in `_total`.

Where `/metrics` is served depends on `admin.listen`:

- With `admin.listen` set, it is served at `http://<admin.listen>/metrics` with no credential, and not on the public listener.
- Without it, `https://admin.<base>/metrics` needs `Authorization: Bearer <telemetry.metrics.token>`. While the token is empty, the public listener does not serve `/metrics` at all.
- The admin socket always serves it with no credential, and so does a `localhost` base domain.

The token keeps the metrics, and the trace ids attached to them as exemplars, for your own scraper. To generate one, run:

```sh
openssl rand -hex 32
```

Set the result as `KAZARI_TELEMETRY_METRICS_TOKEN` in the dotenv file and restart.

### Available metrics

Kazari exports the metrics below, under their Prometheus names. Over OTLP they keep the OpenTelemetry names, with dots: `http.server.request.duration`, `kazari.artifact.publishes` and so on. Every label comes from a fixed set, so a client cannot create new series; a value outside the set is `_OTHER`.

These describe requests on every surface:

| Metric                                  | Type      | Labels                                                                                                                              |
| --------------------------------------- | --------- | ----------------------------------------------------------------------------------------------------------------------------------- |
| `http_server_request_duration_seconds`  | histogram | `kazari_surface`, `http_request_method`, `http_response_status_code`, `http_route`, `url_scheme`, `network_protocol_version`, `error_type` |
| `http_server_active_requests`           | gauge     | `kazari_surface`, `http_request_method`, `url_scheme`                                                                               |
| `http_server_response_body_size_bytes`  | histogram | As `http_server_request_duration_seconds`; its sum is each surface's bandwidth                                                     |
| `kazari_api_errors_total`               | counter   | `kazari_surface` (`api`, `admin`), `kazari_error_code`                                                                             |
| `kazari_auth_requests_total`            | counter   | `kazari_surface`, `kazari_credential_type` (`api_key`, `oauth`, `admin_token`, `local`, `session`), `kazari_auth_result` (`ok`, `missing`, `invalid`, `forbidden`) |
| `kazari_rate_limit_rejected_requests_total` | counter | `kazari_surface`, `kazari_rate_limit_scope` (`ip`, `credential`, `account`)                                                     |

These describe MCP and what agents do:

| Metric                                   | Type      | Labels                                                                                                   |
| ---------------------------------------- | --------- | -------------------------------------------------------------------------------------------------------- |
| `mcp_server_operation_duration_seconds`  | histogram | `mcp_method_name`, `gen_ai_tool_name`, `gen_ai_operation_name`, `error_type`, `rpc_response_status_code`, `mcp_protocol_version`, `kazari_error_code`, `kazari_credential_type`, `kazari_mcp_input_required` |
| `kazari_mcp_tool_arguments_size_bytes`   | histogram | `gen_ai_tool_name`: what clients send inline instead of uploading                                        |
| `kazari_mcp_tool_result_size_bytes`      | histogram | `gen_ai_tool_name`: what a call adds to the model's context                                              |
| `kazari_artifact_publishes_total`        | counter   | `kazari_publish_kind` (`create`, `update`, `edit`, `import`, `rollback`), `kazari_surface`, `error_type` |
| `kazari_artifact_publish_size_bytes`     | histogram | `kazari_publish_kind`                                                                                    |
| `kazari_artifact_actions_total`          | counter   | `kazari_audit_action`, `kazari_surface` (`api`, `mcp`, `admin`, `scheduler`)                             |
| `kazari_artifact_edits_total`            | counter   | `kazari_edit_kind` (`text`, `anchor`), `kazari_edit_match` (`exact`, `trailing_whitespace`, `indentation`, `anchor`) |
| `kazari_artifact_edit_retries_total`     | counter   | None: edits applied again after a concurrent publish                                                     |
| `kazari_artifact_imports_total`          | counter   | `error_type`                                                                                             |
| `kazari_share_link_operations_total`     | counter   | `kazari_share_link_operation` (`create`, `revoke`, `open`), `error_type`                                 |
| `kazari_upload_chunks_total`             | counter   | `error_type`                                                                                             |
| `kazari_fetch_duration_seconds`          | histogram | `kazari_fetch_purpose` (`file`, `import`, `cimd`, `oidc`, `update`), `error_type` (a status code, `refused`, `timeout`, `too_large`) |
| `kazari_blob_writes_total`               | counter   | `kazari_blob_existing` (`true`, `false`): whether the content was already stored                         |
| `kazari_fetch_body_size_bytes`           | histogram | `kazari_fetch_purpose`                                                                                   |

These describe sign-in and access:

| Metric                                      | Type    | Labels                                                                                  |
| ------------------------------------------- | ------- | --------------------------------------------------------------------------------------- |
| `kazari_gate_requests_total`                | counter | `kazari_gate_result` (`public`, `member`, `share_link`, `redirect`, `denied`)           |
| `kazari_views_requests_total`               | counter | `kazari_views_result` (`counted`, `not_page`, `prefetch`, `bot`, `dropped`): what became of each HTML page served |
| `kazari_user_sign_ins_total`                | counter | `kazari_sign_in_method` (`oidc`, `password`, `passkey`, `api_key`), `kazari_sign_in_result` (`ok`, `not_admitted`, `failed`, `delayed`) |
| `kazari_user_link_operations_total`         | counter | `kazari_user_link_purpose` (`invite`, `reset`, `setup`), `kazari_user_link_operation` (`create`, `use`, `revoke`) |
| `kazari_user_password_hash_duration_seconds` | histogram | `kazari_user_password_operation` (`hash`, `verify`): the time includes the wait for a free slot |
| `kazari_oauth_token_requests_total`         | counter | `kazari_oauth_grant_type`, `error_type` (the RFC 6749 error code)                       |
| `kazari_oauth_client_registrations_total`   | counter | `kazari_oauth_registration_method` (`dcr`, `cimd`), `error_type`                        |

These describe the instance's state, read from the database at most every 30 seconds:

| Metric                                  | Type  | Labels                                                                                                      |
| --------------------------------------- | ----- | ----------------------------------------------------------------------------------------------------------- |
| `kazari_artifact_count`                 | gauge | `kazari_artifact_status` (`published`, `stale`, `unpublished`, `deleted`, `purged`), `kazari_artifact_visibility` |
| `kazari_artifact_version_count`         | gauge | None: every version of every artifact                                                                       |
| `kazari_artifact_version_usage_bytes`   | gauge | None: their logical size, which grows with every edit                                                      |
| `kazari_blob_count`, `kazari_blob_usage_bytes` | gauge | None: the blobs on disk, stored once however many versions share them                              |
| `kazari_user_count`                     | gauge | `kazari_user_state` (`active`, `disabled`), `kazari_user_role` (`admin`, `member`)                         |
| `kazari_upload_count`                   | gauge | None: chunked uploads in progress                                                                           |
| `db_client_connection_count`            | gauge | `db_client_connection_state` (`idle`, `used`), `db_client_connection_pool_name`                            |
| `db_client_connection_max`              | gauge | `db_client_connection_pool_name`                                                                            |

These describe the server itself:

| Metric                                         | Type      | Labels                                                                    |
| ---------------------------------------------- | --------- | ------------------------------------------------------------------------- |
| `kazari_job_duration_seconds`                  | histogram | `kazari_job_name`, `error_type`                                           |
| `kazari_job_last_success_timestamp_seconds`    | gauge     | `kazari_job_name`                                                         |
| `kazari_certificate_expiry_timestamp_seconds`  | gauge     | None: the served certificate; absent in proxy mode                        |
| `kazari_backup_duration_seconds`               | histogram | `error_type` (`backup_running`, `no_space`, `canceled`, `internal`) on a backup that was refused, failed or lost its reader |
| `kazari_backup_size_bytes`                     | histogram | None: the content of each archive written                                 |
| `kazari_backup_last_success_timestamp_seconds` | gauge     | None: absent until this process has written a backup                      |
| `kazari_ready_checks`                          | gauge     | `kazari_ready_check`, `kazari_ready_status` (`ok`, `warn`, `fail`, `skip`): the findings of the last readiness run |
| `kazari_ready_timestamp_seconds`               | gauge     | None: when the readiness checks last ran; absent until they have          |
| `kazari_update_available`                      | gauge     | None: `1` when a newer release is known, `0` when not; absent until a check has answered |
| `otel_sdk_exporter_span_exported_total`, `otel_sdk_exporter_metric_data_point_exported_total`, `otel_sdk_exporter_log_exported_total` | counter | `otel_component_type`, `server_address`, `error_type`: items exported over OTLP, and failures |
| `go_*`                                         | various   | The Go runtime: `go_memory_used_bytes`, `go_goroutine_count`, `go_schedule_duration_seconds` and others |
| `process_*`                                    | various   | CPU, memory and file descriptors of the process                           |
| `target_info`                                  | gauge     | The service: `service_name`, `service_version`, `service_instance_id`, `host_name` |

`kazari_surface` is `site` (artifact pages), `api`, `mcp`, `admin` (the admin API and the dashboard), `account` (the account pages under `/account/`), `apex` (the rest of the base domain itself) or `redirect` (the plain-HTTP listener that redirects to HTTPS). `http_route` is the route that matched, such as `/v1/artifacts/{slug}` or `/{path...}` for an artifact's files; it is absent when no route matched. `error_type` is set on failures only, and `kazari_error_code` is Kazari's error code, such as `version_conflict` or `quota_exceeded`.

A histogram's `_count` counts requests, calls or runs, so no separate counter exists: the request rate is `rate(http_server_request_duration_seconds_count[5m])`.

To push the same metrics over OTLP as well, set `telemetry.metrics.otlp: true` ([`telemetry.metrics`](configuration.md#telemetrymetrics)). Process metrics stay Prometheus-only; a collector's `hostmetrics` receiver covers them for an OTLP backend.

### Scraping

On the public listener, the Prometheus scrape job reads the token from a file:

```yaml
scrape_configs:
  - job_name: kazari
    scheme: https
    authorization:
      credentials_file: /etc/prometheus/kazari-token
    static_configs:
      - targets: ["admin.art.example.com"]
```

With `admin.listen` set to `"127.0.0.1:9443"`, no token is needed:

```yaml
scrape_configs:
  - job_name: kazari
    static_configs:
      - targets: ["127.0.0.1:9443"]      # admin.listen: "127.0.0.1:9443"
```

To keep the trace ids that link a latency bucket to a trace, start Prometheus with `--enable-feature=exemplar-storage`.

### Alerts

These alerts catch what usually goes wrong:

```yaml
groups:
  - name: kazari
    rules:
      - alert: KazariCertificateExpiring
        expr: kazari_certificate_expiry_timestamp_seconds - time() < 14 * 86400
      - alert: KazariJobNotSucceeding
        # three intervals of gc, the slowest job but the daily update-check
        expr: time() - kazari_job_last_success_timestamp_seconds{kazari_job_name!="update-check"} > 3 * 3600
      - alert: KazariServerErrors
        expr: sum(rate(http_server_request_duration_seconds_count{http_response_status_code=~"5.."}[5m])) > 0.1
      - alert: KazariRefusedAuthentications
        expr: sum(rate(kazari_auth_requests_total{kazari_auth_result="invalid"}[15m])) > 1
      - alert: KazariToolErrors
        expr: |
          sum by (gen_ai_tool_name) (rate(mcp_server_operation_duration_seconds_count{error_type!=""}[1h]))
            / sum by (gen_ai_tool_name) (rate(mcp_server_operation_duration_seconds_count[1h])) > 0.2
      - alert: KazariTelemetryExportFailing
        expr: sum(rate(otel_sdk_exporter_span_exported_total{error_type!=""}[15m])) > 0
      - alert: KazariBackupOverdue
        # for a timer that takes a backup every day
        expr: time() - kazari_backup_last_success_timestamp_seconds > 2 * 86400
      - alert: KazariReadinessCheckFailing
        expr: sum by (kazari_ready_check) (kazari_ready_checks{kazari_ready_status="fail"}) > 0
```

`KazariJobNotSucceeding` uses the hourly `gc` job; with other `lifecycle` intervals, use three of the longest. It leaves out `update-check`, which runs once a day and whose failure changes nothing on the instance. A tool's error ratio is worth reading by `kazari_error_code`: `version_conflict` means agents edit concurrently, `anchor_mismatch` that they edit from stale reads.

### Upgrading from before 0.6

Kazari 0.6.0 renamed every metric, and since then `https://admin.<base>/metrics` needs the token described above. When upgrading from a release before 0.6.0, update the scrape job and these queries:

| Before 0.6.0                                           | Since 0.6.0                                                                                               |
| ------------------------------------------------------ | --------------------------------------------------------------------------------------------------------- |
| `kazari_http_requests_total{surface, method, status}`  | `http_server_request_duration_seconds_count{kazari_surface, http_request_method, http_response_status_code}` |
| `kazari_http_request_duration_seconds{surface}`        | `http_server_request_duration_seconds`, with the buckets of the semantic conventions                      |
| `kazari_http_requests_in_flight{surface}`              | `http_server_active_requests{kazari_surface, http_request_method, url_scheme}`                            |
| `kazari_job_runs_total{job, result}`                   | `kazari_job_duration_seconds_count{kazari_job_name, error_type}`                                          |
| `kazari_job_duration_seconds{job}`                     | The same name, with the label `kazari_job_name`                                                           |
| `go_*` from the Prometheus client                      | `go_*` with semantic-convention names, such as `go_memory_used_bytes` and `go_goroutine_count`           |

The old `job` label collided with Prometheus's own target label, which stored it as `exported_job`; `kazari_job_name` does not.

## Tracing

Kazari traces each request and background job with OpenTelemetry and exports the spans over OTLP to a collector or tracing vendor, such as Grafana Tempo, Honeycomb or Datadog. A trace shows what an agent's request did, where the time went and why it failed.

### Turning tracing on

Tracing starts as soon as an endpoint is set. To send spans to a collector on the same Docker network, set:

```yaml
telemetry:
  otlp:
    endpoint: http://otel-collector:4318
```

A vendor usually needs an API token as a header and an `https` endpoint ([`telemetry.otlp`](configuration.md#telemetryotlp)). The standard variables work too: with `telemetry.otlp.endpoint` unset, Kazari exports to `OTEL_EXPORTER_OTLP_ENDPOINT`, as any OpenTelemetry service does.

To check the export, look for the line `telemetry: exporting over OTLP` at start, which names each signal's endpoint and protocol. Then open a page and an API route, and search the backend for the service `kazari`. Spans leave in batches every few seconds.

### What is traced

Every request gets a server span named after its method and route, such as `GET /v1/artifacts/{slug}` or `GET /{path...}` for an artifact's files. The span carries the status code, the route, the surface (`kazari.surface`), the request id and, on the control plane, the credential type and OAuth client. Scheduled jobs get a span named `job <name>`, and startup a `kazari.start` span with the database migration and OIDC discovery under it.

On `mcp.`, each JSON-RPC method gets its own span under the request's, named as the MCP semantic conventions do: `tools/call create_artifact` for a tool call, or the method alone, such as `tools/list`, `resources/read` or `server/discover`, the call a client of protocol version 2026-07-28 opens with. Kazari advertises no `list_changed` notifications, so such a client never holds a `subscriptions/listen` stream open; a request on `mcp.` lasts as long as its tool call. A tool span carries the tool, the negotiated protocol version, the sizes of the arguments and the result, and the outcome: a tool that failed is `error.type=tool_error` with Kazari's error code in `kazari.error.code`; one that asked the user for confirmation has a `kazari.mcp.input_request` event. Tool arguments and results themselves are never recorded.

Below the request, each change or lookup has a span named for the operation, the same whether it came over REST or MCP: `artifact.create`, `artifact.edit`, `upload.append_chunk`, `oauth.token` and so on, with the slug, version, file count and size. Under those come a span per store method, such as `ArtifactStore.AddVersion`, and per blob read or write (`blob.put`, `blob.open`), which show where a request's time went. A fetch for an import, a file named by URL, a client metadata document or the identity provider adds a `GET` client span per attempt, redirects included, with the host and the URL's query values hidden.

The pages Kazari renders itself are traced the same way. A dashboard request is `GET /people/{handle}` or `POST /system/backup` on the `admin` surface, an account page `POST /account/keys` on the `account` surface, and a sign-in `POST /oauth/signin` on `mcp`, each with the operations it ran below it: `user.invite`, `user.redeem_link`, `user.check_password`, `oauth.redeem`, `oauth.end_sessions`, `backup.write` with the archive's file count and size, and `preflight.ready` with how many checks failed or warned. A readiness run can outlive the request that started it; its span ends when the run does. The update check's request to GitHub is a client span under `job update-check`.

An MCP client can also pass `traceparent` in a request's `params._meta`. The tool span then joins that trace, with a link to the HTTP request's span.

A server error (`5xx`) marks the span as failed, with `error.type` set to Kazari's error code. A panic adds an `exception` event. Requests for `/health`, `/v1/health` and `/metrics` are counted but not traced.

### Sampling

By default, Kazari keeps every trace that starts on `api.`, `mcp.`, `admin.`, the admin socket or the account pages under `/account/`, and one in a hundred on artifact hosts and the rest of the base domain. Errors on artifact hosts are still counted in full by the metrics; to keep every failing page view as a trace, sample in the collector instead ([`telemetry.traces`](configuration.md#telemetrytraces)).

A trace is kept or dropped whole: a span's children follow its decision.

### Running a collector

A collector between Kazari and the backend batches the export, can keep every failing request's trace however Kazari samples, and can drop attributes before they leave your network. To start from an example, fetch the configuration for the OpenTelemetry Collector's contrib distribution:

```sh
curl -fsSLO https://kazari.io/deploy/otel-collector.yaml
```

It receives OTLP on ports 4317 (gRPC) and 4318 (HTTP), keeps every trace with an error or a span over two seconds plus one in ten of the rest, and prints what it receives until you add your backend's exporter. Its `attributes/drop-paths` processor removes slugs and paths from spans, for a hosted backend.

### Joining a caller's trace

On `api.`, `mcp.`, `admin.` and the account pages, a request with a W3C `traceparent` header continues the caller's trace, so an agent runtime or proxy that traces its own requests sees Kazari's spans under them. Artifact hosts and the rest of the base domain ignore the header: their visitors are anonymous, and a parent they sent would decide sampling. Kazari never sends `traceparent` to the sites it fetches.

### What spans never contain

Spans never contain who made a request: no handle, email, OIDC subject, client IP or credential id (API key prefix, OAuth grant id). On the admin API, the dashboard and the account pages, the handle, key prefix and other ids in `url.path` read `REDACTED`. Query strings keep their keys and hide every value, because share tokens, OAuth codes and the tokens of invite, reset and setup links travel there. Request and response bodies, file contents and credentials are never recorded. The audit log and the access log keep who did what, on the server.

Spans do contain slugs, versions, file paths, OAuth client ids and control-plane user agents. Traces often go to a hosted vendor, so treat them as you would the access log.

## Scheduled jobs

Kazari runs seven maintenance jobs on its own, and an eighth with an identity provider:

| Job              | Interval                                           | What it does                                                                                         |
| ---------------- | -------------------------------------------------- | ---------------------------------------------------------------------------------------------------- |
| `sweep`          | `lifecycle.sweep_interval`, 15 minutes by default  | Moves artifacts along the [lifecycle](#lifecycle): soft-deletes expired stale ones, purges soft-deleted ones past retention and removes tombstones past the tombstone period. It also deletes the versions past an artifact's `kazari.keep_versions` |
| `gc`             | `lifecycle.gc_interval`, 1 hour by default         | Deletes blobs that no version references, in batches of 1000, sparing blobs written in the last hour |
| `uploads-expire` | 10 minutes                                         | Removes chunked uploads past `limits.upload_ttl` and staging directories left by a crash             |
| `oauth-cleanup`  | 1 hour                                             | Deletes expired OAuth authorization requests, codes and tokens, expired browser sign-ins, sign-in codes and sessions, and OAuth clients that completed no grant within a day of registering |
| `oidc-discovery` | 30 seconds                                         | With `auth.oidc`, retries discovery until the identity provider answers, then does nothing ([sign-in is temporarily unavailable](identity.md#sign-in-is-temporarily-unavailable)) |
| `update-check`   | 1 day                                              | Asks GitHub for the newest release, so the dashboard can say when the instance is behind ([the update check](#the-update-check)) |
| `views-flush`    | 1 minute                                           | Writes the [page views](#page-views) counted since its last run                                     |
| `views-prune`    | 1 day                                              | Deletes daily view counts older than `analytics.retention`                                           |

`gc` spares recent blobs so that a publish in progress never loses one. The unreferenced blobs it finds are those of purged or hard-deleted artifacts and of a publish that a crash interrupted, since a failed publish deletes its own. It also removes temporary files a crash left behind: blob writes, spilled bundles and database copies of a backup older than an hour. While a [backup](#taking-a-backup) is being written, `gc` skips its run and logs `gc: skipped, a backup is running`.

Each interval is jittered by ±10%, and the first run comes after a random delay of up to 10% of the interval. A job never runs twice at once.

### Checking the jobs

To see each job's interval, last run, next run and last error, list them:

```sh
kazari admin jobs list
```

A sweep or gc that changed something also logs a summary line, such as `sweep soft_deleted=1 purged=0 freed=0 versions_deleted=0`.

### Running a job now

To run a job now, name it. The job runs synchronously, and the command returns when it finishes:

```sh
kazari admin jobs run sweep

# the same through the admin API
curl -X POST -H "Authorization: Bearer $KAZARI_ADMIN_TOKEN" https://admin.art.example.com/v1/jobs/gc/run
```

### The update check

The `update-check` job is the one request Kazari makes on its own account. Once a day it asks `https://api.github.com/repos/PeterBooker/kazari/releases/latest` for the newest release and compares its version with the running one.

The request carries nothing about the instance: no version, no domain and no identifier, only the `User-Agent` `kazari`. When a newer release exists, the server logs `a newer release is available`, and the dashboard's Overview and System pages say so. A check that fails is logged as a failed job run and changes nothing else.

The job exists only in a build with a release version, so a build from source does not have it. To turn the check off, set `admin.update_check` to `false`:

```yaml
admin:
  update_check: false
```

## Page views

Kazari counts how often each artifact's pages are opened, for the [dashboard](dashboard.md#page-views) and `kazari admin views`. Counting costs serving no database work: views add up in memory, and the `views-flush` job writes them once a minute in one transaction.

### What counts as a view

A view is a `GET` for an HTML file of an artifact, on its own address or a version's, that loads a page into a browser tab. These do not count:

- scripts, styles, images and every other file that is not HTML;
- a page in a frame, a fetch from a script, a `HEAD` or a range request;
- a prefetch or prerender that a browser makes before anyone clicks;
- a request whose user agent names a crawler, a link preview or a command-line client, or names nothing.

A mail scanner that opens links in a real browser looks like a person and counts. The `kazari_views_requests_total` metric shows how many requests each rule left out.

### What is kept

Kazari stores daily totals per page and per file: views, daily visitors, and how many views came from members and through share links. It keeps no request log, IP address, user agent or referrer.

A daily visitor is one IP address and user agent on one page on one day. Kazari tells them apart with a hash held in memory and replaced at midnight UTC, so a visitor cannot be followed from one day to the next, and a restart during a day counts a returning visitor again.

On a members-only page, Kazari also records which members opened it, with their first and last view and a count. `analytics.readers: false` turns that off.

### Retention

The daily counts are kept for `analytics.retention`, 366 days by default, and the `views-prune` job deletes older ones. The list of members who opened a page is kept while the page exists. Removing a page for good deletes its counts with it.

### Limits

Between two flushes Kazari holds at most 100,000 counts (a page and a day, a file and a day, or a reader) and tells apart at most 200,000 visitors a day. Past either limit it counts the view as `dropped` or stops counting new visitors, so serving never takes more memory. A failed flush keeps its counts for the next run, within the same limit, and the dashboard lists the failing job under Needs attention.

When the server stops, it writes the counts it holds after the last request has finished. A crash loses at most the last minute.

## Lifecycle

An artifact that is no longer needed moves through these states until its slug is free again:

```text
published ──stale──▶ stale ──grace──▶ soft-deleted ──retention──▶ purged ──tombstone──▶ slug free
    ▲                  │                   │
    └──── unstale ─────┘                   └── restore (PUT/PATCH {"restore": true})
```

Each state works as follows:

- **Stale**: `kazari stale <slug> [--grace 72h]` sets `stale_at`, and sets `expires_at` to now plus the grace period. The page keeps serving. Publishing a new version or restoring the artifact clears the mark unless `keep_stale` is set. A credential without `artifacts:delete` can mark only its own user's artifacts.
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

### Service workers

A page can register a service worker on its own host. The browser keeps that worker after the person who uploaded it loses the artifact, so Kazari answers every worker-script request (`Service-Worker: script`) itself. The answer is a small script that replaces the worker and unregisters it, sent instead of the file when:

- the artifact or version is not served: unpublished, soft-deleted, purged, deleted or never there;
- the path holds no file, or a file that is not JavaScript;
- the artifact is `members` and the visitor has no access;
- the script's uploader can no longer change the artifact: they are disabled, or under `auth.write_policy: owner` they are neither its owner nor an admin.

The uploader is whoever first put that content at that path in the artifact, so a new version that carries the script on does not change it, and nor does deleting the version it was uploaded in. To keep a worker after a transfer, or after its author is disabled, publish the script with changed content. Enabling the user again restores the script.

A purged artifact, and the host of a deleted version of a page that is not `members`, also send `Clear-Site-Data: "storage"`. Other answers leave the origin's storage alone, because a first registration looks the same as an update check, and a page that registers a missing script would lose its visitors' data on every load.

The browser checks for an update only when it returns to the host, and it runs the check after the navigation. The first visit after the change therefore still passes through the old worker, which can see that request and its response, and a browser that never returns keeps the worker. On a `--v<N>` host, whose files are cached as immutable, a worker that asked for cached updates can put the check off for up to a day. After disabling a user or transferring an artifact, revoke any share link that matters.

### Unpublished pages

`unpublished` is separate from this chain. It comes from `kazari visibility <slug> unpublished` or `admin hide`. The page answers `404`, nothing is deleted, and nothing moves on until someone deletes it.

## Quotas

Each user has a quota, `quota_bytes`. A new user gets `limits.default_user_quota`, 2 GiB by default, and `0` means unlimited.

Usage is the sum of the sizes of every version of every artifact the user owns, counted logically, plus 4 KiB for each file of each version, whatever its size, because every version keeps a database row per file. A 5 MiB site of 20 files with ten versions uses 50.8 MiB of quota, even though unchanged files are stored once. Soft-deleted artifacts keep counting until they are purged; a hard delete frees the space at once.

To free the space of an artifact edited often, delete its old versions: run `kazari versions prune <slug> --keep 20` once, or set its `kazari.keep_versions` metadata for every [sweep](#scheduled-jobs) ([deleting versions](cli.md#versions)). The usage drops at once.

Kazari checks the owner's quota whenever a version is added, whoever adds it. Under the `shared` write policy, a version that another member adds to someone else's artifact counts against the owner's quota too, and a version that would exceed it gets `403 quota_exceeded`. An admin transfer moves the artifact's usage to the new owner.

A publish that fails keeps nothing: Kazari deletes the blobs it wrote before it answers, except any that a version uses, so a refused publish takes no disk once it has finished. While it runs, it can hold up to the owner's remaining quota or `max_version_bytes`, whichever is smaller.

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

Disabling a user revokes every API key at once, makes every OAuth access and refresh token fail, ends their browser sessions, revokes every share link they created on any artifact, and refuses their sign-in. An invite or reset link that was waiting for them stops working too. Links they gave to outside reviewers end too: a reviewer who still needs the page needs a new link from someone else. `--enable` lets them sign in again, but revoked keys and links stay revoked.

Kazari has no command that deletes a user. A person who leaves is disabled and their pages are [handed over](#handing-over-a-users-pages), so the audit log and the versions they published keep naming a handle that exists.

### What Kazari refuses

Two changes to a user are refused, whichever way they are asked for:

- **The last active admin cannot be disabled or demoted.** The request fails with `409 last_admin`. Make another person an admin first.
- **Nobody disables or demotes their own account.** The request fails with `403 forbidden`. Ask another admin.

The admin socket and the admin token are no person, so the second rule never stops them. An admin who is locked out is let back in on the server ([a locked-out admin](dashboard.md#the-only-admin-cannot-sign-in)).

### Signing a user out

To end a user's browser sessions without touching their keys or connected apps, for example after a laptop was lost:

```sh
kazari admin users sign-out ana
```

### Revoking one API key

To revoke a single key, list the user's keys and revoke one by its prefix:

```sh
kazari admin keys list ana
kazari admin keys revoke ana abcdefgh
```

Revoking a key also revokes every key its owner issued with it, and ends every OAuth grant approved with one of them. The list shows, under `ISSUED BY`, the key each key was issued with. To retire a key that issued others and keep those, rotate it instead.

To revoke one OAuth grant instead, which also revokes every token rotated from it, see [Revoking one client's access](identity.md#revoking-one-clients-access).

### Rotating an API key

To replace a key without an interruption, rotate it. The new key has the same owner, label, scopes, restriction and lifetime, and the old key keeps working for the overlap:

```sh
kazari admin keys rotate ana abcdefgh --overlap 24h
```

The command prints the new key once. Put it wherever the old one is stored before the overlap ends; the old key then expires on its own. The overlap is 24 hours by default and at most 7 days, and `--overlap 0` ends the old key at once.

A person rotates their own keys with [`kazari keys rotate`](cli.md#keys). An OAuth grant approved with the old key ends with it, so its client must connect again with the new key. To make every key expire, set [`auth.tokens.max_key_ttl`](configuration.md#authtokens).

### Handing over a user's pages

A disabled user's artifacts keep serving, apart from any [service worker](#service-workers) they uploaded. To hand one over to another user, transfer it:

```sh
kazari admin transfer q3-report peter
```

To hand over everything a user owns at once, soft-deleted pages included, name the user and the new owner:

```sh
kazari admin users transfer ana peter
```

The command prints how many pages moved and their slugs. Their versions then count against the new owner's quota, and the audit log records an `artifact.transfer` for each.

## Responding to abuse

To take a page down at once, hide it and disable its owner:

```sh
kazari admin hide q3-report                 # unpublishes: the page answers 404 immediately
kazari admin users update ana --disable
```

`hide` deletes nothing, and the audit log records it as `artifact.visibility`. It is not a lock: the owner, or under the `shared` policy any writer, can publish the page again. That is why disabling the owner usually goes with it.

**Take down** on a page's page in the [dashboard](dashboard.md#taking-a-page-down) does both in one step.

### Removing the content for good

To remove the content for good, use an admin user's key, with the `admin` scope, on the user API, then collect the blobs:

```sh
KAZARI_TOKEN=kzk_admin_key kazari rm q3-report --hard
kazari admin jobs run gc
```

Blobs written in the last hour survive one more `gc` run.

> [!WARNING]
> Version hosts are served with `Cache-Control: public, max-age=<lifecycle.tombstone, at most a year>, immutable`, 90 days by default. Browsers and any CDN in front can keep copies of a pinned version for that long, a deleted version too; purge them there if needed. A [staged version](api.md#staging-a-version) is `private, no-cache` until it is published.

## Audit log

Kazari records every change with its time, actor, action, slug and details. The actor is a handle, `admin` for the admin token and the admin socket, or `scheduler` for jobs; no user can have either of those handles.

Events about a person's account also carry a subject: the handle of that person, whoever acted. A reset link an admin made for `ana` has the admin as actor and `ana` as subject, and so does a key the admin issued her. Sign-ins, keys, passwords, links, grants, sessions and user changes have one; artifact events do not. Older events got theirs from their details at the upgrade that added it, except a grant revoked by id whose tokens were already gone.

Artifact events also carry `credential`, the credential that acted: `api_key:<prefix>`, `oauth:<grant>`, `admin_token`, or `local` for the admin socket. An `oauth:` credential has one id per grant, the same across refreshes.

The actions, with the details each records:

| Action                | When                                                                 |
| --------------------- | -------------------------------------------------------------------- |
| `artifact.create`     | Created (details: `version`, `size`)                                 |
| `artifact.update`     | Fields changed or version added (`version`, `size` when a version; `staged` when not published) |
| `artifact.publish`    | Published version changed (`version`)                                |
| `artifact.version_delete` | Versions deleted (`versions`, comma-separated; `reason: keep_versions` from the sweep) |
| `artifact.stale`      | Marked stale (`expires_at`)                                          |
| `artifact.unstale`    | Stale mark cleared                                                   |
| `artifact.visibility` | Visibility set, including unpublish and `hide` (`visibility`)        |
| `artifact.delete`     | Soft-deleted, by a user or by the sweep (`reason: expired`)          |
| `artifact.restore`    | Restored from soft delete                                            |
| `artifact.purge`      | Purged, by `rm --hard` or the sweep (`reason: retention`)            |
| `artifact.free`       | Tombstone removed, slug free                                         |
| `artifact.transfer`   | Owner changed (`from`, `to`)                                         |
| `user.create`         | User created by an admin, provisioned at sign-in (`via: identity`) or created with the setup link (`via: setup`) |
| `user.update`         | User changed, identity linked or unlinked, or renamed at upgrade (`handle`, `renamed_from`) |
| `key.issue`           | API key issued (`prefix`, `label`, `scopes`; `slugs`, `expires_at` and `issued_by` when it has them) |
| `key.rotate`          | API key replaced (`prefix`, `new_prefix`, `old_expires_at`, and the new key's details) |
| `key.revoke`          | API key revoked (`prefix`; `issued_by` when revoking the key it was issued with did it) |
| `oauth.grant`         | Consent approved (`client_id`, `scopes`, `via: oidc`, `password` or `api_key`, and `key_prefix` for `api_key`); `password` stands for any sign-in to a built-in account, a passkey's too |
| `oauth.revoke`        | OAuth grant revoked (`grant`, or `user` for all of a user's; `reason: refresh_replay` when Kazari revoked it because a retired refresh token was presented again) |
| `oauth.signin`        | A browser signed in (`via: oidc`, `password`, `passkey`, `api_key` or `link` (choosing a password with a link), and `key_prefix` for `api_key`) |
| `dashboard.signin`    | An admin opened a session on the dashboard (`credential`)            |
| `session.revoke`      | A person's browser sessions ended (`user`, `sessions`)               |
| `invite.create`       | Invite, reset or setup link made (`id`, `purpose`, `expires_at`; `handle` unless `setup`; `revoke_all` when set) |
| `invite.use`          | Link used to set a password (the same details)                       |
| `invite.revoke`       | Link revoked before it was used (the same details)                   |
| `password.set`        | Password set (`handle`, `via: invite`, `reset`, `setup`, `change` or `recovery`) |
| `recovery.create`     | A new set of recovery codes made, ending the old one (`handle`, `codes`) |
| `passkey.create`      | A passkey added on the account page (`handle`, `id`, `name`)         |
| `passkey.delete`      | A passkey removed (`handle`, `id`, `name`)                           |
| `recovery.use`        | A recovery code used to set a password (`handle`, `left`)            |
| `backup.create`       | Backup written (`files`, `bytes`; `certificates` is `true` when the archive holds `certmagic/`) |
| `audit.export`        | The log exported as CSV from the dashboard                           |
| `share_link.create`   | Share link created (`prefix`, `expires_at`)                          |
| `share_link.revoke`   | Share link revoked (`prefix`; `created_by` when disabling its creator did it) |

An event is written just after the change it describes, outside its transaction, so a crash in between loses that one row. The log is append-only and never pruned.

### Reading the log

To read the log, filter it by time, slug, actor, subject or action:

```sh
kazari admin audit --since 24h
kazari admin audit --slug q3-report --limit 50
kazari admin audit --actor ana --action key. --since 168h
kazari admin audit --subject ana --since 720h
curl -H "Authorization: Bearer $KAZARI_ADMIN_TOKEN" "https://admin.art.example.com/v1/audit?actor=ana&since=2026-10-01T00:00:00Z"
```

`--action` takes an exact action such as `key.issue`, or a prefix ending in a dot such as `artifact.`, and can be repeated. When more events match than one page holds, stderr prints the `--cursor` for the next page. The dashboard's [Activity page](dashboard.md#activity) shows the same log with the same filters.

## Troubleshooting

Each entry is named for the symptom. Certificate and DNS problems are in [DNS and certificates](certificates.md#troubleshooting), and `kazari check` reproduces most of them in one run.

### `401 Unauthorized`

Kazari did not accept the credential. Check that:

- the header is `Authorization: Bearer <token>`;
- the admin token is used only on `admin.<base>`, not on `api.` or `mcp.`;
- the key is not revoked or expired (`kazari admin keys list <handle>`), and its user is not disabled;
- an OAuth client can refresh its access token, which lives one hour. A client that cannot refresh must sign in again. `kazari admin grants list <handle>` shows each connection's last refresh and expiry.

A `403` instead means the credential is valid but lacks a scope: `artifacts:share` for a change of visibility or a share link, `artifacts:delete` for `rm` or for `stale` on an artifact you do not own, or the `admin` scope for the admin API. A key [restricted to some artifacts](api.md#restricted-credentials) also gets `403` for every other slug; `kazari me` shows the restriction.

### `413 Payload Too Large`

The `message` names the limit that was hit:

- a file over `limits.max_file_bytes`;
- a version over `limits.max_version_bytes`, which counts all files of the version, not only the ones sent;
- a chunk over `limits.max_chunk_bytes`;
- the request body.

Raise the limit and restart, or split the bundle across a chunked upload.

In proxy mode, the proxy has its own limit. nginx defaults to `client_max_body_size 1m`, which answers `413` with an HTML page before Kazari sees the request.

### `404` or `410` on a page

`404` with the instance page means there is no such slug, the artifact is unpublished or soft-deleted, or there is no such version on a `--v<N>` host. `410 Gone` means the artifact was purged and its slug is still in the tombstone period, or, on a `--v<N>` host of a page that is not `members`, that the version was deleted.

`kazari get <slug>` shows which timestamps are set.

### `401` or a sign-in redirect on a page

The artifact's visibility is `members`. A browser is sent to sign in and other clients get `401`; signed-in members and holders of a share link see the page.

To let in someone outside the instance, create a share link with `kazari share create <slug>`. To open the page to everyone, run `kazari visibility <slug> public`. If the redirect never ends, see [A members page keeps redirecting to sign-in](identity.md#a-members-page-keeps-redirecting-to-sign-in).

### A CDN serves a members page without sign-in

Responses of `members` pages are `Cache-Control: private, no-cache`, so a shared cache does not keep them. Copies it took while the page was still `public` stay, though, and pinned versions are cached for up to `lifecycle.tombstone`.

When you switch a page from public to `members`, purge `https://<slug>.<base>/` and `https://<slug>--v*.<base>/` at the CDN.
