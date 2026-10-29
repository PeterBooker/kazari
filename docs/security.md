# Security

This page is the threat model and security reference for Kazari v1. It describes what Kazari protects, where the trust boundaries are, which limits apply, what an operator must do, and what the security reviews found.

It is for administrators deciding how to run an instance and for anyone reviewing Kazari's design. An administrator should at least work through the [operator checklist](#operator-checklist).

## Threat model

The threat model names what Kazari protects, who acts on it, and where untrusted input crosses into the server.

### Assets

Kazari protects five assets:

| Asset | Why it matters |
|---|---|
| Artifact content and history | Members publish work, such as internal reports and prototypes, that may not be meant for the open web. Versions are immutable, and rollback depends on them. |
| Artifact integrity | Readers trust a page at `<slug>.<base>` as the owner's work. Nobody else should be able to change it unless the write policy allows that. |
| Credentials | API keys, OAuth access and refresh tokens, the admin token and the OIDC client secret. |
| The control plane | The `api.`, `mcp.` and `admin.` hosts. Anyone who can act there can change any artifact or user. |
| Instance availability | Disk (blobs, staging and spill files), memory and the SQLite database. |

### Actors

Six kinds of actor reach an instance:

- **Anonymous visitor.** Reads artifact pages that are not `members` only. Can send any host header, path, query, method and header, and holds no credential.
- **Viewer.** Reads `members` pages, either signed in to the instance in a browser or holding a share link. Holds only host-only cookies, on the hosts they opened.
- **Authenticated member.** Holds an API key or OAuth token with some of `artifacts:read`, `artifacts:write` and `artifacts:delete`. Might try to escape their scopes, act on other users' artifacts beyond the write policy, exceed limits or quota, attack the server with what they upload, or make the server fetch URLs it should not reach.
- **Malicious bundle.** Content a member uploads: HTML and JavaScript that runs in readers' browsers, archives built to escape paths or exhaust resources, and metadata built to inject headers.
- **Malicious OAuth client.** Anyone can register a client through Dynamic Client Registration (DCR) or name a client ID metadata document (CIMD) URL. Such a client can send users through the authorization flow and make the server fetch URLs.
- **Administrator.** Trusted. Holds the admin token or an admin-scoped key.

### Trust boundaries

Untrusted input crosses into Kazari at five boundaries:

1. **Browser to artifact host.** Untrusted requests meet untrusted content, so everything served here is treated as hostile to everything else. For a `members` artifact, the gate first decides whether the request may see the page ([gated artifacts](#gated-artifacts)).
2. **Browser or client to control plane.** Every request must carry a bearer credential, or the consent cookie on the OAuth pages. Every action is authorised against scopes and the write policy.
3. **Upload boundary.** Bytes from members arrive as inline JSON, a multipart tar.gz or chunked staging. They are size-capped, path-validated and content-addressed before anything references them.
4. **Kazari to the identity provider, to CIMD URLs and to URLs members name.** These are outbound requests. OIDC tokens are verified cryptographically. Metadata fetches, files named by `url` and page imports go to public addresses only ([files from URLs and imported pages](#files-from-urls-and-imported-pages)).
5. **Reverse proxy to Kazari**, in proxy mode only. Kazari trusts the proxy's `X-Forwarded-For` and `X-Real-IP` headers.

## Origin isolation

Arbitrary HTML and JavaScript is the product, so Kazari does not try to make uploaded content safe. Instead it makes sure each artifact's content can harm only its own origin.

Every artifact is served from its own host, `<slug>.<base>`, and every pinned version from `<slug>--v<N>.<base>`. Each host is a separate web origin, so the browser's same-origin policy keeps artifacts apart:

- Script in one artifact cannot read another artifact's DOM, storage, cookies, IndexedDB or service workers, and cannot make credentialed same-origin requests to it.
- Artifacts cannot reach the control plane as same-origin. API, MCP and admin credentials are bearer headers, never cookies, so a page cannot ride on a visitor's session.
- The site handler serves only `GET` and `HEAD`, except for the gate's sign-out form under `/.kazari/`, and only the gate sets cookies. An artifact's own script can still set cookies for its own host. Those stay confined to that origin, and the script cannot read or overwrite the gate's `HttpOnly` cookies.

### Kazari's cookies

Every cookie Kazari sets is a `__Host-` cookie: path `/`, `HttpOnly; Secure; SameSite=Lax`, and no `Domain`. Kazari never sets a cookie with a `Domain` attribute, and the `__Host-` prefix makes browsers refuse a cookie with one of these names from any other host.

On `mcp.`, Kazari sets three cookies:

| Cookie | Holds | Lifetime |
|---|---|---|
| `__Host-kazari_oauth` | An OAuth authorization in progress | Ten minutes |
| `__Host-kazari_signin` | A browser sign-in in progress | Ten minutes |
| `__Host-kazari_session` | A browser's sign-in | Twelve hours |

The API and MCP endpoints read none of them. On an artifact host, the gate sets `__Host-kazari_nonce` and `__Host-kazari_view`, described under [gated artifacts](#gated-artifacts).

### One site, many origins

Origins are separate, but every artifact host and the control plane share one *site*, the registrable domain, and cookies are scoped by site, not by origin. That has three consequences:

- An artifact's script can set a cookie with `Domain=<base>`, or with the parent domain of `<base>`, and the browser then sends it to every host under that domain. Kazari's own cookies cannot be planted this way because of their `__Host-` prefix, and the consent and sign-in forms also check `Origin`. A page can still fill a visitor's cookie jar for `<base>` past the 64 KiB header limit, after which every Kazari host answers that visitor `431` until they clear their cookies.
- Requests from one artifact host to another, or to `mcp.<base>`, are same-site, so `SameSite=Lax` does not stop them. A `members` page therefore also sends `Cross-Origin-Resource-Policy: same-origin`, so another artifact cannot include its scripts, JSON or images.
- If `<base>` sits under a domain that serves other applications, such as `art.example.com` under `example.com`, artifact scripts can read that domain's non-`HttpOnly` cookies set with `Domain=example.com`, and can set cookies those applications will receive.

So give `base_domain` a registrable domain of its own, such as `example-artifacts.com` rather than a subdomain of the company domain. Consider listing it on the [Public Suffix List](https://publicsuffix.org) as well, which makes browsers treat each artifact host as its own site.

### Host handling

The site handler accepts a host in one shape only (`internal/site/host.go`). It lower-cases the host and strips its port and trailing dot. The host must then end in `.<base>` with exactly one label in front.

The label splits on the last `--v` followed by digits, with no leading zeros and at most nine digits, and the slug must pass the [slug rules](#slug-rules). Anything else gets the instance 404 page, including the apex, deeper subdomains and IDNA labels (which contain `--`).

Reserved slugs such as `api`, `mcp`, `admin` and `www` cannot be created, and the host router sends the control-plane hosts elsewhere before the site handler runs.

### Path handling

Requests under `/.kazari/` go to the gate, never to the artifact's files, and no artifact can store a file under `.kazari/` (`internal/site/site.go`).

Every other request path is decoded first, then refused with `404` if it contains a control character, a backslash or a `..` segment. Because the check runs after decoding, it catches the percent-encoded forms too.

A path that passes is cleaned and looked up in the version's file map, which is built from validated paths only. No path is ever joined onto a filesystem path: blobs are opened by content hash. Directory redirects are always relative to the host (`/<clean>/`), so they cannot be turned into open redirects.

## Header policy

Every artifact response carries these headers:

| Header | Value | Reason |
|---|---|---|
| `X-Content-Type-Options` | `nosniff` | The type is decided at upload (manifest, then extension, then a 512-byte sniff), never from the request. |
| `Content-Security-Policy` | The instance default, or the artifact's `kazari.csp` when `allow_csp_override` is on | See [Default CSP](#default-csp). Instance error pages always use the default. |
| `Referrer-Policy` | `strict-origin-when-cross-origin` | Paths do not leak to third parties. |
| `X-Robots-Tag` | `noindex` unless `kazari.indexable = "true"`; always on errors | Self-hosted content is not indexed by default. |
| `Cache-Control` | Depends on the host and the visibility | See [Caching](#caching). |
| `Cross-Origin-Resource-Policy` | `same-origin`, on `members` artifacts only | Sibling artifact hosts are same-site, so a viewer's cookie rides along when another artifact loads this one's files; the browser then refuses to hand them over. |
| `ETag` | `"<sha256>"` | The content address. |

### Caching

`Cache-Control` depends on which host answers and what it answers with:

| Response | `Cache-Control` |
|---|---|
| The latest host | `no-cache` |
| A version host | `public, max-age=<lifecycle.tombstone, at most a year>, immutable` |
| Any error | `no-cache` |
| Any host of a `members` artifact | `private, no-cache` |

Versions never change, while the latest host revalidates cheaply against the strong ETag.

The version-host lifetime never exceeds the tombstone period. Once a slug is freed, another owner's `<slug>--v1` is therefore never answered from a cache of the old owner's content.

A shared cache never keeps a `members` page, and the browser asks again each time, even for a version it has seen. A sign-out or a revoked link therefore takes effect there too, and the ETag keeps the repeat request a `304`.

### Control-plane headers

The control plane sends its own headers:

- Every control-plane host adds `Cross-Origin-Opener-Policy: same-origin`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer` and `Cache-Control: no-store`.
- The OAuth pages add `X-Frame-Options: DENY` and the CSP `default-src 'none'; style-src 'unsafe-inline'; form-action 'self' <redirect origin>`.
- Raw file reads through the user API (`GET /v1/artifacts/{slug}/files/...`) carry `Content-Security-Policy: default-src 'none'; sandbox`, so artifact bytes can never run as a document on the `api.` origin.

### Header values from users

Kazari restricts header values that come from users before it stores them. `kazari.csp` and per-file content types must be printable ASCII: no CR, LF, NUL, tabs or non-ASCII bytes. Content types must also parse as a media type and be at most 255 bytes.

### Default CSP

The default value of `serve.csp` is:

```text
default-src 'self';
script-src 'self' 'unsafe-inline' https://cdnjs.cloudflare.com https://cdn.jsdelivr.net https://unpkg.com;
style-src 'self' 'unsafe-inline' https://fonts.googleapis.com;
font-src 'self' https://fonts.gstatic.com;
img-src 'self' data: https:;
connect-src 'self';
frame-ancestors 'none'
```

The reasoning behind it:

- Origin isolation is the main control, not the CSP. The CSP limits what an artifact can pull in and send out, not what it can do to itself.
- Inline scripts and styles (`'unsafe-inline'`) are allowed because generated artifacts are usually single-file HTML. The common public CDNs are allowed for the same reason.
- `connect-src 'self'` stops a page from sending data to a third party with `fetch`, XHR or WebSockets, which makes casual exfiltration of what a visitor types harder. It is not a hard guarantee: navigation and image beacons are still possible.
- `frame-ancestors 'none'` stops other sites from framing artifacts, which rules out clickjacking.
- `allow_csp_override`, on by default, lets an artifact replace the policy through `kazari.csp`. The override affects only that artifact's own origin, so it can relax the artifact's own protections but nobody else's. Operators who want a fixed policy set it to `false`.

### Framing and the preview app

The MCP server's [preview app](mcp.md#preview-app) (`ui://kazari/preview.html`) runs inside the MCP host's sandbox. It is static HTML with inline script and carries no credential. It shows the name, description and URLs from the tool result with `textContent`, never as markup.

The app embeds the published page only when the instance CSP's `frame-ancestors` allows framing. The default, `'none'`, keeps the card to links.

Relaxing `frame-ancestors` to an MCP host's app origins lets those origins frame every page of the instance, so pages become clickjackable from there. Artifact pages hold no sessions, so what an attacker can make a visitor click is limited to the page's own controls.

> [!WARNING]
> Allow only hosts you trust in `frame-ancestors`, and keep `'none'` when pages carry forms that matter.

## Gated artifacts

An artifact with visibility `members` is served only to people who sign in to the instance in their browser, meaning the users the provisioning policy admits, and to holders of one of its share links. The same check runs on the latest host and on every version host.

[Viewing members pages](identity.md#viewing-members-pages) describes the flow. This section covers its security properties.

### No cookie spans hosts

Gating never uses a parent-domain cookie. Such a cookie would be refused once `<base>` is on the Public Suffix List, would be sent to every artifact, and could be shadowed or pushed out by any artifact's script (finding #19 below is that attack).

Instead the sign-in session stays on `mcp.`, and each artifact host gets its own view session through a redirect.

### The handshake

A navigation without access starts a handshake:

1. The artifact host sets `__Host-kazari_nonce` (32 random bytes, ten minutes) and sends the browser to `mcp.<base>/oauth/signin` with the host's label, the path and the S256 of the nonce.
2. A live session on `mcp.`, or a fresh sign-in, returns the browser to the artifact host with a code.

The code is 32 random bytes, stored hashed and single use. It lasts one minute and is valid only for that host, and only with the matching nonce. A code that someone else started therefore cannot sign a visitor in as that person, and a code is useless anywhere else.

The return path must be a path on the same host (`/`, not `//` or `/\`), so the sign-in is never an open redirect. The MCP host sends codes only to labels that name an artifact host.

### The view cookie

`__Host-kazari_view` (`HttpOnly; Secure; SameSite=Lax`, path `/`) holds one of two tokens:

- A view session token: 32 random bytes, stored hashed, valid for one host, and ending with its sign-in session.
- A share link token.

Every request checks the cookie against the database. The user must still be enabled, an API-key sign-in's key must still be active, and a share link must be unrevoked, unexpired and belong to this artifact row. A slug reused after its tombstone therefore never opens with an old link.

### Share links

A share link token has the form `kzs_<prefix>_<secret>`, from 40 + 256 random bits. Kazari stores its SHA-256 and shows the link once. A link lasts 7 days by default, and at most `serve.share_link_max_ttl` (90 days by default).

Links are multi-use, so mail scanners and chat previews that fetch links do not use them up, and a `HEAD` request changes nothing. Opening a link swaps the token into the cookie and redirects to `/` with `Referrer-Policy: no-referrer`.

Kazari's request log records paths, not query strings, so tokens and codes stay out of it. A reverse proxy in front logs whole URLs unless told not to.

### Managing share links

Creating, listing and revoking a link need the same rights as changing the artifact under the write policy. Creating and revoking also need the `artifacts:write` scope, and listing needs `artifacts:read`. An offline artifact's links can be managed only by the people who can see it.

### Responses

Kazari never sends a gated page to a request without access. A page navigation gets a `302` to sign in and every other request a `401`, both with `Cache-Control: no-store`.

Gated content carries `Cache-Control: private, no-cache` on every host, version hosts included, because an `immutable` version would otherwise be shown from the browser's cache after a sign-out. It also carries `Cross-Origin-Resource-Policy: same-origin`. The gate's own pages carry `default-src 'none'` and `X-Frame-Options: DENY`.

Sign-out (`/.kazari/signout`, on a host the browser opened) is a form post that must come from the same host. It ends the browser's sign-in session, and with it every view session.

### Agents

MCP and REST already read `members` artifacts with bearer credentials. Artifact hosts do not accept bearer tokens: headless browsers add extra headers to every request a page makes, including requests to the CDNs the CSP allows, so a key sent that way would leak to third parties. To let a headless browser open a `members` page, give it a share link.

### What gating does not do

Gating has four limits:

- **It does not hide the slug.** An unknown slug answers `404` and a `members` one redirects to sign in, so a guessed slug's existence shows. Choose slugs that can be seen.
- **It does not recall copies.** A page's own script, a service worker it installed, a download or a screenshot keeps what a viewer already saw. Revoking a link or disabling a user stops new responses. Copies that a shared cache took while a page was still `public` stay until you purge them, on version hosts for up to `lifecycle.tombstone`.
- **It does not protect a page from its own artifact.** Under the `shared` write policy any member can change any artifact, including a `members` page's script, which runs with the viewer's access to that page.
- **It does not choose people.** `members` means every user of the instance. Per-person or per-group lists are not part of this version.

## Upload trust boundary and limits

Content enters through four paths: inline JSON, multipart bundles, chunked uploads and edits. All of them end in `artifact.Service`, which applies the same rules to each.

### Inline JSON

Inline JSON arrives through `POST` and `PUT /v1/artifacts` and through the MCP tools. File contents are UTF-8 or base64, and the body is capped with `http.MaxBytesReader`.

### Multipart bundles

A multipart upload has a `manifest` part, capped at 64 KiB, then a `bundle` part holding a tar.gz. The multipart stream is capped at `MaxBodyBytes`.

Kazari decodes the gzip and tar streams into one temporary spill file, never into memory. Every decompressed byte of the tar stream counts against the same `MaxBodyBytes`: headers, PAX and GNU long-name records, padding, the data of skipped entries, and file content. A gzip bomb, a flood of empty entries or a stack of 1 MiB names therefore stops at the cap.

A kept name longer than 1024 bytes is refused as soon as it is read. Only regular files are kept: directories, symlinks, hard links, devices and FIFOs are skipped and never followed. A leading `./` is stripped, and path validation rejects absolute and `..` names.

The spill file is removed after the publish, and the hourly `gc` job removes any that a crash left behind.

### Chunked uploads

A chunked upload (`/v1/uploads`) belongs to the user who began it. Anyone else gets `upload_not_found`, and so does a request for an expired upload.

Upload ids are 16 random bytes. Each id's exact shape is checked before it is used as a directory name, so it cannot escape the staging root. Staged files are stored under numeric names, never under their paths.

Each chunk must start at the staged size, and a retry of the last chunk is acknowledged. A chunk's content type is checked like a manifest's when the chunk arrives. A publish reads each staged file up to the size recorded for it, so a chunk that arrives while the publish runs is not published.

The open-upload limit is checked in the transaction that creates the upload. Expired uploads and orphaned directories are swept.

### Edits

An edit (`POST /v1/artifacts/{slug}/edit`, or the `edit_file` tool) reads the stored file, applies the edits in memory and publishes the result through the same `Service.Update` path as any other version. The write policy, quota, size and path rules all apply.

Only UTF-8 text without NUL bytes is edited, up to `limits.max_file_bytes`. A call carries at most 100 edits, and each edit makes at most 10000 replacements. The size of each result is checked against the file limit before the result is built.

Matching streams over the file's lines. The whitespace-tolerant fallback is a linear Knuth-Morris-Pratt search. The fallback and the closest-match search apply only to an `old_text` of at most 2000 lines, and the closest-match search scores at most 40 candidate windows in full.

Excerpts in results clip long lines, and an anchored read (`?anchors=true`) is streamed, so neither holds a copy of a large file's lines in memory.

### Files from URLs and imported pages

A member can make Kazari fetch a public URL: as the content of a file in a create or update, or as a page to copy with `import_page`, `kazari import` or `source_url`. One client serves both and the CIMD fetch (`internal/fetch`):

- It dials public unicast addresses only. The check runs on the resolved address of every connection, so a name that points at a private, loopback, link-local, carrier-grade NAT or IPv6 transition address is refused, on redirects too. At most five redirects are followed, and no proxy from the environment is used. The error says only that the address is not public, or that the host did not resolve, never which address a name resolved to.
- It sends no cookies and no credentials: a URL that carries them is refused, and the address a page or report records is the one after redirects, without credentials. It identifies itself as `Kazari/<version>`.
- One request stops at `fetch.timeout` (15 s), and the files one publish names by `url`, or one import, stop at `fetch.budget` (60 s) together. A body is capped at `limits.max_file_bytes`, all of a publish's `url` files together at `limits.max_version_bytes`, and every body is spilled to `data_dir/tmp`, so a slow host never holds a publish open; the `gc` job removes spill files a crash left behind.
- An imported page, and each of its stylesheets, is held in memory to be parsed and rewritten and is therefore capped at 4 MiB; at most four imports run at once per instance, and a fifth is refused with `rate_limited`. The parser refuses documents nested deeper than 512 elements. Scripts, event handlers and `javascript:` URLs are removed unless the caller keeps them, and the copy is served under the member's artifact origin like any upload. What the import fetches is bounded by the version limits and counted against the owner's quota; a resource that fails is tried once, and the report lists at most a hundred of them.
- Every import is audited with its source, and imports and URL files count against the write rate like any publish.

`fetch.enabled: false` removes the tool and refuses `url` and `source_url`. A member can still make the server request any public URL within these limits, which an operator of an instance with a sensitive network position should weigh.

### Path rules

Every file path must pass `artifact.ValidatePath`:

- It is not empty, it is at most 1024 bytes, and each segment is at most 255 bytes.
- It is relative and is not a directory (no trailing `/`).
- It holds no control characters, DEL, backslash or invalid UTF-8.
- It is cleaned with `path.Clean`, and no `..` is left after cleaning.
- It is not `.kazari` and not under `.kazari/`.

Paths are compared byte for byte, with no case folding or Unicode normalisation, so `a.html` and `A.html`, or the NFC and NFD forms of one name, are different files. Duplicate paths in one request are rejected after cleaning.

### Slug rules

A slug matches `^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`, is 3 to 63 bytes long, contains no `--` and is not reserved. It cannot change after the artifact is created.

### Text fields

`name` and a version's `label` are at most 256 bytes, and `description` at most 4096 bytes. All three must be valid UTF-8 with no C0 or C1 control characters, DEL, or bidirectional override and isolate characters; `description` can hold newlines and tabs.

Every listing returns these fields and the CLI prints them, replacing control characters in what it prints.

### Metadata

An artifact holds at most 64 metadata pairs by default (`limits.metadata_max_pairs`). Keys match `^[a-zA-Z0-9_.-]{1,64}$`, and values are at most 4096 bytes.

The only `kazari.*` keys allowed are `kazari.spa` and `kazari.indexable` (`"true"` or `"false"`), and `kazari.csp` (printable ASCII).

### Limits

Each limit, its default, and where it is enforced:

| Limit | Default | Where |
|---|---|---|
| Version size | 50 MiB (`limits.max_version_bytes`) | streamed through a counting reader |
| File size | 25 MiB (`limits.max_file_bytes`) | streamed through a counting reader |
| Files per version | 2000 (`limits.max_files`) | before any content is read |
| Owner quota | 2 GiB (`limits.default_user_quota`, per user) | streamed through a counting reader, then checked again in the transaction that adds the version |
| JSON body with inline files, multipart stream, decompressed tar stream | version size + 1 MiB | `MaxBytesReader`, bundle decoder |
| Name, label, description | 256, 256, 4096 bytes | before any content is read |
| Other JSON bodies | 1 MiB | `MaxBytesReader` |
| Multipart manifest | 64 KiB | |
| Chunk | 4 MiB (`limits.max_chunk_bytes`) | |
| Chunked upload total | 50 MiB | |
| Open uploads per user | 5 (`limits.max_open_uploads`) | counted in the transaction that creates the upload |
| Upload lifetime | 1 h (`limits.upload_ttl`) | |
| Writes per credential | 60 per minute (`limits.write_rate_per_minute`) | one budget across the user API and the MCP write and delete tools; 429 with `Retry-After` |
| Requests per client IP | 120 per minute on each of `api.`, `mcp.` (MCP and OAuth together) and `admin.` `/v1/` | before authentication; IPv6 grouped by /64 |
| Rate-limiter state | at most 65536 buckets per limiter | idle buckets swept |
| OAuth request bodies | 64 KiB | |
| Reading one request | 10 min (headers 10 s) | `http.Server` timeouts, so a stalled client cannot hold a connection; artifact hosts close a connection whose `GET` announces a body |
| Writing one response | 15 min | |
| Admin token | at least 32 characters | refused at startup, except under a `localhost` base domain |
| Share link lifetime | 7 days by default, at most 90 days (`serve.share_link_max_ttl`) | |
| Browser sign-in session | 12 h; a view code 1 min, single use | every view session ends with its sign-in session |
| Client ID metadata document | 64 KiB, 10 s, cached 1 h, no redirects, public addresses only | |
| Registered or fetched client without a grant | kept 7 days | removed by the `oauth-cleanup` job; a client that has completed a grant is kept |
| File named by `url` | 25 MiB (`limits.max_file_bytes`), all of a publish's together 50 MiB, 15 s per request (`fetch.timeout`), 60 s together (`fetch.budget`), 5 redirects, public addresses only, no credentials | spilled to `data_dir/tmp` before the publish |
| Page import | the version limits, 4 MiB for the page and each stylesheet, 60 s in all (`fetch.budget`), 4 imports at once per instance | resources that do not fit stay remote and are reported, at most 100 of them |

## Authentication and authorisation

Every credential is checked on each request, then every action against scopes and the write policy.

### API keys

An API key has the form `kzk_<8-char prefix>_<52-char secret>`, in lower-case base32 from 40 + 256 random bits. Only the SHA-256 of the whole key is stored, and it is compared with `crypto/subtle`. The key is shown once, when it is issued.

Revoked and expired keys are refused, and so are the keys of disabled users. Disabling a user revokes all their keys. `last_used_at` is written at most once a minute.

### Scopes

There are four scopes: `artifacts:read`, `artifacts:write`, `artifacts:delete` and `admin`. Every user-API route needs its scope:

| Routes | Scope |
|---|---|
| Reads | `artifacts:read` |
| Create, update, publish, stale and uploads | `artifacts:write` |
| Delete | `artifacts:delete` |

A key's or token's scopes are capped at what the owner's current role allows, so a demoted admin loses `admin` straight away.

Admin power comes from the `admin` scope, not from the role alone: a principal is an admin only if the user has the admin role and the credential carries `admin`. An admin's key issued as read-only stays read-only.

### Admin host

`/v1/` on `admin.` accepts the configured admin token, or a credential that resolves to an admin principal (the admin role and the `admin` scope). The admin token is compared by SHA-256, then in constant time, and must be at least 32 characters.

The same API is served without a credential on the admin socket, `admin.sock` in the data directory (`admin.socket`), which the server creates with mode `0600` and the CLI uses on the server's host. The socket's permissions are the trust boundary: whoever can open it already owns the database beside it. Requests over it act as the `admin` principal with the source `local` and are not rate-limited.

Everything else gets `401` or `403`. Every attempt counts against the per-IP limit, so tokens cannot be guessed at speed.

`/health` and `/metrics` on the admin host are unauthenticated. The admin host's `/health` reports the version; the public `/health` on the apex does not.

With `admin.listen` set, the admin host is served only on that listener, and the public listener answers `admin.<base>` with `404`.

### Write policy

`auth.write_policy` decides who can change an artifact. The default, `shared`, lets any member with `artifacts:write` change any artifact; `owner` limits changes to the owner and admins. A version is charged to the owner's quota whoever adds it. Transfer is admin-only, because it moves the artifact's usage onto the new owner's quota without asking them.

Listings show public artifacts plus the caller's own. Unlisted artifacts are hidden from other people's listings but served to anyone who knows the URL.

An unpublished (including hidden) or soft-deleted artifact is invisible to everyone except admins, its owner and the users who created or updated it. Only they can list it, get its record or read its files through the API or MCP.

Changes are read-modify-write inside one transaction, so a concurrent update cannot undo a transfer, a delete or a visibility change, and the sweep re-checks a row inside the transaction that expires or purges it, so a restore between its listing and its write holds. Blob garbage collection is locked out while a publish writes and commits its blobs.

### OAuth 2.1

The `mcp.` host runs an OAuth 2.1 authorization server for MCP clients.

#### Clients and redirects

Only public clients are supported, and PKCE with S256 is required: a 43-character challenge and a verifier of 43 to 128 characters, compared in constant time.

Registration is open and rate-limited per IP. A client that completes no grant within seven days is forgotten by the `oauth-cleanup` job, so registrations that never go anywhere do not accumulate; a client that has completed a grant is kept.

Redirect URIs must be `https`, or `http` on a loopback host. They are matched exactly, except that the port of a loopback URI is ignored (RFC 8252). `resource` must name this server's MCP URL.

Nothing is sent to a redirect URI until the person has seen the consent page, which names its host, and approved or denied the request. Anyone can register a client, so a redirect for an error before that point (a wrong `response_type`, missing PKCE, an unknown scope or resource, a failed upstream sign-in) would make `mcp.<base>` an open redirector (RFC 9700 section 4.11.2); those errors render a page instead.

#### Codes and tokens

Authorization codes are single use, consumed before they are checked, and live five minutes. A code presented a second time revokes the access and refresh tokens its first use obtained: one of the two presenters stole it, and the server cannot tell which (OAuth 2.1 section 4.1.2). Access tokens live one hour and refresh tokens thirty days, and all of them are stored hashed.

Refresh tokens rotate, and replaying a rotated token revokes its whole family. An OAuth principal's rate-limit and audit identity is its grant (`oauth:<family>`), which stays the same across refreshes.

#### Scopes and API-key grants

The authorization server offers and grants only the `artifacts:*` scopes. An OAuth token never carries `admin`, so approving a client never hands it the admin host.

A grant approved with an API key, on an instance with no identity provider, is bound to that key. It stops working when the key is revoked or expires or its user is disabled, and no token outlives the key. Disabling a user revokes their grants.

#### The consent form

The consent form must come from the consent page itself: `Origin` must equal the MCP origin, or `Sec-Fetch-Site` must be `same-origin`. Its request id must also equal the `__Host-` consent cookie, which no sibling host can set. Both checks matter because artifact pages are same-site with `mcp.` and run member-authored script.

The upstream `state` and `nonce` are random per request and checked in constant time.

#### Client ID metadata documents

Client ID metadata documents are fetched only from public unicast addresses. Loopback, RFC 1918, link-local, CGNAT, NAT64/6to4, documentation, benchmarking and reserved ranges are refused at dial time, after DNS resolution, so DNS rebinding cannot get around the check.

### Confirmation before a purge

When a client supports form elicitation on protocol 2026-07-28 or later, the MCP tool `delete_artifact` with `hard: true` asks the user through an `input_required` result. The answer comes back from the client, which is trusted to represent the user as it is for every other call, and the server keeps no state between the two calls.

Older MCP clients are not asked, because a stateless server cannot send them a request. The REST API and the CLI never ask: they rely on the caller, as before.

### OIDC relying party

ID tokens are verified against the provider's JWKS, with issuer, audience (the client id), expiry and nonce checks. The upstream code exchange uses PKCE. Email-based linking and provisioning trust only a verified email, and an existing admin links by email only when `admin_emails` names the address: otherwise an admin needs an identity linked by hand, so a verified address alone never takes over an admin account.

For Google, a non-Gmail address counts as verified only when the `hd` claim equals its domain. A consumer Google account registered on a company address, for example by someone who has since left, therefore cannot sign in as that domain.

For Entra ID, a missing `email_verified` counts as verified, for the configured tenant issuer alone, since the ID token's issuer is checked against it. Only single-tenant apps are supported: the multi-tenant `common` issuer is not.

## Out of scope for v1

Version 1 does not cover:

- **Public-instance hardening.** v1 assumes the members are colleagues: any member can publish arbitrary HTML on the instance's domain. There is no malware scanning, abuse reporting flow or per-artifact takedown other than the admin hide endpoint.
- **Per-person access.** `public` and `unlisted` pages are served to anyone with the URL, and `unlisted` is not private, because slugs are easy to guess. `members` pages are served to every user of the instance and to share-link holders. There are no per-person or per-group lists, and no mode that trusts an identity header from a proxy in front ([what gating does not do](#what-gating-does-not-do)).
- **Isolation between artifacts on the same instance beyond the browser's origin model.** For example, a CSP override that allows framing or third parties is the artifact owner's choice.
- **Protecting artifacts from admins.** Admins can read, transfer and delete everything.
- **Enterprise managed auth**, the RFC 7523 JWT bearer grant.

> [!WARNING]
> Do not run an instance with open sign-up, that is `auto_provision` without `allowed_domains`. If strangers can sign up, the domain will be used for phishing.

## Operator checklist

Work through this list when you set up an instance:

1. **Keep the admin host on its own listener**, such as `admin.listen: 127.0.0.1:9443`, or reach it only through a VPN. With `admin.listen` set, the public listener stops serving `admin.<base>`, including the unauthenticated `/metrics` and `/health`.
2. **Do without the admin token.** Create the first admin user over the admin socket on the server, and issue that user a key with the `admin` scope for administering from elsewhere. Set `auth.admin_token` (`KAZARI_AUTH_ADMIN_TOKEN`) only for scripts that run before any user exists, and remove it again afterwards. While the token exists it must be at least 32 characters, such as the output of `openssl rand -hex 32`.
3. **Pass secrets through the environment.** Refer to them as `${NAME}` in the YAML file, and put the values in `kazari.env` or `.env`.
4. **Use proxy mode only behind exactly one reverse proxy** (`listen.proxy_mode`), and make sure nothing but that proxy can reach Kazari's plain-HTTP port. Kazari takes the rightmost `X-Forwarded-For` entry, the one your proxy added, then `X-Real-IP`. A proxy that appends (nginx's `$proxy_add_x_forwarded_for`) and one that overwrites (Caddy) both work. With two proxies in a chain, such as a CDN then a proxy, the per-IP limit applies to the CDN's address.
5. **Restrict the identity provider.** Set `allowed_domains`, and use `auto_provision` only with it. For Entra ID, use a single-tenant app. Keep `admin_emails` short: the people in it become admins on their first sign-in, and it is the only way an existing admin links by email rather than by an identity linked by hand.
6. **Choose the write policy.** Set `owner` if members should not be able to change each other's artifacts.
7. **Decide on CSP overrides.** Set `serve.allow_csp_override: false` if the instance policy must hold for every artifact.
8. **Back up `data_dir`**: `kazari.db`, with SQLite's online backup or with the service stopped, and `blobs/`. `certmagic/` holds certificate keys, and the database holds key and token hashes and user emails, so treat backups as secrets. `uploads/` is transient and does not need backing up.
9. **Run the latest release.** Releases are built with `-trimpath` and `CGO_ENABLED=0`, by workflows whose actions are pinned to commit SHAs, from base images pinned by digest.
10. **Give artifacts a domain of their own**: a registrable domain, not a subdomain of a domain your other applications use ([origin isolation](#origin-isolation)).
11. **Set the default visibility to `members`** (`serve.default_visibility: members`) on an instance whose pages are not for the open web, so a page published without a visibility is not public. Share links then cover the people outside.
12. **Keep query strings out of the proxy's access log**, or accept that share-link tokens and sign-in codes land there.
13. **Purge a page's hosts from any CDN in front** when a page that was `public` becomes `members`.
14. **Decide whether members may make the server fetch URLs.** Files named by `url` and page imports reach public addresses only, within [their limits](#files-from-urls-and-imported-pages); set `fetch.enabled: false` where the instance must make no outbound requests on members' behalf.

## Reporting a vulnerability

Report vulnerabilities privately, not in a public issue. Use the repository's private vulnerability reporting: the **Security** tab, then **Report a vulnerability**.

Include the version (`kazari version`), the configuration that matters and the steps to reproduce. Expect an answer within a week, and allow a reasonable window for a fix to ship before you disclose.

## Findings log (pre-v1.0 review, 2026-10-24)

Scope: `internal/site`, `artifact`, `httpapi`, `upload`, `auth`, `user`, `oauth`, `oidc`, `middleware`. Every fix has a regression test.

| # | Severity | Finding | Status |
|---|---|---|---|
| 1 | High | An admin user's key or OAuth token counted as admin whatever its scopes. `Principal.Admin` followed the role alone and `HasScope` gives admins every scope, so a key issued read-only (or with the default member scopes) could write, delete and use the admin host. | Fixed: `Admin` needs the admin role and the `admin` scope (`user.Service.Authenticate`, `oauth.Service.Authenticate`), and `RequireAdmin` checks the scope as well. Tests: `TestAuthenticateAdminKeyWithoutAdminScope`, `TestRequireAdmin`, OAuth "narrow request". |
| 2 | Medium | Google `hd` claim was not checked, so a consumer Google account on a company address passed as verified for that domain (domain allow-list, email linking, auto-provisioning). | Fixed in `oidc` identity mapping. Test: `TestIdentityMapping`. |
| 3 | Medium | In proxy mode, `RealIP` trusted the leftmost `X-Forwarded-For` entry, which is client-controlled behind appending proxies. Any client could pick its own IP to dodge per-IP limits and poison logs. | Fixed: rightmost entry. Test: `TestRealIP`. |
| 4 | Medium | The rate-limiter map had no hard cap, and once past 1024 keys every new key scanned the whole map under the lock. Rotating addresses (trivial within an IPv6 /64) grew memory and CPU without bound. | Fixed: hard cap of 65536, idle sweep at most once a minute, IPv6 bucketed by /64. Tests: `TestLimiterBounded`, `TestIPBucket`. |
| 5 | Medium | The bundle decoder counted only file content against the uncompressed cap. A small gzip of millions of empty (or skipped link) entries decoded without bound and kept a `FileInput` per regular file in memory. | Fixed: each tar header costs 512 bytes against the cap. Test: `TestReadBundleEntryCap`. Incomplete; superseded by #21. |
| 6 | Medium | The CIMD fetch guard allowed CGNAT (`100.64.0.0/10`, used by Tailscale), `0.0.0.0/8`, NAT64/6to4 and other special ranges, so server-side requests could reach internal hosts. | Fixed: public unicast only. Test: `TestPublicAddr`. |
| 7 | Low | Per-file content types (manifest, JSON, chunk `Content-Type`) were stored unvalidated and sent back as the `Content-Type` header. Go folds CR/LF, but NUL, non-ASCII and oversized values reached proxies. | Fixed: printable ASCII, at most 255 bytes, must parse. Test: `TestContentTypeValidation`. |
| 8 | Low | `kazari.csp` rejected only CR/LF. Other control characters and non-ASCII went into the header. | Fixed: printable ASCII only. Test: `TestValidateMetadataCSP`. |
| 9 | Low | `GET /v1/artifacts/{slug}/files/...` served artifact HTML/SVG inline, unsandboxed, on the `api.` origin. Bearer auth makes this hard to exploit from a browser, but it is the control-plane origin. | Fixed: `Content-Security-Policy: default-src 'none'; sandbox`. Test: `TestCreateMultipart` header table. |
| 10 | Info | The site path check let backslashes through. No stored path can contain one, but browsers read `\` as `/` in `Location`. | Fixed as defence in depth: backslash paths are refused. Test: `TestPathRules`. |
| 11 | Medium | Quota can be dodged. Under the shared policy, a version added by a non-owner is charged to nobody (`artifact/service.go` `Update`). An owner can also transfer an artifact to any user without their consent (`Transfer`), moving usage onto that user's quota and then growing it without limit. | Fixed: a version is charged to the owner's quota whoever adds it (`artifact.Options.Quotas` looks the owner's up), and transfers are admin-only. Tests: `TestQuotaChargesOwner`, `TestTransfer`. |
| 12 | Low | Before consent, authorization errors redirect to any redirect URI the client registered. DCR and CIMD are open to anyone, so `mcp.<base>` can be used as an open redirect (RFC 9700 section 4.11.2). | Fixed: every error before the consent decision renders a page; only the person's approval or denial on the consent page redirects. Tests: `TestAuthorizeErrorsRenderPage`, `TestCallbackFailures`. |
| 13 | Low | Dynamic client registration is unauthenticated and unbounded: every POST adds a row. | Fixed: #24 limits `mcp.` per client IP, and `oauth-cleanup` removes clients that complete no grant within 7 days (`granted_at`, migration `0005`). Tests: `TestOAuthStoreDeleteUnusedClients`, `TestCleanup`. |
| 14 | Low | Reuse of an authorization code is logged, but tokens already issued from that code are not revoked (OAuth 2.1 recommends revoking). | Fixed: the code records the token family it was exchanged for (migration `0006`), and a second presentation revokes that family. Tests: `TestCodeReuseRevokesTokens`, `TestOAuthStoreConsumeCode`. |
| 15 | Low | For Entra ID, a missing `email_verified` counts as verified and `preferred_username` stands in for email. Any verified email links to the existing user with that email, admins included. | Fixed: an existing admin links by email only when `admin_emails` names the address; any other admin needs an identity linked by hand. A missing Entra `email_verified` still counts as verified, for the configured tenant issuer alone (the ID token's issuer is checked). The `preferred_username` stand-in is Entra-only (#36). Test: `TestResolveIdentity`. |
| 16 | Info | The site handler does not check reserved slugs itself. It relies on the host router (in `app`, not reviewed here) and on create-time validation, so an artifact created before its slug was added to `reserved_slugs` stays servable. | Left: add a `ReservedSlugs` field to `site.Options` and wire it from `app`? |
| 17 | Info | Staged upload files are handed to the publish as open files. A concurrent chunk to the same upload during the publish can grow the content past its recorded size. This only affects the owner's own upload, and the artifact caps still apply. | Fixed: each staged file is read through a section reader capped at its recorded size. Test: `TestFilesCapAtRecordedSize`. |
| 18 | Info | The admin-token principal uses the handle `admin`, which a real user can also have, so audit attribution is ambiguous. | Left: reserve the handle or use a distinct actor name. |

## Findings log (whole-repository review, 2026-10-24)

Scope: everything, including what the first review left out (`app`, `mcpserver`, `sqlite`, `blob`, `scheduler`, `metrics`, `certs`, `config`, `cli`, `present`, the deploy files and the workflows). Every code fix has a regression test; the server timeouts are constants without one.

| # | Severity | Finding | Status |
|---|---|---|---|
| 19 | High | Consent CSRF by cookie tossing. Artifact pages are same-site with `mcp.` and run member-authored script, so one could set `kazari_oauth=<id>; Domain=<base>` for a request the attacker started with their own client, drive the victim through the upstream sign-in (its `state` known to the attacker) and post the consent form. The double-submit check passed, and the code went to the attacker's redirect URI, giving them tokens for the victim's account. Needs OIDC and a victim signed in at the IdP. | Fixed: the cookie is `__Host-kazari_oauth` (path `/`), which no other host can set, and the consent post needs `Origin` equal to the MCP origin or `Sec-Fetch-Site: same-origin`. Tests: `TestRequestCookie`, `TestConsentRequiresSameOrigin`, `TestConsentIgnoresTossedCookie`. |
| 20 | High | Artifact `name`, `description` and version `label` had no limits beyond the 51 MiB body cap. A 40 MiB name made every listing that included the artifact run SQLite out of memory: the driver panicked, leaked a pool connection each time (eight requests froze every host) and, through MCP `list_artifacts`, ended the process. The row survived restarts. | Fixed: 256, 4096 and 256 bytes, valid UTF-8, no control or bidi control characters; chunk content types are checked on arrival. Tests: `TestValidateText`, `TestTextFieldLimits`, `TestAppendChunk`. |
| 21 | High | The fix for #5 counted only the headers the decoder saw. Long-name records (PAX `path`, GNU `L`, up to 1 MiB each, kept as names) and the data of skipped entry types were read inside `tar.Reader.Next` and never charged: a 140 KB request held 128 MiB of names, and a 51 MiB one could exhaust memory or force about 50 GiB of inflate work. | Fixed: a capping reader under `tar.NewReader` charges every decompressed byte; over-long names are refused at once. Test: `TestReadBundleCapsWholeStream`. |
| 22 | High | Unauthenticated memory exhaustion: the request method, which net/http accepts as any token, was a Prometheus label, so every new method made a series that was never freed (455 MB after 4000 requests, `/metrics` 240 MB). | Fixed: methods outside a fixed set are counted as `other`. Test: `TestHTTPMethodLabelBounded`. |
| 23 | Medium | Garbage collection could delete a blob that a concurrent publish had just found already stored and was about to reference, leaving a live version pointing at missing content. | Fixed: publishes hold a read lock from their first blob write to the commit; each GC batch holds the write lock. Test: `TestCollectGarbageSparesBlobOfPublishInProgress`. |
| 24 | Medium | Only the user API was rate-limited. MCP had no per-key write limit and no per-IP limit (20 of 20 creates accepted after REST answered 429), and the OAuth endpoints and the admin host were unthrottled. | Fixed: shared `internal/ratelimit`; one write budget per credential across REST and MCP; per-IP limits on `mcp.` and the admin `/v1/` routes. Tests: `TestWriteRateLimit`, `TestWriteBudgetShared`, `TestAdminIPRateLimit`, `TestMiddleware`. |
| 25 | Medium | A grant approved with an API key (no identity provider) kept no link to the key, and refreshes slid its expiry, so revoking or expiring the key never ended it. | Fixed: grants record the approving key (migration `0003`); tokens fail and refreshes are refused once the key is revoked, expired or its user disabled, and no token outlives the key. Tests: `TestGrantEndsWithApprovingKey`, `TestKeyStatus`, `TestOAuthStoreKeyPrefix`. |
| 26 | Medium | An empty `scope` meant every scope, `admin` included, so an admin approving any self-registered client gave it a token that `RequireAdmin` accepted on the admin host. | Fixed: the authorization server never offers or grants `admin`. Test: `TestAdminScopeNeverGranted`. |
| 27 | Medium | Setting `admin.listen` added a listener but left `admin.<base>`, `/metrics` and the token check on the public one, contrary to the operator checklist. The admin token's strength was never checked (the example shipped `change-me`) and guesses were not throttled. | Fixed: with `admin.listen` the public listener answers `admin.<base>` with 404; tokens under 32 characters stop startup (except under `localhost`); #24 throttles guesses. Tests: `TestAdminListener`, `TestAdminTokenLength`. |
| 28 | Low | No `ReadTimeout` or `WriteTimeout`: a client announcing a body it never sent, or reading slowly, held a connection and goroutine indefinitely. | Fixed: 10 min to read a request, 15 min to write a response; artifact hosts close a connection whose `GET` announces a body. Test: `TestAnnouncedBodyDoesNotHold`. |
| 29 | Low | The quota was checked before streaming only, so concurrent publishes could each pass it. | Fixed: checked again inside the transaction that adds the version. Tests: `TestQuotaHoldsAcrossConcurrentPublishes`, conformance `Quota`. |
| 30 | Low | The open-upload limit was counted and inserted in separate statements (20 open against a limit of 5). | Fixed: one transaction. Tests: `TestBeginConcurrentLimit`, `TestUploadStoreCreateLimit`. |
| 31 | Low | Unpublished (including hidden) and soft-deleted artifacts were hidden from listings but readable by any member through `GET .../files/...`, `GET /v1/artifacts/{slug}` and MCP. | Fixed: only admins, the owner and involved users can read them. Tests: `TestGetAndOpenFileHideOfflineArtifacts`, conformance `Involved`. |
| 32 | Low | A panic in an MCP tool or resource handler ended the process: the SDK runs handlers on goroutines of its own, outside any recovery. | Fixed: handlers recover into an `internal` error. Test: `TestToolPanicRecovered`. |
| 33 | Low | Disabling a user revoked their API keys but not their OAuth grants, so enabling them again revived the grants. | Fixed: disabling revokes the grants. Tests: `TestDisableRunsOnDisable`, `TestDisableRevokesGrants`. |
| 34 | Low | Version hosts were cached for a year, but a slug can pass to another owner after the 90-day tombstone, whose `<slug>--v1` could then be answered from caches of the old content. | Fixed: the version-host `max-age` is the tombstone period, at most a year. Test: `TestImmutableMaxAge`. |
| 35 | Low | The CLI printed names, descriptions, labels and metadata raw, so another member's text could send terminal escape sequences (clipboard writes, hidden text). | Fixed: tables replace control and bidi characters and flatten line breaks. Test: `TestRunGetTerminalSafe`. |
| 36 | Low | `preferred_username` stood in for a missing email for every OIDC issuer, not only Entra. | Fixed: Entra only. Test: `TestIdentityMapping`. |
| 37 | Low | `deploy/dev.yaml` listened on every interface with the published admin token `dev-admin-token`, so anyone on the network could administer a development server. | Fixed: it binds `127.0.0.1:8080`; `compose.yml` overrides the address inside the container. Test: `TestDevFile`. |
| 38 | Low | The workflows, including the release job with `contents`, `packages` and `id-token` write, pinned third-party actions by mutable tags; base images were pinned by tag. | Fixed: actions pinned to commit SHAs, base images to digests; the maintenance skill updates both. |
| 39 | Low | An OAuth principal's `KeyID` was the access token's row id, new on every refresh, so a client could reset its per-key write budget by refreshing. | Fixed: it names the grant (`oauth:<family>`). Test: `TestKeyIDStableAcrossRefresh`. |
| 40 | Info | Artifact hosts share a registrable domain with each other and the control plane: a page can set cookies for `<base>` (filling a visitor's jar past the header limit gets them `431` everywhere), and under a shared parent domain it can read and set that domain's cookies. | Documented: give `base_domain` a registrable domain of its own, or list it on the Public Suffix List. #19 removes the one exploitable use. |
| 41 | Info | Lost updates: a concurrent PATCH or PUT wrote back `owner_id`, `deleted_at` and `unpublished_at` as read before the change, undoing a transfer or soft delete. | Fixed: read-modify-write inside one transaction; the sweep re-checks a row before expiring it. Tests: `TestUpdateKeepsConcurrentChanges`, `TestSweepSkipsArtifactChangedSinceListing`, conformance `UpdateCurrentRow`. |
| 42 | Info | Temporary blob files and bundle spill files left by a crash were never removed. | Fixed: the `gc` job removes those older than an hour. Tests: `TestFSRemoveStaleTemp`, `TestRemoveStaleSpills`. |
| 43 | Info | Audit events named the handle only, not the credential that acted. | Fixed: artifact events carry `credential`; `oauth.grant` carries `key_prefix`. Tests: `TestAuditRecordsCredential`, `TestActorCredential`. |
| 44 | Info | The public `/health` and the apex page disclosed the exact build version. | Fixed: only the admin host reports it. Test: `TestEndToEnd`. |
| 45 | Low | The sweep's retention purge can still destroy an artifact restored between the sweep's listing and the purge. | Fixed: `Store.Purge` takes a check that runs on the current row in its transaction; the sweep skips a row restored or purged since the listing, and a hard delete checks the write policy there too. Tests: `TestSweepSparesArtifactRestoredSinceListing`, conformance `PurgeAndDeleteTombstone`. |
| 46 | Info | Under the `shared` policy a member can republish or restore an artifact they cannot read (#31). | Left: design decision. Should `canWrite` under `shared` also require involvement for offline artifacts? |
| 47 | Info | Refused authentication attempts (401, 403) are logged per request but not written to the audit log. | Left: the request log covers them; an audit entry per failure would need its own rate limit. |
