# REST API

Kazari has two REST APIs on two hosts:

- the **user API** at `https://api.<base>/v1`, for publishing and managing artifacts;
- the **admin API** at `https://admin.<base>/v1`, for users, keys, jobs, settings and the audit log.

Both speak JSON (`Content-Type: application/json`) unless noted. The MCP server and the CLI use the same wire types, so an artifact looks the same everywhere.

## Authentication

Every user API route needs a bearer credential:

```http
Authorization: Bearer kzk_abcdefgh_<52 characters>
```

Either an API key (`kzk_<8-character prefix>_<secret>`, issued by an admin) or an OAuth access token issued to an MCP client ([identity.md](identity.md)). Credentials carry scopes:

| Scope              | Allows                                                                      |
| ------------------ | --------------------------------------------------------------------------- |
| `artifacts:read`   | `GET` routes                                                                |
| `artifacts:write`  | create, update, publish, stale, visibility, uploads                         |
| `artifacts:delete` | `DELETE /artifacts/{slug}` (soft and hard)                                  |
| `admin`            | the admin API; held only by admin users, and implies every other scope      |

A missing or invalid credential gets `401` with `WWW-Authenticate: Bearer error="invalid_token"`; a valid one without the scope gets `403 forbidden`.

The admin API accepts the instance admin token (`auth.admin_token`) or any credential of an admin user that carries the `admin` scope. A valid credential of anyone else gets `403`.

## Errors

Every error has the same envelope:

```json
{ "error": { "code": "slug_taken", "message": "slug \"demo\" is already taken" } }
```

`upload_offset_mismatch` adds `current`, the staged size to resume from:

```json
{ "error": { "code": "upload_offset_mismatch", "message": "...", "current": 4194304 } }
```

| Code                     | Status | Meaning                                                                            |
| ------------------------ | ------ | ---------------------------------------------------------------------------------- |
| `invalid_argument`       | 422    | Malformed or missing body, unknown JSON field, bad query parameter or value        |
| `invalid_argument`       | 415    | Body is neither `application/json` nor `multipart/form-data` (create and update)   |
| `invalid_slug`           | 422    | Slug breaks the slug rules                                                         |
| `invalid_path`           | 422    | File path breaks the path rules                                                    |
| `invalid_metadata`       | 422    | Bad metadata key, value, pair count or `kazari.*` key                              |
| `too_many_files`         | 422    | More than `limits.max_files` in a version or upload                                |
| `unauthorized`           | 401    | Missing, invalid, expired or revoked credential                                    |
| `forbidden`              | 403    | Missing scope, write policy, disabled user, or admin role required                 |
| `quota_exceeded`         | 403    | The owner's storage quota would be exceeded                                        |
| `too_many_uploads`       | 403    | The caller already has `limits.max_open_uploads` open uploads                      |
| `not_found`              | 404    | No such artifact, user, key, grant, job or route; also a soft-deleted artifact on write routes |
| `version_not_found`      | 404    | No such version                                                                    |
| `file_not_found`         | 404    | No such file in the version                                                        |
| `upload_not_found`       | 404    | Unknown, expired or someone else's upload                                          |
| `method_not_allowed`     | 405    | Route exists, method does not (`Allow` header lists the methods)                   |
| `slug_taken`             | 409    | Slug exists (including a purged slug still in its tombstone period)                |
| `upload_offset_mismatch` | 409    | Chunk offset does not match the staged size                                        |
| `handle_taken`           | 409    | Admin API: handle exists                                                           |
| `email_taken`            | 409    | Admin API: another user has the email                                              |
| `job_running`            | 409    | Admin API: the job is already running                                              |
| `gone`                   | 410    | The artifact has been purged                                                       |
| `version_conflict`       | 412    | `expected_version` / `If-Match` is not the latest version                          |
| `too_large`              | 413    | A file, version, chunk, upload, manifest or request body is over its limit         |
| `rate_limited`           | 429    | Too many requests; see `Retry-After`                                               |
| `internal`               | 500    | Server fault; details are logged, never returned                                   |

## Rate limits

Every control-plane host applies token buckets and answers `429 rate_limited` with `Retry-After: <seconds>` when one is empty:

- **per client IP**, before authentication: 120 requests a minute (burst 120). The user API, the `mcp.` host (MCP endpoint and OAuth endpoints together) and the admin API's `/v1/` routes each have their own bucket. IPv6 clients share one bucket per `/64`. `/health` and `/metrics` are not limited.
- **per credential**, on write and delete routes and the MCP write and delete tools: `limits.write_rate_per_minute` (default 60, burst the same). One budget covers a credential on both the user API and MCP. An MCP tool over it fails with `rate_limited: too many writes; retry in <n>s`.

## The artifact record

Every response that returns an artifact uses this shape. Timestamps are RFC 3339 in UTC; lifecycle timestamps are omitted when unset.

```json
{
  "slug": "q3-report",
  "name": "Q3 report",
  "description": "Revenue by region",
  "owner": "peter",
  "visibility": "public",
  "metadata": { "team": "finance", "kazari.spa": "true" },
  "url": "https://q3-report.art.example.com/",
  "published_version": 2,
  "latest_version": 3,
  "size_bytes": 184320,
  "stale_at": "2026-10-24T09:00:00Z",
  "expires_at": "2026-11-07T09:00:00Z",
  "unpublished_at": "2026-10-24T09:30:00Z",
  "deleted_at": "2026-10-24T10:00:00Z",
  "purged_at": "2026-11-23T10:00:00Z",
  "created_at": "2026-10-01T12:00:00Z",
  "updated_at": "2026-10-24T09:00:00Z"
}
```

- `visibility` is the stored listing visibility, `public` or `unlisted`. Taking a page offline sets `unpublished_at` and keeps `visibility` for when it comes back.
- `size_bytes` is the sum of every version's size.
- A version: `{ "number", "label", "created_by", "created_at", "size_bytes", "url", "files" }`, where `url` is the pinned `https://<slug>--v<N>.<base>/` and `files` (only on request) is a list of `{ "path", "hash", "size", "content_type" }` with `hash` the hex SHA-256.

Create, update, publish, unstale and visibility changes return a **publish result**:

```json
{ "url": "https://q3-report.art.example.com/", "version": 3, "artifact": { ... } }
```

`version` is the version the call created or moved to; for a fields-only change it is the published version.

### Rules

- **Slug**: `^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`, 3 to 63 characters, no `--` (reserved for version hosts), not in `serve.reserved_slugs`. Fixed at creation.
- **File path**: relative, slash-separated, cleaned with `path.Clean`; no leading or trailing `/`, no `..` escaping the bundle, no control characters or backslashes, segments up to 255 bytes, whole path up to 1024 bytes. A path may appear once per request.
- **Content type**: the one given with the file, else from the extension, else sniffed from the first 512 bytes. Never from the request that later reads it. A given type must be a parseable media type of at most 255 printable ASCII bytes (`422 invalid_argument` otherwise).
- **Metadata**: keys `[a-zA-Z0-9_.-]{1,64}`, values up to 4096 bytes, at most `limits.metadata_max_pairs` pairs. Keys under `kazari.` are interpreted by the server and limited to:

  | Key                | Values           | Effect                                                         |
  | ------------------ | ---------------- | -------------------------------------------------------------- |
  | `kazari.spa`       | `true`, `false`  | `true` serves `index.html` for any missing path without an extension |
  | `kazari.indexable` | `true`, `false`  | `true` drops `X-Robots-Tag: noindex`                           |
  | `kazari.csp`       | printable ASCII, one line | Replaces the instance CSP, when `serve.allow_csp_override` is on |

- **Write policy**: under `auth.write_policy: owner`, only the owner and admins may change an artifact; under `shared` (default) anyone with `artifacts:write` may.

## User API

Base URL `https://api.<base>/v1`.

| Method   | Path                                  | Scope    | Success                     |
| -------- | ------------------------------------- | -------- | --------------------------- |
| `POST`   | `/artifacts`                          | write    | `201` publish result        |
| `GET`    | `/artifacts`                          | read     | `200` list page             |
| `GET`    | `/artifacts/{slug}`                   | read     | `200` artifact and versions |
| `PUT`    | `/artifacts/{slug}`                   | write    | `200` publish result        |
| `PATCH`  | `/artifacts/{slug}`                   | write    | `200` publish result        |
| `DELETE` | `/artifacts/{slug}`                   | delete   | `204`                       |
| `POST`   | `/artifacts/{slug}/publish`           | write    | `200` publish result        |
| `POST`   | `/artifacts/{slug}/stale`             | write    | `200` stale result          |
| `DELETE` | `/artifacts/{slug}/stale`             | write    | `200` publish result        |
| `GET`    | `/artifacts/{slug}/files/{path...}`   | read     | `200` file bytes            |
| `POST`   | `/uploads`                            | write    | `201` upload                |
| `PATCH`  | `/uploads/{id}/files/{path...}`       | write    | `200` chunk result          |
| `DELETE` | `/uploads/{id}`                       | write    | `204`                       |
| `GET`    | `/me`                                 | any      | `200` caller                |
| `GET`    | `/me/keys`                            | any      | `200` caller's keys         |

### Create an artifact: `POST /artifacts`

Two body formats. Either way the first version is published at once and the response is `201 Created` with a publish result.

**JSON, inline files.** `content` is UTF-8 text by default; set `"encoding": "base64"` for binary.

```sh
curl -sS https://api.art.example.com/v1/artifacts \
  -H "Authorization: Bearer $KAZARI_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "slug": "hello",
    "name": "Hello",
    "description": "A first page",
    "visibility": "public",
    "metadata": { "team": "docs" },
    "label": "first draft",
    "files": [
      { "path": "index.html", "content": "<!doctype html><h1>Hello</h1>" },
      { "path": "logo.png", "content": "iVBORw0KGgo...", "encoding": "base64", "content_type": "image/png" }
    ]
  }'
```

| Field         | Type   | Notes                                                              |
| ------------- | ------ | ------------------------------------------------------------------ |
| `slug`        | string | Required                                                           |
| `name`        | string | At most 256 bytes, one line                                        |
| `description` | string | At most 4096 bytes; may contain line breaks and tabs               |
| `visibility`  | string | `public` (default) or `unlisted`                                   |
| `metadata`    | object | String to string                                                   |
| `label`       | string | Label of version 1; at most 256 bytes, one line                    |
| `files`       | array  | `{ path, content, encoding?, content_type? }`                      |
| `upload_id`   | string | Staged files from a chunked upload; merged with `files`, which win a path clash |

Text fields must be valid UTF-8 without control characters (C0, DEL, C1) or the bidirectional override and isolate characters (U+202A to U+202E, U+2066 to U+2069); `description` may contain newlines and tabs. The same rules apply to updates and over MCP; a violation is `422 invalid_argument`.

At least one file is required. The whole JSON body is capped at `limits.max_version_bytes` plus 1 MiB, so base64 content (a third larger than the bytes it carries) reaches the cap sooner; use a bundle or a chunked upload for anything large.

**Multipart, tar.gz bundle.** The first part must be `manifest` (JSON: `slug`, `name`, `description`, `visibility`, `metadata`, `label`), the second `bundle`, a gzip-compressed tar.

```sh
tar -czf site.tar.gz -C site .
curl -sS https://api.art.example.com/v1/artifacts \
  -H "Authorization: Bearer $KAZARI_TOKEN" \
  -F 'manifest={"slug":"q3-report","name":"Q3 report","label":"v1"};type=application/json' \
  -F 'bundle=@site.tar.gz;type=application/gzip'
```

Only regular files in the tar are kept: directories, links and devices are skipped, and leading `./` is stripped. The manifest is capped at 64 KiB; the request body and the decompressed tar stream (every byte of it: headers, long-name records, padding, skipped entries and file content) are each capped at `limits.max_version_bytes` plus 1 MiB. A file name longer than 1024 bytes is refused as soon as it is read.

### List artifacts: `GET /artifacts`

```sh
curl -sS -H "Authorization: Bearer $KAZARI_TOKEN" \
  'https://api.art.example.com/v1/artifacts?mine=any&stale=exclude&metadata.team=finance&limit=20'
```

| Parameter         | Values                                  | Default   |
| ----------------- | --------------------------------------- | --------- |
| `mine`            | `created`, `updated`, `any`             | no filter |
| `metadata.<key>`  | exact value; repeat for several keys    |           |
| `stale`           | `include`, `exclude`, `only`            | `include` |
| `deleted`         | `exclude`, `include`, `only`            | `exclude` |
| `cursor`          | `next_cursor` of the previous page      |           |
| `limit`           | 1 to 200                                | 50        |

You see public, published, live artifacts plus every artifact you own, created or updated (whatever its visibility). Purged artifacts never appear. Results are newest-updated first:

```json
{ "items": [ { ... } ], "next_cursor": "eyJ..." }
```

`next_cursor` is absent on the last page.

### Get an artifact: `GET /artifacts/{slug}`

`?files=true` adds each version's file list. Versions are newest first.

```json
{ "artifact": { ... }, "versions": [ { "number": 3, "url": "https://q3-report--v3.art.example.com/", ... } ] }
```

An unpublished or soft-deleted artifact is returned (with `unpublished_at` or `deleted_at` set) only to admins, its owner and the users who created or updated it, the people who also see it in their listings; anyone else gets `404 not_found`. A purged one is `410 gone`.

### Update an artifact: `PUT /artifacts/{slug}`

Changes files and fields in one call. Sending files creates a new version built from the **latest** version's files plus the changes: paths you send are added or replaced, `delete: true` entries are removed, and everything else is carried over. Sending no files changes fields only and creates no version.

JSON body:

```json
{
  "name": "Q3 report (final)",
  "description": "...",
  "metadata": { "team": "finance", "draft": null },
  "visibility": "unlisted",
  "label": "final",
  "expected_version": 3,
  "keep_stale": false,
  "restore": false,
  "files": [
    { "path": "index.html", "content": "<!doctype html>..." },
    { "path": "old.css", "delete": true }
  ],
  "upload_id": "optional"
}
```

| Field              | Notes                                                                                       |
| ------------------ | ------------------------------------------------------------------------------------------- |
| `metadata`         | Merged into the existing metadata; `null` deletes a key                                     |
| `visibility`       | `public`, `unlisted` or `unpublished`; applied after everything else                        |
| `expected_version` | Fail with `412 version_conflict` unless this is the latest version                          |
| `keep_stale`       | A new version normally clears `stale_at` and `expires_at`; `true` keeps them                |
| `restore`          | Clear `deleted_at` on a soft-deleted artifact (without it, a soft-deleted artifact is `404`) |

Multipart works as for create: a `manifest` part with the fields above (including `files` with `delete` entries), then an optional `bundle` part whose files are added or replaced. Inline `files` win a path clash with the bundle; the bundle does not remove files it lacks.

**Optimistic concurrency.** Send `If-Match: "3"` (or `3`) instead of `expected_version`; a body value takes precedence, and `If-Match: *` means no check.

```sh
curl -sS -X PUT https://api.art.example.com/v1/artifacts/q3-report \
  -H "Authorization: Bearer $KAZARI_TOKEN" -H 'If-Match: "3"' \
  -F 'manifest={"label":"fix chart"};type=application/json' \
  -F 'bundle=@site.tar.gz;type=application/gzip'
```

### Change fields: `PATCH /artifacts/{slug}`

The JSON body of `PUT` without `files` or `upload_id` (sending them is `422`). Capped at 1 MiB. Use it for name, description, metadata, visibility (including `unpublished`) and `restore`:

```sh
curl -sS -X PATCH https://api.art.example.com/v1/artifacts/q3-report \
  -H "Authorization: Bearer $KAZARI_TOKEN" -H 'Content-Type: application/json' \
  -d '{"visibility":"unpublished"}'
```

`"public"` or `"unlisted"` brings an unpublished artifact back.

### Delete: `DELETE /artifacts/{slug}`

Soft delete: the page stops serving (`404`), the slug stays reserved and the artifact can be restored with `{"restore": true}` until `lifecycle.retention` elapses. `?hard=true` purges now: versions and files are deleted, the page answers `410` until the tombstone period ends, and blobs are removed by the next `gc` run. Both forms need `artifacts:delete`. `204 No Content`.

### Roll back or forward: `POST /artifacts/{slug}/publish`

```json
{ "version": 2 }
```

Serves an existing version. `404 version_not_found` if it does not exist.

### Stale: `POST` and `DELETE /artifacts/{slug}/stale`

`POST` marks the artifact stale; the page keeps serving until the grace period ends, then the sweep soft-deletes it. The body is optional:

```json
{ "grace": "72h" }
```

`grace` is a Go duration (hours, not days: `336h` for 14 days); without it `lifecycle.default_grace` applies. Response:

```json
{ "stale_at": "2026-10-24T09:00:00Z", "expires_at": "2026-10-27T09:00:00Z", "artifact": { ... } }
```

`DELETE` clears the mark and returns a publish result.

### Read a file: `GET /artifacts/{slug}/files/{path...}`

Streams one file of the published version, or of `?version=N`. Headers: the stored `Content-Type`, `ETag: "<sha256>"`, `Cache-Control: private, no-cache`, `Content-Disposition: inline` and `X-Content-Type-Options: nosniff`; `Range` and `If-None-Match` work. Files of an unpublished or soft-deleted artifact are readable only by admins, its owner and the users who created or updated it (so an owner can recover content before the purge); anyone else gets `404 not_found`.

```sh
curl -sS -H "Authorization: Bearer $KAZARI_TOKEN" \
  'https://api.art.example.com/v1/artifacts/q3-report/files/index.html?version=1'
```

### Chunked uploads

For bundles that are too big for one request, or clients that cannot build a tar. Files are staged on the server and then published by passing `upload_id` to create or update.

1. **Begin**: `POST /uploads` (no body).

   ```json
   { "upload_id": "Zm9vYmFyYmF6cXV4cXV1eA", "expires_at": "2026-10-24T10:00:00Z",
     "max_chunk_bytes": 4194304, "max_total_bytes": 52428800 }
   ```

2. **Send chunks**: `PATCH /uploads/{id}/files/{path}?offset=N` with the raw bytes as the body. The first chunk of a path names the file's content type in `Content-Type` (`application/octet-stream` means "work it out from the extension"). Each response is `{ "path": "data/big.json", "bytes_received": 8388608 }`.
3. **Publish**: `POST /artifacts` or `PUT /artifacts/{slug}` with `"upload_id"`, optionally with inline `files` (small files and `delete` entries) in the same body.

Offset rules make retries safe:

- `offset` equal to the bytes staged for that path appends the chunk;
- a chunk that ends exactly at the staged size is a retry of one already written: `200`, nothing written;
- anything else is `409 upload_offset_mismatch` with `current`; resume from there.

Limits: `limits.max_chunk_bytes` per chunk (`413` beyond), `limits.max_version_bytes` per upload, `limits.max_files` paths, `limits.max_open_uploads` open uploads per user (`403 too_many_uploads`), and `limits.upload_ttl` until an untouched upload expires (`404 upload_not_found` afterwards). The staging area is deleted after the publish call **whether it succeeds or fails**, so after a failed publish start a new upload. `DELETE /uploads/{id}` discards an upload early.

```sh
API=https://api.art.example.com/v1
AUTH="Authorization: Bearer $KAZARI_TOKEN"
ID=$(curl -sS -X POST -H "$AUTH" $API/uploads | jq -r .upload_id)

split -b 4M data.json part.
offset=0
for p in part.*; do
  curl -sS -X PATCH -H "$AUTH" -H 'Content-Type: application/json' \
    --data-binary @"$p" "$API/uploads/$ID/files/data.json?offset=$offset"
  offset=$((offset + $(stat -c %s "$p")))
done

curl -sS -H "$AUTH" -H 'Content-Type: application/json' $API/artifacts -d "{
  \"slug\": \"big-data\", \"upload_id\": \"$ID\",
  \"files\": [{ \"path\": \"index.html\", \"content\": \"<script src=data.json></script>\" }]
}"
```

Always set `Content-Type` on chunks: `curl --data-binary` otherwise sends `application/x-www-form-urlencoded`, which would become the file's type.

### Who am I: `GET /me` and `GET /me/keys`

```json
{ "handle": "peter", "admin": false, "scopes": ["artifacts:read", "artifacts:write", "artifacts:delete"],
  "source": "api_key", "quota_bytes": 2147483648, "used_bytes": 184320 }
```

`source` is `api_key` or `oauth`; `quota_bytes` of 0 means unlimited. `/me/keys` lists the caller's API keys (`prefix`, `label`, `scopes`, `last_used_at`, `expires_at`, `revoked_at`, `created_at`; never the secret). OAuth callers get `[]`.

## Admin API

Base URL `https://admin.<base>/v1`. Every `/v1` route needs the admin token or an admin credential. `/health`, `/v1/health` and `/metrics` need no credential.

| Method   | Path                                   | Body                                   | Success                    |
| -------- | -------------------------------------- | -------------------------------------- | -------------------------- |
| `GET`    | `/users`                               |                                        | `200` users                |
| `POST`   | `/users`                               | `{handle, email?, display_name?, role?, quota_bytes?}` | `201` user |
| `GET`    | `/users/{handle}`                      |                                        | `200` user with `identities` and `used_bytes` |
| `PATCH`  | `/users/{handle}`                      | `{email?, display_name?, role?, quota_bytes?, disabled?}` | `200` user |
| `GET`    | `/users/{handle}/keys`                 |                                        | `200` keys                 |
| `POST`   | `/users/{handle}/keys`                 | `{label?, scopes?, expires_in?}`       | `201` issued key           |
| `DELETE` | `/users/{handle}/keys/{prefix}`        |                                        | `204`                      |
| `POST`   | `/users/{handle}/identities`           | `{issuer, subject, email?}`            | `204`                      |
| `POST`   | `/users/{handle}/identities/unlink`    | `{issuer, subject}`                    | `204`                      |
| `GET`    | `/users/{handle}/grants`               |                                        | `200` OAuth grants         |
| `DELETE` | `/grants/{id}`                         |                                        | `204`                      |
| `GET`    | `/artifacts`                           | list parameters plus `owner`           | `200` list page            |
| `POST`   | `/artifacts/{slug}/transfer`           | `{owner}`                              | `200` artifact             |
| `POST`   | `/artifacts/{slug}/hide`               |                                        | `200` artifact             |
| `GET`    | `/settings`                            |                                        | `200` effective config     |
| `GET`    | `/jobs`                                |                                        | `200` job statuses         |
| `POST`   | `/jobs/{name}/run`                     |                                        | `200` job status           |
| `GET`    | `/audit`                               | `since`, `slug`, `actor`, `limit`      | `200` events               |
| `GET`    | `/health` (also `/v1/health`)          |                                        | `200` `{"status":"ok","version":"..."}` |
| `GET`    | `/metrics`                             |                                        | Prometheus text            |

Details:

- **Users.** `handle` is 2 to 32 characters of `a-z`, `0-9`, `_` and `-`, starting with a letter or digit. `role` is `member` (default) or `admin`. `quota_bytes` omitted means `limits.default_user_quota`; `0` means unlimited. Emails are stored lower-case and must be unique. `"disabled": true` stamps `disabled_at` and revokes every API key; `false` re-enables the user (revoked keys stay revoked).
- **Keys.** `scopes` defaults to `artifacts:read`, `artifacts:write`, `artifacts:delete`; only admin users may hold `admin`, and an admin user's key only has admin powers if it holds `admin`. `expires_in` is a Go duration (`720h`); omitted means no expiry. The response carries the secret once: `{ "key": "kzk_...", "api_key": { "prefix": "abcdefgh", ... } }`.
- **Identities.** Link an upstream `(issuer, subject)` to a user before they sign in, or move one from another user; unlink removes it. See [identity.md](identity.md#admin-created-users).
- **Grants.** One entry per live refresh token: `{ "id", "client_id", "client", "user", "scopes", "created_at", "expires_at" }`. Deleting a grant revokes it and every token derived from it.
- **Artifacts.** Lists every artifact regardless of visibility, with the user API's filters and `owner=<handle>`. **Transfer** gives an artifact to another user (`422` for an unknown handle). **Hide** unpublishes it in one call (the abuse response, see [ops.md](ops.md#abuse-response)).
- **Jobs.** `POST /jobs/{name}/run` runs the job now and waits for it. `404` for an unknown name, `409 job_running` if it is running, `500` if the run fails (the error is in `last_error`). A status is `{ "name", "every", "last_run", "last_error", "next_run", "running" }`.
- **Audit.** `since` is an RFC 3339 time; `limit` defaults to 100, at most 1000. Newest first: `{ "at", "actor", "action", "slug", "details" }`.
- **Settings.** The configuration keyed like the YAML file, sizes and durations as strings, secrets as `"***"`.

```sh
ADMIN=https://admin.art.example.com/v1
curl -sS -H "Authorization: Bearer $KAZARI_ADMIN_TOKEN" -H 'Content-Type: application/json' \
  $ADMIN/users -d '{"handle":"ana","email":"ana@example.com","quota_bytes":1073741824}'
curl -sS -H "Authorization: Bearer $KAZARI_ADMIN_TOKEN" -H 'Content-Type: application/json' \
  $ADMIN/users/ana/keys -d '{"label":"ci","scopes":["artifacts:read","artifacts:write"],"expires_in":"2160h"}'
curl -sS -X POST -H "Authorization: Bearer $KAZARI_ADMIN_TOKEN" $ADMIN/jobs/sweep/run
curl -sS -H "Authorization: Bearer $KAZARI_ADMIN_TOKEN" "$ADMIN/audit?slug=q3-report&limit=20"
```
