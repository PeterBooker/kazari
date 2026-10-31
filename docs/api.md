# REST API

Kazari has two REST APIs, each on its own host:

| API                     | Base URL                  | What it is for                                |
| ----------------------- | ------------------------- | --------------------------------------------- |
| [User API](#user-api)   | `https://api.<base>/v1`   | Publishing and managing artifacts             |
| [Admin API](#admin-api) | `https://admin.<base>/v1` | Users, keys, jobs, settings and the audit log |

`<base>` is the instance's base domain, such as `art.example.com`. Both APIs speak JSON (`Content-Type: application/json`) unless a route says otherwise. The MCP server and the CLI use the same wire types, so an artifact looks the same everywhere.

This page covers authentication, the artifact record and its rules, every route of both APIs, rate limits and error codes.

## Authentication

Every user API route needs a bearer credential in the `Authorization` header:

```http
Authorization: Bearer kzk_abcdefgh_<52 characters>
```

The credential is one of these:

- An API key, issued by an admin, in the form `kzk_<8-character prefix>_<secret>`.
- An OAuth access token issued to an MCP client. [Sign-in and identity](identity.md) explains how clients get one.

### Scopes

Every credential carries scopes, which decide the routes it can call:

| Scope              | Allows                                                                  |
| ------------------ | ----------------------------------------------------------------------- |
| `artifacts:read`   | `GET` routes                                                            |
| `artifacts:write`  | Create, update, publish, stale, visibility, share links, uploads        |
| `artifacts:delete` | `DELETE /artifacts/{slug}`, soft and hard                               |
| `admin`            | The admin API. Held only by admin users, and implies every other scope  |

A missing or invalid credential gets `401` with `WWW-Authenticate: Bearer error="invalid_token"`. A valid credential without the scope gets `403 forbidden`.

### Admin credentials

The admin API accepts the instance admin token (`auth.admin_token`), or any credential of an admin user that carries the `admin` scope. A valid credential of anyone else gets `403`.

## The artifact record

Every response that returns an artifact uses this shape:

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

Timestamps are RFC 3339 in UTC. The lifecycle timestamps (`stale_at`, `expires_at`, `unpublished_at`, `deleted_at` and `purged_at`) are left out when unset. `size_bytes` is the sum of every version's size.

`visibility` decides who can open the page and who sees it listed:

| Visibility | Who can open the page                                              | Listings                            |
| ---------- | ------------------------------------------------------------------ | ----------------------------------- |
| `public`   | Anyone                                                             | Listed                              |
| `unlisted` | Anyone with the URL                                                | Left out of other people's listings |
| `members`  | People signed in to the instance, and holders of a [share link](#share-links) | Listed to members        |

Taking a page offline sets `unpublished_at` and keeps `visibility` for when the page comes back.

### Versions

A version has the fields `number`, `label`, `created_by`, `created_at`, `size_bytes`, `url` and `files`:

- `url` is the version's pinned address, `https://<slug>--v<N>.<base>/`.
- `files` is included only on request. It lists `{ "path", "hash", "size", "content_type" }` for each file, where `hash` is the hex SHA-256.

### The publish result

Create, update, publish, unstale and visibility changes return a publish result:

```json
{ "url": "https://q3-report.art.example.com/", "version": 3, "artifact": { ... } }
```

`version` is the version the call created or moved to. For a change to fields only, it is the published version. A create with `source_url` adds an `import` field ([Importing a web page](#importing-a-web-page)).

### Rules

Every artifact follows these rules, whichever way it was published.

#### Slugs

A slug matches `^[a-z0-9]([a-z0-9-]*[a-z0-9])?$` and is 3 to 63 characters long. It cannot contain `--`, which is reserved for version hosts, or be one of `serve.reserved_slugs`. A slug is fixed when the artifact is created.

#### File paths

A file path is relative and slash-separated, and Kazari cleans it with `path.Clean`. A path cannot have:

- a leading or trailing `/`;
- a `..` that escapes the bundle;
- control characters or backslashes;
- a segment longer than 255 bytes, or more than 1024 bytes in all;
- a place under the top-level `.kazari/` directory, which every artifact host keeps for sign-in and share links.

A path can appear only once per request.

#### Content types

A file's content type is the one given with the file, else the one its extension implies, else one sniffed from its first 512 bytes. It never comes from the request that later reads the file.

A given type must be a parseable media type of at most 255 printable ASCII bytes. Anything else is `422 invalid_argument`.

#### Metadata

Metadata keys match `[a-zA-Z0-9_.-]{1,64}`, and values can be up to 4096 bytes. An artifact holds at most `limits.metadata_max_pairs` pairs.

The server interprets keys under `kazari.` itself, and accepts only these:

| Key                | Values                    | Effect                                                               |
| ------------------ | ------------------------- | -------------------------------------------------------------------- |
| `kazari.spa`       | `true`, `false`           | `true` serves `index.html` for any missing path without an extension |
| `kazari.indexable` | `true`, `false`           | `true` drops `X-Robots-Tag: noindex`                                 |
| `kazari.csp`       | Printable ASCII, one line | Replaces the instance CSP, when `serve.allow_csp_override` is on     |

#### Write policy

By default (`auth.write_policy: shared`), anyone with `artifacts:write` can change an artifact. Under `owner`, only the artifact's owner and admins can. An unpublished or soft-deleted artifact is the exception under both: only the people who can [read it](#get-an-artifact-get-artifactsslug) can change, republish or restore it.

## User API

The user API's base URL is `https://api.<base>/v1`. These are its routes, with the scope each needs and its response on success:

| Method   | Path                                     | Scope  | Success                     |
| -------- | ---------------------------------------- | ------ | --------------------------- |
| `POST`   | `/artifacts`                             | write  | `201` publish result        |
| `GET`    | `/artifacts`                             | read   | `200` list page             |
| `GET`    | `/artifacts/{slug}`                      | read   | `200` artifact and versions |
| `PUT`    | `/artifacts/{slug}`                      | write  | `200` publish result        |
| `PATCH`  | `/artifacts/{slug}`                      | write  | `200` publish result        |
| `DELETE` | `/artifacts/{slug}`                      | delete | `204`                       |
| `POST`   | `/artifacts/{slug}/edit`                 | write  | `200` edit result           |
| `POST`   | `/artifacts/{slug}/publish`              | write  | `200` publish result        |
| `POST`   | `/artifacts/{slug}/stale`                | write  | `200` stale result          |
| `DELETE` | `/artifacts/{slug}/stale`                | write  | `200` publish result        |
| `GET`    | `/artifacts/{slug}/files/{path...}`      | read   | `200` file bytes            |
| `POST`   | `/artifacts/{slug}/share-links`          | write  | `201` new share link        |
| `GET`    | `/artifacts/{slug}/share-links`          | read   | `200` share links           |
| `DELETE` | `/artifacts/{slug}/share-links/{prefix}` | write  | `204`                       |
| `POST`   | `/uploads`                               | write  | `201` upload                |
| `PATCH`  | `/uploads/{id}/files/{path...}`          | write  | `200` chunk result          |
| `DELETE` | `/uploads/{id}`                          | write  | `204`                       |
| `GET`    | `/skill`                                 | read   | `200` the instance's skill  |
| `GET`    | `/me`                                    | any    | `200` caller                |
| `GET`    | `/me/keys`                               | any    | `200` caller's keys         |

In the scope column, `read`, `write` and `delete` stand for `artifacts:read`, `artifacts:write` and `artifacts:delete`, and `any` means any valid credential.

### Create an artifact: `POST /artifacts`

Creates an artifact and publishes its first version at once. The response is `201 Created` with a [publish result](#the-publish-result). The body is either JSON with the files inline, or a multipart request carrying a tar.gz bundle.

#### JSON body with inline files

A JSON body lists the files in `files`. `content` is UTF-8 text by default; set `"encoding": "base64"` for binary files. For example, this creates `hello` from an HTML page and a PNG image:

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

The body takes these fields:

| Field         | Type   | Notes                                                                       |
| ------------- | ------ | --------------------------------------------------------------------------- |
| `slug`        | string | Required                                                                    |
| `name`        | string | At most 256 bytes, one line                                                 |
| `description` | string | At most 4096 bytes; can contain line breaks and tabs                        |
| `visibility`  | string | `public`, `unlisted` or `members`; defaults to `serve.default_visibility` (`public`) |
| `metadata`    | object | String to string                                                            |
| `label`       | string | Label of version 1; at most 256 bytes, one line                             |
| `files`       | array  | `{ path, content, encoding?, content_type? }`, or `{ path, url, content_type? }` ([Files named by URL](#files-named-by-url)) |
| `upload_id`   | string | Staged files from a [chunked upload](#chunked-uploads), merged with `files`, which win a path clash |
| `source_url`  | string | A web page to copy as the artifact ([Importing a web page](#importing-a-web-page)) |
| `keep_scripts` | bool  | With `source_url`: keep the page's scripts; by default they are removed |

At least one file is required.

Text fields must be valid UTF-8, without control characters (C0, DEL, C1) and without the bidirectional override and isolate characters (U+202A to U+202E, U+2066 to U+2069). `description` can contain newlines and tabs. The same rules apply to updates and over MCP, and a violation is `422 invalid_argument`.

The whole JSON body is capped at `limits.max_version_bytes` plus 1 MiB. Base64 content is a third larger than the bytes it carries, so it reaches the cap sooner: send anything large as a bundle or a chunked upload.

#### Files named by URL

A file can give `url` instead of `content`: an `http` or `https` address on a public host, without credentials, that Kazari downloads as the file's content. One download is capped at `limits.max_file_bytes`, all of a request's together at `limits.max_version_bytes`, and they share the request's `fetch.budget`. `content_type` comes from the response when the file does not give one. To publish a page with an image that is already online:

```json
{
  "slug": "hello",
  "files": [
    { "path": "index.html", "content": "<!doctype html><img src=\"team.jpg\">" },
    { "path": "team.jpg", "url": "https://www.example.com/images/team.jpg" }
  ]
}
```

Every URL is downloaded before anything is published. A download that fails is `502 fetch_failed`, and nothing is created; a file with both `url` and `content` is `422 invalid_argument`. Updates take `url` the same way. An instance with `fetch.enabled: false` answers `403 forbidden`.

#### Importing a web page

With `source_url`, Kazari copies that page into the new artifact: the HTML with its stylesheets, fonts, images, icons and media fetched and stored under `assets/`, links made absolute to the source, scripts removed unless `keep_scripts` is `true`. `files` are added to the copy and win on a path clash. The other fields are the same, and `name` and `description` default to the page's title and meta description. To copy a page:

```sh
curl -sS https://api.art.example.com/v1/artifacts \
  -H "Authorization: Bearer $KAZARI_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{ "slug": "fundraiser", "source_url": "https://www.example.com/" }'
```

The publish result carries an `import` field with the source, how many resources were fetched, which stayed remote and why (`skipped`), how many scripts were removed, `notes` and an `outline` of the page's sections. [The MCP server page](mcp.md#import_page) describes each field and what the copy contains. `source_url` cannot be combined with `upload_id`.

#### Multipart body with a tar.gz bundle

A multipart request carries the files as a gzip-compressed tar. The first part must be `manifest`, JSON with the fields `slug`, `name`, `description`, `visibility`, `metadata` and `label`. The second part is `bundle`, the archive:

```sh
tar -czf site.tar.gz -C site .
curl -sS https://api.art.example.com/v1/artifacts \
  -H "Authorization: Bearer $KAZARI_TOKEN" \
  -F 'manifest={"slug":"q3-report","name":"Q3 report","label":"v1"};type=application/json' \
  -F 'bundle=@site.tar.gz;type=application/gzip'
```

Kazari keeps only the regular files in the tar. It skips directories, links and devices, and strips a leading `./` from each name.

A multipart request has these limits:

- The manifest is capped at 64 KiB.
- The request body is capped at `limits.max_version_bytes` plus 1 MiB.
- The decompressed tar stream has the same cap, counting every byte of it: headers, long-name records, padding, skipped entries and file content.
- A file name longer than 1024 bytes is refused as soon as it is read.

### List artifacts: `GET /artifacts`

Returns one page of the artifacts you can see, newest-updated first. For example, this lists up to 20 artifacts you created or updated that have the metadata `team=finance`, leaving out stale ones:

```sh
curl -sS -H "Authorization: Bearer $KAZARI_TOKEN" \
  'https://api.art.example.com/v1/artifacts?mine=any&stale=exclude&metadata.team=finance&limit=20'
```

The query takes these parameters:

| Parameter        | Values                               | Default    |
| ---------------- | ------------------------------------ | ---------- |
| `mine`           | `created`, `updated`, `any`          | No filter  |
| `metadata.<key>` | Exact value; repeat for several keys | No filter  |
| `stale`          | `include`, `exclude`, `only`         | `include`  |
| `deleted`        | `exclude`, `include`, `only`         | `exclude`  |
| `cursor`         | `next_cursor` of the previous page   | First page |
| `limit`          | 1 to 200                             | 50         |

You see public and `members` artifacts that are published and live, plus every artifact you own, created or updated, whatever its visibility. Purged artifacts never appear.

The response holds the page and a cursor for the next one:

```json
{ "items": [ { ... } ], "next_cursor": "eyJ..." }
```

`next_cursor` is absent on the last page.

### Get an artifact: `GET /artifacts/{slug}`

Returns the artifact record and its versions, newest first. Add `?files=true` to include each version's file list:

```json
{ "artifact": { ... }, "versions": [ { "number": 3, "url": "https://q3-report--v3.art.example.com/", ... } ] }
```

An unpublished or soft-deleted artifact is returned, with `unpublished_at` or `deleted_at` set, only to admins, its owner and the users who created or updated it: the people who also see it in their listings, and the only ones who can change, republish or restore it. Anyone else gets `404 not_found`. A purged artifact is `410 gone`.

### Update an artifact: `PUT /artifacts/{slug}`

Changes files and fields in one call, and returns a publish result. Sending files creates a new version built from the **latest** version's files plus the changes:

- Paths you send are added or replaced.
- Entries with `delete: true` are removed.
- Everything else is carried over.

Sending no files changes fields only and creates no version.

#### JSON body

A JSON body can carry any of these fields:

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

These fields work differently from create, or exist only here:

| Field              | Notes                                                                           |
| ------------------ | ------------------------------------------------------------------------------- |
| `metadata`         | Merged into the existing metadata; `null` deletes a key                         |
| `visibility`       | `public`, `unlisted`, `members` or `unpublished`; applied after everything else |
| `expected_version` | Fails with `412 version_conflict` unless this is the latest version             |
| `keep_stale`       | `true` keeps `stale_at` and `expires_at`, which a new version otherwise clears  |
| `restore`          | Clears `deleted_at` on a soft-deleted artifact, which is `404` without it       |

#### Multipart body

A multipart update works as create does. The `manifest` part holds the fields above, including `files` with `delete` entries. An optional `bundle` part follows, whose files are added or replaced.

Inline `files` win a path clash with the bundle. The bundle does not remove files it lacks.

#### Optimistic concurrency

To publish only if nobody has published since you read the artifact, send `If-Match: "3"` (or `3`) instead of `expected_version`. A value in the body takes precedence, and `If-Match: *` means no check. For example, this adds a version from a bundle only while version 3 is the latest:

```sh
curl -sS -X PUT https://api.art.example.com/v1/artifacts/q3-report \
  -H "Authorization: Bearer $KAZARI_TOKEN" -H 'If-Match: "3"' \
  -F 'manifest={"label":"fix chart"};type=application/json' \
  -F 'bundle=@site.tar.gz;type=application/gzip'
```

### Change fields: `PATCH /artifacts/{slug}`

Changes the name, description, metadata, visibility (including `unpublished`) or `restore`, without adding a version. The body is the JSON body of `PUT` without `files` or `upload_id`, and sending either is `422`. The body is capped at 1 MiB.

To take a page offline, set its visibility to `unpublished`:

```sh
curl -sS -X PATCH https://api.art.example.com/v1/artifacts/q3-report \
  -H "Authorization: Bearer $KAZARI_TOKEN" -H 'Content-Type: application/json' \
  -d '{"visibility":"unpublished"}'
```

Setting `"public"`, `"unlisted"` or `"members"` brings an unpublished artifact back.

### Delete: `DELETE /artifacts/{slug}`

Soft-deletes the artifact. The page stops serving and answers `404`, and the slug stays reserved. Until `lifecycle.retention` elapses, the artifact can be restored with `{"restore": true}`.

Add `?hard=true` to purge it now. The versions and files are deleted, the page answers `410` until the tombstone period ends, and the next `gc` run removes the blobs.

> [!WARNING]
> A purge cannot be undone. Use a soft delete unless the content has to go now.

Both forms need `artifacts:delete`, and both answer `204 No Content`.

### Edit a file: `POST /artifacts/{slug}/edit`

Changes one text file of the latest version and publishes the result as a new version. Every other file carries over. The body names the file and up to 100 edits, all of one kind:

```json
{
  "path": "index.html",
  "expected_version": 4,
  "label": "fix the title",
  "edits": [
    { "old_text": "<h1>Q3 report</h1>", "new_text": "<h1>Q4 report</h1>" },
    { "old_text": "2025", "new_text": "2026", "replace_all": true }
  ]
}
```

In a file with CRLF line breaks, the new text gets CRLF line breaks too.

#### Text edits

A text edit replaces `old_text` with `new_text`. `old_text` must match once, unless `replace_all` is set. Edits apply in order.

When `old_text` does not match exactly, Kazari compares whole lines, first ignoring trailing whitespace, then ignoring indentation. A match that ignores indentation re-indents `new_text` to the file's.

This whitespace-tolerant fallback applies to an `old_text` of at most 2000 lines. `replace_all` changes at most 10000 places.

#### Anchor edits

An anchor edit replaces a range of lines. This one replaces lines 12 to 14, inclusive:

```json
{ "start": "12:3fa0", "end": "14:9b1c", "new_text": "..." }
```

`end` defaults to `start`, and an empty `new_text` deletes the lines. Anchors come from reading the file with `?anchors=true` (see [Read a file](#read-a-file-get-artifactsslugfilespath)).

Every anchor refers to the file as read, and the ranges must not overlap. If a line's content has changed since it was read, the request fails with `409 anchor_mismatch`.

#### Conflicts and limits

These rules apply to both kinds of edit:

- `expected_version` (or `If-Match`) fails the request with `412 version_conflict` if someone published since. Without it, edits apply to the newest content.
- A binary file is `422 invalid_argument`.
- A result larger than `limits.max_file_bytes` is `413 too_large`.

#### The edit result

The response is an edit result:

```json
{
  "url": "https://q3-report.art.example.com/", "version": 5, "path": "index.html",
  "edits": [
    { "match": "exact", "replacements": 1, "start_line": 12, "end_line": 12,
      "excerpt": "10:5d2e|  <body>\n11:0b7f|    <header>\n12:a41c|      <h1>Q4 report</h1>\n13:77e0|    </header>\n14:2c9d|    <main>" },
    { "match": "exact", "replacements": 3, "start_line": 20, "end_line": 20, "excerpt": "..." }
  ],
  "artifact": { ... }
}
```

Each entry in `edits` describes one edit:

- `match` is `exact`, `trailing_whitespace`, `indentation` or `anchor`.
- `start_line` and `end_line` are where the new text of the edit's first replacement sits in the new version. `end_line` is one less than `start_line` when the edit left no text.
- `excerpt` shows those lines, with two more on each side, anchored, so a follow-up anchor edit needs no new read. It holds at most 24 lines of new text, and lines longer than 160 bytes are clipped with a note.

### Roll back or forward: `POST /artifacts/{slug}/publish`

Serves an existing version, older or newer than the one served now. The body names the version:

```json
{ "version": 2 }
```

The response is a publish result. A version that does not exist is `404 version_not_found`.

### Stale: `POST` and `DELETE /artifacts/{slug}/stale`

`POST` marks the artifact stale. The page keeps serving until the grace period ends, then the sweep soft-deletes it. The body is optional; to set the grace period, send `grace`:

```json
{ "grace": "72h" }
```

`grace` is a Go duration, so it counts in hours, not days: `336h` is 14 days. Without it, `lifecycle.default_grace` applies. The response is a stale result:

```json
{ "stale_at": "2026-10-24T09:00:00Z", "expires_at": "2026-10-27T09:00:00Z", "artifact": { ... } }
```

`DELETE` clears the mark and returns a publish result.

### Read a file: `GET /artifacts/{slug}/files/{path...}`

Streams one file of the published version. Add `?version=N` to read another version:

```sh
curl -sS -H "Authorization: Bearer $KAZARI_TOKEN" \
  'https://api.art.example.com/v1/artifacts/q3-report/files/index.html?version=1'
```

The response carries these headers:

- the stored `Content-Type`;
- `ETag: "<sha256>"`;
- `Cache-Control: private, no-cache`;
- `Content-Disposition: inline`;
- `X-Content-Type-Options: nosniff`.

`Range` and `If-None-Match` work.

Add `?anchors=true` to get a text file as `text/plain` with every line prefixed by its anchor, the form that [anchor edits](#anchor-edits) refer to:

```text
12:3fa0|<h1>Title</h1>
```

A binary file with `?anchors=true` is `422 invalid_argument`.

Files of an unpublished or soft-deleted artifact are readable only by admins, its owner and the users who created or updated it, so an owner can recover content before the purge. Anyone else gets `404 not_found`.

### Share links

A share link opens one `members` artifact in a browser without signing in. It is for people who cannot sign in to the instance: a client, an outside reviewer, a headless browser. It works until it expires or is revoked.

The link's URL opens the latest host. The cookie it sets is per host, so to open a pinned version, open `https://<slug>--v<N>.<base>/.kazari/share?token=...` with the same token.

Managing links needs the same rights as changing the artifact, so the write policy applies. An offline artifact's links can be managed only by the people who can see the artifact.

A link on a `public` or `unlisted` artifact works but changes nothing, since those pages open for anyone anyway.

#### Create a share link: `POST /artifacts/{slug}/share-links`

The body is optional. To name the link and set how long it lasts, send `label` and `expires_in`:

```json
{ "label": "Acme review", "expires_in": "14d" }
```

`expires_in` is a duration such as `72h` or `14d`. It defaults to 7 days, and can be at most `serve.share_link_max_ttl`, which is 90 days by default. `label` follows the version label rules.

The response is the only time the link's URL, which holds its token, is returned:

```json
{
  "url": "https://q3-report.art.example.com/.kazari/share?token=kzs_ab2cd3ef_...",
  "share_link": {
    "prefix": "ab2cd3ef", "label": "Acme review", "created_by": "peter",
    "created_at": "2026-10-25T09:00:00Z", "expires_at": "2026-11-08T09:00:00Z", "uses": 0, "active": true
  },
  "artifact": { ... }
}
```

The token is `kzs_<8-character prefix>_<52-character secret>`, and only its SHA-256 is stored. Opening the URL stores the token in a cookie on that host and redirects to `/`, so the token leaves the address bar.

#### List share links: `GET /artifacts/{slug}/share-links`

Lists the artifact's links, newest first, without their tokens, as `{ "items": [ ... ] }`. Each item has `prefix`, `label`, `created_by`, `created_at`, `expires_at`, `revoked_at`, `last_used_at`, `uses` and `active`.

`uses` counts openings of the URL, not page views.

#### Revoke a share link: `DELETE /artifacts/{slug}/share-links/{prefix}`

Revokes a link at once: browsers that opened it lose access on their next request. Revoking twice answers `204` again. An unknown prefix is `404 share_link_not_found`.

### Chunked uploads

Chunked uploads are for bundles that are too big for one request, and for clients that cannot build a tar. Files are staged on the server, then published by passing the `upload_id` to create or update. An upload takes three steps:

1. Begin an upload with `POST /uploads`.
2. Send each file in chunks with `PATCH /uploads/{id}/files/{path...}`.
3. Publish with `POST /artifacts` or `PUT /artifacts/{slug}`, passing `upload_id`.

#### Begin an upload: `POST /uploads`

The request has no body. The response gives the upload's id, when it expires and its size limits:

```json
{ "upload_id": "Zm9vYmFyYmF6cXV4cXV1eA", "expires_at": "2026-10-24T10:00:00Z",
  "max_chunk_bytes": 4194304, "max_total_bytes": 52428800 }
```

#### Send a chunk: `PATCH /uploads/{id}/files/{path...}`

Send the raw bytes as the body, and the chunk's position in the file as `?offset=N`. The first chunk of a path names the file's content type in `Content-Type`; `application/octet-stream` means "work it out from the extension". Each response reports the bytes staged for that path:

```json
{ "path": "data/big.json", "bytes_received": 8388608 }
```

Always set `Content-Type` on chunks. Without it, `curl --data-binary` sends `application/x-www-form-urlencoded`, which would become the file's type.

#### Retrying a chunk

The offset rules make retries safe:

- An `offset` equal to the bytes staged for that path appends the chunk.
- A chunk that ends exactly at the staged size is a retry of one already written. It gets `200`, and nothing is written.
- Any other offset is `409 upload_offset_mismatch` with `current`, the staged size. Resume from there.

#### Publishing the upload

Pass `"upload_id"` to `POST /artifacts` or `PUT /artifacts/{slug}`. The same body can also carry inline `files`, for small files and `delete` entries.

The staging area is deleted after the publish call **whether it succeeds or fails**, so after a failed publish, start a new upload.

#### Upload limits

Each upload is held to these limits:

| Limit                      | Applies to                             | Over the limit                       |
| -------------------------- | -------------------------------------- | ------------------------------------ |
| `limits.max_chunk_bytes`   | Each chunk                             | `413 too_large`                      |
| `limits.max_version_bytes` | Each upload                            | `413 too_large`                      |
| `limits.max_files`         | Paths in an upload                     | `422 too_many_files`                 |
| `limits.max_open_uploads`  | Open uploads per user                  | `403 too_many_uploads`               |
| `limits.upload_ttl`        | Time until an untouched upload expires | `404 upload_not_found` afterwards    |

#### Discard an upload: `DELETE /uploads/{id}`

Discards an upload early and answers `204`.

#### Uploading a large file from the shell

This script uploads `data.json` in 4 MiB chunks, then publishes it with an inline `index.html` as the artifact `big-data`:

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

### The instance's skill: `GET /skill`

Returns the instance's [Agent Skill](https://agentskills.io/specification) as `text/markdown`. It is a `SKILL.md` generated from the configuration, with the MCP workflow, the authoring conventions with this instance's limits and CSP, and what to do after each error. The MCP server serves the same text as `skill://kazari/SKILL.md` and through the `get_guide` tool.

To install it, save it with [`kazari skill`](cli.md#skill) as `~/.claude/skills/kazari/SKILL.md`, or wherever your agent reads skills.

### Who am I: `GET /me` and `GET /me/keys`

`GET /me` returns the caller: their handle, whether they are an admin, their scopes, how they authenticated and their storage quota and use:

```json
{ "handle": "peter", "admin": false, "scopes": ["artifacts:read", "artifacts:write", "artifacts:delete"],
  "source": "api_key", "quota_bytes": 2147483648, "used_bytes": 184320 }
```

`source` is `api_key` or `oauth`. A `quota_bytes` of 0 means unlimited.

`GET /me/keys` lists the caller's API keys with their `prefix`, `label`, `scopes`, `last_used_at`, `expires_at`, `revoked_at` and `created_at`, never the secret. OAuth callers get `[]`.

## Admin API

The admin API's base URL is `https://admin.<base>/v1`. Every `/v1` route needs the admin token or an [admin credential](#admin-credentials). `/health` and `/v1/health` need no credential. On the public listener, `/metrics` needs the scrape token, `telemetry.metrics.token`, as a bearer token, and is absent while that is empty ([metrics](ops.md#metrics)). On the server's host the same API is served on the admin socket in the data directory, where no credential is needed; the CLI uses it ([the admin socket](cli.md#the-admin-socket)).

These are its routes, with the body or query parameters each takes and its response on success:

| Method   | Path                                | Body or query                                             | Success                                 |
| -------- | ----------------------------------- | --------------------------------------------------------- | --------------------------------------- |
| `GET`    | `/users`                            |                                                           | `200` users                             |
| `POST`   | `/users`                            | `{handle, email?, display_name?, role?, quota_bytes?}`    | `201` user                              |
| `GET`    | `/users/{handle}`                   |                                                           | `200` user with `identities` and `used_bytes` |
| `PATCH`  | `/users/{handle}`                   | `{email?, display_name?, role?, quota_bytes?, disabled?}` | `200` user                              |
| `GET`    | `/users/{handle}/keys`              |                                                           | `200` keys                              |
| `POST`   | `/users/{handle}/keys`              | `{label?, scopes?, expires_in?}`                          | `201` issued key                        |
| `DELETE` | `/users/{handle}/keys/{prefix}`     |                                                           | `204`                                   |
| `POST`   | `/users/{handle}/identities`        | `{issuer, subject, email?}`                               | `204`                                   |
| `POST`   | `/users/{handle}/identities/unlink` | `{issuer, subject}`                                       | `204`                                   |
| `GET`    | `/users/{handle}/grants`            |                                                           | `200` OAuth grants                      |
| `DELETE` | `/grants/{id}`                      |                                                           | `204`                                   |
| `GET`    | `/artifacts`                        | List parameters plus `owner`                              | `200` list page                         |
| `POST`   | `/artifacts/{slug}/transfer`        | `{owner}`                                                 | `200` artifact                          |
| `POST`   | `/artifacts/{slug}/hide`            |                                                           | `200` artifact                          |
| `GET`    | `/settings`                         |                                                           | `200` effective config                  |
| `GET`    | `/jobs`                             |                                                           | `200` job statuses                      |
| `POST`   | `/jobs/{name}/run`                  |                                                           | `200` job status                        |
| `GET`    | `/audit`                            | `since`, `slug`, `actor`, `limit`                         | `200` events                            |
| `GET`    | `/health` (also `/v1/health`)       |                                                           | `200` `{"status":"ok","version":"..."}` |
| `GET`    | `/metrics`                          | `Authorization: Bearer <telemetry.metrics.token>`         | Prometheus text or OpenMetrics          |

For example, these calls create a user with a 1 GiB quota, issue them a key for CI that can read and write and expires in 90 days, run the sweep job and read the last 20 audit events for `q3-report`:

```sh
ADMIN=https://admin.art.example.com/v1
curl -sS -H "Authorization: Bearer $KAZARI_ADMIN_TOKEN" -H 'Content-Type: application/json' \
  $ADMIN/users -d '{"handle":"ana","email":"ana@example.com","quota_bytes":1073741824}'
curl -sS -H "Authorization: Bearer $KAZARI_ADMIN_TOKEN" -H 'Content-Type: application/json' \
  $ADMIN/users/ana/keys -d '{"label":"ci","scopes":["artifacts:read","artifacts:write"],"expires_in":"2160h"}'
curl -sS -X POST -H "Authorization: Bearer $KAZARI_ADMIN_TOKEN" $ADMIN/jobs/sweep/run
curl -sS -H "Authorization: Bearer $KAZARI_ADMIN_TOKEN" "$ADMIN/audit?slug=q3-report&limit=20"
```

### Users

A user's fields follow these rules:

- `handle` is 2 to 32 characters of `a-z`, `0-9`, `_` and `-`, starting with a letter or digit. `admin` and `scheduler` are reserved: they name the admin token and the scheduler in the audit log.
- `role` is `member` (the default) or `admin`.
- `quota_bytes` defaults to `limits.default_user_quota` when omitted, and `0` means unlimited.
- `email` is stored lower-case and must be unique.

`"disabled": true` stamps `disabled_at` and revokes every API key of the user. `false` re-enables the user, but revoked keys stay revoked.

### Keys

A key's fields follow these rules:

- `scopes` defaults to `artifacts:read`, `artifacts:write` and `artifacts:delete`.
- Only admin users can hold `admin`, and an admin user's key has admin powers only if it holds `admin`.
- `expires_in` is a Go duration such as `720h`. Without it, the key never expires.

The response carries the secret, this once only:

```json
{ "key": "kzk_...", "api_key": { "prefix": "abcdefgh", ... } }
```

### Identities

`POST /users/{handle}/identities` links an upstream `(issuer, subject)` to a user before they sign in, or moves one from another user. `POST /users/{handle}/identities/unlink` removes it. [Admin-created users](identity.md#admin-created-users) explains when to use them.

### Grants

`GET /users/{handle}/grants` lists one entry per live refresh token, with `id`, `client_id`, `client`, `user`, `scopes`, `created_at` and `expires_at`. `DELETE /grants/{id}` revokes the grant and every token derived from it.

### Artifacts

`GET /artifacts` lists every artifact regardless of visibility. It takes the user API's [list parameters](#list-artifacts-get-artifacts), plus `owner=<handle>`.

`POST /artifacts/{slug}/transfer` gives an artifact to the user named in `owner`, whose quota its versions then count against. Only admins can transfer. An unknown handle is `422`.

`POST /artifacts/{slug}/hide` unpublishes an artifact in one call, as part of the [abuse response](ops.md#responding-to-abuse).

### Jobs

`POST /jobs/{name}/run` runs the job now and waits for it to finish. It answers `404` for an unknown name, `409 job_running` if the job is already running, and `500` if the run fails, with the error in `last_error`.

A job status has `name`, `every`, `last_run`, `last_error`, `next_run` and `running`.

### Audit log

`GET /audit` returns events newest first, each with `at`, `actor`, `action`, `slug` and `details`. `since` is an RFC 3339 time, and `limit` defaults to 100, at most 1000.

### Settings

`GET /settings` returns the effective configuration, keyed like the YAML file. Sizes and durations appear as strings, and secrets as `"***"`.

## Rate limits

Every control-plane host applies token buckets. When a bucket is empty, the host answers `429 rate_limited` with `Retry-After: <seconds>`.

### Per client IP

Before authentication, each client IP can make 120 requests a minute, with a burst of 120. The user API, the `mcp.` host and the admin API's `/v1/` routes each have their own bucket; on the `mcp.` host, the MCP endpoint and the OAuth endpoints share one.

IPv6 clients share one bucket per `/64`. `/health` and `/metrics` are not limited.

### Per credential

Write and delete routes, and the MCP write and delete tools, also draw on a budget per credential. It is `limits.write_rate_per_minute`, 60 a minute by default, with a burst of the same size. One budget covers a credential on both the user API and MCP.

An MCP tool over the budget fails with `rate_limited: too many writes; retry in <n>s`.

## Errors

Every error has the same envelope:

```json
{ "error": { "code": "slug_taken", "message": "slug \"demo\" is already taken" } }
```

`upload_offset_mismatch` also carries `current`, the staged size to resume from:

```json
{ "error": { "code": "upload_offset_mismatch", "message": "...", "current": 4194304 } }
```

These are the codes, with their HTTP status and meaning:

| Code                     | Status | Meaning                                                                            |
| ------------------------ | ------ | ---------------------------------------------------------------------------------- |
| `invalid_argument`       | 422    | Malformed or missing body, unknown JSON field, bad query parameter or value        |
| `invalid_argument`       | 415    | Body is neither `application/json` nor `multipart/form-data` (create and update)   |
| `invalid_slug`           | 422    | Slug breaks the [slug rules](#slugs)                                               |
| `invalid_path`           | 422    | File path breaks the [path rules](#file-paths)                                     |
| `invalid_metadata`       | 422    | Bad metadata key, value, pair count or `kazari.*` key                              |
| `too_many_files`         | 422    | More than `limits.max_files` in a version or upload                                |
| `edit_not_found`         | 422    | Edit: an `old_text` matched nowhere; the message names the closest lines           |
| `edit_ambiguous`         | 422    | Edit: an `old_text` matched more than once without `replace_all`                   |
| `unauthorized`           | 401    | Missing, invalid, expired or revoked credential                                    |
| `forbidden`              | 403    | Missing scope, write policy, disabled user, or admin role required                 |
| `quota_exceeded`         | 403    | The owner's storage quota would be exceeded                                        |
| `too_many_uploads`       | 403    | The caller already has `limits.max_open_uploads` open uploads                      |
| `not_found`              | 404    | No such artifact, user, key, grant, job or route; also a soft-deleted artifact on write routes |
| `version_not_found`      | 404    | No such version                                                                    |
| `file_not_found`         | 404    | No such file in the version                                                        |
| `upload_not_found`       | 404    | Unknown, expired or someone else's upload                                          |
| `share_link_not_found`   | 404    | The artifact has no share link with that prefix                                    |
| `method_not_allowed`     | 405    | Route exists, method does not (`Allow` header lists the methods)                   |
| `slug_taken`             | 409    | Slug exists (including a purged slug still in its tombstone period)                |
| `upload_offset_mismatch` | 409    | Chunk offset does not match the staged size                                        |
| `anchor_mismatch`        | 409    | Edit: an anchor's line changed since it was read, or is past the end of the file   |
| `handle_taken`           | 409    | Admin API: handle exists                                                           |
| `email_taken`            | 409    | Admin API: another user has the email                                              |
| `job_running`            | 409    | Admin API: the job is already running                                              |
| `gone`                   | 410    | The artifact has been purged                                                       |
| `fetch_failed`           | 502    | A URL named by `url` or `source_url` could not be fetched; the message says why    |
| `version_conflict`       | 412    | `expected_version` / `If-Match` is not the latest version                          |
| `too_large`              | 413    | A file, version, chunk, upload, manifest or request body is over its limit         |
| `rate_limited`           | 429    | Too many requests; see `Retry-After` and [Rate limits](#rate-limits)               |
| `internal`               | 500    | Server fault; details are logged, never returned                                   |
