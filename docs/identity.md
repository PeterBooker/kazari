# Identity and sign-in

People sign in to Kazari in one of two ways, chosen for the whole instance: with a built-in account, a password or a passkey that Kazari keeps, or at your organisation's identity provider. Agents and scripts use API keys, or OAuth access tokens that an MCP client obtains by sending a person through that sign-in.

This page explains how sign-in works, covers built-in accounts and the account page, connects an identity provider, sets who is let in, and covers token lifetimes, revoking access and troubleshooting. It assumes a running instance ([installation guide](install.md)).

## How sign-in works

Kazari is an OAuth 2.1 authorization server towards MCP clients. Towards people it is either the keeper of their password or an OpenID Connect relying party of one identity provider (IdP) per instance.

### How people sign in

An instance signs people in one way, never both:

- **Built-in accounts** are the default. A person signs in with their handle or email and a password, which they set through a link an admin sends them, or with a [passkey](#passkeys) they add later ([Built-in accounts](#built-in-accounts)).
- **An identity provider** takes over once the configuration has an `auth.oidc` section. People then sign in with their organisation account, and Kazari keeps no password ([Connecting an identity provider](#connecting-an-identity-provider)).

One sign-in page, on `mcp.<base>`, serves every place a person signs in: connecting an MCP client, opening a `members` page, the [account page](#the-account-page) and the [dashboard](dashboard.md).

### Credentials

Software acts for a person with one of two credentials:

- **API keys** (`kzk_...`) are issued by an admin or by the person themselves, and sent as `Authorization: Bearer`. They work on every surface and suit the CLI, scripts and agents that take a key in their configuration.
- **OAuth access tokens** are what MCP clients obtain by sending the person through a sign-in flow. This covers cloud agents' connectors, and agents on your machine configured without a key.

### The OAuth flow

An MCP client without a token goes through these steps. The diagram shows an instance with an identity provider; with built-in accounts, step 4 is Kazari's own sign-in page ([signing in with a password](#signing-in-with-a-password)):

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
2. The client reads the protected-resource metadata, then `https://mcp.<base>/.well-known/oauth-authorization-server`. The issuer is `https://mcp.<base>`. Every response to the client's redirect URI, a code or an error, carries it as `iss`, so a client that talks to several servers can tell which one answered (RFC 9207).
3. The browser goes to `/oauth/authorize` with PKCE (S256 only). Kazari checks the client, the redirect URI, the scopes and the `resource`, refuses a `state` over 1 KiB, and stores the request. It sets a short-lived cookie and redirects to the IdP with its own `state`, `nonce` and PKCE pair.
4. The person signs in at the IdP, which returns to **`https://mcp.<base>/oauth/callback`**. This is the redirect URI to register at the IdP.
5. Kazari redeems the code and verifies the ID token: signature against the IdP's keys, issuer, audience, expiry and nonce. It then maps the person to a Kazari user by the [provisioning policy](#provisioning-policy).
6. A consent page names the client, the account and the scopes. **Approve** issues a single-use code that lasts five minutes; **Deny** returns `access_denied` to the client.
7. The client exchanges the code at `/oauth/token` and receives an access token and a refresh token.

#### The sign-in cookie

The cookie set at step 3 is `__Host-kazari_oauth`: `HttpOnly`, `Secure`, `SameSite=Lax`, path `/`, for ten minutes. On a plain-HTTP development instance it is plain `kazari_oauth`. The `__Host-` prefix means no other host, such as an artifact page on `<slug>.<base>`, can set that cookie.

#### The consent form

Kazari accepts the consent form only from the consent page itself. A post whose `Origin` is another origin, or whose `Sec-Fetch-Site` is not `same-origin`, gets "Request rejected".

### Scopes

The scopes granted are those the client asked for, capped at what the user's role allows: `artifacts:read`, `artifacts:write`, `artifacts:share` and `artifacts:delete`. A client that asks for none gets all of them. [Scopes](api.md#scopes) says what each one allows.

A person who wants a connection to do less than they can [approves it with an API key](#approving-with-an-api-key), which caps the grant at that key's scopes.

An OAuth token always acts as a member, so it never opens the admin host. Kazari never offers the `admin` scope (`scopes_supported` does not list it) and never grants it, even to admins; asking for it gets `invalid_scope`. Administer the instance in the [dashboard](dashboard.md), over the admin socket on the server, with an admin-scoped API key, or with the admin token.

## Built-in accounts

A built-in account is a user with a password, and optionally passkeys. Built-in accounts are on by default when the configuration has no `auth.oidc` section, and off when it has one ([`auth.local`](configuration.md#authlocal)).

Kazari sends no email. Every step that another product would do by email is a link that an admin copies and sends: an invite for a new person, a reset for a lost password, and the setup link that creates the first admin of a fresh instance ([creating the first admin](install.md#creating-the-first-admin)).

A person who keeps [recovery codes](#recovery-codes) sets a new password themselves, with no admin and no link.

### Inviting a person

An invite creates the user and a link that lets them choose a password. Invite a person from the dashboard's People page, or with the CLI:

```sh
kazari admin users invite ana --email ana@example.com --name "Ana Lima"
```

The command prints the link, `https://art.example.com/account/invite?token=kzl_...`, on stdout. Send it to the person over a channel you trust, with the connector address, `https://mcp.art.example.com/`, for their agent. The dashboard writes this message for you beside the link.

A link behaves the same whatever it is for:

- It is shown once, when it is made. Kazari keeps only a hash of it, so a lost link is replaced, not shown again.
- It works once. Opening it changes nothing; setting the password uses it up.
- An invite works for 7 days by default (`auth.local.invite_ttl`).
- One link per person and purpose works at a time. Making a new one ends the one before it.
- Setting a password ends the person's other links, whether the password is set with a link or changed on the account page. Disabling the person ends them too.

Until the person sets a password, they show as `invited` in `kazari admin users list` and on the People page. To replace an invite that was lost or has lapsed, add `--replace`:

```sh
kazari admin users invite ana --replace
```

An invite link is for a person who has no password yet. For a person who has one, the command fails and names the [reset link](#resetting-a-password), which is what they need.

To see the links that still work, or end one before it is used, list them and revoke one by its id:

```sh
kazari admin invites list
kazari admin invites revoke 12
```

Only an admin can invite an admin: add `--role admin`. The person's email, when given, is a second name to sign in with. Only an admin changes it.

### Passwords

A password has at least 15 characters by default (`auth.local.min_password_length`) and at most 256. Length is the only rule: no character classes are required, and a password manager's output or a few unrelated words both work.

Setting a password with an invite, a reset or the setup link signs the person in, in that browser, as signing in with the password would: the browser passes through the sign-in page once and lands on the account page, which welcomes them with connecting an agent as the first step. The dashboard, members pages and an agent's sign-in then open with that sign-in, and signing out anywhere ends it everywhere.

### Signing in with a password

The sign-in page asks for a handle or an email, and the password. A browser stays signed in for 12 hours.

When an MCP client connects, `/oauth/authorize` shows this sign-in page first, unless the browser is already signed in. The consent page follows, naming the client, the account and the scopes, with **Approve** and **Deny**. A grant approved this way lasts as one approved after a provider sign-in does: no key bounds it.

**Use an API key instead**, under **More ways to sign in** below the form, opens the form that takes a key in place of the password ([Signing in with an API key](#signing-in-with-an-api-key)).

After several wrong passwords the page asks the person to wait before the next attempt, a second at first and never more than a minute. A wait ends by itself, and no account is ever locked.

### Passkeys

A passkey signs a person in with their fingerprint, their face or their device's PIN, in place of the password. Their device or password manager keeps it, and a passkey works only on the instance it was made for, so it cannot be phished.

To add one, a person opens the [account page](#the-account-page), [confirms it is them](#confirming-it-is-you) under **Passkeys**, gives the passkey a name such as Work laptop, and chooses **Add a passkey**. The browser then asks them to confirm with a fingerprint, face or PIN. A person can hold 20 passkeys.

To sign in with a passkey, choose **Sign in with a passkey** under the password form, or pick the passkey where the browser offers it in the **Handle or email** field. No name or password is typed. A passkey sign-in opens the same 12-hour session as a password, and leads to the consent page of an MCP client the same way.

The account page lists each passkey with when it was added and last used. **Remove** stops a passkey signing in at once. A reset link made with `--revoke-all` removes every passkey of the person.

#### Requirements

Passkeys need a browser with JavaScript, and a base domain that is a registrable name such as `art.example.com`. The apex and the sign-in host both end in it, so a passkey added on the account page signs in on the sign-in page.

On a bare `localhost` base domain, browsers refuse to make a passkey that both hosts share, so the passkey form is left out and people sign in with their password. The same holds for a base domain that is itself a public suffix, such as `github.io`. A name under `localhost` works: `kazari serve --local` runs on `kazari.localhost`, where passkeys work over plain `http`, because browsers treat `*.localhost` as a secure origin.

### Resetting a password

A person who has lost their password asks an admin for a reset link, which works for 24 hours by default (`auth.local.reset_ttl`). Make one on the person's page in the dashboard, or with the CLI:

```sh
kazari admin users reset-password ana
```

Using a reset link sets the new password and signs the person out of every browser. Their API keys and connected apps keep working.

To end those as well, for example when someone else may have used the account, add `--revoke-all`. Using the link then also revokes every API key of the person, removes their passkeys and recovery codes, and disconnects every app:

```sh
kazari admin users reset-password ana --revoke-all
```

An instance that used API keys before built-in accounts existed keeps working after an upgrade. The People page and `kazari admin users list` show who has no password yet, and a reset link gives each of them one.

### Recovery codes

Recovery codes let a person set a new password without asking an admin. Each is a single-use code such as `abcd-efgh-ijkl-mnop`, and a set holds 10.

To make a set, open the [account page](#the-account-page), [confirm it is you](#confirming-it-is-you) under **Recovery codes** and choose **Make recovery codes**. The next page shows the codes once: keep them in a password manager or on paper, apart from the password. Kazari keeps only a hash of each.

Making a new set ends the codes held before. The account page says how many unused codes are left, and so do the person's page in the dashboard and `kazari admin users show`.

#### Using a recovery code

A person who has lost their password opens **Lost your password? Use a recovery code** under the sign-in form, or `https://<base>/account/recover`. The page asks for the handle or email, one code and the new password twice.

Using a code sets the new password and signs the person out of every browser, as a reset link does. The code is used up, and their API keys and connected apps keep working.

When the person came from a sign-in, such as connecting an agent or opening a members page, the recovery form returns them to it with their name filled in, and they sign in with the new password and carry on.

Wrong codes slow the next attempt as wrong passwords do, a second at first and never more than a minute. A person with no codes left asks an admin for a [reset link](#resetting-a-password).

### The account page

Every person has an account page at `https://<base>/account/`, where they sign in as anywhere else. It shows, in this order:

- **Connect your AI agent**: the connector address with a copy button, the steps for claude.ai and the Claude desktop app, and the Claude Code command;
- the pages they own, each with **Manage**, which opens [the page's own page](#managing-your-pages);
- their connected apps, each of which they can disconnect;
- their profile: handle, name, email and role, which only an admin changes;
- their recent sign-ins and the recent security events of the account, such as a changed password, a new key or a reset link an admin made for them, each with who acted, since nobody is told of these by email;
- the password form, passkeys and recovery codes, on an instance with built-in accounts;
- their API keys, with issue, rotate and revoke.

On an instance with an identity provider the page has no password form, passkeys or recovery codes, and everything else is the same.

#### Confirming it is you

Three forms change how a person signs in, so they ask the person to confirm it is them first: changing the password, adding a passkey and making recovery codes. A browser left signed in cannot use them alone.

To confirm, enter the current password. A person who holds a passkey can leave that field empty instead: sending the form then asks the browser for one of their own passkeys, with a fingerprint, face or PIN.

Wrong passwords and refused passkeys slow the next attempt alike, a second at first and never more than a minute.

#### Changing a password

The password form [confirms it is you](#confirming-it-is-you) and asks for the new password twice. Changing it signs the person out of every other browser and keeps the session it was changed in. API keys and connected apps keep working.

**Sign out of my other sessions** does the same without a new password.

#### Keys from the account page

A person issues, rotates and revokes their own API keys on the account page, without a key in hand. **What the key may do** has a box for each scope, all ticked at first, and **Page addresses** limits the key to pages whose address matches, such as `weekly-*`. What a new key can hold depends on how they signed in:

- After signing in with a password or at the identity provider, a new key holds the member scopes at most (`artifacts:read`, `artifacts:write`, `artifacts:share`, `artifacts:delete`), for an admin too. A key with the `admin` scope comes from the dashboard or from `kazari admin keys issue`.
- After signing in with an API key, a new key is bounded by that key, as on [`/me/keys`](api.md#your-own-keys-mekeys): no scope it lacks, a restriction inside its own, and an expiry no later.

The page revokes any of the person's keys. It rotates any but one that holds `admin`, which is rotated in the dashboard or on the admin API.

A new or rotated key is shown once, on the page that answers the form. Reloading that page offers to send the form again, and doing so makes no second key: the page says **Form already sent**.

A person can hold 50 live keys at most, however they are issued. Revoked and expired keys do not count. A key can still be rotated at the limit, because the key it replaces does not count against it.

#### Managing your pages

**Manage** beside a page opens `https://<base>/account/pages/<slug>`, where a person changes their own page without an agent or the command line:

- **Who can open it**: public, unlisted or members only, or publish a hidden page again;
- **Share links**: make one for someone who cannot sign in, such as a client, and revoke one. The link is shown once. The form is offered on a published members-only page alone; elsewhere the section says why, with a button that makes the page members-only;
- **Versions**: **Publish this version** puts an earlier version back at the page's address;
- **Lifecycle**: **Mark stale** with a grace period in days, **Keep the page**, **Move to the bin** and **Restore the page**.

Each change is made as the person, with the rules of the [API](api.md): the write policy decides whose pages they may change, and after a sign-in with an API key the page offers only what that key's scopes allow. Under the default `owner` policy another member's page opens for looking, with nothing to change.

### Moving to an identity provider, or back

To move an instance from built-in accounts to an identity provider, add the `auth.oidc` section and restart ([Adding the provider to Kazari](#adding-the-provider-to-kazari)). Built-in accounts then turn off: passwords and passkeys stop signing anyone in, and the invite, reset, setup and recovery pages answer `404`.

Users, their pages, keys and connected apps stay. A person whose email at the provider is the one on their Kazari user links to it at their first sign-in ([Provisioning policy](#provisioning-policy)).

To move back, remove `auth.oidc` and restart. The stored passwords work again, and a person who never had one needs a reset link.

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

At startup, Kazari discovers the provider from `<issuer>/.well-known/openid-configuration`. If discovery fails, the server starts anyway and serves its pages, logs `identity provider unreachable` with the error, and tries again every 30 seconds through the `oidc-discovery` job. Until it succeeds, sign-in answers `503` with "Sign-in is temporarily unavailable". Each request to the provider, for discovery, the token exchange or its signing keys, gives up after 10 seconds without an answer. Kazari requests the scopes `openid email profile`.

`kazari check` and `kazari admin ready` fetch the discovery document too, and fail with what to fix when the issuer cannot be reached or names itself differently.

The `issuer` must be exactly the `issuer` value that the provider's discovery document states, including any trailing slash.

## Provisioning policy

After the ID token is verified, Kazari resolves the person to a user in this order:

1. **Known identity.** An identity already linked by `(issuer, subject)` signs in as its user.
2. **Link by verified email.** Otherwise, if the email is verified and its domain is in `allowed_domains` (or `allowed_domains` is empty, or `admin_emails` lists the address), an existing user with that email gets this identity linked and signs in. An existing **admin** links this way only when `admin_emails` names the address; any other admin needs an identity linked by hand ([Admin-created users](#admin-created-users)), so a verified address alone never takes over an admin account.
3. **Provision.** Otherwise, if the verified email is in `admin_emails`, Kazari creates a new admin, whatever `allowed_domains` and `auto_provision` say. If `auto_provision` is on and the verified email's domain is in `allowed_domains`, it creates a new member. The new user's quota is `limits.default_user_quota`, and its display name is the IdP's `name`.
4. **Refused.** Otherwise the person sees "No access ... Ask your administrator for access." and nothing is issued.

A disabled user is always refused at step 1 or 2. Every successful sign-in updates the identity's `last_login_at` and the email it last signed in with.

What this means in practice:

- With `allowed_domains` empty, nobody outside `admin_emails` is auto-provisioned. Only the addresses in `admin_emails`, admin-created users with a matching email, or a linked identity, can sign in. At start, an instance with no users whose `auth.oidc` would let nobody in logs a warning.
- With `auto_provision: false`, `allowed_domains` limits which admin-created users can link by email.
- `admin_emails` does two things: a listed email is provisioned as an admin on its first sign-in, and an existing admin with a listed email may link by email. It never changes the role of an existing user; to promote one, use `kazari admin users update <handle> --role admin`.
- After the first sign-in the match is on `(issuer, subject)`, so a later change of email address at the IdP does not create a second user.

### Handles

Kazari makes a provisioned user's handle from the local part of their email:

1. Lower-case it.
2. Replace every character other than `a-z`, `0-9`, `_` and `-` with `-`.
3. Trim leading `-` and `_`, and trailing `-`.
4. Cut it to 32 characters.

A result shorter than 2 characters becomes `user`. If the handle is taken, or is one of the reserved `admin` and `scheduler`, Kazari adds a suffix: `jane.doe@example.com` becomes `jane-doe`, then `jane-doe-2`, `jane-doe-3`.

### Admin-created users

To let someone in ahead of their first sign-in, create their user with their email. They link at step 2 when they first sign in:

```sh
kazari admin users create jdoe --email jane.doe@example.com
```

An admin created this way signs in by email only if `admin_emails` lists the address. Otherwise, and to pin any user to a specific IdP account whatever its email, link the identity by its issuer and subject, the `sub` claim:

```sh
kazari admin identities link jdoe \
  --issuer https://login.microsoftonline.com/<tenant-id>/v2.0 --subject <sub claim>
```

An identity linked to another user moves to this one. To see a user's identities with their last sign-in, or to remove one:

```sh
kazari admin identities list jdoe
kazari admin identities unlink jdoe \
  --issuer https://login.microsoftonline.com/<tenant-id>/v2.0 --subject <sub claim>
```

The dashboard has the same on a person's page, and the [admin API](api.md#identities) has the routes.

## Signing in with an API key

On an instance without an identity provider, an API key signs a person in wherever a password does. With built-in accounts on, the sign-in page offers it as **Use an API key instead**, under **More ways to sign in**. With `auth.local.enabled: false`, the key form is the only one.

The form takes any of the person's keys that holds `artifacts:read`. The browser session lasts 12 hours or until the key is revoked or expires, whichever comes first. A key [restricted to some artifacts](api.md#restricted-credentials) is refused there, because a session opens every members page.

What the session can do follows the key. It opens the dashboard only when the key holds the `admin` scope, and a key issued from it on the account page is [bounded by it](#keys-from-the-account-page).

When an MCP client connects, the same link leads to a consent page with an API key field in place of a sign-in. Approving with a valid key binds the grant to that key's user and to the key ([Approving with an API key](#approving-with-an-api-key)).

Browser sign-ins are recorded as `oauth.signin`, with `via: oidc`, `via: password`, `via: passkey`, `via: api_key`, or `via: link` for the sign-in that follows choosing a password with a link.

### Approving with an API key

A grant approved with an API key is that key over OAuth. It is for a connection that should do less than the person can, such as a [scheduled agent](mcp.md#scheduled-and-unattended-agents) whose host speaks only OAuth.

After a sign-in, at the identity provider or with a password, the consent page offers it: open **Approve with an API key instead**, paste one of your own keys and approve. A key of another user is refused. Left empty, the form approves as yourself.

The grant takes everything from the key:

- Its scopes are the key's, where the client asked for them.
- It is [restricted](api.md#restricted-credentials) to the key's slug patterns.
- It lives only as long as the key. Once the key expires or is revoked, or its user is disabled, the grant's access and refresh tokens stop working, and no token outlives the key's expiry.

Rotating the key ends the grant when the old key does, so connect again with the new key inside the overlap. The audit log records the consent as `oauth.grant` with `via: api_key` and the key's `key_prefix`, and [`kazari admin grants list`](cli.md#oauth-grants) shows the key beside the grant.

## Viewing members pages

An artifact with visibility `members` opens only for people who can sign in to this instance, and for holders of a [share link](api.md#share-links). The rule is the same on the latest host and on every version host.

A [staged version](api.md#staging-a-version) goes through the same sign-in on its version host, whatever the artifact's visibility, and a share link does not open it. It opens for everyone who can open the page once it is published.

"Members" means exactly the people the [provisioning policy](#provisioning-policy) admits, so there is no second list to keep. With `auto_provision` on, a first sign-in to view a page creates the account, as connecting an agent would.

A browser that opens a members page without a view session goes through these steps:

```text
browser ──1──▶ team.<base>/report       no view cookie: 302, sets a nonce cookie on team.
        ──2──▶ mcp.<base>/oauth/signin  ?host=team&path=/report&nonce=<S256 of the nonce>
                   │  signed in here already? skip to 4
                   └──3──▶ your IdP  (or, without one, Kazari's own sign-in page)
        ◀──4── 302 team.<base>/.kazari/callback?code=...   single use, 1 minute, for team. only
        ──5──▶ team. redeems the code with its nonce cookie, sets __Host-kazari_view, 302 /report
```

1. A page navigation to a `members` artifact without a valid view cookie is redirected to the MCP host. Anything else, such as a script, an image, a `fetch`, a frame or `curl`, gets `401` with a short explanation instead, since a sign-in page is no use to it. A service worker's script request is the exception: it gets a script that removes the worker ([Service workers](ops.md#service-workers)).
2. The MCP host keeps its own sign-in session, `__Host-kazari_session` (`HttpOnly`, `Secure`, `SameSite=Lax`, path `/`). If the browser has a live one, it is sent straight back with a code, and the person sees nothing.
3. Otherwise the person signs in at the IdP, which returns to the same `https://mcp.<base>/oauth/callback` that MCP clients use, so nothing changes in the IdP's configuration. Without an IdP, the sign-in page asks for the password of their [built-in account](#signing-in-with-a-password), or for an [API key](#signing-in-with-an-api-key), and a session opened with a key ends with the key.
4. The code is single use and lives one minute. It works only on the host it was issued for, and only in the browser holding the matching nonce cookie, so a code started by someone else cannot sign a visitor in as them.
5. The artifact host turns the code into a view session of its own: the cookie `__Host-kazari_view`, sent only to that host and never readable by the page's script. The next host, such as another artifact or `team--v2.<base>`, repeats steps 1, 2, 4 and 5 without the IdP.

> [!NOTE]
> On a plain-HTTP instance (`external_scheme: http`, development only), the cookies drop `Secure` and the `__Host-` prefix: `kazari_session`, `kazari_view` and `kazari_nonce`.

Every response to a `GET` on a members host, whether a page, an error, the redirect to sign in or a `/.kazari/` page, carries `Cross-Origin-Opener-Policy: same-origin`, as the sign-in page does. A window opened by another site therefore looks the same to its opener with access or without, so the opener cannot tell whether the visitor can see the page. It also means a members page has no `window.opener` from a page on another origin, and a popup it opens to another origin returns a handle that reads as closed. Public and unlisted artifacts do not send this header.

Every sign-in session lasts 12 hours, and every view session it opened ends with it. A disabled user, or a revoked API key, loses access on the next request.

The sign-in page of a members page starts with a note for a visitor who is no member: ask the person who sent the page for a share link. A share link to a page that is offline or gone opens a page that says the same.

The account page and the dashboard are opened the same way, each with a cookie of its own on its own host: `__Host-kazari_account` on the apex and `__Host-kazari_admin` on the admin host.

### Signing out

To sign out, open `https://mcp.<base>/oauth/signout` and press the button. That ends every sign-in of this browser, including one finished in a second tab, and every view session they opened, on every host. Other browsers stay signed in. **Sign out** on the account page and in the dashboard does the same. An artifact host cannot end a sign-in.

`/.kazari/signout` on an artifact host, for example `https://<slug>.<base>/.kazari/signout`, ends that host's access only: its view session and cookie, or a share link's cookie. The sign-in stays, so opening a members page there again needs no prompt. The page links to the sign-out page on the MCP host.

### Share links

Share links are for people who cannot sign in. Opening one stores its token as the view cookie on that host until the link expires, then redirects to `/`. Disabling the user who created a link [revokes it](#revoking-everything-a-user-holds). A share link looks like this:

```text
https://<slug>.<base>/.kazari/share?token=kzs_...
```

The token works on every host of the artifact, but each host needs it once. For a pinned version, send `https://<slug>--v<N>.<base>/.kazari/share?token=kzs_...`. Revoking the link ends access on the next request.

A link made with a password shows a password form before the page. The cookie it sets after the right password works only with that link, and ends when the link expires or is revoked ([creating a share link](api.md#create-a-share-link-post-artifactsslugshare-links)).

Wrong passwords make the next try wait, from a second up to a minute. The first three from an address cost nothing, and the wait applies to that address first: the link's other visitors wait too only after about thirty wrong tries in all, and the right password clears both. Someone who has the link but not the password can keep slowing the form down by guessing. If that happens, revoke the link and make a new one.

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
- **Client ID Metadata Documents**: Kazari fetches a `client_id` that is an `https` URL, with a 10 s timeout, a 64 KiB cap, no redirects, JSON only and public addresses only. The document must contain the same `client_id`, and Kazari caches it for an hour. A `client_id` longer than 2 KiB is refused. The server metadata advertises `client_id_metadata_document_supported: true`, and `authorization_response_iss_parameter_supported: true` for the [`iss` on every response](#the-oauth-flow).

Registering needs no credential, so Kazari limits what it keeps. A client that completes no grant is deleted a day after it registers, and at most 10,000 such clients exist at a time. At that limit a new client is refused until the [hourly cleanup](ops.md#scheduled-jobs) frees room: registration answers `503` with `temporarily_unavailable`, the authorize page for a new metadata document answers `503`, and Kazari logs `oauth: client limit reached`. Clients with a grant are not counted and keep working.

#### Redirect URIs

Redirect URIs must be `https`, or `http` on `localhost`, `127.0.0.1` or `::1`, and at most 2 KiB long, both when registered and when requested. They must match a registered URI exactly, except that for loopback URIs the port is ignored (RFC 8252), since native clients pick a free port each time.

## Tokens, lifetimes and rotation

Each credential and session in the sign-in flows has a fixed lifetime:

| Item                    | Lifetime                       | Notes                                                        |
| ----------------------- | ------------------------------ | ------------------------------------------------------------ |
| Authorization request   | 10 minutes                     | The time a person has to sign in and approve                 |
| Authorization code      | 5 minutes                      | Single use; a second use fails                               |
| Access token            | 1 hour                         | `auth.tokens.access_ttl`. Opaque, stored as a SHA-256 hash   |
| Refresh token           | 90 days                        | `auth.tokens.refresh_ttl`. Rotated on every use              |
| API key                 | Until revoked, unless issued with an expiry | Up to `auth.tokens.max_key_ttl`, when that is set |
| Browser sign-in session | 12 hours                       | On the MCP host; ends sooner with the API key that opened it |
| View session            | Until its sign-in session ends | One per artifact host, on that host only; the account page and the dashboard each hold one too |
| Invite link             | 7 days                         | `auth.local.invite_ttl`. Single use; replaced by a newer one |
| Reset link              | 24 hours                       | `auth.local.reset_ttl`. Single use; replaced by a newer one  |
| Setup link              | 1 hour                         | Single use; works only while the instance has no user        |
| Sign-in code            | 1 minute                       | Single use, one host, bound to the browser's nonce cookie    |
| Share link              | 7 days by default              | Up to `serve.share_link_max_ttl` (90 days); revocable        |

The lifetimes with a key are set under [`auth.tokens`](configuration.md#authtokens) and [`auth.local`](configuration.md#authlocal); the others are fixed in this version.

A connection has no fixed end. Each refresh returns a new access token and a new refresh token with a full lifetime, so a client that refreshes at least once every `auth.tokens.refresh_ttl` stays connected. A client that pauses for longer must sign in again.

Each refresh retires the old refresh token. Presenting a retired refresh token again revokes the whole chain, and the client must sign in again; this is what limits the damage of a stolen token. A refresh can narrow the scopes but never widen them.

A client that never receives the answer to a refresh holds only the retired token. For that case, a refresh token is accepted once more for `auth.tokens.refresh_reuse_window` (30 seconds by default) after it was retired: Kazari answers with another new pair in the same grant, and the client carries on. A second repeat, a repeat after the window, or a repeat once the newer token has been used revokes the chain as above. Kazari records that as `oauth.revoke` with `reason: refresh_replay` in the [audit log](ops.md#audit-log).

Tokens are checked against the user on every request. A disabled user's tokens stop working, scopes beyond the user's current role are dropped, and a grant [approved with an API key](#approving-with-an-api-key) needs that key to be active and takes its restriction.

The `oauth-cleanup` job deletes expired requests, codes, tokens, browser sign-ins and sessions every hour ([scheduled jobs](ops.md#scheduled-jobs)).

## Revoking access

Each kind of access ends in its own way.

### Revoking one client's access

Each grant is a chain of refresh tokens, and revoking it also revokes the access tokens issued from it. List a user's grants, then revoke one by its id:

```sh
kazari admin grants list jdoe
kazari admin grants revoke 17
```

The list shows each connection's client, scopes, last refresh and expiry. The id is the number in the `oauth:<id>` credential that the [audit log](ops.md#audit-log) records for every change the connection made, so a line there leads to the grant to revoke. The [admin API](api.md#grants) has the same two calls.

A grant approved with an API key also ends when that key is revoked. A person disconnects their own apps on the [account page](#the-account-page).

### Signing a person out everywhere

To end every browser session of a person, on every host, and leave their keys and connected apps alone:

```sh
kazari admin users sign-out jdoe
```

The audit log records it as `session.revoke`. The dashboard has the same on a person's page.

### Revoking everything a user holds

To cut off a user completely, disable them:

```sh
kazari admin users update jdoe --disable
```

This revokes their API keys, OAuth grants and every share link they created, ends their browser sessions and refuses their sign-ins. Links they gave to outside reviewers end too; a reviewer who still needs the page needs a new link from someone else. Enabling the user again restores none of them. [Disabling users](ops.md#disabling-users-and-revoking-access) covers revoking single API keys and handing over the user's pages.

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

### "This link no longer works"

An invite, reset or setup link was already used, was replaced by a newer one, was revoked or has expired. A link also stops working when its person sets or changes their password another way, or is disabled. The page looks the same for each. Make a new link: `kazari admin users invite <handle> --replace` for a person who never set a password, `kazari admin users reset-password <handle>` for one who did, or `kazari admin setup-link` on a fresh instance.

A setup link also stops working, with `404`, as soon as the instance has any user. After a link is opened, the page waits 15 minutes for the password; later than that, open the link again.

### "Form already sent"

The form that makes a key on the account page was sent a second time, usually by reloading the page that showed the key. Nothing new was made, and the key from the first time keeps working. To make another key, open the account page and use the form there.

**Form expired** means the page was opened more than 12 hours earlier or before the server restarted. Open the account page again and send the form from there.

### "That name or password was not accepted."

The handle or email is unknown, the password is wrong, the account has no password yet or the user is disabled. The page gives one answer for all four. `kazari admin users show <handle>` says whether the user has a password, has an invite waiting or is disabled.

### "Too many attempts. Try again in ... seconds."

Several sign-ins with a wrong password came for this account, or from this address, shortly before. The wait starts at a second, doubles with each further failure and is never more than a minute. It ends by itself, and a sign-in with the right password clears the account's count.

### "Sign-in is temporarily unavailable"

Kazari has not reached the identity provider since it started, and the log has `identity provider unreachable` with the reason. The issuer is unreachable from the server, or its discovery document names a different issuer. Run `sudo kazari check`, whose `oidc` line says which; then fetch `<issuer>/.well-known/openid-configuration` from the server and copy its `issuer` value exactly, trailing slash included. For Entra, use the tenant ID, not `common`.

Kazari retries every 30 seconds and logs `identity provider reached` once it succeeds; `kazari admin jobs run oidc-discovery` tries at once.

### "Sign-in failed: The identity provider's response could not be verified."

The code exchange or the ID token check failed, and the log line `oauth: identity provider exchange` says why. The usual causes:

- a wrong or expired client secret;
- a redirect URI at the IdP that is not exactly `https://mcp.<base>/oauth/callback`;
- a server clock out of sync (check NTP).

### "No access ... Ask your administrator for access."

The person signed in, but the [provisioning policy](#provisioning-policy) did not admit them. The log line `oauth: identity not admitted` records the issuer, subject and email. Check, in order:

1. Is the email present at all? For Entra, add the `email` optional claim, or make `preferred_username` an address.
2. Is it verified? For Google, check that the account is a Workspace account of that domain.
3. Is its domain in `allowed_domains`, or the address in `admin_emails`?
4. Is `auto_provision` on, is the address in `admin_emails`, or does an admin-created user have that email?
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

The code was already used or has expired, or the PKCE verifier does not match. For a refresh, the refresh token was rotated, revoked or expired, belongs to a disabled user, or belongs to a grant whose API key has expired or been revoked. The client must start the flow again.

If a client that runs unattended meets this after a pause, compare the pause with `auth.tokens.refresh_ttl`. If the audit log shows `oauth.revoke` with `reason: refresh_replay`, the client sent a retired refresh token outside the [reuse window](#tokens-lifetimes-and-rotation).
