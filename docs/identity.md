# Identity and sign-in

Kazari has two kinds of credential:

- **API keys** (`kzk_...`), issued by an admin, sent as `Authorization: Bearer`. They work on every surface and suit the CLI, scripts and agents that take a key in their configuration.
- **OAuth access tokens**, which MCP clients (cloud agents' connectors, and agents on your machine configured without a key) obtain by sending the person through a sign-in flow. This page is about those.

## How sign-in works

Kazari is an OAuth 2.1 authorization server towards MCP clients and an OpenID Connect relying party towards one identity provider (IdP) per instance. It stores no passwords.

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

1. An MCP request without a token gets `401` with `WWW-Authenticate: Bearer error="invalid_token", resource_metadata="https://mcp.<base>/.well-known/oauth-protected-resource"`.
2. The client reads the protected-resource metadata, then `https://mcp.<base>/.well-known/oauth-authorization-server`. The issuer is `https://mcp.<base>`.
3. The browser goes to `/oauth/authorize` with PKCE (S256 only). Kazari checks the client, the redirect URI, the scopes and the `resource`, stores the request, sets a short-lived cookie (`__Host-kazari_oauth`, `HttpOnly`, `Secure`, `SameSite=Lax`, path `/`, ten minutes; plain `kazari_oauth` on a plain-HTTP development instance) and redirects to the IdP with its own `state`, `nonce` and PKCE pair. The `__Host-` prefix means no other host, such as an artifact page on `<slug>.<base>`, can set that cookie.
4. The person signs in at the IdP, which returns to **`https://mcp.<base>/oauth/callback`**: this is the redirect URI to register at the IdP.
5. Kazari redeems the code, verifies the ID token (signature against the IdP's keys, issuer, audience, expiry, nonce) and maps the person to a Kazari user by the [provisioning policy](#provisioning-policy).
6. A consent page names the client, the account and the scopes. **Approve** issues a single-use code (five minutes); **Deny** returns `access_denied` to the client. The form is accepted only from the consent page itself: a post whose `Origin` is another origin, or whose `Sec-Fetch-Site` is not `same-origin`, gets "Request rejected".
7. The client exchanges the code at `/oauth/token` and receives an access token and a refresh token.

The scopes granted are those the client asked for (all of them if it asked for none), capped at what the user's role allows: `artifacts:read`, `artifacts:write`, `artifacts:delete`. The `admin` scope is never offered (`scopes_supported` does not list it) or granted, even to admins, and asking for it gets `invalid_scope`: an OAuth token always acts as a member, so it never opens the admin host. Administer the instance with the admin token or an admin-scoped API key.

## Configuring a provider

Add an `auth.oidc` section ([configuration.md](configuration.md#authoidc)) and restart. Kazari discovers the provider at startup from `<issuer>/.well-known/openid-configuration`; if discovery fails, the server does not start and logs `oidc: discover <issuer>: ...`. It requests the scopes `openid email profile`.

```yaml
auth:
  oidc:
    issuer: https://login.microsoftonline.com/<tenant-id>/v2.0
    client_id: 00000000-0000-0000-0000-000000000000
    client_secret: ${KAZARI_AUTH_OIDC_CLIENT_SECRET}
    allowed_domains: [example.com]
    auto_provision: true
    admin_emails: [peter@example.com]
```

```sh
# /etc/kazari/kazari.env
KAZARI_AUTH_OIDC_CLIENT_SECRET=the-client-secret
```

The `issuer` must be exactly the `issuer` value the provider's discovery document states, including any trailing slash.

### Microsoft Entra ID

1. In the Entra admin centre, **Applications → App registrations → New registration**.
   - Name: `Kazari`.
   - Supported account types: **Accounts in this organizational directory only** (single tenant).
   - Redirect URI: platform **Web**, `https://mcp.art.example.com/oauth/callback`.
2. From **Overview**, note the **Application (client) ID** (`client_id`) and the **Directory (tenant) ID**. The issuer is `https://login.microsoftonline.com/<tenant-id>/v2.0`.
3. **Certificates & secrets → New client secret**. Copy the value (not the ID) into `KAZARI_AUTH_OIDC_CLIENT_SECRET`, and note its expiry: Kazari's sign-in stops working when the secret expires.
4. **Token configuration → Add optional claim → ID → `email`**, and accept the prompt to add the Microsoft Graph `email` permission. Without it Entra leaves `email` out of the ID token.
5. **API permissions** should list Microsoft Graph `openid`, `email` and `profile` (delegated). Grant admin consent if your tenant requires it.

The multi-tenant endpoints (`common`, `organizations`) are not supported: their discovery document names a per-tenant issuer template, so discovery fails the issuer check.

How Kazari reads Entra's ID token:

- If `email` is missing, `preferred_username` is used when it looks like an address (it is usually the user principal name, which may differ from the mail address). This applies to Entra issuers only; for every other provider `preferred_username` is ignored.
- Entra does not send `email_verified`; for issuers on `login.microsoftonline.com` the address counts as verified, since the tenant manages it.

### Google Workspace

1. In the Google Cloud console, **APIs & Services → OAuth consent screen**: user type **Internal**, which limits sign-in to accounts of your Workspace organisation.
2. **Credentials → Create credentials → OAuth client ID**, type **Web application**, authorised redirect URI `https://mcp.art.example.com/oauth/callback`.
3. Use issuer `https://accounts.google.com`, the client ID and the client secret.

Google sends `email_verified` and, for Workspace accounts, `hd` (the hosted domain). For Google issuers, Kazari treats an address as verified only when `hd` equals the address's domain, or the address is `@gmail.com` or `@googlemail.com`. A consumer Google account created on a work address therefore cannot pose as a Workspace user. Combine with `allowed_domains` to admit only your domain.

### Other providers

Any OpenID Connect provider with discovery works; register a confidential web client with the redirect URI above and the `openid email profile` scopes. Typical issuers:

| Provider  | Issuer                                                     |
| --------- | ---------------------------------------------------------- |
| Okta      | `https://<org>.okta.com/oauth2/default` (custom authorisation server) or `https://<org>.okta.com` |
| Keycloak  | `https://<host>/realms/<realm>`                            |
| Authentik | `https://<host>/application/o/<application-slug>/` (note the trailing slash) |
| Zitadel   | `https://<instance>.zitadel.cloud` or your own domain      |
| Auth0     | `https://<tenant>.<region>.auth0.com/` (note the trailing slash) |

Make sure the provider sends `email` and `email_verified` in the ID token (some send them only from the userinfo endpoint, which Kazari does not call). `email_verified` may be a boolean or the string `"true"`.

## Provisioning policy

After the ID token is verified, Kazari resolves the person to a user in this order:

1. **Known identity.** An identity already linked by `(issuer, subject)` signs in as its user.
2. **Link by verified email.** Otherwise, if the email is verified and its domain is in `allowed_domains` (or `allowed_domains` is empty), an existing user with that email gets this identity linked and signs in.
3. **Auto-provision.** Otherwise, if `auto_provision` is on and the verified email's domain is in `allowed_domains`, a new user is created: role `admin` if the email is in `admin_emails`, else `member`, with quota `limits.default_user_quota` and the IdP's `name` as display name.
4. **Refused.** Otherwise the person sees "No access ... Ask your administrator for access." and nothing is issued.

A disabled user is always refused at step 1 or 2. Every successful sign-in updates the identity's `last_login_at` and the email it last signed in with.

Consequences worth knowing:

- With `allowed_domains` empty nobody is auto-provisioned; only admin-created users with a matching email (or a linked identity) can sign in.
- With `auto_provision: false`, `allowed_domains` limits which admin-created users may link by email.
- `admin_emails` applies only when a user is first provisioned. To promote an existing user, use `kazari admin users update <handle> --role admin`.
- The match is on `(issuer, subject)` after the first sign-in, so a later change of email address at the IdP does not create a second user.

**Handles** for provisioned users come from the email's local part: lower-cased, characters other than `a-z`, `0-9`, `_` and `-` replaced by `-`, leading `-` and `_` and trailing `-` trimmed, cut to 32 characters; anything shorter than 2 characters becomes `user`. If the handle is taken a suffix is added: `jane.doe@example.com` becomes `jane-doe`, then `jane-doe-2`, `jane-doe-3`.

### Admin-created users

Create users ahead of time with their email, and they link on first sign-in by step 2:

```sh
kazari admin users create jdoe --email jane.doe@example.com
```

To pin a user to a specific IdP account regardless of email, link the identity directly with the admin API (there is no CLI command for this yet):

```sh
curl -X POST -H "Authorization: Bearer $KAZARI_ADMIN_TOKEN" -H 'Content-Type: application/json' \
  https://admin.art.example.com/v1/users/jdoe/identities \
  -d '{"issuer":"https://login.microsoftonline.com/<tenant-id>/v2.0","subject":"<sub claim>"}'
```

`POST /v1/users/{handle}/identities/unlink` with the same body removes it. `GET /v1/users/{handle}` lists a user's identities with their last sign-in.

## Without an identity provider: API-key sign-in

When `auth.oidc` is absent, `/oauth/authorize` skips the IdP and shows the consent page with an API key field. Approving with a valid key binds the grant to that key's user, capped at the key's scopes. The grant lives only as long as the key: once the key is revoked or expires, or its user is disabled, the grant's access and refresh tokens stop working, and no token outlives the key's expiry. A one-person or small instance can therefore connect a cloud agent with no IdP set up: issue yourself a key, add the connector, paste the key once. The consent is recorded in the audit log as `oauth.grant` with `via: api_key` and the key's `key_prefix`.

## MCP clients

**Client registration.** Kazari accepts both ways an MCP client identifies itself, and only public clients (no client secret, `token_endpoint_auth_method: none`):

- **Dynamic Client Registration** (RFC 7591): `POST https://mcp.<base>/oauth/register` with `redirect_uris` (and optionally `client_name`) returns a `client_id` starting `dcr_`.
- **Client ID Metadata Documents**: a `client_id` that is an `https` URL is fetched (10 s timeout, 64 KiB cap, no redirects, JSON only, public addresses only), must contain the same `client_id`, and is cached for an hour. The metadata advertises `client_id_metadata_document_supported: true`.

Redirect URIs must be `https`, or `http` on `localhost`, `127.0.0.1` or `::1`. They must match a registered URI exactly, except that for loopback URIs the port is ignored (RFC 8252), since native clients pick a free port each time.

**Cloud agents**: add a custom connector with the URL `https://mcp.<base>/`. A client that supports Client ID Metadata Documents identifies itself with one when the server advertises support (Kazari does), and others fall back to registration; the person signs in at your IdP and approves the consent page once per connection. claude.ai, for example, uses a metadata document.

**Agents on your machine**: an agent configured with the URL and no `Authorization` header runs the same flow itself when you first use the server, usually with a loopback redirect. With an `Authorization: Bearer kzk_...` header it uses the API key and skips OAuth. Claude Code, for example, signs in this way after `claude mcp add --transport http kazari https://mcp.<base>/`. See [mcp.md](mcp.md).

## Tokens, lifetimes and rotation

| Item                   | Lifetime   | Notes                                                              |
| ---------------------- | ---------- | ------------------------------------------------------------------ |
| Authorization request  | 10 minutes | The time a person has to sign in and approve                       |
| Authorization code     | 5 minutes  | Single use; a second use fails                                     |
| Access token           | 1 hour     | Opaque, stored as a SHA-256 hash                                   |
| Refresh token          | 30 days    | Rotated on every use                                               |

These lifetimes are fixed in this version. Each refresh returns a new refresh token and retires the old one; presenting a retired refresh token again revokes the whole chain (the client must sign in again), which is what limits the damage of a stolen token. A refresh may narrow the scopes but never widen them. Tokens are checked against the user on every request: a disabled user's tokens stop working, scopes beyond the user's current role are dropped, and a grant approved with an API key needs that key to be active.

The `oauth-cleanup` job deletes expired requests, codes and tokens hourly.

## Revoking access

- **One client's access for one user**: list and revoke the grant (each grant is a chain of refresh tokens; revoking it also revokes the access tokens from it).

  ```sh
  curl -H "Authorization: Bearer $KAZARI_ADMIN_TOKEN" https://admin.art.example.com/v1/users/jdoe/grants
  curl -X DELETE -H "Authorization: Bearer $KAZARI_ADMIN_TOKEN" https://admin.art.example.com/v1/grants/17
  ```

- **Everything for a user**: `kazari admin users update jdoe --disable` revokes their API keys and OAuth grants and refuses their sign-ins. Enabling the user again restores neither.
- **At the IdP**: removing the person from the IdP stops new sign-ins, but an existing refresh token stays valid in Kazari until it expires or is revoked; disable the user in Kazari too.

## Troubleshooting

**Kazari does not start: `oidc: discover <issuer>: ...`.** The issuer is unreachable from the server or its discovery document names a different issuer. Fetch `<issuer>/.well-known/openid-configuration` from the server and copy its `issuer` value exactly (trailing slash included). For Entra, use the tenant ID, not `common`.

**"Sign-in failed: The identity provider's response could not be verified."** The code exchange or ID token check failed; the log line `oauth: identity provider exchange` says why. Usual causes: a wrong or expired client secret, a redirect URI at the IdP that is not exactly `https://mcp.<base>/oauth/callback`, or a server clock out of sync (check NTP).

**"No access ... Ask your administrator for access."** The person signed in but the policy did not admit them. The log line `oauth: identity not admitted` records the issuer, subject and email. Check in order: is the email present at all (Entra: add the `email` optional claim, or make `preferred_username` an address)? Is it verified (Google: is the account a Workspace account of that domain)? Is its domain in `allowed_domains`? Is `auto_provision` on, or does an admin-created user have that email? Is the user disabled?

**"Sign-in expired."** The ten-minute window passed, or the browser did not send the `__Host-kazari_oauth` cookie back (blocked cookies; or a plain-HTTP instance where `external_scheme` is not set to `http`, so the cookie was marked `Secure`). Start again from the client.

**"Request rejected."** The consent form was posted from somewhere other than the consent page (a different origin, or `Sec-Fetch-Site` other than `same-origin`), or without the matching cookie. Start again from the client.

**"Unknown application."** The `client_id` is neither registered nor a fetchable metadata document; the log line `oauth: client metadata fetch failed` gives the reason.

**"Invalid redirect address."** The client asked to return to a URI it did not register.

**`invalid_grant` from the token endpoint.** The code was already used or expired, the PKCE verifier does not match, or the refresh token was rotated, revoked, expired or belongs to a disabled user. The client must start the flow again.
