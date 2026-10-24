# Identity and sign-in

Agalma has two kinds of credential:

- **API keys** (`agk_...`), issued by an admin, sent as `Authorization: Bearer`. They work on every
  surface and suit the CLI, scripts and Claude Code.
- **OAuth access tokens**, which MCP clients such as claude.ai and Claude Code obtain by sending the
  person through a sign-in flow. This page is about those.

## How sign-in works

Agalma is an OAuth 2.1 authorization server towards MCP clients and an OpenID Connect relying party
towards one identity provider (IdP) per instance. It stores no passwords.

```text
MCP client ──1──▶ mcp.<base>/            401 + resource_metadata
           ──2──▶ /.well-known/...        discovers the authorization server
           ──3──▶ /oauth/authorize        (browser) validated, stored for 10 minutes
                       │
                       └──4──▶ your IdP   person signs in (often silently, via the IdP's SSO session)
                                 │
           ◀──────────5──────────┘ /oauth/callback  ID token verified, person mapped to a user
           consent page "Approve" ──6──▶ code ──7──▶ /oauth/token ──▶ access + refresh token
```

1. An MCP request without a token gets `401` with
   `WWW-Authenticate: Bearer error="invalid_token", resource_metadata="https://mcp.<base>/.well-known/oauth-protected-resource"`.
2. The client reads the protected-resource metadata, then
   `https://mcp.<base>/.well-known/oauth-authorization-server`. The issuer is `https://mcp.<base>`.
3. The browser goes to `/oauth/authorize` with PKCE (S256 only). Agalma checks the client, the
   redirect URI, the scopes and the `resource`, stores the request, sets a short-lived cookie
   (`agalma_oauth`, `HttpOnly`, `Secure`, `SameSite=Lax`, path `/oauth`, ten minutes) and redirects
   to the IdP with its own `state`, `nonce` and PKCE pair.
4. The person signs in at the IdP, which returns to **`https://mcp.<base>/oauth/callback`**: this is
   the redirect URI to register at the IdP.
5. Agalma redeems the code, verifies the ID token (signature against the IdP's keys, issuer,
   audience, expiry, nonce) and maps the person to an Agalma user by the
   [provisioning policy](#provisioning-policy).
6. A consent page names the client, the account and the scopes. **Approve** issues a single-use
   code (five minutes); **Deny** returns `access_denied` to the client.
7. The client exchanges the code at `/oauth/token` and receives an access token and a refresh token.

The scopes granted are those the client asked for (all of them if it asked for none), capped at what
the user's role allows: members get `artifacts:read`, `artifacts:write`, `artifacts:delete`; admins
also get `admin`. Admin powers come only with the `admin` scope, so a client that asked for less
acts as a member.

## Configuring a provider

Add an `[auth.oidc]` table ([configuration.md](configuration.md#authoidc)) and restart. Agalma
discovers the provider at startup from `<issuer>/.well-known/openid-configuration`; if discovery
fails, the server does not start and logs `oidc: discover <issuer>: ...`. It requests the scopes
`openid email profile`.

```toml
[auth.oidc]
issuer          = "https://login.microsoftonline.com/<tenant-id>/v2.0"
client_id       = "00000000-0000-0000-0000-000000000000"
allowed_domains = ["example.com"]
auto_provision  = true
admin_emails    = ["peter@example.com"]
```

```sh
# /etc/agalma/agalma.env
AGALMA_AUTH_OIDC_CLIENT_SECRET=the-client-secret
```

The `issuer` must be exactly the `issuer` value the provider's discovery document states, including
any trailing slash.

### Microsoft Entra ID

1. In the Entra admin centre, **Applications → App registrations → New registration**.
   - Name: `Agalma`.
   - Supported account types: **Accounts in this organizational directory only** (single tenant).
   - Redirect URI: platform **Web**, `https://mcp.art.example.com/oauth/callback`.
2. From **Overview**, note the **Application (client) ID** (`client_id`) and the **Directory
   (tenant) ID**. The issuer is `https://login.microsoftonline.com/<tenant-id>/v2.0`.
3. **Certificates & secrets → New client secret**. Copy the value (not the ID) into
   `AGALMA_AUTH_OIDC_CLIENT_SECRET`, and note its expiry: Agalma's sign-in stops working when the
   secret expires.
4. **Token configuration → Add optional claim → ID → `email`**, and accept the prompt to add the
   Microsoft Graph `email` permission. Without it Entra leaves `email` out of the ID token.
5. **API permissions** should list Microsoft Graph `openid`, `email` and `profile` (delegated).
   Grant admin consent if your tenant requires it.

The multi-tenant endpoints (`common`, `organizations`) are not supported: their discovery document
names a per-tenant issuer template, so discovery fails the issuer check.

How Agalma reads Entra's ID token:

- If `email` is missing, `preferred_username` is used when it looks like an address (it is usually
  the user principal name, which may differ from the mail address).
- Entra does not send `email_verified`; for issuers on `login.microsoftonline.com` the address
  counts as verified, since the tenant manages it.

### Google Workspace

1. In the Google Cloud console, **APIs & Services → OAuth consent screen**: user type **Internal**,
   which limits sign-in to accounts of your Workspace organisation.
2. **Credentials → Create credentials → OAuth client ID**, type **Web application**, authorised
   redirect URI `https://mcp.art.example.com/oauth/callback`.
3. Use issuer `https://accounts.google.com`, the client ID and the client secret.

Google sends `email_verified` and, for Workspace accounts, `hd` (the hosted domain). For Google
issuers, Agalma treats an address as verified only when `hd` equals the address's domain, or the
address is `@gmail.com` or `@googlemail.com`. A consumer Google account created on a work address
therefore cannot pose as a Workspace user. Combine with `allowed_domains` to admit only your domain.

### Other providers

Any OpenID Connect provider with discovery works; register a confidential web client with the
redirect URI above and the `openid email profile` scopes. Typical issuers:

| Provider  | Issuer                                                     |
| --------- | ---------------------------------------------------------- |
| Okta      | `https://<org>.okta.com/oauth2/default` (custom authorisation server) or `https://<org>.okta.com` |
| Keycloak  | `https://<host>/realms/<realm>`                            |
| Authentik | `https://<host>/application/o/<application-slug>/` (note the trailing slash) |
| Zitadel   | `https://<instance>.zitadel.cloud` or your own domain      |
| Auth0     | `https://<tenant>.<region>.auth0.com/` (note the trailing slash) |

Make sure the provider sends `email` and `email_verified` in the ID token (some send them only from
the userinfo endpoint, which Agalma does not call). `email_verified` may be a boolean or the string
`"true"`.

## Provisioning policy

After the ID token is verified, Agalma resolves the person to a user in this order:

1. **Known identity.** An identity already linked by `(issuer, subject)` signs in as its user.
2. **Link by verified email.** Otherwise, if the email is verified and its domain is in
   `allowed_domains` (or `allowed_domains` is empty), an existing user with that email gets this
   identity linked and signs in.
3. **Auto-provision.** Otherwise, if `auto_provision` is on and the verified email's domain is in
   `allowed_domains`, a new user is created: role `admin` if the email is in `admin_emails`, else
   `member`, with quota `limits.default_user_quota` and the IdP's `name` as display name.
4. **Refused.** Otherwise the person sees "No access ... Ask your administrator for access." and
   nothing is issued.

A disabled user is always refused at step 1 or 2. Every successful sign-in updates the identity's
`last_login_at` and the email it last signed in with.

Consequences worth knowing:

- With `allowed_domains` empty nobody is auto-provisioned; only admin-created users with a matching
  email (or a linked identity) can sign in.
- With `auto_provision = false`, `allowed_domains` limits which admin-created users may link by email.
- `admin_emails` applies only when a user is first provisioned. To promote an existing user, use
  `agalma admin users update <handle> --role admin`.
- The match is on `(issuer, subject)` after the first sign-in, so a later change of email address at
  the IdP does not create a second user.

**Handles** for provisioned users come from the email's local part: lower-cased, characters other
than `a-z`, `0-9`, `_` and `-` replaced by `-`, leading `-` and `_` and trailing `-` trimmed, cut
to 32 characters; anything shorter than 2 characters becomes `user`. If the handle is taken a suffix
is added: `jane.doe@example.com` becomes `jane-doe`, then `jane-doe-2`, `jane-doe-3`.

### Admin-created users

Create users ahead of time with their email, and they link on first sign-in by step 2:

```sh
agalma admin users create jdoe --email jane.doe@example.com
```

To pin a user to a specific IdP account regardless of email, link the identity directly with the
admin API (there is no CLI command for this yet):

```sh
curl -X POST -H "Authorization: Bearer $AGALMA_ADMIN_TOKEN" -H 'Content-Type: application/json' \
  https://admin.art.example.com/v1/users/jdoe/identities \
  -d '{"issuer":"https://login.microsoftonline.com/<tenant-id>/v2.0","subject":"<sub claim>"}'
```

`POST /v1/users/{handle}/identities/unlink` with the same body removes it. `GET /v1/users/{handle}`
lists a user's identities with their last sign-in.

## Without an identity provider: API-key sign-in

When `[auth.oidc]` is absent, `/oauth/authorize` skips the IdP and shows the consent page with an API
key field. Approving with a valid key binds the grant to that key's user, capped at the key's scopes.
A one-person or small instance can therefore connect claude.ai with no IdP set up: issue yourself a
key, add the connector, paste the key once. The consent is recorded in the audit log as
`oauth.grant` with `via: api_key`.

## MCP clients

**Client registration.** Agalma accepts both ways an MCP client identifies itself, and only public
clients (no client secret, `token_endpoint_auth_method: none`):

- **Dynamic Client Registration** (RFC 7591): `POST https://mcp.<base>/oauth/register` with
  `redirect_uris` (and optionally `client_name`) returns a `client_id` starting `dcr_`.
- **Client ID Metadata Documents**: a `client_id` that is an `https` URL is fetched (10 s timeout,
  64 KiB cap, no redirects, JSON only, public addresses only), must contain the same `client_id`, and
  is cached for an hour. The metadata advertises `client_id_metadata_document_supported: true`.

Redirect URIs must be `https`, or `http` on `localhost`, `127.0.0.1` or `::1`. They must match a
registered URI exactly, except that for loopback URIs the port is ignored (RFC 8252), since native
clients pick a free port each time.

**claude.ai**: add a custom connector with the URL `https://mcp.<base>/`. claude.ai identifies itself
with a Client ID Metadata Document when the server advertises support (Agalma does), and falls back
to registration otherwise; the person signs in at your IdP and approves the consent page once per
connection.

**Claude Code**: `claude mcp add --transport http agalma https://mcp.<base>/` without a header makes
Claude Code run the same flow itself, with a loopback redirect, when you first use the server (or
via `/mcp`). With `--header "Authorization: Bearer agk_..."` it uses the API key and skips OAuth.
See [mcp.md](mcp.md).

## Tokens, lifetimes and rotation

| Item                   | Lifetime   | Notes                                                              |
| ---------------------- | ---------- | ------------------------------------------------------------------ |
| Authorization request  | 10 minutes | The time a person has to sign in and approve                       |
| Authorization code     | 5 minutes  | Single use; a second use fails                                     |
| Access token           | 1 hour     | Opaque, stored as a SHA-256 hash                                   |
| Refresh token          | 30 days    | Rotated on every use                                               |

These lifetimes are fixed in this version. Each refresh returns a new refresh token and retires the
old one; presenting a retired refresh token again revokes the whole chain (the client must sign in
again), which is what limits the damage of a stolen token. A refresh may narrow the scopes but never
widen them. Tokens are checked against the user on every request: a disabled user's tokens stop
working, and scopes beyond the user's current role are dropped.

The `oauth-cleanup` job deletes expired requests, codes and tokens hourly.

## Revoking access

- **One client's access for one user**: list and revoke the grant (each grant is a chain of refresh
  tokens; revoking it also revokes the access tokens from it).

  ```sh
  curl -H "Authorization: Bearer $AGALMA_ADMIN_TOKEN" https://admin.art.example.com/v1/users/jdoe/grants
  curl -X DELETE -H "Authorization: Bearer $AGALMA_ADMIN_TOKEN" https://admin.art.example.com/v1/grants/17
  ```

- **Everything for a user**: `agalma admin users update jdoe --disable` revokes their API keys,
  refuses their sign-ins and makes their OAuth tokens fail while disabled.
- **At the IdP**: removing the person from the IdP stops new sign-ins, but an existing refresh token
  stays valid in Agalma until it expires or is revoked; disable the user in Agalma too.

## Troubleshooting

**Agalma does not start: `oidc: discover <issuer>: ...`.** The issuer is unreachable from the server
or its discovery document names a different issuer. Fetch
`<issuer>/.well-known/openid-configuration` from the server and copy its `issuer` value exactly
(trailing slash included). For Entra, use the tenant ID, not `common`.

**"Sign-in failed: The identity provider's response could not be verified."** The code exchange or
ID token check failed; the log line `oauth: identity provider exchange` says why. Usual causes: a
wrong or expired client secret, a redirect URI at the IdP that is not exactly
`https://mcp.<base>/oauth/callback`, or a server clock out of sync (check NTP).

**"No access ... Ask your administrator for access."** The person signed in but the policy did not
admit them. The log line `oauth: identity not admitted` records the issuer, subject and email. Check
in order: is the email present at all (Entra: add the `email` optional claim, or make
`preferred_username` an address)? Is it verified (Google: is the account a Workspace account of that
domain)? Is its domain in `allowed_domains`? Is `auto_provision` on, or does an admin-created user
have that email? Is the user disabled?

**"Sign-in expired."** The ten-minute window passed, or the browser did not send the `agalma_oauth`
cookie back (blocked cookies; or a plain-HTTP instance where `external_scheme` is not set to `http`,
so the cookie was marked `Secure`). Start again from the client.

**"Unknown application."** The `client_id` is neither registered nor a fetchable metadata document;
the log line `oauth: client metadata fetch failed` gives the reason.

**"Invalid redirect address."** The client asked to return to a URI it did not register.

**`invalid_grant` from the token endpoint.** The code was already used or expired, the PKCE verifier
does not match, or the refresh token was rotated, revoked, expired or belongs to a disabled user.
The client must start the flow again.
