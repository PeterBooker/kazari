# MCP server

Kazari exposes its artifact operations as an MCP server at `https://mcp.<base>/`, over Streamable HTTP. An agent connected to it can publish, edit, share and retire pages.

This page covers connecting an agent, the tools, resources and skill the server offers, and its errors and request checks. The examples use `art.example.com` as the base domain. Setting up OAuth sign-in for MCP clients is in [Identity and sign-in](identity.md).

## How the server works

The server is stateless: every POST builds a fresh server for the authenticated caller. There is no session to keep, and `GET` and `DELETE` on the endpoint answer `405`.

Every request carries a bearer token: a Kazari API key, or an OAuth access token once OAuth is set up.

## Connecting an agent

Any agent or MCP client that supports Streamable HTTP can connect, whether it runs in the cloud or on your own machine. It authenticates with an API key or with OAuth.

### Using an API key

Agents that take the server in their configuration can send a key in the `Authorization` header. Many take a JSON entry along these lines:

```json
{
  "mcpServers": {
    "kazari": {
      "url": "https://mcp.art.example.com/",
      "headers": { "Authorization": "Bearer kzk_..." }
    }
  }
}
```

Claude Code, for example, adds the same server with one command:

```sh
claude mcp add --transport http kazari https://mcp.art.example.com/ \
  --header "Authorization: Bearer kzk_..."
```

An administrator issues the key with `POST /users/{handle}/keys` on the [admin API](api.md#admin-api), or with the [CLI's admin commands](cli.md#admin-commands). The secret is shown once.

A key carries scopes, which decide the tools it can call:

| Scope              | Tools                                  |
| ------------------ | -------------------------------------- |
| `artifacts:read`   | The read tools                         |
| `artifacts:write`  | Publishing and lifecycle changes       |
| `artifacts:delete` | `delete_artifact`                      |

Agents that read skills from disk can also install the server's skill ([Installing the skill from disk](#installing-the-skill-from-disk)).

### Using OAuth

Cloud agents' custom connectors, and agents configured without a key, authenticate with OAuth. Add the URL `https://mcp.art.example.com/` to the client.

A request without a token gets `401` with this header, from which the client discovers the authorization server:

```text
WWW-Authenticate: Bearer resource_metadata="https://mcp.art.example.com/.well-known/oauth-protected-resource"
```

The sign-in that follows, and how an administrator sets it up, are in [Identity and sign-in](identity.md).

## Tools

The server offers eighteen tools. The four with more to know have their own sections below the table:

| Tool | What it does |
| ---- | ------------ |
| `create_artifact` | Create an artifact from inline `files[]`, an `upload_id` or both, and publish it as version 1. |
| `update_artifact` | Add a version, change the name, description, metadata or visibility, or restore a soft-deleted artifact with `restore: true`. |
| `edit_file` | Change one text file of the latest version with text or anchor edits, and publish the result as a new version ([Editing files](#editing-files)). |
| `begin_upload` | Start a [chunked upload](#uploading-in-chunks); returns `upload_id`, `expires_at`, `max_chunk_bytes` and `max_total_bytes`. |
| `upload_chunk` | Append one chunk of one file at a stated offset. |
| `discard_upload` | Remove an upload you no longer need. |
| `get_artifact` | The artifact record and its versions; `include_files` adds file lists. |
| `read_file` | One file of a version, as text, a base64 blob or a download URL; `start_line` and `end_line` read a window of a large text file. |
| `list_artifacts` | One page of visible artifacts, filtered by `mine`, `metadata`, `stale` and `deleted`. |
| `publish_version` | Serve an existing version (rollback or roll-forward). |
| `mark_stale` | Start the grace period (`grace`, such as `72h` or `14d`) after which the artifact is soft-deleted. |
| `unmark_stale` | Cancel a pending expiry. |
| `set_visibility` | Make the artifact `public`, `unlisted`, `members` or `unpublished`. |
| `create_share_link` | Create a URL that opens a `members` artifact without signing in. |
| `list_share_links` | An artifact's share links (prefix, label, expiry, uses, whether active), without their URLs. |
| `revoke_share_link` | End a share link by its prefix; browsers that opened it lose access on their next request. |
| `delete_artifact` | Soft-delete, which `update_artifact` with `restore: true` undoes, or purge now with `hard: true` after the user confirms ([Confirmation before a purge](#confirmation-before-a-purge)). |
| `get_guide` | The instance's skill as Markdown, for clients that do not load skills over MCP. |

### `update_artifact`

Adds a version, changes the artifact's fields, or both. A new version comes from changed files, `delete: true` entries and an `upload_id`, and every file you do not mention carries over.

The fields are the name, description, metadata and visibility. In metadata, `null` deletes a key.

`expected_version` guards against concurrent publishes: when a newer version is the latest, the call fails with `version_conflict`.

To make the new version hold exactly a set of files, list the latest version's paths with `get_artifact` and `include_files`, and send a `delete: true` entry for each path not in the set. That is what `kazari publish --update` does for a directory.

`restore: true` brings back a soft-deleted artifact, which is otherwise `not_found`, and then applies the other changes. It works until the retention period ends and the artifact is purged. `list_artifacts` with `deleted: "include"` or `"only"` shows the soft-deleted artifacts you may still see: your own, and the ones you created or updated.

### `read_file`

Returns one file of the published version, or of the version given in `version`. Text comes inline, and `anchors: true` prefixes every line with its [anchor](#anchor-edits). Binaries up to 256 KiB come as a base64 blob, and larger files as a REST download URL.

The result names the `version` read and the `latest_version`. For text, its `content` field repeats the text, anchored when asked, for clients that show only the structured content.

A text file larger than the inline limit, 1 MiB, comes back as a download URL. To read it over MCP anyway, pass `start_line` and `end_line`, 1-based and inclusive. The result holds those lines, anchored when asked, and states `start_line`, `end_line` and `total_lines`. A window holds as many whole lines as fit the inline limit; when it was cut short, the note after the text says where to continue. Windows work on text files of any size up to `limits.max_file_bytes`, small ones included.

### `set_visibility`

Sets one of four visibilities. `members` means that only people signed in to the instance, and holders of a share link, can open the page. `unpublished` takes the page offline, and setting `public`, `unlisted` or `members` brings it back.

### `create_share_link`

Returns a URL that opens a `members` artifact without signing in. The link works until `expires_in` passes or it is revoked; by default it lasts 7 days, and at most `serve.share_link_max_ttl`.

The URL is returned only this once. `list_share_links` shows the links without their URLs.

### Results

Every result starts with a one-line text summary, then the JSON of the result, which is also the structured content. For publishing tools, the summary names the address and version:

```text
Published https://q3-report.art.example.com/ (version 3)
```

The structured types are the REST API's: `PublishResult`, `EditResult`, `ArtifactDetail`, `ArtifactList`, `Upload`, `ChunkResult`, `StaleResult`, `CreatedShareLink`, `ShareLinkList` and `OK`. `get_guide` returns the Markdown alone.

### Descriptions and instructions

Tool descriptions are generated from the instance's configuration, so they state its real slug rules, reserved slugs, URL shape, size limits, CSP allow-list, inline-upload threshold, chunk limit, default visibility and longest share-link lifetime. A model needs nothing else to use the tools.

The server's instructions say how the tools fit together: read with anchors, edit with `expected_version`, and read again on a conflict. They also point to the [skill](#skill).

## Editing files

`edit_file` changes one text file of the artifact's latest version and publishes the result as a new version. Every other file carries over, as with `update_artifact`. Only the changed text travels, so a one-line change to a large page is a small call.

The same operation is [`POST /artifacts/{slug}/edit`](api.md#edit-a-file-post-artifactsslugedit) in the REST API and [`kazari edit`](cli.md#edit) in the CLI.

A call carries up to 100 edits of one kind, text edits or anchor edits, and its result can be at most the file size limit. The input schema states each edit as a `oneOf` of the two kinds, so clients that validate arguments reject an edit that mixes them. The server refuses a call that mixes text edits with anchor edits.

### Text edits

A text edit is `{old_text, new_text, replace_all}`. `old_text` must match the file once, unless `replace_all` replaces every match. Edits apply in order, each to the result of the one before.

To change a heading, provided version 3 is still the latest:

```json
{
  "slug": "q3-report",
  "path": "index.html",
  "expected_version": 3,
  "edits": [{ "old_text": "<h1>Q3 report</h1>", "new_text": "<h1>Q4 report</h1>" }]
}
```

When `old_text` does not match exactly, whole lines are compared, first ignoring trailing whitespace, then ignoring indentation. On an indentation match, `new_text` is re-indented to the file's indentation. In a file with CRLF line breaks, `new_text` gets them too.

These fallbacks apply to an `old_text` of at most 2000 lines, and `replace_all` changes at most 10000 places. The result reports each edit's `match` (`exact`, `trailing_whitespace` or `indentation`), so a fuzzy match is never silent.

### Anchor edits

`read_file` with `anchors: true` prefixes every line with an anchor: its line number and a 4-digit hash of its content, then `|`:

```text
12:3fa0|<h1>Title</h1>
```

An anchor edit is `{start, end, new_text}`. It replaces the lines from `start` to `end`, inclusive, with `new_text`, and an empty `new_text` deletes them. `end` defaults to `start`.

To delete lines 20 and 21:

```json
{
  "slug": "q3-report",
  "path": "index.html",
  "edits": [{ "start": "20:7c1e", "end": "21:e03a", "new_text": "" }]
}
```

All anchors in a call refer to the file as read, and their ranges must not overlap. If a line's content changed since it was read, the call fails with `anchor_mismatch` before anything is written.

### Versions and conflicts

`expected_version` works as on `update_artifact`: a newer latest version fails the call with `version_conflict`. Without it, the edits that still apply are applied to the newest content.

`edit_file` always edits the latest version, while `read_file` reads the published version unless given `version`. The two differ only after `publish_version` rolled back. `read_file` returns both numbers, so an agent can read `latest_version` before editing.

### Edit results

The result, `EditResult`, has the new `version` and, for each edit, `match`, `replacements`, `start_line`, `end_line` and an anchored `excerpt`. The excerpt shows the new text with two lines around it, so a follow-up anchor edit needs no fresh read. It holds at most 24 lines of new text, each clipped to 160 bytes with a note of what is left out.

A miss (`edit_not_found`) names the most similar lines with their anchors. An ambiguous `old_text` (`edit_ambiguous`) lists the lines it matched.

## Uploading in chunks

Inline content is fine up to the inline guidance, which is 2 MiB by default for any single file or for the files together. Above that, stage the files in a chunked upload:

1. Call `begin_upload`. It returns `upload_id`, `expires_at`, `max_chunk_bytes` and `max_total_bytes`. `max_chunk_bytes` is 4 MiB by default, measured after base64 decoding.
2. Call `upload_chunk` for each chunk of each file, with `upload_id`, `path`, `offset`, `content` and `encoding` (`base64` for binary, `utf8` for text). The first chunk of a path has `offset: 0`, and each next one uses the `bytes_received` of the previous result.
3. Call `create_artifact` or `update_artifact` with `upload_id`. Inline `files[]` in the same call are merged with the staged files and win on a path clash.

The staging area is removed after the publishing call, whether it succeeded or failed. `discard_upload` removes an upload you no longer need, and unused uploads expire after their TTL. Each user may hold `limits.max_open_uploads` uploads open, 5 by default; beyond that `begin_upload` fails with `too_many_uploads`.

### Retrying a chunk

A chunk that ends exactly where the staged file ends is treated as a retry and acknowledged without writing, so resending after a lost response is safe. Any other wrong offset fails with an error that says where to resume:

```text
upload_offset_mismatch: ... (current=<n>: resend this path from offset <n>)
```

## Confirmation before a purge

`delete_artifact` with `hard: true` cannot be undone, so the server asks the user first where the client allows it. Soft deletes ask nothing and are reversible with `update_artifact` and `restore: true` until the retention period ends.

When the client speaks protocol 2026-07-28 or later and declared form elicitation, the call first returns an `input_required` result (SEP-2322) asking the user to confirm. The client shows the question and calls again with the answer. If the user declines, the result is `{"ok": false}` and nothing changes.

Older clients cannot be asked, because the server is stateless and cannot send a request of its own. For them, the tool's destructive hint is the host's cue to ask, as before.

## Resources and prompt

The server offers four resources:

| URI | Content |
| --- | ------- |
| `kazari://artifacts/{slug}` | `application/json`: the artifact record and versions with file lists. |
| `kazari://artifacts/{slug}/files/{+path}` | One file of the published version, with its own content type. |
| `skill://kazari/SKILL.md` | `text/markdown`: the instance's [skill](#skill). |
| `ui://kazari/preview.html` | `text/html;profile=mcp-app`: the [preview app](#preview-app). |

A file resource carries text inline and binaries as a blob, up to 1 MiB by default. A larger file is an error; use `read_file` for it instead. `{+path}` can contain slashes.

The `publish-page` prompt, with an optional `topic` argument, returns the authoring conventions:

- a single `index.html`
- relative links
- inline CSS and JS, or only the CDNs the CSP allows
- no server-side code
- the slug rules
- when to use a chunked upload
- a reminder to report the URL back

## Skill

The server publishes one [Agent Skill](https://agentskills.io/specification), `kazari`, generated from the instance's configuration. It holds the workflow, how to edit files, the authoring conventions with this instance's limits, CSP and URL shapes, and the next step after each error.

The skill reaches an agent four ways:

- **Skills over MCP**, the `io.modelcontextprotocol/skills` extension (SEP-2640). The server declares the extension and answers `skills/list` and `skills/get` with the skill's frontmatter and a SHA-256 digest of its one file. Its `skills/list` answer is cacheable for five minutes (`private`). The file itself is the resource `skill://kazari/SKILL.md`.
- **The `get_guide` tool**, for clients that do not implement the extension yet.
- **The server's instructions**, which name the skill's URI.
- **`GET /v1/skill`** on the REST API and `kazari skill` on the CLI, for agents that load skills from disk.

After a configuration change, restart the server: every copy then states the new settings.

### Installing the skill from disk

Agents that read skills from disk, such as Claude Code, take the output of [`kazari skill`](cli.md#skill). Save it as `~/.claude/skills/kazari/SKILL.md` for all your projects, or in a project's `.claude/skills/kazari/`:

```sh
mkdir -p ~/.claude/skills/kazari && kazari skill > ~/.claude/skills/kazari/SKILL.md
```

The repository's `examples/kazari/SKILL.md` is the skill generated from the default settings, with `<base>` in place of a domain. The test `TestExampleSkill` keeps it current.

## Preview app

`create_artifact`, `update_artifact`, `edit_file` and `publish_version` link their results to the MCP App `ui://kazari/preview.html`, from the `io.modelcontextprotocol/ui` extension (SEP-1865). A host that supports MCP Apps renders the result as a card with the page's name, its version, and links to the page and to the pinned version. Other hosts show the text result as before.

### Embedding the page

The card also embeds the pinned version of the page when the instance's Content-Security-Policy lets other sites frame pages. That is the case when `serve.csp` has no `frame-ancestors` directive, or one that names a source other than `'none'` and `'self'`.

By default the policy has `frame-ancestors 'none'`, so the card shows links only. To embed pages, add the origins your MCP host renders apps from to `frame-ancestors`. The resource then asks the host to allow frames from `https://*.art.example.com`.

> [!WARNING]
> Allowing framing lets those sites show any page of the instance inside their own. List only hosts you trust.

Per-artifact `kazari.csp` overrides do not change the decision. A `members` page is never embedded, whatever the policy, because it needs a sign-in, which cannot run inside a frame.

## Errors

A failed tool call is an MCP tool error (`isError: true`) whose text is `<code>: <message>`, with the same codes as the REST API. For most codes a second line, `Next step: ...`, says what to do:

```text
version_conflict: latest version is 5, expected 3
Next step: Someone published since you read it: read the artifact again (get_artifact, or read_file for the file), redo your change on the new content and retry with expected_version set to the new latest_version.
```

The codes are:

| Code | Meaning |
| ---- | ------- |
| `invalid_slug`, `invalid_path`, `invalid_metadata`, `invalid_argument` | Input rejected; the message says why. |
| `slug_taken` | Another artifact, or a recent tombstone, holds the slug. |
| `version_conflict` | `expected_version` is not the latest version. |
| `edit_not_found` | An `old_text` matched nowhere, even ignoring whitespace; the message names the closest lines. |
| `edit_ambiguous` | An `old_text` matched more than once without `replace_all`; the message lists the lines. |
| `anchor_mismatch` | An anchor's line changed since the file was read, or is past its end. |
| `too_large`, `too_many_files`, `quota_exceeded` | A file, version, chunk, upload or quota limit was hit. |
| `upload_offset_mismatch` | Wrong chunk offset; the text carries `current=<n>`. |
| `upload_not_found` | Unknown, expired, foreign or already consumed upload. |
| `share_link_not_found` | The artifact has no share link with that prefix. |
| `too_many_uploads` | Too many open uploads for this user; `discard_upload` frees one. |
| `not_found`, `version_not_found`, `file_not_found` | Nothing at that slug, version or path. |
| `gone` | The artifact was purged. |
| `forbidden` | The token lacks the tool's scope (`forbidden: scope "artifacts:write" required`), or the write policy denies the change. |
| `rate_limited` | The credential used up its write budget (`rate_limited: too many writes; retry in 12s`). |
| `internal` | Server error, including a handler that crashed; details are in the server log. |

The write budget is `limits.write_rate_per_minute`, shared with the REST API. Only the write and delete tools can return `rate_limited`.

The skill lists the next step for every code a tool returns.

## HTTP checks

Every request to the `mcp.` host first passes a rate limit. The MCP endpoint then applies the two checks the MCP specification requires against DNS rebinding and version confusion, and checks the bearer token last.

### Rate limit

Before authentication, the `mcp.` host allows 120 requests a minute per client IP, shared by the MCP endpoint and the OAuth endpoints. Beyond that, requests get `429 rate_limited` with `Retry-After`.

### Origin

A request with an `Origin` header is rejected with `403` and a JSON error, unless the origin is the MCP host itself (`https://mcp.art.example.com`) or listed in the allowed origins. Requests without `Origin`, which includes every non-browser client, pass.

### Protocol version

A request whose `MCP-Protocol-Version` header names a version the server does not speak gets `400` with a JSON error. The server speaks 2026-07-28, 2025-11-25, 2025-06-18, 2025-03-26 and 2024-11-05.

### Bearer token

A missing or invalid token gets `401` with a JSON error body and this header:

```text
WWW-Authenticate: Bearer error="invalid_token", resource_metadata="..."
```

### Request size

The request body limit is large enough for a whole version sent inline in base64, or for one full chunk. The artifact and upload services enforce the real limits.
