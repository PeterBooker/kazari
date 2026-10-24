# Configuration

Agalma reads one TOML file plus environment overrides, validates the result at startup and refuses
to start with a list of every problem it found, each named by its key. There are no settings stored
in the database: to change a limit, edit the file and restart (it takes under a second).
`agalma admin settings` (or `GET /v1/settings` on the admin API) prints the effective configuration
with secrets redacted.

`deploy/agalma.toml` is a complete example with every default.

## Where the file comes from

`agalma serve` uses the first of:

1. `--config PATH` (the file must exist);
2. `$AGALMA_CONFIG`;
3. `/etc/agalma/agalma.toml`, if it exists;
4. no file: defaults plus environment variables.

A key the server does not know is an error, so a typo fails loudly rather than being ignored.

## Environment overrides

Every key has an environment variable: `AGALMA_` followed by the key's full path, upper-cased, with
dots replaced by underscores. The environment always wins over the file.

| Key                        | Variable                          |
| -------------------------- | --------------------------------- |
| `base_domain`              | `AGALMA_BASE_DOMAIN`              |
| `listen.proxy_mode`        | `AGALMA_LISTEN_PROXY_MODE`        |
| `tls.dns_provider`         | `AGALMA_TLS_DNS_PROVIDER`         |
| `tls.dns.api_token`        | `AGALMA_TLS_DNS_API_TOKEN`        |
| `limits.max_version_bytes` | `AGALMA_LIMITS_MAX_VERSION_BYTES` |
| `auth.oidc.allowed_domains`| `AGALMA_AUTH_OIDC_ALLOWED_DOMAINS`|

Value formats:

- **Booleans**: `true`, `false`, `1`, `0` (anything Go's `strconv.ParseBool` takes).
- **Lists**: comma-separated, spaces trimmed: `AGALMA_AUTH_OIDC_ALLOWED_DOMAINS=example.com,example.org`.
  A variable replaces the whole list.
- **Sizes**: an integer with an optional unit, case-insensitive, optional space: `B`, `KiB`, `MiB`,
  `GiB`, `TiB` (powers of 1024) or `KB`, `MB`, `GB`, `TB` (powers of 1000). `"50MiB"`, `"2 GiB"`,
  `52428800`.
- **Durations**: Go durations (`"15m"`, `"336h"`, `"1h30m"`) with an optional leading whole number
  of days: `"14d"`, `"1d12h"`.

Setting any `AGALMA_AUTH_OIDC_*` variable switches on the `[auth.oidc]` table, with its defaults,
even when the file has none.

Do not confuse the server's variables with the CLI's: `AGALMA_URL`, `AGALMA_TOKEN`,
`AGALMA_ADMIN_URL` and `AGALMA_ADMIN_TOKEN` configure the client ([cli.md](cli.md)); the server's
admin token is `AGALMA_AUTH_ADMIN_TOKEN`.

## Keys

### Top level

| Key               | Type    | Default  | Meaning                                                                                                    |
| ----------------- | ------- | -------- | ---------------------------------------------------------------------------------------------------------- |
| `base_domain`     | string  | none     | Domain artifacts live under, as `<slug>.<base_domain>`. Lower-case host name, no scheme, port or trailing dot. Required. |
| `data_dir`        | string  | none     | Holds `agalma.db`, `blobs/`, `uploads/` and `certmagic/`. Required. The systemd unit and the Docker image set `/var/lib/agalma`. |
| `external_scheme` | string  | `https`  | Scheme of the public URLs Agalma prints and returns: `https` or `http`.                                    |
| `external_port`   | integer | `0`      | Port appended to public URLs when not the scheme's default; `0` omits it. `8080` gives `http://demo.localhost:8080/`. |

`external_scheme` and `external_port` only shape the URLs in responses; they do not change what
Agalma listens on.

### `[listen]`

| Key          | Type   | Default | Meaning                                                                                                   |
| ------------ | ------ | ------- | --------------------------------------------------------------------------------------------------------- |
| `https`      | string | `:443`  | TLS listener address. Required unless `proxy_mode` is on.                                                 |
| `http`       | string | `:80`   | Plain listener. Outside proxy mode it answers `/health` and redirects every request for the base domain or its subdomains to HTTPS (`301`); `""` disables it. In proxy mode it is the only listener and is required. |
| `proxy_mode` | bool   | `false` | Serve plain HTTP on `http` behind one TLS-terminating proxy and take the client IP from the last `X-Forwarded-For` entry, then `X-Real-IP`. |

### `[tls]`

| Key            | Type   | Default       | Meaning                                                                                       |
| -------------- | ------ | ------------- | --------------------------------------------------------------------------------------------- |
| `mode`         | string | `acme`        | `acme`, `files` or `self-signed`. See [install.md](install.md#tls-modes).                     |
| `email`        | string | none          | ACME account contact. Required for `acme` outside proxy mode.                                 |
| `ca`           | string | `letsencrypt` | `letsencrypt`, `letsencrypt-staging` or an `https` ACME directory URL.                        |
| `dns_provider` | string | `cloudflare`  | DNS-01 provider: `cloudflare`, `route53` or `hetzner`.                                        |
| `cert_file`    | string | `""`          | PEM certificate chain for `files`. Must cover the base domain and its wildcard.               |
| `key_file`     | string | `""`          | PEM private key for `files`.                                                                  |

### `[tls.dns]`

| Key         | Type   | Default | Meaning                                                                                              |
| ----------- | ------ | ------- | ---------------------------------------------------------------------------------------------------- |
| `api_token` | secret | `""`    | DNS provider credential. Required for `cloudflare` and `hetzner`. For `route53`, leave empty to use the AWS default credential chain, or set `ACCESS_KEY_ID:SECRET_ACCESS_KEY`. |

### `[limits]`

| Key                     | Type     | Default | Meaning                                                                                                   |
| ----------------------- | -------- | ------- | --------------------------------------------------------------------------------------------------------- |
| `max_version_bytes`     | size     | `50MiB` | Total size of all files in one version. Also bounds a request body (plus 1 MiB of headroom), a bundle's uncompressed size and one chunked upload. |
| `max_file_bytes`        | size     | `25MiB` | Size of one file.                                                                                         |
| `max_files`             | integer  | `2000`  | Files in one version.                                                                                     |
| `metadata_max_pairs`    | integer  | `64`    | Metadata key/value pairs per artifact.                                                                    |
| `write_rate_per_minute` | integer  | `60`    | Write and delete requests per minute per credential on the user API (burst of the same size); `429` beyond it. |
| `default_user_quota`    | size     | `2GiB`  | Quota given to users created without one, whether by an admin or by sign-in. See [ops.md](ops.md#quotas). |
| `inline_guidance_bytes` | size     | `2MiB`  | Quoted in the MCP tool descriptions as the size above which a model should switch to chunked uploads. Advisory; nothing enforces it. |
| `max_chunk_bytes`       | size     | `4MiB`  | Largest chunk of a chunked upload, after base64 decoding on MCP.                                          |
| `upload_ttl`            | duration | `1h`    | How long a chunked upload stays open before the `uploads-expire` job removes it.                          |
| `max_open_uploads`      | integer  | `5`     | Open chunked uploads per user.                                                                            |

### `[lifecycle]`

| Key              | Type     | Default            | Meaning                                                                           |
| ---------------- | -------- | ------------------ | --------------------------------------------------------------------------------- |
| `default_grace`  | duration | `336h` (14 days)   | Grace period of a `stale` call that names none.                                   |
| `retention`      | duration | `720h` (30 days)   | Time a soft-deleted artifact stays restorable before the sweep purges it.         |
| `tombstone`      | duration | `2160h` (90 days)  | Time a purged slug stays reserved (and answers `410`) before it is freed.         |
| `sweep_interval` | duration | `15m`              | How often the `sweep` job runs.                                                   |
| `gc_interval`    | duration | `1h`               | How often the `gc` job deletes unreferenced blobs.                                |

See [ops.md](ops.md#lifecycle) for how these combine.

### `[serve]`

| Key                  | Type        | Default       | Meaning                                                                                       |
| -------------------- | ----------- | ------------- | --------------------------------------------------------------------------------------------- |
| `csp`                | string      | see below     | `Content-Security-Policy` sent with every artifact response; `""` sends none.                 |
| `allow_csp_override` | bool        | `true`        | Let an artifact's `agalma.csp` metadata replace `csp` for its own files.                      |
| `reserved_slugs`     | string list | see below     | Slugs no artifact may take. Each must be non-empty and lower-case.                            |

The default CSP allows inline scripts and styles plus the common CDNs:

```text
default-src 'self'; script-src 'self' 'unsafe-inline' https://cdnjs.cloudflare.com https://cdn.jsdelivr.net https://unpkg.com; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com; img-src 'self' data: https:; connect-src 'self'; frame-ancestors 'none'
```

The default reserved slugs are `api`, `mcp`, `admin`, `www`, `mail`, `ns1`, `ns2`, `autoconfig`,
`autodiscover` and `localhost`. If you replace the list, keep `api`, `mcp` and `admin`.

Each artifact is its own origin, so a looser per-artifact CSP only affects that artifact's visitors;
on a multi-tenant instance consider `allow_csp_override = false`.

### `[auth]`

| Key            | Type   | Default  | Meaning                                                                                              |
| -------------- | ------ | -------- | ---------------------------------------------------------------------------------------------------- |
| `write_policy` | string | `shared` | `shared`: anyone with `artifacts:write` may change any artifact. `owner`: only the owner and admins may. |
| `admin_token`  | secret | `""`     | Bearer token accepted on the admin host. Empty disables it, leaving admin users' credentials that carry the `admin` scope. |

### `[auth.oidc]`

Present means "users sign in through this OpenID Connect provider"; absent means the OAuth consent
page asks for an API key instead. See [identity.md](identity.md).

| Key               | Type        | Default | Meaning                                                                                            |
| ----------------- | ----------- | ------- | -------------------------------------------------------------------------------------------------- |
| `issuer`          | string      | none    | The provider's issuer URL, exactly as its discovery document states it. Must be `https`. Required. |
| `client_id`       | string      | none    | Client ID of the app registered at the provider. Required.                                         |
| `client_secret`   | secret      | none    | Client secret. Required.                                                                           |
| `allowed_domains` | string list | `[]`    | Verified email domains that may sign in.                                                           |
| `auto_provision`  | bool        | `true`  | Create a member on first sign-in from an allowed domain.                                           |
| `admin_emails`    | string list | `[]`    | Emails provisioned with the admin role.                                                            |

### `[admin]`

| Key      | Type   | Default | Meaning                                                                                         |
| -------- | ------ | ------- | ----------------------------------------------------------------------------------------------- |
| `listen` | string | `""`    | Optional second listener, plain HTTP, that serves the admin API, `/health` and `/metrics` whatever the `Host` header, for example `127.0.0.1:9443`. The admin host stays reachable on the public listener too. |

### `[log]`

| Key      | Type   | Default | Meaning                              |
| -------- | ------ | ------- | ------------------------------------ |
| `level`  | string | `info`  | `debug`, `info`, `warn` or `error`.  |
| `format` | string | `text`  | `text` or `json` (slog handlers).    |

## Validation rules you may hit

All of these are checked together; the error lists every failure as `key: problem`.

- `base_domain` is required, lower-case, without scheme, port or trailing dot, and made of labels of
  letters, digits and hyphens.
- `data_dir` is required.
- `external_scheme` is `http` or `https`; `external_port` is 0 to 65535.
- Outside proxy mode `listen.https` is required; in proxy mode `listen.http` is required.
- `tls.mode` is one of the three modes. Outside proxy mode:
  - `acme` needs `tls.email`, a valid `tls.ca`, a known `tls.dns_provider` and, except for `route53`,
    `tls.dns.api_token`;
  - `files` needs `tls.cert_file` and `tls.key_file`.
- Every number and duration in `[limits]` and `[lifecycle]` must be greater than 0. In particular
  `write_rate_per_minute` cannot be set to 0 to disable rate limiting, and `default_user_quota` cannot
  be 0; give individual users an unlimited quota through the admin API instead
  (`agalma admin users update <handle> --quota 0`).
- `limits.max_file_bytes` and `limits.max_chunk_bytes` must not exceed `limits.max_version_bytes`.
- `auth.write_policy` is `shared` or `owner`.
- With `[auth.oidc]`: `issuer` is an `https` URL, `client_id` and `client_secret` are set.
- `log.level` and `log.format` are among the listed values.

## Secrets

Three keys are secrets: `tls.dns.api_token`, `auth.admin_token` and `auth.oidc.client_secret`. They are
shown as `"***"` by the settings endpoint. Keep them out of the TOML file, which under systemd must be
world-readable, and set them through the environment: `/etc/agalma/agalma.env` (mode `0600`, read by
systemd as root) for the unit, `--env-file` for Docker, or your secret manager's equivalent.

Generate the admin token with `openssl rand -hex 32`. Once an admin user has an API key with the
`admin` scope, you can unset `AGALMA_AUTH_ADMIN_TOKEN` entirely.
