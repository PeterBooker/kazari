---
name: kazari
description: Publish static web pages (HTML artifacts) to an Kazari instance through its MCP tools, and update, inspect, roll back, expire or delete them. Use when the user asks to publish, share, host or update a page, report or demo on Kazari, or mentions an Kazari URL or slug.
---

# Kazari

Kazari hosts static web pages. Each artifact has a permanent **slug** and is served at `https://<slug>.<base>/` (the published version) and `https://<slug>--v<N>.<base>/` (a pinned version). The tool descriptions state this instance's exact limits and CSP; trust them over this file.

## Connect (once)

```sh
claude mcp add --transport http kazari https://mcp.<base>/ --header "Authorization: Bearer <key>"
```

`<key>` is an Kazari API key, issued by an admin (`POST /users/{handle}/keys` on the admin API). The tools then appear as `mcp__kazari__*`.

## Authoring conventions

- Build a single self-contained `index.html`; it is served at the root URL. Extra files (CSS, JS, images) are fine.
- Use **relative links** (`assets/app.js`, `about.html`), never paths starting with `/`, so the page also works on pinned version URLs.
- Put CSS and JS inline, or load libraries only from the CDNs the Content-Security-Policy allows (by default scripts from cdnjs.cloudflare.com, cdn.jsdelivr.net and unpkg.com, styles from fonts.googleapis.com, fonts from fonts.gstatic.com, images from any https URL; `fetch` only to the page's own files). Anything else is blocked by the browser.
- No server-side code: no PHP, API routes, databases or server build step. Ship built output.
- Slug: 3-63 characters of `a-z`, `0-9` and `-`, starting and ending with a letter or digit, no `--`, not a reserved name (`api`, `mcp`, `admin`, `www`, ...). It is permanent. On `slug_taken` pick another.
- Metadata keys the server reads: `kazari.spa = "true"` (serve `index.html` for extension-less misses, for client-side routing), `kazari.indexable = "true"` (allow search engines), `kazari.csp` (per-artifact CSP, if the instance allows it). Other keys are free-form labels.
- Always report the `url` from the result back to the user.

The `publish-page` prompt carries the same conventions, generated from the instance settings.

## Recipes

**Publish a page.** `create_artifact` with `slug`, `name`, `description` and `files: [{path: "index.html", content: "..."}]`. Binary files use `encoding: "base64"`.

**Update a page.** `get_artifact` to read `latest_version`, then `update_artifact` with `files` (changed files only; `{path, delete: true}` removes one; everything else carries over) and `expected_version: <latest_version>`. On `version_conflict`, fetch again, re-apply and retry. Without files only name, description, metadata or visibility change.

**Large content (any file or the total above 2 MiB by default): chunked upload.**

1. `begin_upload` returns `upload_id`, `expires_at` and `max_chunk_bytes`.
2. For each file: `upload_chunk` with `upload_id`, `path`, `offset: 0` and the first piece of `content` (`encoding: "base64"` for binary, `"utf8"` for text). Each next call uses `offset` = the `bytes_received` returned by the previous call for that path. A chunk is at most `max_chunk_bytes` after base64 decoding.
3. If a call fails or its result is lost, resend the same chunk at the same offset (a repeat is acknowledged, not written twice). On `upload_offset_mismatch ... current=<n>`, resume that path from offset `n`.
4. `create_artifact` or `update_artifact` with `upload_id` (inline `files` in the same call are merged and win on a path clash). The call consumes the upload whether it succeeds or fails.

**Inspect.** `get_artifact` (with `include_files`), `read_file` (text inline, small binaries as base64, large files as a REST download URL), `list_artifacts` (`mine: created|updated|any`, `metadata`, `stale`, `cursor`).

**Lifecycle.** `publish_version` rolls back or forward. `set_visibility unpublished` takes a page offline; `public` or `unlisted` brings it back. `mark_stale` (grace such as `72h` or `14d`) schedules expiry; `unmark_stale` or a new version cancels it. `delete_artifact` soft-deletes; `hard: true` purges permanently, so ask the user first.

## Errors

Tool errors read `<code>: <message>`. Common codes: `invalid_slug`, `slug_taken`, `version_conflict`, `too_large`, `too_many_files`, `quota_exceeded`, `upload_offset_mismatch`, `upload_not_found` (expired or already consumed: begin a new upload), `not_found`, `gone`, `forbidden` (the key lacks a scope).
