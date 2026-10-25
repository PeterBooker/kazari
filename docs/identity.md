# Identity and sign-in

Kazari stores no passwords. It accepts two kinds of credential: API keys, which an admin issues, and OAuth access tokens, which MCP clients obtain by sending a person through a sign-in flow. People who open a `members` page in a browser sign in through the same identity provider and the same callback.

This page explains how sign-in works, connects an identity provider, sets who is let in, and covers token lifetimes, revoking access and troubleshooting. It assumes a running instance ([installation guide](install.md)).

## How sign-in works

Kazari is an OAuth 2.1 authorization server towards MCP clients and an OpenID Connect relying party towards one identity provider (IdP) per instance.

### Credentials

Kazari has two kinds of credential:

- **API keys** (`kzk_...`) are issued by an admin and sent as `Authorization: Bearer`. They work on every surface and suit the CLI, scripts and agents that take a key in their configuration.
- **OAuth access tokens** are what MCP clients obtain by sending the person through a sign-in flow. This covers cloud agents' connectors, and agents on your machine configured without a key. Most of this page is about these.

### The OAuth flow

An MCP client without a token goes through these steps:

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
3. The browser goes to `/oauth/authorize` with PKCE (S256 only). Kazari checks the client, the redirect URI, the scopes and the `resource`, and stores the request. It sets a short-lived cookie and redirects to the IdP with its own `state`, `nonce` and PKCE pair.
4. The person signs in at the IdP, which returns to **`https://mcp.<base>/oauth/callback`**. This is the redirect URI to register at the IdP.
5. Kazari redeems the code and verifies the ID token: signature against the IdP's keys, issuer, audience, expiry and nonce. It then maps the person to a Kazari user by the [provisioning policy](#provisioning-policy).
6. A consent page names the client, the account and the scopes. **Approve** issues a single-use code that lasts five minutes; **Deny** returns `access_denied` to the client.
7. The client exchanges the code at `/oauth/token` and receives an access token and a refresh token.

#### The sign-in cookie

The cookie set at step 3 is `__Host-kazari_oauth`: `HttpOnly`, `Secure`, `SameSite=Lax`, path `/`, for ten minutes. On a plain-HTTP development instance it is plain `kazari_oauth`. The `__Host-` prefix means no other host, such as an artifact page on `<slug>.<base>`, can set that cookie.

#### The consent form

Kazari accepts the consent form only from the consent page itself. A post whose `Origin` is another origin, or whose `Sec-Fetch-Site` is not `same-origin`, gets "Request rejected".

### Scopes

The scopes granted are those the client asked for, capped at what the user's role allows: `artifacts:read`, `artifacts:write` and `artifacts:delete`. A client that asks for none gets all of them.

An OAuth token always acts as a member, so it never opens the admin host. Kazari never offers the `admin` scope (`scopes_supported` does not list it) and never grants it, even to admins; asking for it gets `invalid_scope`. Administer the instance with the admin token or an admin-scoped API key.

## Connecting an identity provider

Kazari works with one OpenID Connect provider per instance. Connecting it takes two steps: register Kazari as a web application at the provider, then add the provider to Kazari's configuration. Every provider gets the same redirect URI, `https://mcp.<base>/oauth/callback`, and gives you three values for the second step: the issuer, a client ID and a client secret.

### Microsoft Entra ID

To register Kazari in Entra:

1. In the Entra admin centre, go to **Applications → App registrations → New registration** and fill in:
   - Name: `Kazari`.
   - Supported account types: **Accounts in this organizational directory only** (single tenant).
   - Redirect URI: platform **Web**, `https://mcp.art.example.com/oauth/callback`.
2. From **Overview**, note the **Application (client) ID**, which is the `client_id`, and the **Directory (tenant) ID**. The issuer is `https://login.microsoftonline.com/<tenant-id>/v2.0`.
3. Under **Certificates & secrets → New client secret**, create a secret. Copy the value, not the ID, into `KAZARI_AUTH_OIDC_CLIENT_SECRET`, and note its expiry: Kazari's sign-in stops working when the secret expires.
4. Under **Token configuration → Add optional claim → ID → `email`**, add the claim and accept the prompt to add the Microsoft Graph `email` permission. Without it, Entra leaves `email` out of the ID token.
5. Check that **API permissions** lists Microsoft Graph `openid`, `email` and `profile` (delegated). Grant admin consent if your tenant requires it.

Kazari does not support the multi-tenant endpoints (`common`, `organizations`). Their discovery document names a per-tenant issuer template, so discovery fails the issuer check.

#### How Kazari reads Entra's ID token

Entra's ID token differs from other providers' in two ways, and Kazari handles both for Entra issuers only:

- If `email` is missing, Kazari uses `preferred_username` when it looks like an address. It is usually the user principal name, which can differ from the mail address. For every other provider, `preferred_username` is ignored.
- Entra does not send `email_verified`. For issuers on `login.microsoftonline.com`, the address counts as verified, since the tenant manages it.

### Google Workspace

To register Kazari in Google Cloud:

1. In the Google Cloud console, go to **APIs & Services → OAuth consent screen** and choose the user type **Internal**, which limits sign-in to accounts of your Workspace organisation.
2. Under **Credentials → Create credentials → OAuth client ID**, create a client of type **Web application** with the authorised redirect URI `https://mcp.art.example.com/oauth/callback`.
3. Use the issuer `https://accounts.google.com`, with the client ID and the client secret.

#### How Kazari reads Google's ID token

Google sends `email_verified` and, for Workspace accounts, `hd`, the hosted domain. For Google issuers, Kazari treats an address as verified only when `hd` equals the address's domain, or the address is `@gmail.com` or `@googlemail.com`.

A consumer Google account created on a work address therefore cannot pose as a Workspace user. Combine this with `allowed_domains` to admit only your domain.

### Other providers

Any OpenID Connect provider with discovery works. Register a confidential web client with the redirect URI above and the `openid email profile` scopes. Typical issuers:

| Provider  | Issuer                                                     |
| --------- | ---------------------------------------------------------- |
| Okta      | `https://<org>.okta.com/oauth2/default` (custom authorization server) or `https://<org>.okta.com` |
| Keycloak  | `https://<host>/realms/<realm>`                            |
| Authentik | `https://<host>/application/o/<application-slug>/`, with the trailing slash |
| Zitadel   | `https://<instance>.zitadel.cloud` or your own domain      |
| Auth0     | `https://<tenant>.<region>.auth0.com/`, with the trailing slash |

Make sure the provider sends `email` and `email_verified` in the ID token. Some send them only from the userinfo endpoint, which Kazari does not call. `email_verified` can be a boolean or the string `"true"`.

### Adding the provider to Kazari

Add an `auth.oidc` section to the configuration with the three values, then restart. The settings that matter are:

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

`allowed_domains`, `auto_provision` and `admin_emails` decide who gets in ([Provisioning policy](#provisioning-policy)). Every key is described in the [configuration reference](configuration.md#authoidc).

Put the client secret in the environment file, not the YAML:

```sh
# /etc/kazari/kazari.env
KAZARI_AUTH_OIDC_CLIENT_SECRET=the-client-secret
```

At startup, Kazari discovers the provider from `<issuer>/.well-known/openid-configuration`. If discovery fails, the server does not start and logs `oidc: discover <issuer>: ...`. Kazari requests the scopes `openid email profile`.

The `issuer` must be exactly the `issuer` value that the provider's discovery document states, including any trailing slash.

## Provisioning policy

After the ID token is verified, Kazari resolves the person to a user in this order:

1. **Known identity.** An identity already linked by `(issuer, subject)` signs in as its user.
2. **Link by verified email.** Otherwise, if the email is verified and its domain is in `allowed_domains` (or `allowed_domains` is empty), an existing user with that email gets this identity linked and signs in.
3. **Auto-provision.** Otherwise, if `auto_provision` is on and the verified email's domain is in `allowed_domains`, Kazari creates a new user. Its role is `admin` if the email is in `admin_emails`, else `member`; its quota is `limits.default_user_quota`; its display name is the IdP's `name`.
4. **Refused.** Otherwise the person sees "No access ... Ask your administrator for access." and nothing is issued.

A disabled user is always refused at step 1 or 2. Every successful sign-in updates the identity's `last_login_at` and the email it last signed in with.

What this means in practice:

- With `allowed_domains` empty, nobody is auto-provisioned. Only admin-created users with a matching email, or a linked identity, can sign in.
- With `auto_provision: false`, `allowed_domains` limits which admin-created users can link by email.
- `admin_emails` applies only when a user is first provisioned. To promote an existing user, use `kazari admin users update <handle> --role admin`.
- After the first sign-in the match is on `(issuer, subject)`, so a later change of email address at the IdP does not create a second user.

### Handles

Kazari makes a provisioned user's handle from the local part of their email:

1. Lower-case it.
2. Replace every character other than `a-z`, `0-9`, `_` and `-` with `-`.
3. Trim leading `-` and `_`, and trailing `-`.
4. Cut it to 32 characters.

A result shorter than 2 characters becomes `user`. If the handle is taken, Kazari adds a suffix: `jane.doe@example.com` becomes `jane-doe`, then `jane-doe-2`, `jane-doe-3`.

### Admin-created users

To let someone in ahead of their first sign-in, create their user with their email. They link at step 2 when they first sign in:

```sh
kazari admin users create jdoe --email jane.doe@example.com
```

To pin a user to a specific IdP account whatever its email, link the identity with the admin API. There is no CLI command for this yet:

```sh
curl -X POST -H "Authorization: Bearer $KAZARI_ADMIN_TOKEN" -H 'Content-Type: application/json' \
  https://admin.art.example.com/v1/users/jdoe/identities \
  -d '{"issuer":"https://login.microsoftonline.com/<tenant-id>/v2.0","subject":"<sub claim>"}'
```

To remove the link, send the same body to `POST /v1/users/{handle}/identities/unlink`. `GET /v1/users/{handle}` lists a user's identities with their last sign-in.

## Signing in with an API key

When `auth.oidc` is absent, `/oauth/authorize` skips the IdP and shows the consent page with an API key field. Approving with a valid key binds the grant to that key's user, capped at the key's scopes.

The grant lives only as long as the key. Once the key is revoked or expires, or its user is disabled, the grant's access and refresh tokens stop working, and no token outlives the key's expiry.

A one-person or small instance can therefore connect a cloud agent with no IdP set up: issue yourself a key, add the connector and paste the key once. The audit log records the consent as `oauth.grant` with `via: api_key` and the key's `key_prefix`.

Opening a `members` page works the same way. The sign-in page asks for an API key with `artifacts:read`, and the browser session lasts 12 hours or until the key is revoked or expires, whichever comes first. Browser sign-ins are recorded as `oauth.signin`, with `via: oidc` or `via: api_key`.

## Viewing members pages

An artifact with visibility `members` opens only for people who can sign in to this instance, and for holders of a [share link](api.md#share-links). The rule is the same on the latest host and on every version host.

"Members" means exactly the people the [provisioning policy](#provisioning-policy) admits, so there is no second list to keep. With `auto_provision` on, a first sign-in to view a page creates the account, as connecting an agent would.

A browser that opens a members page without a view session goes through these steps:

```text
browser ──1──▶ team.<base>/report       no view cookie: 302, sets a nonce cookie on team.
        ──2──▶ mcp.<base>/oauth/signin  ?host=team&path=/report&nonce=<S256 of the nonce>
                   │  signed in here already? skip to 4
                   └──3──▶ your IdP  (or, without one, a form asking for an API key)
        ◀──4── 302 team.<base>/.kazari/callback?code=...   single use, 1 minute, for team. only
        ──5──▶ team. redeems the code with its nonce cookie, sets __Host-kazari_view, 302 /report
```

1. A page navigation to a `members` artifact without a valid view cookie is redirected to the MCP host. Anything else, such as a script, an image, a `fetch`, a frame or `curl`, gets `401` with a short explanation instead, since a sign-in page is no use to it.
2. The MCP host keeps its own sign-in session, `__Host-kazari_session` (`HttpOnly`, `Secure`, `SameSite=Lax`, path `/`). If the browser has a live one, it is sent straight back with a code, and the person sees nothing.
3. Otherwise the person signs in at the IdP, which returns to the same `https://mcp.<base>/oauth/callback` that MCP clients use, so nothing changes in the IdP's configuration. Without an IdP, a page asks for an API key with `artifacts:read`, and that session ends with the key.
4. The code is single use and lives one minute. It works only on the host it was issued for, and only in the browser holding the matching nonce cookie, so a code started by someone else cannot sign a visitor in as them.
5. The artifact host turns the code into a view session of its own: the cookie `__Host-kazari_view`, sent only to that host and never readable by the page's script. The next host, such as another artifact or `team--v2.<base>`, repeats steps 1, 2, 4 and 5 without the IdP.

> [!NOTE]
> On a plain-HTTP instance (`external_scheme: http`, development only), the cookies drop `Secure` and the `__Host-` prefix: `kazari_session`, `kazari_view` and `kazari_nonce`.

Every sign-in session lasts 12 hours, and every view session it opened ends with it. A disabled user, or a revoked API key, loses access on the next request.

### Signing out

To sign out, open `/.kazari/signout` on a members page the browser has open, for example `https://<slug>.<base>/.kazari/signout`. That ends its sign-in on every host.

### Share links

Share links are for people who cannot sign in. Opening one stores its token as the view cookie on that host until the link expires, then redirects to `/`. A share link looks like this:

```text
https://<slug>.<base>/.kazari/share?token=kzs_...
```

The token works on every host of the artifact, but each host needs it once. For a pinned version, send `https://<slug>--v<N>.<base>/.kazari/share?token=kzs_...`. Revoking the link ends access on the next request.

## MCP clients

How an MCP client starts the OAuth flow depends on where it runs. The [MCP server reference](mcp.md) covers connecting with an API key instead.

### Cloud agents

Add a custom connector with the URL `https://mcp.<base>/`. The person signs in at your IdP and approves the consent page once per connection.

A client that supports Client ID Metadata Documents identifies itself with one when the server advertises support, as Kazari does; others fall back to registration. claude.ai, for example, uses a metadata document.

### Agents on your machine

An agent configured with the URL and no `Authorization` header runs the same flow itself when you first use the server, usually with a loopback redirect. Claude Code, for example, signs in this way after this command:

```sh
claude mcp add --transport http kazari https://mcp.art.example.com/
```

With an `Authorization: Bearer kzk_...` header instead, the agent uses the API key and skips OAuth.

### Client registration

Kazari accepts only public clients: no client secret, `token_endpoint_auth_method: none`. It accepts both ways an MCP client identifies itself:

- **Dynamic Client Registration** (RFC 7591): `POST https://mcp.<base>/oauth/register` with `redirect_uris`, and optionally `client_name`, returns a `client_id` starting `dcr_`.
- **Client ID Metadata Documents**: Kazari fetches a `client_id` that is an `https` URL, with a 10 s timeout, a 64 KiB cap, no redirects, JSON only and public addresses only. The document must contain the same `client_id`, and Kazari caches it for an hour. The server metadata advertises `client_id_metadata_document_supported: true`.

#### Redirect URIs

Redirect URIs must be `https`, or `http` on `localhost`, `127.0.0.1` or `::1`. They must match a registered URI exactly, except that for loopback URIs the port is ignored (RFC 8252), since native clients pick a free port each time.

## Tokens, lifetimes and rotation

Each credential and session in the sign-in flows has a fixed lifetime:

| Item                    | Lifetime                       | Notes                                                        |
| ----------------------- | ------------------------------ | ------------------------------------------------------------ |
| Authorization request   | 10 minutes                     | The time a person has to sign in and approve                 |
| Authorization code      | 5 minutes                      | Single use; a second use fails                               |
| Access token            | 1 hour                         | Opaque, stored as a SHA-256 hash                             |
| Refresh token           | 30 days                        | Rotated on every use                                         |
| Browser sign-in session | 12 hours                       | On the MCP host; ends sooner with the API key that opened it |
| View session            | Until its sign-in session ends | One per artifact host, on that host only                     |
| Sign-in code            | 1 minute                       | Single use, one host, bound to the browser's nonce cookie    |
| Share link              | 7 days by default              | Up to `serve.share_link_max_ttl` (90 days); revocable        |

These lifetimes are fixed in this version.

Each refresh returns a new refresh token and retires the old one. Presenting a retired refresh token again revokes the whole chain, and the client must sign in again; this is what limits the damage of a stolen token. A refresh can narrow the scopes but never widen them.

Tokens are checked against the user on every request. A disabled user's tokens stop working, scopes beyond the user's current role are dropped, and a grant approved with an API key needs that key to be active.

The `oauth-cleanup` job deletes expired requests, codes, tokens, browser sign-ins and sessions every hour ([scheduled jobs](ops.md#scheduled-jobs)).

## Revoking access

Each kind of access ends in its own way.

### Revoking one client's access

Each grant is a chain of refresh tokens, and revoking it also revokes the access tokens issued from it. List a user's grants, then revoke one by its id:

```sh
curl -H "Authorization: Bearer $KAZARI_ADMIN_TOKEN" https://admin.art.example.com/v1/users/jdoe/grants
curl -X DELETE -H "Authorization: Bearer $KAZARI_ADMIN_TOKEN" https://admin.art.example.com/v1/grants/17
```

### Revoking everything a user holds

To cut off a user completely, disable them:

```sh
kazari admin users update jdoe --disable
```

This revokes their API keys and OAuth grants, ends their browser sessions and refuses their sign-ins. Enabling the user again restores none of them. [Disabling users](ops.md#disabling-users-and-revoking-access) covers revoking single API keys and handing over the user's pages.

### Revoking a share link

To end a share link, revoke it by its prefix. `kazari share list` shows the prefixes:

```sh
kazari share list q3-report
kazari share revoke q3-report ab2cd3ef
```

Agents do the same with the `revoke_share_link` tool.

### Removing someone at the identity provider

Removing the person from the IdP stops new sign-ins, but an existing refresh token stays valid in Kazari until it expires or is revoked. Disable the user in Kazari too.

## Troubleshooting

Each entry is named for what the person or the server shows.

### Kazari does not start: `oidc: discover <issuer>: ...`

The issuer is unreachable from the server, or its discovery document names a different issuer. Fetch `<issuer>/.well-known/openid-configuration` from the server and copy its `issuer` value exactly, trailing slash included. For Entra, use the tenant ID, not `common`.

### "Sign-in failed: The identity provider's response could not be verified."

The code exchange or the ID token check failed, and the log line `oauth: identity provider exchange` says why. The usual causes:

- a wrong or expired client secret;
- a redirect URI at the IdP that is not exactly `https://mcp.<base>/oauth/callback`;
- a server clock out of sync (check NTP).

### "No access ... Ask your administrator for access."

The person signed in, but the [provisioning policy](#provisioning-policy) did not admit them. The log line `oauth: identity not admitted` records the issuer, subject and email. Check, in order:

1. Is the email present at all? For Entra, add the `email` optional claim, or make `preferred_username` an address.
2. Is it verified? For Google, check that the account is a Workspace account of that domain.
3. Is its domain in `allowed_domains`?
4. Is `auto_provision` on, or does an admin-created user have that email?
5. Is the user disabled?

### "Sign-in expired."

The ten-minute window passed, or the browser did not send the `__Host-kazari_oauth` cookie back. Cookies can be blocked, or a plain-HTTP instance can have `external_scheme` not set to `http`, so the cookie was marked `Secure`. Start again from the client.

### "Request rejected."

The consent form was posted from somewhere other than the consent page, or without the matching cookie. Somewhere else means a different origin, or `Sec-Fetch-Site` other than `same-origin`. Start again from the client.

### A members page keeps redirecting to sign-in

The browser does not keep the `__Host-kazari_view` cookie. Usually Kazari is served over plain HTTP while `external_scheme` is `https`, so the cookie is `Secure` and the browser drops it. Otherwise, cookies are blocked for the instance's domain. The browser stops after about twenty redirects.

### "Sign-in failed" on `/.kazari/callback`

The code expired (it lives a minute), was already used (a reload of the callback URL), or the browser lost the nonce cookie it set before the redirect. Open the page again.

### "Unknown application."

The `client_id` is neither registered nor a fetchable metadata document. The log line `oauth: client metadata fetch failed` gives the reason.

### "Invalid redirect address."

The client asked to return to a URI it did not register.

### `invalid_grant` from the token endpoint

The code was already used or has expired, or the PKCE verifier does not match. For a refresh, the refresh token was rotated, revoked or expired, or belongs to a disabled user. The client must start the flow again.
