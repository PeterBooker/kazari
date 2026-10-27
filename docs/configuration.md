# Configuration

Kazari reads one YAML file plus environment variables and checks the result at startup. If anything is wrong, it refuses to start and lists every problem, each named by its key. No settings are stored in the database: to change a limit, edit the file and restart, which takes under a second.

This page covers how the configuration is loaded, the file's format, environment variables and dotenv files, secrets, the preflight check, and every key with its default. Three files in `deploy/`, served at <https://kazari.io/deploy/>, are the starting points: `kazari.yaml`, the short file with the keys every instance sets; `kazari.reference.yaml`, every key with its default; and `kazari.env.example`, the template for the secrets.

## How configuration is loaded

The configuration has three sources: built-in defaults, the YAML file and environment variables. This section says where the file is found and which source wins.

### Where the file comes from

`kazari serve` and `kazari check` use the first of:

1. `--config PATH`. The file must exist.
2. `$KAZARI_CONFIG`.
3. `/etc/kazari/kazari.yaml`, if it exists.
4. No file: defaults plus environment variables.

A key the server does not know is an error, so a typo fails loudly rather than being ignored.

### The local preset

`kazari serve --local` reads no file. It starts from a preset for a server on your own machine: plain HTTP on `127.0.0.1:8080` in proxy mode, the base domain `localhost`, `external_scheme: http` with `external_port: 8080`, the data in `./.data`, and `log.level: debug`. `KAZARI_*` variables apply on top of it, so `KAZARI_DATA_DIR` moves the data and `KAZARI_LISTEN_HTTP=:8080` makes it reachable from outside a container. `--local` and `--config` exclude each other.

### Precedence

From lowest to highest:

1. Built-in defaults.
2. The YAML file, after `${}` references are expanded.
3. `KAZARI_*` overrides, from the process environment or the `.env` file.

Validation then checks the result.

## Writing the file

Sections are nested mappings, with the keys and their nesting listed under [Keys](#keys). Sizes and durations are plain strings, such as `50MiB` and `336h`, and lists are YAML sequences. A production file with ACME certificates and OpenID Connect sign-in looks like this:

```yaml
base_domain: art.example.com
data_dir: /var/lib/kazari

tls:
  mode: acme
  email: ops@example.com
  dns_provider: cloudflare
  dns:
    api_token: ${KAZARI_TLS_DNS_API_TOKEN}

auth:
  admin_token: ${KAZARI_AUTH_ADMIN_TOKEN}
  oidc:
    issuer: https://login.microsoftonline.com/<tenant-id>/v2.0
    client_id: 00000000-0000-0000-0000-000000000000
    client_secret: ${KAZARI_AUTH_OIDC_CLIENT_SECRET}
    allowed_domains: [example.com]
```

Each `${NAME}` is read from the environment, so the file holds no secrets.

### Environment references

Any value can refer to an environment variable:

| Written             | Becomes                                                     |
| ------------------- | ----------------------------------------------------------- |
| `${NAME}`           | The value of `NAME`                                         |
| `${NAME:-default}`  | The value of `NAME`, falling back to `default`              |
| `$$`                | A literal `$`                                               |

A reference to a variable that is not set and has no default stops startup with an error naming the variable. A default makes the reference optional:

```yaml
tls:
  ca: ${KAZARI_TLS_CA:-letsencrypt}
```

### The `.env` file

`kazari serve` also loads variables from a dotenv file. It uses the first of:

1. `--env-file PATH`.
2. `$KAZARI_ENV_FILE`.
3. `./.env` in the working directory, if it exists.

The file has one `KEY=VALUE` per line. `#` starts a comment, `export ` before the key is allowed, and a value can be wrapped in single or double quotes:

```sh
# .env
KAZARI_AUTH_ADMIN_TOKEN=3f9c2e...
export KAZARI_TLS_DNS_API_TOKEN="your-cloudflare-token"
KAZARI_LISTEN_PROXY_MODE=true
```

A variable already set in the process environment wins over the file. Variables from the file feed both `${NAME}` references and the [`KAZARI_*` overrides](#overriding-keys-from-the-environment).

The file holds secrets, so keep it mode `0600` and out of version control. Under systemd, the unit reads `/etc/kazari/kazari.env` with `EnvironmentFile=` instead, so those variables arrive as process environment. Under Docker Compose, `compose.yml` passes `.env` into the container the same way.

The CLI reads `./.env` too, for its own variables: see [CLI configuration](cli.md#configuring-the-client).

## Keeping secrets out of the file

Three keys are secrets: `tls.dns.api_token`, `auth.admin_token` and `auth.oidc.client_secret`. The settings endpoint shows them as `"***"`.

Never write them into the YAML file, which must be world-readable under systemd. Refer to them as `${NAME}`, or leave them out and set the `KAZARI_*` override. Then supply the variables from one of these places:

- `/etc/kazari/kazari.env` for the systemd unit, with mode `0600`. systemd reads it as root.
- `.env` beside `compose.yml`, or in the working directory in development.
- `--env-file` for any other setup.
- A file per secret, as the next section describes.

### Reading a secret from a file

Docker, Swarm and Kubernetes mount secrets as files. For each secret key, the variable with `_FILE` added to its name takes the path of a file holding the value, without its trailing newline:

```sh
KAZARI_TLS_DNS_API_TOKEN_FILE=/run/secrets/dns-token
```

The plain variable wins when both are set. A file that cannot be read stops startup with an error naming the variable.

### The admin token

On the server's host, `kazari admin` commands reach the admin API over the Unix socket in the data directory and need no token ([`admin`](#admin)). The admin token is for administering from elsewhere, or from scripts that run before any user exists. Generate it with `openssl`:

```sh
openssl rand -hex 32
```

It prints 64 hexadecimal characters. A token shorter than 32 characters stops startup, unless `base_domain` is `localhost` or ends in `.localhost`.

To retire the token once an admin user has an API key with the `admin` scope ([administering from another machine](install.md#administering-from-another-machine)):

1. Delete the `admin_token` line from the file, if there is one. Otherwise its `${}` reference to a variable that is no longer set stops startup.
2. Unset `KAZARI_AUTH_ADMIN_TOKEN`.
3. Restart the server.

## Overriding keys from the environment

Every key has an environment variable: `KAZARI_` followed by the key's full path, upper-cased, with dots replaced by underscores. For example:

| Key                        | Variable                          |
| -------------------------- | --------------------------------- |
| `base_domain`              | `KAZARI_BASE_DOMAIN`              |
| `listen.proxy_mode`        | `KAZARI_LISTEN_PROXY_MODE`        |
| `tls.dns_provider`         | `KAZARI_TLS_DNS_PROVIDER`         |
| `tls.dns.api_token`        | `KAZARI_TLS_DNS_API_TOKEN`        |
| `limits.max_version_bytes` | `KAZARI_LIMITS_MAX_VERSION_BYTES` |
| `auth.oidc.allowed_domains`| `KAZARI_AUTH_OIDC_ALLOWED_DOMAINS`|

An override wins over the file, whether or not the file mentions the key. A deployment configured entirely from the environment therefore needs no file at all.

The secrets in `deploy/kazari.yaml` use the same names in their `${}` references, so one variable serves either way. A secret also reads the variable with `_FILE` added ([reading a secret from a file](#reading-a-secret-from-a-file)).

Setting any `KAZARI_AUTH_OIDC_*` variable switches on the `auth.oidc` section, with its defaults, even when the file has none.

> [!NOTE]
> The CLI has variables of its own: `KAZARI_URL`, `KAZARI_TOKEN`, `KAZARI_ADMIN_URL` and `KAZARI_ADMIN_TOKEN` configure the client (see the [CLI reference](cli.md)). The server's admin token is `KAZARI_AUTH_ADMIN_TOKEN`.

### Value formats

An override's value is a string. Keys that are not strings take these formats:

- **Booleans**: `true`, `false`, `1`, `0`, or anything else Go's `strconv.ParseBool` takes.
- **Lists**: comma-separated, with spaces trimmed: `KAZARI_AUTH_OIDC_ALLOWED_DOMAINS=example.com,example.org`. A variable replaces the whole list.
- **Sizes**: an integer with an optional unit, case-insensitive, with an optional space. `B`, `KiB`, `MiB`, `GiB` and `TiB` are powers of 1024; `KB`, `MB`, `GB` and `TB` are powers of 1000. For example `"50MiB"`, `"2 GiB"` or `52428800`.
- **Durations**: Go durations such as `"15m"`, `"336h"` or `"1h30m"`, with an optional leading whole number of days: `"14d"`, `"1d12h"`.

## Checking the configuration

`kazari check` loads the configuration exactly as `serve` does, lists every problem if it is invalid, and otherwise checks what the first start depends on: the data directory, the DNS records, the DNS provider token and the ports. Run it before the first start and whenever something changes:

```sh
sudo kazari check
```

It prints one line per check and exits with status 1 if any failed, or 2 if the configuration is invalid; [checking the setup](install.md#checking-the-setup) shows the output.

To see the configuration a running server uses, ask the admin API:

```sh
kazari admin settings
```

It prints the effective configuration, with secrets redacted. The same is available as `GET /v1/settings` on the [admin API](api.md#admin-api).

## Keys

Every key, by section, with its type and default.

### Top level

The domain, the data directory and the shape of public URLs:

| Key               | Type    | Default  | Meaning                                                              |
| ----------------- | ------- | -------- | -------------------------------------------------------------------- |
| `base_domain`     | string  | none     | Domain artifacts live under, as `<slug>.<base_domain>`. Required.    |
| `data_dir`        | string  | none     | Directory that holds all state. Required.                            |
| `external_scheme` | string  | `https`  | Scheme of the public URLs Kazari prints and returns: `https` or `http`. |
| `external_port`   | integer | `0`      | Port appended to public URLs; `0` omits it.                          |

`base_domain` is a lower-case host name, with no scheme, port or trailing dot.

`data_dir` holds `kazari.db`, `blobs/`, `uploads/`, `certmagic/` and the admin socket. The systemd unit and the Docker image set it to `/var/lib/kazari`; `serve --local` uses `./.data`.

`external_port` is appended only when it is not the scheme's default: `8080` gives `http://demo.localhost:8080/`, as under `--local`. `external_scheme` and `external_port` only shape the URLs in responses; they do not change what Kazari listens on.

### `listen`

Where Kazari accepts connections:

| Key          | Type   | Default | Meaning                                                    |
| ------------ | ------ | ------- | ---------------------------------------------------------- |
| `https`      | string | `:443`  | TLS listener address. Required unless `proxy_mode` is on.  |
| `http`       | string | `:80`   | Plain HTTP listener; `""` disables it.                     |
| `proxy_mode` | bool   | `false` | Serve plain HTTP behind one TLS-terminating proxy.         |

Outside proxy mode, the `http` listener answers `/health` and redirects every request for the base domain or its subdomains to HTTPS with a `301`. In proxy mode it is the only listener, and it is required.

In proxy mode Kazari serves plain HTTP on `http` and takes the client IP from the last `X-Forwarded-For` entry, then from `X-Real-IP`. [Running behind a reverse proxy](install.md#running-behind-a-reverse-proxy) sets it up, and `serve --local` uses it.

### `tls`

Where the certificate comes from. [How the certificate is obtained](certificates.md#how-the-certificate-is-obtained) explains the three modes:

| Key            | Type   | Default       | Meaning                                                                         |
| -------------- | ------ | ------------- | ------------------------------------------------------------------------------- |
| `mode`         | string | `acme`        | `acme`, `files` or `self-signed`.                                               |
| `email`        | string | none          | ACME account contact. Required for `acme` outside proxy mode.                   |
| `ca`           | string | `letsencrypt` | `letsencrypt`, `letsencrypt-staging` or an `https` ACME directory URL.          |
| `dns_provider` | string | `cloudflare`  | DNS-01 provider: `cloudflare`, `route53`, `hetzner`, `digitalocean`, `gandi`, `ovh`, `namecheap` or `duckdns`. |
| `cert_file`    | string | `""`          | PEM certificate chain for `files`. Must cover the base domain and its wildcard. |
| `key_file`     | string | `""`          | PEM private key for `files`.                                                    |

### `tls.dns`

The DNS provider's credentials for `acme`. [Providers](certificates.md#providers) says what each provider needs:

| Key         | Type   | Default | Meaning                                                                            |
| ----------- | ------ | ------- | ---------------------------------------------------------------------------------- |
| `api_token` | secret | `""`    | The provider's credential. Required for every provider but `route53`.               |
| `endpoint`  | string | `""`    | `ovh` only: `ovh-eu` (the default when empty), `ovh-ca` or `ovh-us`.               |
| `user`      | string | `""`    | `namecheap` only: the API user. Required.                                          |
| `client_ip` | string | `""`    | `namecheap` only: the allow-listed IP; the server's public address when empty.     |

For `route53`, leave `api_token` empty to use the AWS default credential chain, or set it to `ACCESS_KEY_ID:SECRET_ACCESS_KEY`. For `ovh`, `api_token` is `APPLICATION_KEY:APPLICATION_SECRET:CONSUMER_KEY`.

### `limits`

Sizes, counts and rates per version, user and credential:

| Key                     | Type     | Default | Meaning                                                                  |
| ----------------------- | -------- | ------- | ------------------------------------------------------------------------ |
| `max_version_bytes`     | size     | `50MiB` | Total size of all files in one version.                                  |
| `max_file_bytes`        | size     | `25MiB` | Size of one file.                                                        |
| `max_files`             | integer  | `2000`  | Files in one version.                                                    |
| `metadata_max_pairs`    | integer  | `64`    | Metadata key/value pairs per artifact.                                   |
| `write_rate_per_minute` | integer  | `60`    | Write and delete requests per minute per credential.                     |
| `default_user_quota`    | size     | `2GiB`  | Quota given to users created without one.                                |
| `inline_guidance_bytes` | size     | `2MiB`  | Size above which a model should switch to chunked uploads. Advisory.     |
| `max_chunk_bytes`       | size     | `4MiB`  | Largest chunk of a chunked upload, after base64 decoding on MCP.         |
| `upload_ttl`            | duration | `1h`    | How long a chunked upload stays open before the `uploads-expire` job removes it. |
| `max_open_uploads`      | integer  | `5`     | Open chunked uploads per user.                                           |

`max_version_bytes` also bounds a request body and a bundle's decompressed tar stream, each with 1 MiB of headroom, and one chunked upload.

`write_rate_per_minute` allows a burst of the same size. It is one budget across the user API and the MCP write tools. Beyond it, the API answers `429` and MCP returns a `rate_limited` error.

`default_user_quota` applies to users created without a quota, whether by an admin or by sign-in. See [quotas](ops.md#quotas).

`inline_guidance_bytes` is quoted in the MCP tool descriptions, and nothing enforces it.

### `lifecycle`

How long stale and deleted artifacts last, and how often the jobs that retire them run:

| Key              | Type     | Default            | Meaning                                                                           |
| ---------------- | -------- | ------------------ | --------------------------------------------------------------------------------- |
| `default_grace`  | duration | `336h` (14 days)   | Grace period of a `stale` call that names none.                                   |
| `retention`      | duration | `720h` (30 days)   | Time a soft-deleted artifact stays restorable before the sweep purges it.         |
| `tombstone`      | duration | `2160h` (90 days)  | Time a purged slug stays reserved, answering `410`, before it is freed.           |
| `sweep_interval` | duration | `15m`              | How often the `sweep` job runs.                                                   |
| `gc_interval`    | duration | `1h`               | How often the `gc` job deletes unreferenced blobs.                                |

[The page lifecycle](ops.md#lifecycle) shows how these combine.

### `serve`

How artifacts are served:

| Key                  | Type        | Default   | Meaning                                                                          |
| -------------------- | ----------- | --------- | -------------------------------------------------------------------------------- |
| `csp`                | string      | see below | `Content-Security-Policy` sent with every artifact response; `""` sends none.    |
| `allow_csp_override` | bool        | `true`    | Let an artifact's `kazari.csp` metadata replace `csp` for its own files.         |
| `reserved_slugs`     | string list | see below | Slugs no artifact can take. Each must be non-empty and lower-case.               |
| `default_visibility` | string      | `public`  | Visibility of an artifact created without one: `public`, `unlisted` or `members`. |
| `share_link_max_ttl` | duration    | `90d`     | The longest a share link can stay valid.                                         |

#### The default CSP

The default policy allows inline scripts and styles plus the common CDNs:

```text
default-src 'self'; script-src 'self' 'unsafe-inline' https://cdnjs.cloudflare.com https://cdn.jsdelivr.net https://unpkg.com; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com; img-src 'self' data: https:; connect-src 'self'; frame-ancestors 'none'
```

`frame-ancestors` in `csp` also decides whether the MCP preview app embeds pages. With `'none'`, the default, or `'self'`, it shows links only. With an MCP host's app origins listed, it embeds the published version. See [the preview app](mcp.md#preview-app).

#### Per-artifact CSP

Each artifact is its own origin, so a looser per-artifact CSP only affects that artifact's visitors. On a multi-tenant instance, consider `allow_csp_override: false`.

#### Reserved slugs

The default reserved slugs are `api`, `mcp`, `admin`, `www`, `mail`, `ns1`, `ns2`, `autoconfig`, `autodiscover` and `localhost`. If you replace the list, keep `api`, `mcp` and `admin`, which are Kazari's own hosts.

#### Default visibility

On an instance that is not meant for the open web, set `default_visibility: members`. A page an agent publishes without saying otherwise then opens only for people who sign in, and someone outside the organisation needs a share link. [Viewing members pages](identity.md#viewing-members-pages) explains how viewers sign in.

> [!WARNING]
> `unlisted` is not private. Slugs are easy to guess, so anyone can open an unlisted page whose address they guess.

#### Share link lifetime

Share links last 7 days unless asked otherwise, or `share_link_max_ttl` if that is shorter.

### `fetch`

The URLs Kazari fetches for members: a file named by `url` in a create or update, and the page an [import](api.md#importing-a-web-page) copies. Fetches go to public addresses only.

| Key       | Type     | Default | Meaning                                                                       |
| --------- | -------- | ------- | ----------------------------------------------------------------------------- |
| `enabled` | bool     | `true`  | `false` removes the `import_page` tool and refuses files named by `url`.      |
| `timeout` | duration | `15s`   | The longest one request may take.                                             |
| `budget`  | duration | `1m`    | The longest one import may take in all; what does not fit stays remote.       |

Set `enabled: false` on an instance that must make no outbound requests on members' behalf. The fetch for OAuth client metadata is separate and stays on.

### `auth`

Who can change artifacts, and the admin token:

| Key            | Type   | Default  | Meaning                                                                                       |
| -------------- | ------ | -------- | --------------------------------------------------------------------------------------------- |
| `write_policy` | string | `shared` | `shared`: anyone with `artifacts:write` can change any artifact. `owner`: only the owner and admins can. |
| `admin_token`  | secret | `""`     | Bearer token accepted on the admin host.                                                      |

An empty `admin_token` disables it, leaving the admin socket and admin users' credentials that carry the `admin` scope. [The admin token](#the-admin-token) covers generating and retiring it.

### `auth.oidc`

With this section, users sign in through an OpenID Connect provider. Without it, the OAuth consent page asks for an API key instead. [Sign-in and identity](identity.md) walks through setting up a provider.

The section's keys:

| Key               | Type        | Default | Meaning                                                                                            |
| ----------------- | ----------- | ------- | -------------------------------------------------------------------------------------------------- |
| `issuer`          | string      | none    | The provider's issuer URL, exactly as its discovery document states it. Must be `https`. Required. |
| `client_id`       | string      | none    | Client ID of the app registered at the provider. Required.                                         |
| `client_secret`   | secret      | none    | Client secret. Required.                                                                           |
| `allowed_domains` | string list | `[]`    | Verified email domains that can sign in.                                                           |
| `auto_provision`  | bool        | `true`  | Create a member on first sign-in from an allowed domain.                                           |
| `admin_emails`    | string list | `[]`    | Emails provisioned with the admin role.                                                            |

### `admin`

The admin socket for this host, and an optional separate listener for the admin API:

| Key      | Type   | Default      | Meaning                                                                                       |
| -------- | ------ | ------------ | --------------------------------------------------------------------------------------------- |
| `socket` | string | `admin.sock` | Unix socket serving the admin API without a credential; relative to `data_dir` unless absolute. `""` disables it. |
| `listen` | string | `""`         | Optional plain-HTTP listener for the admin API, `/health` and `/metrics`, such as `127.0.0.1:9443`. |

The socket is created with mode `0600`, so only the service user and root can use it. The `kazari` command on the same host finds it through the server's configuration (`$KAZARI_CONFIG`, `/etc/kazari/kazari.yaml` or `$KAZARI_DATA_DIR`) and uses it for every `admin` command with no token ([admin commands](cli.md#admin-commands)). Requests over it act as the `admin` principal, with no rate limit.

The listener serves `/v1`, `/health` and `/metrics` whatever the `Host` header. When it is set, `admin.<base>` is no longer served on the public listener and answers `404` there, so point a remote admin CLI at this address.

### `log`

Log level and format:

| Key      | Type   | Default | Meaning                              |
| -------- | ------ | ------- | ------------------------------------ |
| `level`  | string | `info`  | `debug`, `info`, `warn` or `error`.  |
| `format` | string | `text`  | `text` or `json` (slog handlers).    |

## Validation rules

Kazari checks all of these together at startup. The error lists every failure as `key: problem`, and the server exits with status 2:

- `base_domain` is required, lower-case, without scheme, port or trailing dot, and made of labels of letters, digits and hyphens.
- `data_dir` is required.
- `external_scheme` is `http` or `https`, and `external_port` is 0 to 65535.
- Outside proxy mode `listen.https` is required; in proxy mode `listen.http` is required.
- `tls.mode` is one of the three modes. Outside proxy mode:
  - `acme` needs `tls.email`, a valid `tls.ca`, a known `tls.dns_provider` and, except for `route53`, `tls.dns.api_token`; `namecheap` also needs `tls.dns.user`.
  - `files` needs `tls.cert_file` and `tls.key_file`.
- Every number and duration in `limits`, `lifecycle` and `fetch` is greater than 0, and `fetch.budget` is at least `fetch.timeout`.
- `limits.max_file_bytes` and `limits.max_chunk_bytes` do not exceed `limits.max_version_bytes`.
- `serve.default_visibility` is `public`, `unlisted` or `members`, and `serve.share_link_max_ttl` is greater than 0.
- `auth.write_policy` is `shared` or `owner`.
- `auth.admin_token`, when set, is at least 32 characters, unless `base_domain` is `localhost` or ends in `.localhost`: a development instance that only its own machine can reach.
- With `auth.oidc`, `issuer` is an `https` URL, and `client_id` and `client_secret` are set.
- `log.level` and `log.format` are among the listed values.

Because every number in `limits` must be greater than 0, `write_rate_per_minute` cannot be set to 0 to turn off rate limiting, and `default_user_quota` cannot be 0. To give one user an unlimited quota, set it through the admin API instead:

```sh
kazari admin users update peter --quota 0
```
