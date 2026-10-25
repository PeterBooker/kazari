# ADR 0003: Gated visibility with a per-host sign-in handshake and share links

Status: accepted (2026-10-25)

## Context

`public` and `unlisted` artifacts are served to anyone with the URL, and slugs are chosen by people, so they are easy to guess. Teams publish internal reports and prototypes, and they need pages that only colleagues can open, plus a way to show one to an outsider. A research note compared how Cloudflare Access, Vercel, Netlify, GitHub Pages, Google Drive, Figma and others gate shared URLs, and recommended a design. Its constraints come from Kazari itself:

- Every artifact and every pinned version is its own origin (`<slug>.<base>`, `<slug>--v<N>.<base>`), and every one runs member-written script.
- The security guide recommends putting `<base>` on the Public Suffix List, after which browsers refuse a `Domain=<base>` cookie. Without that, any artifact can set, shadow or push out such a cookie (finding #19).
- Sibling artifact hosts are same-site with each other and with `mcp.` unless `<base>` is on the list, so `SameSite=Lax` does not keep requests between them apart.

## Decision

- A third visibility, **`members`**: served only to people who sign in to the instance and to holders of a share link, on the latest host and every version host; listed like `public`. "Members" is exactly the set the existing provisioning policy admits (`user.Service.ResolveIdentity`). There is no second admission rule.
- **A per-host handshake through the sign-in host.** A navigation without access sets a nonce cookie on the artifact host and goes to `mcp.<base>/oauth/signin`. That host keeps a 12-hour browser session (`__Host-kazari_session`), signs the person in at the identity provider when it has none (sharing the `/oauth/callback` that is already registered, told apart by the state's `v.` prefix), and returns a code that is single use, lasts one minute, and is bound to one host and to the S256 of the nonce. The artifact host redeems it in-process for a view session (`__Host-kazari_view`, `HttpOnly`, host-only) that ends with the sign-in session. Without an identity provider, the sign-in page takes an API key with `artifacts:read`, as the consent page does.
- **Share links**: `kzs_<prefix>_<secret>`, stored hashed, multi-use, expiring (7 days by default, at most `serve.share_link_max_ttl`), revocable, tied to the artifact row so a reused slug never inherits them. Opening one stores the token as the view cookie and redirects to a clean URL. REST endpoints, three MCP tools (`create_share_link`, `list_share_links`, `revoke_share_link`) and `kazari share` manage them.
- `/.kazari/` is reserved on every artifact host (callback, share, sign-out), and no artifact may store a file under it.
- Gated responses are `Cache-Control: private, no-cache` on every host and `Cross-Origin-Resource-Policy: same-origin`. Pinned versions of a gated artifact give up `immutable`: a browser would otherwise show a version it cached after the viewer signed out or the link was revoked, without asking the server. Non-navigations without access get `401`, not a redirect.
- `serve.default_visibility` lets an internal instance make `members` the default.

## Where this departs from the research

- **No separate "viewer" admission.** The research proposed admitting anyone whose email passes `allowed_domains` without provisioning a user. With `allowed_domains` empty, that admits any verified address the provider vouches for, which is wider than sign-in is today. A view sign-in may now provision an account, but only where connecting an agent would already have done so.
- **No bearer tokens on artifact hosts.** Headless browsers send extra headers to every request a page makes, including to the CDNs the default CSP allows, so a key would leak to third parties. Agents read through MCP and REST; headless browsers use a share link.
- **No `Clear-Site-Data: "storage"` on revocation.** It would also fire on routine session expiry and wipe a page's own saved state for legitimate users, and it cannot reach a page whose service worker answers from cache. Revocation stops new responses; the security guide says so.
- **`Cross-Origin-Resource-Policy: same-origin`, which the research missed.** Same-site siblings get the viewer's `Lax` cookie when they include a gated script or JSON file with `<script src>`; without CORP that is a cross-site script inclusion leak.
- **The migration needed a runner change.** SQLite cannot widen a `CHECK` constraint in place, migrations run inside a transaction where `PRAGMA foreign_keys` is ignored, and dropping `artifact` with foreign keys on deletes every version. Migrations that start with `-- kazari:foreign-keys-off` now run on their own connection with foreign keys off, and commit only after `PRAGMA foreign_key_check`.
- **Unknown slugs stay `404`.** Redirecting them to sign-in as well would hide which gated slugs exist, but would send every mistyped address to a login page. The guide says a gated slug's existence shows.

## Not now

Per-person and per-group lists (`restricted`), an instance policy of allowed visibilities, and a mode that trusts an identity header (Cloudflare Access, Pomerium) from a proxy in front. Each fits on the same check: a list is another condition on the view session, and a proxy identity would be another way to open one.

## Consequences

- One more database lookup per request for `members` pages (the view session with its sign-in session and user), none for public ones. Version hosts of `members` pages are revalidated on every load instead of cached for months.
- One redirect round trip per newly opened host per browser session; the identity provider is visited only once in 12 hours.
- The MCP host now sets a session cookie. It is `__Host-`, `HttpOnly`, and read only by the sign-in endpoint, never by the MCP, OAuth token or API endpoints.
- Copies that a CDN took while a page was public are not recalled when it becomes `members`; operators purge them.
