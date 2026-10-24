# Security

This document is the threat model and security reference for Agalma v1. It
describes what Agalma protects, where the trust boundaries are, which limits
apply, what an operator must do, and what the pre-v1.0 review found.

## Threat model

### Assets

| Asset | Why it matters |
|---|---|
| Artifact content and history | Members publish work (internal reports, prototypes) that may not be meant for the open web. Versions are immutable and rollback depends on them. |
| Artifact integrity | A page served at `<slug>.<base>` is trusted by its readers as the owner's work. Nobody else should be able to change it unless the write policy allows that. |
| Credentials | API keys, OAuth access and refresh tokens, the admin token, the OIDC client secret. |
| The control plane | `api.`, `mcp.` and `admin.` hosts: anyone who can act there can change any artifact or user. |
| Instance availability | Disk (blobs, staging, spill files), memory, and the SQLite database. |

### Actors

- **Anonymous visitor.** Reads artifact pages. Can send any host header, path,
  query, method and header. Has no credential.
- **Authenticated member.** Holds an API key or OAuth token with some of
  `artifacts:read`, `artifacts:write`, `artifacts:delete`. May try to escape
  their scopes, act on other users' artifacts beyond the write policy, exceed
  limits or quota, or attack the server with what they upload.
- **Malicious bundle.** Content uploaded by a member: HTML and JavaScript that
  runs in readers' browsers, archives built to escape paths or exhaust
  resources, metadata built to inject headers.
- **Malicious OAuth client.** Anyone can register a client (DCR) or name a
  client ID metadata document URL. Such clients can send users through the
  authorization flow and make the server fetch URLs.
- **Administrator.** Trusted. Holds the admin token or an admin-scoped key.

### Trust boundaries

1. **Browser to artifact host.** Untrusted requests meet untrusted content.
   Everything served here is treated as hostile to everything else.
2. **Browser or client to control plane.** Every request must carry a bearer
   credential (or, for the OAuth pages, the consent cookie), and every action
   is authorised against scopes and the write policy.
3. **Upload boundary.** Bytes from members enter through inline JSON,
   multipart tar.gz or chunked staging. They are size-capped, path-validated
   and content-addressed before anything references them.
4. **Agalma to the identity provider and to CIMD URLs.** Outbound requests:
   OIDC tokens are verified cryptographically; metadata fetches are restricted
   to public addresses.
5. **Reverse proxy to Agalma** (proxy mode only). Agalma trusts the proxy's
   `X-Forwarded-For` / `X-Real-IP`.

## Origin isolation

Every artifact is served from its own host, `<slug>.<base>`, and every pinned
version from `<slug>--v<N>.<base>`. Each host is a separate web origin, so
the browser's same-origin policy keeps artifacts apart:

- Script in one artifact cannot read another artifact's DOM, storage,
  cookies, IndexedDB or service workers, and cannot make credentialed
  same-origin requests to it.
- Artifacts cannot reach the control plane as same-origin. Control-plane
  credentials are bearer headers, never cookies, so a page cannot ride on a
  visitor's session. The only cookie in the system is the OAuth consent
  cookie: host-only on `mcp.`, path `/oauth`, `HttpOnly; Secure;
  SameSite=Lax`, ten minutes. No cookie is ever set with a `Domain`
  attribute, so nothing is shared across subdomains.
- The site handler serves only `GET` and `HEAD` and never sets cookies. An
  artifact's own script can still set cookies for its own host, but those
  are confined to that origin.

Arbitrary HTML and JavaScript is the product, so Agalma does not try to make
uploaded content safe. It makes sure each artifact's content can only harm
its own origin.

Host handling (`internal/site/host.go`): the host is lower-cased and its port
and trailing dot stripped. It must end in `.<base>` with exactly one label in
front. The label splits on the last `--v` followed by digits (no leading
zeros, at most nine digits), and the slug must pass the slug rules. Anything
else, including the apex, deeper subdomains and IDNA labels (they contain
`--`), gets the instance 404 page. Reserved slugs (`api`, `mcp`, `admin`,
`www`, ...) cannot be created, and the host router sends the control-plane
hosts elsewhere before the site handler runs.

Path handling (`internal/site/site.go`): the decoded request path is refused
(404) if it contains a control character, a backslash or a `..` segment. That
includes the percent-encoded forms, because the check runs after decoding.
It is then cleaned and looked up in the version's file map, which is built
from validated paths only. No path is ever joined onto a filesystem path:
blobs are opened by content hash. Directory redirects are always relative to
the host (`/<clean>/`), so they cannot be turned into open redirects.

## Header policy

Every artifact response carries:

| Header | Value | Reason |
|---|---|---|
| `X-Content-Type-Options` | `nosniff` | The type is decided at upload (manifest, then extension, then a 512-byte sniff) and never from the request. |
| `Content-Security-Policy` | instance default, or the artifact's `agalma.csp` when `allow_csp_override` is on | See below. Instance error pages always use the default. |
| `Referrer-Policy` | `strict-origin-when-cross-origin` | Does not leak paths to third parties. |
| `X-Robots-Tag` | `noindex` unless `agalma.indexable = "true"`; always on errors | Self-hosted content is not indexed by default. |
| `Cache-Control` | `no-cache` on the latest host, `public, max-age=31536000, immutable` on version hosts, `no-cache` on every error | Versions never change. Latest revalidates cheaply against the strong ETag. |
| `ETag` | `"<sha256>"` | Content address. |

Control-plane hosts add `Cross-Origin-Opener-Policy: same-origin`, `nosniff`,
`Referrer-Policy: no-referrer` and `Cache-Control: no-store`. The OAuth pages
add `X-Frame-Options: DENY` and a CSP of `default-src 'none'; style-src
'unsafe-inline'; form-action 'self' <redirect origin>`. Raw file reads
through the user API (`GET /v1/artifacts/{slug}/files/...`) are sent with
`Content-Security-Policy: default-src 'none'; sandbox`, so artifact bytes can
never run as a document on the `api.` origin.

Header values that come from users are restricted before they are stored.
`agalma.csp` and per-file content types must be printable ASCII (no CR, LF,
NUL, tabs or non-ASCII bytes). Content types must also parse as a media type
and be at most 255 bytes.

### Default CSP

```
default-src 'self';
script-src 'self' 'unsafe-inline' https://cdnjs.cloudflare.com https://cdn.jsdelivr.net https://unpkg.com;
style-src 'self' 'unsafe-inline' https://fonts.googleapis.com;
font-src 'self' https://fonts.gstatic.com;
img-src 'self' data: https:;
connect-src 'self';
frame-ancestors 'none'
```

The reasoning:

- Origin isolation is the main control, not the CSP. The CSP limits what an
  artifact can pull in and send out, not what it can do to itself.
- `'unsafe-inline'` scripts and styles are allowed because generated
  artifacts are usually single-file HTML. The common public CDNs are allowed
  for the same reason.
- `connect-src 'self'` stops a page from sending data to a third party with
  `fetch`, XHR or WebSockets. This makes casual exfiltration of what a
  visitor types harder. It is not a hard guarantee: navigation and image
  beacons are still possible.
- `frame-ancestors 'none'` stops other sites from framing artifacts, which
  rules out clickjacking.
- `allow_csp_override` (on by default) lets an artifact replace the policy
  through `agalma.csp`. The override only affects that artifact's own origin,
  so it can relax the artifact's own protections but nobody else's.
  Operators who want a fixed policy set it to `false`.

## Upload trust boundary and limits

Content enters through three paths, and all of them end in
`artifact.Service`, which applies the same rules.

1. **Inline JSON** (`POST/PUT /v1/artifacts`, MCP tools): the body is capped
   with `http.MaxBytesReader`. UTF-8 or base64 file contents.
2. **Multipart tar.gz:** a `manifest` part (64 KiB cap) then a `bundle`
   part. The multipart stream is capped at `MaxBodyBytes`. The gzip and tar
   streams are decoded into one temporary spill file (never memory). The
   uncompressed total, file content plus 512 bytes per tar header, is capped
   at the same `MaxBodyBytes`, so a gzip bomb or a flood of empty entries
   stops at the cap. Only regular files are kept: directories, symlinks,
   hard links, devices and FIFOs are skipped and never followed. Leading
   `./` is stripped, and absolute and `..` names are rejected by path
   validation. The spill file is removed after the publish.
3. **Chunked staging** (`/v1/uploads`): an upload belongs to the user who
   began it; anyone else, and an expired upload, gets `upload_not_found`.
   Upload ids are 16 random bytes and are checked against their exact shape
   before they are used as a directory name, so they cannot escape the
   staging root. Staged files are stored under numeric names, never under
   their paths. Each chunk must start at the staged size; a retry of the last
   chunk is acknowledged. Expired uploads and orphaned directories are swept.

Path rules (`artifact.ValidatePath`): non-empty; at most 1024 bytes; segments
at most 255 bytes; relative; not a directory (no trailing `/`); no control
characters, DEL, backslash or invalid UTF-8; cleaned with `path.Clean`; no
`..` left after cleaning. Paths are compared byte-for-byte: no case folding or
Unicode normalisation, so `a.html` and `A.html`, or NFC and NFD forms, are
different files. Duplicate paths in one request are rejected after cleaning.

Slug rules: `^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`, 3 to 63 bytes, no `--`, not
reserved, immutable after create.

Metadata: at most 64 pairs (configurable), keys `^[a-zA-Z0-9_.-]{1,64}$`,
values at most 4096 bytes. `agalma.*` keys are restricted to `agalma.spa`
and `agalma.indexable` (`"true"`/`"false"`) and `agalma.csp` (printable
ASCII).

| Limit | Default | Where |
|---|---|---|
| Version size | 50 MiB (`limits.max_version_bytes`) | streamed through a counting reader |
| File size | 25 MiB (`limits.max_file_bytes`) | streamed through a counting reader |
| Files per version | 2000 (`limits.max_files`) | before any content is read |
| Owner quota | 2 GiB (`limits.default_user_quota`, per user) | streamed through a counting reader |
| JSON body with inline files, multipart stream, uncompressed bundle | version size + 1 MiB | `MaxBytesReader`, bundle decoder |
| Other JSON bodies | 1 MiB | `MaxBytesReader` |
| Multipart manifest | 64 KiB | |
| Chunk | 4 MiB (`limits.max_chunk_bytes`) | |
| Chunked upload total | 50 MiB | |
| Open uploads per user | 5 (`limits.max_open_uploads`) | |
| Upload lifetime | 1 h (`limits.upload_ttl`) | |
| Writes per key | 60 per minute (`limits.write_rate_per_minute`) | 429 with `Retry-After` |
| Requests per client IP on the user API | 120 per minute | before authentication; IPv6 grouped by /64 |
| Rate-limiter state | at most 65536 buckets per limiter | idle buckets swept |
| OAuth request bodies | 64 KiB | |
| Client ID metadata document | 64 KiB, 10 s, cached 1 h, no redirects, public addresses only | |

## Authentication and authorisation

**API keys.** Format `agk_<8-char prefix>_<52-char secret>`, lower-case base32
from 40 + 256 random bits. Only the SHA-256 of the whole key is stored, and
it is compared with `crypto/subtle`. The key is shown once at issue. Revoked
and expired keys, and keys of disabled users, are refused. Disabling a user
revokes all their keys. `last_used_at` is written at most once a minute.

**Scopes.** `artifacts:read`, `artifacts:write`, `artifacts:delete`, `admin`.
Every user-API route needs its scope: reads `read`; create, update, publish,
stale and uploads `write`; delete `delete`. A key or token's scopes are capped
at what the owner's current role allows, so a demoted admin loses `admin`
straight away. Admin power comes from the `admin` scope, not from the role
alone: a principal is an admin only if the user has the admin role and the
credential carries `admin`. An admin's key issued as read-only stays
read-only.

**Admin host.** `/v1/` on `admin.` accepts the configured admin token
(SHA-256 then constant-time compare) or a credential that resolves to an admin
principal (admin role and `admin` scope). Everything else gets 401 or 403.
`/health` and `/metrics` are unauthenticated: keep the admin listener private.

**Write policy.** `auth.write_policy: shared` (default) lets any member
with `artifacts:write` change any artifact. `"owner"` limits changes to the
owner and admins. Transfer is always owner- or admin-only. Listings show
public artifacts plus the caller's own; unlisted artifacts are hidden from
other people's listings but served to anyone who knows the URL.

**OAuth 2.1** (`mcp.` host): public clients only; PKCE S256 required (43-char
challenge, 43 to 128-char verifier, constant-time comparison). Redirect URIs
must be https, or http on a loopback host. They are matched exactly, except
that the port of a loopback URI is ignored (RFC 8252). `resource` must name
this server's MCP URL. Codes are single-use, consumed before they are
checked, and live five minutes. Access tokens live one hour, refresh tokens
thirty days, and all of them are stored hashed. Refresh tokens rotate; replay
of a rotated token revokes its whole family. The consent form is bound to the
consent cookie by comparing the request id in the form with the cookie value,
which stops cross-site form posts. The upstream `state` and `nonce` are
random per request and checked in constant time. Client ID metadata documents
are fetched only from public unicast addresses: loopback, RFC 1918, link-local,
CGNAT, NAT64/6to4, documentation, benchmarking and reserved ranges are
refused at dial time, after DNS resolution, so DNS rebinding cannot get
around the check.

**OIDC relying party.** ID tokens are verified against the provider's JWKS,
with issuer, audience (client id), expiry and nonce checks. The upstream code
exchange uses PKCE. Email-based linking and provisioning only trust a
verified email. For Google, a non-Gmail address counts as verified only when
the `hd` claim equals its domain, so a consumer Google account registered on
a company address (for example by someone who has since left) cannot sign in
as that domain. For Entra ID, a missing `email_verified` counts as verified
(single-tenant only; the multi-tenant `common` issuer is not supported).

## Out of scope for v1

- **Public-instance hardening.** v1 assumes the members are colleagues: any
  member can publish arbitrary HTML on the instance's domain, and the domain
  will be used for phishing if strangers can sign up. Do not run an
  instance with open sign-up (`auto_provision` without `allowed_domains`).
  There is no malware scanning, abuse reporting flow or per-artifact
  takedown other than the admin hide endpoint.
- **Gated visibility.** `public` and `unlisted` are both served to anyone
  with the URL. There is no per-artifact access control on artifact hosts in
  v1. Do not publish secrets.
- **Isolation between artifacts on the same instance beyond the browser's
  origin model.** For example, a CSP override that allows framing or third
  parties is the artifact owner's choice.
- **Protecting artifacts from admins.** Admins can read, transfer and delete
  everything.
- Enterprise managed auth (RFC 7523 JWT bearer grant) is a Phase 4 item.

## Operator checklist

1. **Keep the admin host on its own listener** (`admin.listen`, for example
   `127.0.0.1:9443`), or reach it only through a VPN. `/metrics` and
   `/health` are unauthenticated.
2. **Rotate the admin token out.** Use it to create the first admin user,
   issue that user a key with the `admin` scope, check that the key works on
   the admin host, then remove `auth.admin_token` / `AGALMA_AUTH_ADMIN_TOKEN`
   and restart. Pass secrets through the environment: `${NAME}`
   references in the YAML file, values in `agalma.env` or `.env`.
3. **Proxy mode.** Turn on `listen.proxy_mode` only behind exactly one
   reverse proxy, and make sure nothing can reach Agalma's plain-HTTP port
   except that proxy. Agalma takes the rightmost `X-Forwarded-For` entry,
   the one your proxy added, then `X-Real-IP`. A proxy that appends
   (nginx's `$proxy_add_x_forwarded_for`) or overwrites (Caddy) both work.
   With two proxies in a chain (CDN, then proxy), the per-IP limit applies
   to the CDN's address.
4. **Identity provider.** Set `allowed_domains`. Use `auto_provision` only
   with it. For Entra ID use a single-tenant app. Keep `admin_emails` short:
   people in it become admins on first sign-in.
5. **Write policy.** Choose `owner` if members should not be able to change
   each other's artifacts.
6. **CSP override.** Set `serve.allow_csp_override: false` if you need the
   instance policy to hold for every artifact.
7. **Backups.** Back up `data_dir`: `agalma.db` (use SQLite's online backup
   or stop the service) and `blobs/`. `certmagic/` holds certificate keys,
   and the database holds key and token hashes and user emails, so treat
   backups as secrets. `uploads/` is transient and does not need backing up.
8. **Updates.** Run the latest release. Releases are built with `-trimpath`
   and `CGO_ENABLED=0`.

## Reporting a vulnerability

Please report vulnerabilities privately, not in a public issue. Use the
repository's private vulnerability reporting (the **Security** tab, then
**Report a vulnerability**). Include the version (`agalma version`), the
configuration that matters, and steps to reproduce. You should get an
answer within a week. Please give us a reasonable window to ship a fix
before you disclose.

## Findings log (pre-v1.0 review, 2026-10-24)

Scope: `internal/site`, `artifact`, `httpapi`, `upload`, `auth`, `user`,
`oauth`, `oidc`, `middleware`. Every fix has a regression test.

| # | Severity | Finding | Status |
|---|---|---|---|
| 1 | High | An admin user's key or OAuth token counted as admin whatever its scopes. `Principal.Admin` followed the role alone and `HasScope` gives admins every scope, so a key issued read-only (or with the default member scopes) could write, delete and use the admin host. | Fixed: `Admin` needs the admin role and the `admin` scope (`user.Service.Authenticate`, `oauth.Service.Authenticate`), and `RequireAdmin` checks the scope as well. Tests: `TestAuthenticateAdminKeyWithoutAdminScope`, `TestRequireAdmin`, OAuth "narrow request". |
| 2 | Medium | Google `hd` claim was not checked, so a consumer Google account on a company address passed as verified for that domain (domain allow-list, email linking, auto-provisioning). | Fixed in `oidc` identity mapping. Test: `TestIdentityMapping`. |
| 3 | Medium | In proxy mode, `RealIP` trusted the leftmost `X-Forwarded-For` entry, which is client-controlled behind appending proxies. Any client could pick its own IP to dodge per-IP limits and poison logs. | Fixed: rightmost entry. Test: `TestRealIP`. |
| 4 | Medium | The rate-limiter map had no hard cap, and once past 1024 keys every new key scanned the whole map under the lock. Rotating addresses (trivial within an IPv6 /64) grew memory and CPU without bound. | Fixed: hard cap of 65536, idle sweep at most once a minute, IPv6 bucketed by /64. Tests: `TestLimiterBounded`, `TestIPBucket`. |
| 5 | Medium | The bundle decoder counted only file content against the uncompressed cap. A small gzip of millions of empty (or skipped link) entries decoded without bound and kept a `FileInput` per regular file in memory. | Fixed: each tar header costs 512 bytes against the cap. Test: `TestReadBundleEntryCap`. |
| 6 | Medium | The CIMD fetch guard allowed CGNAT (`100.64.0.0/10`, used by Tailscale), `0.0.0.0/8`, NAT64/6to4 and other special ranges, so server-side requests could reach internal hosts. | Fixed: public unicast only. Test: `TestPublicAddr`. |
| 7 | Low | Per-file content types (manifest, JSON, chunk `Content-Type`) were stored unvalidated and sent back as the `Content-Type` header. Go folds CR/LF, but NUL, non-ASCII and oversized values reached proxies. | Fixed: printable ASCII, at most 255 bytes, must parse. Test: `TestContentTypeValidation`. |
| 8 | Low | `agalma.csp` rejected only CR/LF. Other control characters and non-ASCII went into the header. | Fixed: printable ASCII only. Test: `TestValidateMetadataCSP`. |
| 9 | Low | `GET /v1/artifacts/{slug}/files/...` served artifact HTML/SVG inline, unsandboxed, on the `api.` origin. Bearer auth makes this hard to exploit from a browser, but it is the control-plane origin. | Fixed: `Content-Security-Policy: default-src 'none'; sandbox`. Test: `TestCreateMultipart` header table. |
| 10 | Info | The site path check let backslashes through. No stored path can contain one, but browsers read `\` as `/` in `Location`. | Fixed as defence in depth: backslash paths are refused. Test: `TestPathRules`. |
| 11 | Medium | Quota can be dodged. Under the shared policy, a version added by a non-owner is charged to nobody (`artifact/service.go` `Update`). An owner can also transfer an artifact to any user without their consent (`Transfer`), moving usage onto that user's quota and then growing it without limit. | Left: design decision. Should a version be charged to the owner's quota (needs the owner's quota at update time), and should non-admin transfers need the recipient's acceptance or be admin-only? |
| 12 | Low | Before consent, authorization errors redirect to any redirect URI the client registered. DCR and CIMD are open to anyone, so `mcp.<base>` can be used as an open redirect (RFC 9700 section 4.11.2). | Left: changing this affects client interop. Should errors for unverified clients render a page instead of redirecting? |
| 13 | Low | Dynamic client registration is unauthenticated and unbounded: every POST adds a row. | Left: needs the control-plane per-IP limit on `mcp.` (wired in `app`), and possibly expiry of DCR clients that never complete a grant. |
| 14 | Low | Reuse of an authorization code is logged, but tokens already issued from that code are not revoked (OAuth 2.1 recommends revoking). | Left: needs the store to link a code to its token family. |
| 15 | Low | For Entra ID, a missing `email_verified` counts as verified and `preferred_username` stands in for email. Any verified email links to the existing user with that email, admins included. | Left: design decision. Require admins to have explicitly linked identities, or use Entra's `xms_edov` claim? |
| 16 | Info | The site handler does not check reserved slugs itself. It relies on the host router (in `app`, not reviewed here) and on create-time validation, so an artifact created before its slug was added to `reserved_slugs` stays servable. | Left: add a `ReservedSlugs` field to `site.Options` and wire it from `app`? |
| 17 | Info | Staged upload files are handed to the publish as open files. A concurrent chunk to the same upload during the publish can grow the content past its recorded size. This only affects the owner's own upload, and the artifact caps still apply. | Left: wrap each staged file in a reader capped at its recorded size if exact staging semantics matter. |
| 18 | Info | The admin-token principal uses the handle `admin`, which a real user can also have, so audit attribution is ambiguous. | Left: reserve the handle or use a distinct actor name. |
