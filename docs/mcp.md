# MCP server

Kazari exposes its artifact operations as an MCP server at `https://mcp.<base>/` over Streamable HTTP. The server is stateless: every POST builds a fresh server for the authenticated caller, so there is no session to keep and `GET`/`DELETE` on the endpoint answer `405`.

Authentication is a bearer token: a Kazari API key, or an OAuth access token once OAuth is set up. OAuth sign-in (for MCP clients that discover the authorization server) is documented in [identity.md](identity.md).

## Connecting

Any agent or MCP client that supports Streamable HTTP can connect, whether it runs in the cloud or on your own machine. It authenticates in one of two ways.

### With an API key

Agents that take the server in their configuration can send a key as an `Authorization` header. Many take a JSON entry along these lines:

```json
{
  "mcpServers": {
    "kazari": {
      "url": "https://mcp.<base>/",
      "headers": { "Authorization": "Bearer <key>" }
    }
  }
}
```

Claude Code, for example, takes the same with `claude mcp add --transport http kazari https://mcp.<base>/ --header "Authorization: Bearer <key>"`.

Issue the key with `POST /users/{handle}/keys` on the admin API (or the CLI's admin commands); the secret is shown once. A key carries scopes: `artifacts:read` for the read tools, `artifacts:write` for publishing and lifecycle changes, `artifacts:delete` for `delete_artifact`.

The server serves its own skill, generated from the instance's settings (see [Skill](#skill)). To install it for an agent that reads skills from disk, such as Claude Code, save the output of `kazari skill` as `~/.claude/skills/kazari/SKILL.md` or in a project's `.claude/skills/kazari/`. The repository's `examples/kazari/SKILL.md` is a static copy with the default settings.

### With OAuth

Cloud agents' custom connectors, and agents configured without a key, authenticate with OAuth. Add the URL `https://mcp.<base>/`; a request without a token gets `401` with `WWW-Authenticate: Bearer resource_metadata="https://mcp.<base>/.well-known/oauth-protected-resource"`, from which the client discovers the authorization server. See [identity.md](identity.md).

## Tools

| Tool | What it does |
| ---- | ------------ |
| `create_artifact` | Create an artifact from inline `files[]` and/or an `upload_id` and publish it as version 1. |
| `update_artifact` | Add a version (changed files, `delete: true` entries, `upload_id`) or change name, description, metadata (`null` deletes a key) or visibility; `expected_version` guards against concurrent publishes. |
| `edit_file` | Change one text file of the latest version with text or anchor edits and publish the result as a new version; see [Editing files](#editing-files). |
| `begin_upload` | Start a chunked upload; returns `upload_id`, `expires_at`, `max_chunk_bytes`, `max_total_bytes`. |
| `upload_chunk` | Append one chunk of one file at a stated offset. |
| `get_artifact` | The artifact record and its versions; `include_files` adds file lists. |
| `read_file` | One file: text inline (`anchors: true` prefixes every line with its anchor), small binaries (up to 256 KiB) as a base64 blob, larger files as a REST download URL. The result names the `version` read and the `latest_version`. |
| `list_artifacts` | One page of visible artifacts, filtered by `mine`, `metadata` and `stale`. |
| `publish_version` | Serve an existing version (rollback or roll-forward). |
| `mark_stale` | Start the grace period (`grace` such as `72h` or `14d`) after which the artifact is soft-deleted. |
| `unmark_stale` | Cancel a pending expiry. |
| `set_visibility` | `public`, `unlisted`, `members` (only people signed in to the instance and share-link holders can open it), or `unpublished` to take the page offline; any of the first three brings it back. |
| `create_share_link` | A URL that opens a `members` artifact without signing in, until `expires_in` (default 7 days, at most `serve.share_link_max_ttl`) passes or it is revoked. The URL is returned only this once. |
| `list_share_links` | An artifact's share links (prefix, label, expiry, uses, whether active), without their URLs. |
| `revoke_share_link` | End a share link by its prefix; browsers that opened it lose access on their next request. |
| `delete_artifact` | Soft delete, or `hard: true` to purge now, after the user confirms where the client supports it ([Confirmation before a purge](#confirmation-before-a-purge)). |
| `get_guide` | The instance's skill as Markdown, for clients that do not load skills over MCP. |

Every result carries a one-line text summary first (for publishing tools `Published https://<slug>.<base>/ (version N)`), then the JSON of the result, which is also the structured content. The structured types are the REST API's (`PublishResult`, `EditResult`, `ArtifactDetail`, `ArtifactList`, `Upload`, `ChunkResult`, `StaleResult`, `CreatedShareLink`, `ShareLinkList`, `OK`). `get_guide` returns the Markdown alone.

Tool descriptions are generated from the instance configuration, so they state the real slug rules, reserved slugs, URL shape, size limits, CSP allow-list, inline-upload threshold, chunk limit, default visibility and longest share-link lifetime. A model needs nothing else to use them. The server's instructions say how the tools fit together (read with anchors, edit with `expected_version`, read again on a conflict) and point to the skill.

## Editing files

`edit_file` changes one text file of the artifact's latest version and publishes the result as a new version; every other file carries over, as with `update_artifact`. Only the changed text travels, so a one-line change to a large page is a small call. The REST API has the same operation as `POST /artifacts/{slug}/edit` and the CLI as `kazari edit`.

A call carries up to 100 edits of one kind, and its result may be at most the file size limit:

- **Text edits** `{old_text, new_text, replace_all}`. `old_text` must match the file once, unless `replace_all` replaces every match. Edits apply in order, each to the result of the one before. When `old_text` does not match exactly, whole lines are compared ignoring trailing whitespace, then ignoring indentation, in which case `new_text` is re-indented to the file's indentation; in a file with CRLF line breaks, `new_text` gets them too. These fallbacks apply to an `old_text` of at most 2000 lines, and `replace_all` changes at most 10000 places. The result reports each edit's `match` (`exact`, `trailing_whitespace` or `indentation`), so a fuzzy match is never silent.
- **Anchor edits** `{start, end, new_text}`. `read_file` with `anchors: true` prefixes every line with an anchor, its line number and a 4-digit hash of its content: `12:3fa0|<h1>Title</h1>`. An anchor edit replaces the lines from `start` to `end` (inclusive; `end` defaults to `start`) with `new_text`; an empty `new_text` deletes them. All anchors in a call refer to the file as read and their ranges must not overlap. A line whose content changed since it was read fails the call with `anchor_mismatch` before anything is written.

`expected_version` works as on `update_artifact`: a newer latest version fails with `version_conflict`. Without it, edits that still apply are applied to the newest content. The input schema states the two kinds as a `oneOf`, so clients that validate arguments reject an edit that mixes them.

The result, `EditResult`, has the new `version` and per edit `match`, `replacements`, `start_line`, `end_line` and an anchored `excerpt` of the new text with two lines around it (at most 24 lines of new text, each line clipped to 160 bytes with a note of what is left out), so a follow-up anchor edit needs no fresh read. A miss (`edit_not_found`) names the most similar lines with their anchors; an ambiguous `old_text` (`edit_ambiguous`) lists the lines it matched.

`edit_file` always edits the latest version, while `read_file` reads the published version unless given `version`. The two differ only after `publish_version` rolled back; `read_file` returns both numbers so an agent can read `latest_version` before editing.

## Resources and prompt

| URI | Content |
| --- | ------- |
| `kazari://artifacts/{slug}` | `application/json`: the artifact record and versions with file lists. |
| `kazari://artifacts/{slug}/files/{+path}` | One file of the published version, with its own content type: text inline, binary as a blob, up to 1 MiB by default; larger files are an error (use `read_file`). `{+path}` may contain slashes. |
| `skill://kazari/SKILL.md` | `text/markdown`: the instance's skill ([Skill](#skill)). |
| `ui://kazari/preview.html` | `text/html;profile=mcp-app`: the preview app ([Preview app](#preview-app)). |

The `publish-page` prompt (optional argument `topic`) returns the authoring conventions: a single `index.html`, relative links, inline CSS/JS or only the CDNs the CSP allows, no server-side code, slug rules, when to use chunked upload, and to report the URL back.

## Skill

The server publishes one [Agent Skill](https://agentskills.io/specification), `kazari`, generated from the instance's configuration: the workflow, how to edit files, the authoring conventions with this instance's limits, CSP and URL shapes, and the next step after each error. It reaches an agent four ways:

- **Skills over MCP** (the `io.modelcontextprotocol/skills` extension, SEP-2640). The server declares the extension, answers `skills/list` (cacheable for five minutes, `private`) and `skills/get` with the skill's frontmatter and a SHA-256 digest of its one file, and serves the file as the resource `skill://kazari/SKILL.md`.
- **The `get_guide` tool**, for clients that do not implement the extension yet.
- **The server's instructions**, which name the skill's URI.
- **`GET /v1/skill`** on the REST API and `kazari skill` on the CLI, for agents that load skills from disk.

Restart the server after a configuration change and every copy states the new settings.

## Preview app

`create_artifact`, `update_artifact`, `edit_file` and `publish_version` link their results to the MCP App `ui://kazari/preview.html` (the `io.modelcontextprotocol/ui` extension, SEP-1865). A host that supports MCP Apps renders the result as a card with the page's name, version and links to the page and to the pinned version; other hosts show the text result as before.

The card also embeds the pinned version of the page when the instance's Content-Security-Policy lets other sites frame pages: when `serve.csp` has no `frame-ancestors` directive, or one that names a source other than `'none'` and `'self'`. The default policy has `frame-ancestors 'none'`, so the card shows links only. To embed pages, add the origins your MCP host renders apps from to `frame-ancestors`; the resource then asks the host to allow frames from `https://*.<base>`. Allowing framing lets those sites show any page of the instance inside their own, so list only hosts you trust. Per-artifact `kazari.csp` overrides do not change the decision. A `members` page is never embedded, whatever the policy: it needs a sign-in, which cannot run inside a frame.

## Confirmation before a purge

`delete_artifact` with `hard: true` cannot be undone. When the client speaks protocol 2026-07-28 or later and declared form elicitation, the call first returns an `input_required` result (SEP-2322) asking the user to confirm; the client shows the question and calls again with the answer. If the user declines, the result is `{"ok": false}` and nothing changes. Older clients cannot be asked, because the server is stateless and cannot send a request of its own; for them the tool's destructive hint is the host's cue to ask, as before. Soft deletes are reversible and ask nothing.

## Chunked upload recipe

Inline content is fine up to the inline guidance (2 MiB by default, any single file or the files together). Above that:

1. `begin_upload` → `upload_id`, `expires_at`, `max_chunk_bytes` (4 MiB by default, measured after base64 decoding), `max_total_bytes`.
2. For each file, call `upload_chunk` with `upload_id`, `path`, `offset`, `content` and `encoding` (`base64` for binary, `utf8` for text). The first chunk of a path has `offset: 0`; each next one uses the `bytes_received` of the previous result.
3. A chunk that ends exactly where the staged file ends is treated as a retry and acknowledged without writing, so resending after a lost response is safe. Any other wrong offset fails with `upload_offset_mismatch: ... (current=<n>: resend this path from offset <n>)`.
4. `create_artifact` or `update_artifact` with `upload_id`. Inline `files[]` in the same call are merged with the staged files and win on a path clash. The staging area is removed after the call whether it succeeded or failed; unused uploads expire after their TTL.

## Errors

Tool failures are MCP tool errors (`isError: true`) whose text is `<code>: <message>`, with the same codes as the REST API, followed for most codes by a line `Next step: ...` that says what to do, for example:

```text
version_conflict: latest version is 5, expected 3
Next step: Someone published since you read it: read the artifact again (get_artifact, or read_file for the file), redo your change on the new content and retry with expected_version set to the new latest_version.
```

| Code | Meaning |
| ---- | ------- |
| `invalid_slug`, `invalid_path`, `invalid_metadata`, `invalid_argument` | Input rejected; the message says why. |
| `slug_taken` | Another artifact (or a recent tombstone) holds the slug. |
| `version_conflict` | `expected_version` is not the latest version. |
| `edit_not_found` | An `old_text` matched nowhere, even ignoring whitespace; the message names the closest lines. |
| `edit_ambiguous` | An `old_text` matched more than once without `replace_all`; the message lists the lines. |
| `anchor_mismatch` | An anchor's line changed since the file was read, or is past its end. |
| `too_large`, `too_many_files`, `quota_exceeded` | A file, version, chunk, upload or quota limit was hit. |
| `upload_offset_mismatch` | Wrong chunk offset; the text carries `current=<n>`. |
| `upload_not_found` | Unknown, expired, foreign or already consumed upload. |
| `share_link_not_found` | The artifact has no share link with that prefix. |
| `too_many_uploads` | Too many open uploads for this user. |
| `not_found`, `version_not_found`, `file_not_found` | Nothing at that slug, version or path. |
| `gone` | The artifact was purged. |
| `forbidden` | The token lacks the tool's scope (`forbidden: scope "artifacts:write" required`) or the write policy denies the change. |
| `rate_limited` | The credential used up its write budget, `limits.write_rate_per_minute` shared with the REST API (`rate_limited: too many writes; retry in 12s`). Applies to the write and delete tools only. |
| `internal` | Server error, including a handler that crashed; details are in the server log. |

The skill lists the next step for every code a tool returns.

## HTTP checks

Before authentication the `mcp.` host applies a per-client-IP limit of 120 requests a minute, shared by the MCP endpoint and the OAuth endpoints; beyond it requests get `429 rate_limited` with `Retry-After`. The endpoint then applies two checks the MCP specification requires against DNS rebinding and version confusion:

- **Origin.** A request with an `Origin` header is rejected with `403` and a JSON error unless the origin is the MCP host itself (`https://mcp.<base>`) or listed in the allowed origins. Requests without `Origin` (every non-browser client) pass.
- **Protocol version.** A request whose `MCP-Protocol-Version` header names a version the server does not speak gets `400` with a JSON error. Supported: 2026-07-28, 2025-11-25, 2025-06-18, 2025-03-26, 2024-11-05.

Then the bearer token is checked: a missing or invalid token gets `401` with a JSON error body and `WWW-Authenticate: Bearer error="invalid_token", resource_metadata="..."`.

The request body limit is large enough for a whole version sent inline in base64 or one full chunk; the artifact and upload services enforce the real limits.
