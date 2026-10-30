# ADR 0005: Archive uploads at a URL without a credential

Status: accepted (2026-10-30)

## Context

Agents in sandboxes build whole sites locally and then cannot get them into Kazari. Every byte of a chunked upload has to pass through the model as tool-call arguments, a third larger again in base64, and MCP clients drop or refuse arguments far below the 4 MiB chunk ceiling. Raising the ceiling changes nothing: the server is not the bottleneck.

The same sandbox has a shell with `curl` and a network stack, but no Kazari credential. The MCP connection is signed in; the shell is not, and connector tokens never enter it. The agent can move the bytes itself if something stands in for the credential.

Kazari already has two capabilities that live in a URL: share links, whose random token is stored hashed, and the `read_file` download URL, which an OAuth client cannot use either. An archive upload is the same shape in the upload direction.

## Decision

- **`begin_upload` takes an optional `archive_sha256` and `archive_bytes`.** Given both, the response also carries `archive_url`: `PUT https://api.<base>/v1/uploads/{id}/archive?token=<43 characters>`. The REST route `POST /uploads` takes the same fields. No new tool: an upload is one thing with two ways to stage files.
- **The route takes no bearer.** It is the only one on the api host that does not; a pattern bound to `PUT` matches that method alone, so a `GET` of the path still meets the 401 every other route gives. The token is 32 random bytes, kept as a SHA-256 hash like a share link, compared in constant time after a length check.
- **The digest is required, not optional.** The URL passes through the model's context, the client's conversation store, the sandbox's shell history and any proxy's access log. Bound to one digest and size, a leaked URL lets its holder stage nothing but bytes the owner already has. Without the binding, whoever used it first could put their own page under the owner's slug when the agent publishes.
- **The body is read only after the token has opened an upload**, capped at the declared size, hashed while it spills to `data_dir/tmp`, and kept only when the hash matches. A miss counts an attempt; after three the upload is removed, so a leaked URL cannot be used to pour bandwidth into the instance until its TTL. A matching body that cannot be staged removes the upload too: the same bytes would fail the same way.
- **One receipt per token.** The statement that marks the archive received is the one that checks it is unreceived, so two bodies racing for one token cannot both be staged.
- **Zip and gzip-compressed tar**, told apart by their first bytes. The tar path is the multipart bundle's decoder. The zip path lists entries and inflates each only as it is written to staging, under the same path validation, file count and total size as a chunk. Both formats are in every sandbox; the tool text recommends zip.
- **Publishing stays as it is.** The archive becomes staged files of the upload; `create_artifact` and `update_artifact` consume it with `upload_id`, authenticated and attributed as before. Mixed uploads work: chunks and an archive may share one upload, and inline files still win a path clash.
- **No new configuration.** The URL lives as long as the upload (`limits.upload_ttl`), the archive is bounded by `limits.max_version_bytes`, the upload by `limits.max_open_uploads`, and the attempt budget is a constant.
- **Alongside, the bundle decoder charges each tar entry's declared size** against its cap before copying it, so a sparse entry's holes, which `archive/tar` fills from zeros that never pass the capping reader, cannot write past the cap. The archive route makes that decoder reachable with a token instead of a key, which is the moment to close the gap.

## Consequences

- The api host serves one route without authentication. It answers 404 before reading a body for any token it does not know, sits behind the per-IP rate limit, and its token is useless without the bytes it was issued for.
- The token appears in the query, so a reverse proxy may log it; Kazari's own request log records the path alone. The documentation says so, with what a logged token is worth.
- Whether a sandbox may reach `api.<base>` is the operator's and the sandbox vendor's policy, not Kazari's. The tool text tells the agent to report a blocked connection and fall back to chunks.
- Signed download URLs for `read_file` are not part of this change, though the same token design would serve them.
