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

The repository has an example skill in `examples/kazari/SKILL.md` with the authoring conventions and tool recipes, for agents that read skills; for Claude Code, copy it to `~/.claude/skills/kazari/` or a project's `.claude/skills/`.

### With OAuth

Cloud agents' custom connectors, and agents configured without a key, authenticate with OAuth. Add the URL `https://mcp.<base>/`; a request without a token gets `401` with `WWW-Authenticate: Bearer resource_metadata="https://mcp.<base>/.well-known/oauth-protected-resource"`, from which the client discovers the authorization server. See [identity.md](identity.md).

## Tools

| Tool | What it does |
| ---- | ------------ |
| `create_artifact` | Create an artifact from inline `files[]` and/or an `upload_id` and publish it as version 1. |
| `update_artifact` | Add a version (changed files, `delete: true` entries, `upload_id`) or change name, description, metadata (`null` deletes a key) or visibility; `expected_version` guards against concurrent publishes. |
| `begin_upload` | Start a chunked upload; returns `upload_id`, `expires_at`, `max_chunk_bytes`, `max_total_bytes`. |
| `upload_chunk` | Append one chunk of one file at a stated offset. |
| `get_artifact` | The artifact record and its versions; `include_files` adds file lists. |
| `read_file` | One file: text inline, small binaries (up to 256 KiB) as a base64 blob, larger files as a REST download URL. |
| `list_artifacts` | One page of visible artifacts, filtered by `mine`, `metadata` and `stale`. |
| `publish_version` | Serve an existing version (rollback or roll-forward). |
| `mark_stale` | Start the grace period (`grace` such as `72h` or `14d`) after which the artifact is soft-deleted. |
| `unmark_stale` | Cancel a pending expiry. |
| `set_visibility` | `public`, `unlisted`, or `unpublished` to take the page offline; `public`/`unlisted` bring it back. |
| `delete_artifact` | Soft delete, or `hard: true` to purge now. |

Every result carries a one-line text summary first (for publishing tools `Published https://<slug>.<base>/ (version N)`), then the JSON of the result, which is also the structured content. The structured types are the REST API's (`PublishResult`, `ArtifactDetail`, `ArtifactList`, `Upload`, `ChunkResult`, `StaleResult`, `OK`).

Tool descriptions are generated from the instance configuration, so they state the real slug rules, reserved slugs, URL shape, size limits, CSP allow-list, inline-upload threshold and chunk limit. A model needs nothing else to use them.

## Resources and prompt

| URI template | Content |
| ------------ | ------- |
| `kazari://artifacts/{slug}` | `application/json`: the artifact record and versions with file lists. |
| `kazari://artifacts/{slug}/files/{+path}` | One file of the published version, with its own content type: text inline, binary as a blob, up to 1 MiB by default; larger files are an error (use `read_file`). `{+path}` may contain slashes. |

The `publish-page` prompt (optional argument `topic`) returns the authoring conventions: a single `index.html`, relative links, inline CSS/JS or only the CDNs the CSP allows, no server-side code, slug rules, when to use chunked upload, and to report the URL back.

## Chunked upload recipe

Inline content is fine up to the inline guidance (2 MiB by default, any single file or the files together). Above that:

1. `begin_upload` → `upload_id`, `expires_at`, `max_chunk_bytes` (4 MiB by default, measured after base64 decoding), `max_total_bytes`.
2. For each file, call `upload_chunk` with `upload_id`, `path`, `offset`, `content` and `encoding` (`base64` for binary, `utf8` for text). The first chunk of a path has `offset: 0`; each next one uses the `bytes_received` of the previous result.
3. A chunk that ends exactly where the staged file ends is treated as a retry and acknowledged without writing, so resending after a lost response is safe. Any other wrong offset fails with `upload_offset_mismatch: ... (current=<n>: resend this path from offset <n>)`.
4. `create_artifact` or `update_artifact` with `upload_id`. Inline `files[]` in the same call are merged with the staged files and win on a path clash. The staging area is removed after the call whether it succeeded or failed; unused uploads expire after their TTL.

## Errors

Tool failures are MCP tool errors (`isError: true`) whose text is `<code>: <message>`, with the same codes as the REST API:

| Code | Meaning |
| ---- | ------- |
| `invalid_slug`, `invalid_path`, `invalid_metadata`, `invalid_argument` | Input rejected; the message says why. |
| `slug_taken` | Another artifact (or a recent tombstone) holds the slug. |
| `version_conflict` | `expected_version` is not the latest version. |
| `too_large`, `too_many_files`, `quota_exceeded` | A file, version, chunk, upload or quota limit was hit. |
| `upload_offset_mismatch` | Wrong chunk offset; the text carries `current=<n>`. |
| `upload_not_found` | Unknown, expired, foreign or already consumed upload. |
| `too_many_uploads` | Too many open uploads for this user. |
| `not_found`, `version_not_found`, `file_not_found` | Nothing at that slug, version or path. |
| `gone` | The artifact was purged. |
| `forbidden` | The token lacks the tool's scope (`forbidden: scope "artifacts:write" required`) or the write policy denies the change. |
| `rate_limited` | The credential used up its write budget, `limits.write_rate_per_minute` shared with the REST API (`rate_limited: too many writes; retry in 12s`). Applies to the write and delete tools only. |
| `internal` | Server error, including a handler that crashed; details are in the server log. |

## HTTP checks

Before authentication the `mcp.` host applies a per-client-IP limit of 120 requests a minute, shared by the MCP endpoint and the OAuth endpoints; beyond it requests get `429 rate_limited` with `Retry-After`. The endpoint then applies two checks the MCP specification requires against DNS rebinding and version confusion:

- **Origin.** A request with an `Origin` header is rejected with `403` and a JSON error unless the origin is the MCP host itself (`https://mcp.<base>`) or listed in the allowed origins. Requests without `Origin` (every non-browser client) pass.
- **Protocol version.** A request whose `MCP-Protocol-Version` header names a version the server does not speak gets `400` with a JSON error. Supported: 2026-07-28, 2025-11-25, 2025-06-18, 2025-03-26, 2024-11-05.

Then the bearer token is checked: a missing or invalid token gets `401` with a JSON error body and `WWW-Authenticate: Bearer error="invalid_token", resource_metadata="..."`.

The request body limit is large enough for a whole version sent inline in base64 or one full chunk; the artifact and upload services enforce the real limits.
