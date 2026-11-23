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

Without a file, an incomplete configuration fails with a note saying where Kazari looked, and that `--local` runs it on your own machine with nothing to configure.

A key the server does not know is an error, with the closest known key when there is one (`unknown key base_domian; did you mean base_domain?`), so a typo fails loudly rather than being ignored. Every error in the file names its line and key, and Kazari lists them all at once:

```text
config: /etc/kazari/kazari.yaml: line 12: limits.max_files: want a whole number, got "lots"
```

### The local preset

`kazari serve --local` reads no file. It starts from a preset for a server on your own machine: plain HTTP on `127.0.0.1:8080` in proxy mode, the base domain `kazari.localhost`, `external_scheme: http` with `external_port: 8080`, the data in `./.data`, and `log.level: debug`. `KAZARI_*` variables apply on top of it, so `KAZARI_DATA_DIR` moves the data and `KAZARI_LISTEN_HTTP=:8080` makes it reachable from outside a container. `--local` and `--config` exclude each other.

Under `--local`, `external_port` follows the port of `listen.http` unless `KAZARI_EXTERNAL_PORT` is set too, so `KAZARI_LISTEN_HTTP=127.0.0.1:18080` moves the printed URLs and the setup link to port 18080 as well.

### Precedence

From lowest to highest:

1. Built-in defaults.
2. The YAML file, after `${}` references are expanded.
3. `KAZARI_*` overrides, from the process environment or a [dotenv file](#the-env-file).

Validation then checks the result.

## Writing the file

Sections are nested mappings, with the keys and their nesting listed under [Keys](#keys). Sizes and durations are plain strings, such as `50MiB` and `336h`, and lists are YAML sequences. A production file with ACME certificates and OpenID Connect sign-in looks like this:

```yaml
base_domain: art.example.com

tls:
  mode: acme
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

When `NAME` is unset or empty and `NAME_FILE` is set, the reference takes the contents of the file `NAME_FILE` names, as for [secrets in files](#reading-a-secret-from-a-file). A reference to a variable that is not set and has no default stops startup with an error naming the variable and its `_FILE` alternative. A default makes the reference optional:

```yaml
tls:
  ca: ${KAZARI_TLS_CA:-letsencrypt}
```

### The `.env` file

`kazari serve` and `kazari check` also load variables from a dotenv file. They use the first of:

1. `--env-file PATH`.
2. `$KAZARI_ENV_FILE`.
3. `kazari.env` beside the configuration file, if it exists: `/etc/kazari/kazari.env` for `/etc/kazari/kazari.yaml`.
4. `./.env` in the working directory, if it exists.

The file has one `KEY=VALUE` per line. `#` starts a comment, `export ` before the key is allowed, and a value can be wrapped in single or double quotes:

```sh
# .env
KAZARI_AUTH_ADMIN_TOKEN=3f9c2e...
export KAZARI_TLS_DNS_API_TOKEN="your-cloudflare-token"
KAZARI_LISTEN_PROXY_MODE=true
```

A variable already set in the process environment wins over the file. Variables from a file named by `--env-file` or `$KAZARI_ENV_FILE`, or from `kazari.env` beside the configuration file, feed both `${NAME}` references and the [`KAZARI_*` overrides](#overriding-keys-from-the-environment).

A `./.env` found in the working directory does the same only when no configuration file is in use, which includes `--local`. Beside a configuration file (`--config`, `$KAZARI_CONFIG` or `/etc/kazari/kazari.yaml`) it feeds `${NAME}` references alone, so `${KAZARI_TLS_DNS_API_TOKEN}` still works but `KAZARI_LISTEN_PROXY_MODE` above would be ignored: a file that merely sits in the directory `serve` or `check` runs from cannot change where the configured credentials go. `kazari check` and the start log warn about each `KAZARI_*` variable such a file held for nothing. To override keys from a dotenv file, name it, or move the lines to `kazari.env` beside the configuration file.

The file holds secrets, so keep it mode `0600` and out of version control. Under systemd, the unit reads `/etc/kazari/kazari.env` with `EnvironmentFile=`, so those variables arrive as process environment; `sudo kazari check` and `sudo kazari admin` read the same file because it sits beside `kazari.yaml`. The service itself cannot read the file, and skips it without an error. Under Docker Compose, `compose.yml` passes `.env` into the container as process environment.

`kazari check` prints the files it read on its first line, and the `kazari started` log line names them too.

The client commands do not read `./.env`, only a file named by `$KAZARI_ENV_FILE`: see [CLI configuration](cli.md#environment-variables-and-env).

## Keeping secrets out of the file

Three keys are secrets: `tls.dns.api_token`, `auth.admin_token` and `auth.oidc.client_secret`. The settings endpoint shows them as `"***"`.

Never write them into the YAML file, which must be world-readable under systemd. Refer to them as `${NAME}`, or leave them out and set the `KAZARI_*` override. Then supply the variables from one of these places:

- `/etc/kazari/kazari.env` for the systemd unit, with mode `0600`. systemd reads it as root.
- `.env` beside `compose.yml`, or in the working directory in development, referred to as `${NAME}` when a configuration file is in use.
- `--env-file` for any other setup.
- A file per secret, as the next section describes.

### Reading a secret from a file

Docker, Swarm and Kubernetes mount secrets as files. For each secret key, the variable with `_FILE` added to its name takes the path of a file holding the value, without its trailing newline:

```sh
KAZARI_TLS_DNS_API_TOKEN_FILE=/run/secrets/dns-token
```

The plain variable wins when both are set. A file that cannot be read stops startup with an error naming the variable. The `_FILE` variable also fills a `${KAZARI_TLS_DNS_API_TOKEN}` reference in the file, so the shipped `kazari.yaml` works with it unchanged.

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
| `auth.local.invite_ttl`    | `KAZARI_AUTH_LOCAL_INVITE_TTL`    |
| `auth.oidc.allowed_domains`| `KAZARI_AUTH_OIDC_ALLOWED_DOMAINS`|
| `admin.public_url`         | `KAZARI_ADMIN_PUBLIC_URL`         |

An override wins over the file, whether or not the file mentions the key. A deployment configured entirely from the environment therefore needs no file at all.

The secrets in `deploy/kazari.yaml` use the same names in their `${}` references, so one variable serves either way. A secret also reads the variable with `_FILE` added ([reading a secret from a file](#reading-a-secret-from-a-file)).

Setting any `KAZARI_AUTH_OIDC_*` variable to a value that is not empty switches on the `auth.oidc` section, with its defaults, even when the file has none. An empty one, such as `KAZARI_AUTH_OIDC_CLIENT_SECRET=` left in a dotenv file, does not. When the section is incomplete, the error names what switched it on.

> [!NOTE]
> The CLI has variables of its own: `KAZARI_URL`, `KAZARI_TOKEN`, `KAZARI_ADMIN_URL` and `KAZARI_ADMIN_TOKEN` configure the client (see the [CLI reference](cli.md)). The server's admin token is `KAZARI_AUTH_ADMIN_TOKEN`, and the dashboard's address is `KAZARI_ADMIN_PUBLIC_URL`; the server reads neither `KAZARI_ADMIN_URL` nor `KAZARI_ADMIN_TOKEN`.

### Value formats

An override's value is a string. Keys that are not strings take these formats:

- **Booleans**: `true`, `false`, `1`, `0`, or anything else Go's `strconv.ParseBool` takes.
- **Lists**: comma-separated, with spaces trimmed: `KAZARI_AUTH_OIDC_ALLOWED_DOMAINS=example.com,example.org`. A variable replaces the whole list.
- **Sizes**: an integer with an optional unit, case-insensitive, with an optional space. `B`, `KiB`, `MiB`, `GiB` and `TiB` are powers of 1024; `KB`, `MB`, `GB` and `TB` are powers of 1000. For example `"50MiB"`, `"2 GiB"` or `52428800`.
- **Durations**: Go durations such as `"15m"`, `"336h"` or `"1h30m"`, led by whole weeks and days if wanted: `"2w"`, `"14d"`, `"1d12h"`.

## Checking the configuration

`kazari check` loads the configuration exactly as `serve` does, lists every problem if it is invalid, and otherwise checks what the first start depends on: the data directory, the DNS records, the DNS provider token, the certificate files, the ports and the identity provider. Run it before the first start and whenever something changes:

```sh
sudo kazari check
```

It prints one line per check and exits with status 1 if any failed, or 2 if the configuration is invalid; [checking the setup](install.md#checking-the-setup) shows the output. After the `config` line, an `override` line names each key a `KAZARI_*` variable set, and where that variable came from:

```text
override     ok    data_dir from KAZARI_DATA_DIR in /etc/kazari/kazari.env, over line 3 of /etc/kazari/kazari.yaml
```

To see the configuration a running server uses, ask the admin API:

```sh
kazari admin settings
```

It prints the effective configuration, with secrets redacted, and after each value that is not a default where it was set: a line of the file or a `KAZARI_*` variable. A key whose default depends on another shows the value in use: `auth.local.enabled` is `true` or `false` there, whether or not the file sets it. The same is available as `GET /v1/settings` and `GET /v1/settings/sources` on the [admin API](api.md#settings).

## Keys

Every key, by section, with its type and default.

### Top level

The domain, the data directory and the shape of public URLs:

| Key               | Type    | Default  | Meaning                                                              |
| ----------------- | ------- | -------- | -------------------------------------------------------------------- |
| `base_domain`     | string  | none     | Domain artifacts live under, as `<slug>.<base_domain>`. Required.    |
| `data_dir`        | string  | `/var/lib/kazari` | Directory that holds all state.                             |
| `external_scheme` | string  | `https`  | Scheme of the public URLs Kazari prints and returns: `https` or `http`. |
| `external_port`   | integer | `0`      | Port appended to public URLs; `0` omits it.                          |

`base_domain` is a lower-case host name, with no scheme, port or trailing dot. The placeholder `example.com`, `example.org` and `example.net`, and any name under them, fail validation, so a copied example cannot start.

`data_dir` holds `kazari.db`, `blobs/`, `uploads/`, `certmagic/`, the admin socket and `kazari.lock`. The default is the directory the systemd unit and the Docker image provide; `serve --local` uses `./.data`. One server at a time holds the directory: a second one started on it stops with `another kazari (pid N) is using /var/lib/kazari`.

`external_port` is appended only when it is not the scheme's default: `8080` gives `http://demo.kazari.localhost:8080/`, as under `--local`. `external_scheme` and `external_port` only shape the URLs in responses; they do not change what Kazari listens on.

### `listen`

Where Kazari accepts connections:

| Key          | Type   | Default | Meaning                                                    |
| ------------ | ------ | ------- | ---------------------------------------------------------- |
| `https`      | string | `:443`  | TLS listener address. Required unless `proxy_mode` is on.  |
| `http`       | string | `:80`   | Plain HTTP listener; `""` disables it.                     |
| `proxy_mode` | bool   | `false` | Serve plain HTTP behind one TLS-terminating proxy.         |

Each address is `host:port`. A bare port such as `8080` fails validation: write `127.0.0.1:8080` for this machine only, or `:8080` for every address.

Outside proxy mode, the `http` listener answers `/health` and redirects every request for the base domain or its subdomains to HTTPS with a `301`. In proxy mode it is the only listener, and it is required.

In proxy mode Kazari serves plain HTTP on `http` and takes the client IP from the last `X-Forwarded-For` entry, then from `X-Real-IP`. Bind `http` where only the proxy reaches it, such as `127.0.0.1:8080`; `kazari check` warns about an address that is neither loopback nor private. `/health` answers on any `Host` outside the base domain, so a probe can target the backend by its address. [Running behind a reverse proxy](install.md#running-behind-a-reverse-proxy) sets it up, and `serve --local` uses it.

### `tls`

Where the certificate comes from. [How the certificate is obtained](certificates.md#how-the-certificate-is-obtained) explains the three modes:

| Key            | Type   | Default       | Meaning                                                                         |
| -------------- | ------ | ------------- | ------------------------------------------------------------------------------- |
| `mode`         | string | `acme`        | `acme`, `files` or `self-signed`.                                               |
| `email`        | string | `""`          | ACME account contact, optional. Empty registers an account without one.          |
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
| `inline_guidance_bytes` | size     | `2MiB`  | Size above which a model should switch to an upload. Advisory.           |
| `max_chunk_bytes`       | size     | `4MiB`  | Largest chunk of a chunked upload, after base64 decoding on MCP.         |
| `upload_ttl`            | duration | `1h`    | How long an upload, and the archive URL of one, stays open before the `uploads-expire` job removes it. |
| `max_open_uploads`      | integer  | `5`     | Open uploads per user.                                                   |

`max_version_bytes` also bounds a request body and a bundle's decompressed tar stream, each with 1 MiB of headroom, one upload, and the archive sent to an upload's archive URL.

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
| `allow_csp_override` | bool        | `true`    | Let an artifact's `kazari.csp` metadata add sources to `csp` for its own files.  |
| `reserved_slugs`     | string list | see below | Slugs no artifact can take or be served under. Each must be non-empty and lower-case. |
| `default_visibility` | string      | `public`  | Visibility of an artifact created without one: `public`, `unlisted` or `members`. |
| `share_link_max_ttl` | duration    | `90d`     | The longest a share link can stay valid.                                         |

#### The default CSP

The default policy allows inline scripts and styles plus the common CDNs:

```text
default-src 'self'; script-src 'self' 'unsafe-inline' https://cdnjs.cloudflare.com https://cdn.jsdelivr.net https://unpkg.com; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com https://cdnjs.cloudflare.com https://cdn.jsdelivr.net https://unpkg.com; font-src 'self' https://fonts.gstatic.com https://cdnjs.cloudflare.com https://cdn.jsdelivr.net https://unpkg.com; img-src 'self' data: https:; connect-src 'self'; frame-ancestors 'none'
```

`frame-ancestors` in `csp` also decides whether the MCP preview app embeds pages. With `'none'`, the default, or `'self'`, it shows links only. With an MCP host's app origins listed, it embeds the published version. See [the preview app](mcp.md#preview-app).

#### Per-artifact CSP

An artifact adds origins to the instance policy with its `kazari.csp` metadata, for a library or an embed the default does not allow. The value lists fetch directives and the sources to add, such as `frame-src https://www.youtube.com; style-src https://cdn.example.com`.

Each source is added to that directive. A directive the instance policy does not list starts from its `default-src` sources. The other directives, among them `frame-ancestors`, `base-uri` and `form-action`, stay as `csp` sets them. A source must name an origin: `*` and a bare scheme such as `https:` are refused.

To keep every artifact on the instance policy, set `allow_csp_override: false`. A publish that sets `kazari.csp` is then refused with `invalid_metadata`, rather than accepted and ignored.

#### Reserved slugs

The default reserved slugs are `api`, `mcp`, `admin`, `www`, `mail`, `ns1`, `ns2`, `autoconfig`, `autodiscover` and `localhost`. If you replace the list, keep `api`, `mcp` and `admin`, which are Kazari's own hosts.

A reserved slug cannot be created, and an artifact that already has it is not served: adding a slug to the list takes an existing artifact with that slug offline at `<slug>.<base>` and on its version hosts, until the slug is removed from the list again.

#### Default visibility

On an instance that is not meant for the open web, set `default_visibility: members`. A page an agent publishes without saying otherwise then opens only for people who sign in, and someone outside the organisation needs a share link. [Viewing members pages](identity.md#viewing-members-pages) explains how viewers sign in.

> [!WARNING]
> `unlisted` is not private. Slugs are easy to guess, so anyone can open an unlisted page whose address they guess.

#### Share link lifetime

Share links last 7 days unless asked otherwise, or `share_link_max_ttl` if that is shorter.

#### Compression

Kazari compresses text files itself, so pages load faster without a proxy that does it. There is no key to set: a file of 1 KiB or more is sent gzip-compressed to every client whose `Accept-Encoding` allows gzip. HTML, CSS, JavaScript, JSON, XML, SVG, WebAssembly and TrueType and OpenType fonts qualify; images, WOFF fonts, archives and media are compressed already and are sent as stored.

The compressed answer has its own `ETag`, the stored file's hash with `-gzip` appended, and every answer for such a file carries `Vary: Accept-Encoding`, so a cache in front keeps the two forms apart. A request with a `Range` header gets the stored bytes, as do `If-Range` and `If-Match` requests. A proxy in front that compresses leaves these answers as they are.

### `fetch`

The URLs Kazari fetches for members: a file named by `url` in a create or update, and the page an [import](api.md#importing-a-web-page) copies. Fetches go to public addresses only.

| Key       | Type     | Default | Meaning                                                                       |
| --------- | -------- | ------- | ----------------------------------------------------------------------------- |
| `enabled` | bool     | `true`  | `false` removes the `import_page` tool and refuses files named by `url`.      |
| `timeout` | duration | `15s`   | The longest one request may take.                                             |
| `budget`  | duration | `1m`    | The longest one import, or the downloads of one publish, may take in all; what does not fit an import stays remote, and a page that cannot be parsed in that time fails the import with `fetch_failed`. |

Set `enabled: false` on an instance that must make no outbound requests on members' behalf. The fetch for OAuth client metadata is separate and stays on.

### `auth`

Who can change artifacts, and the admin token:

| Key            | Type   | Default  | Meaning                                                                                       |
| -------------- | ------ | -------- | --------------------------------------------------------------------------------------------- |
| `write_policy` | string | `shared` | `shared`: anyone with `artifacts:write` can change any artifact. `owner`: only the owner and admins can. |
| `admin_token`  | secret | `""`     | Bearer token accepted on the admin host.                                                      |

An empty `admin_token` disables it, leaving the admin socket and admin users' credentials that carry the `admin` scope. [The admin token](#the-admin-token) covers generating and retiring it.

Under either policy, marking an artifact you do not own stale also takes `artifacts:delete`, because the expiry ends in a purge. Admins are exempt.

### `auth.tokens`

How long OAuth tokens and API keys live:

| Key                    | Type     | Default | Meaning                                                                              |
| ---------------------- | -------- | ------- | ------------------------------------------------------------------------------------ |
| `access_ttl`           | duration | `1h`    | Lifetime of an OAuth access token.                                                   |
| `refresh_ttl`          | duration | `90d`   | Lifetime of an OAuth refresh token, renewed on every refresh.                        |
| `refresh_reuse_window` | duration | `30s`   | How long after a refresh the previous refresh token is accepted once more. `0` never accepts it again. |
| `max_key_ttl`          | duration | `0`     | The longest an API key may live. `0` lets a key be issued without an expiry.         |

An MCP client stays connected for as long as it refreshes at least once every `refresh_ttl`. To keep an agent that runs every three months connected, raise it above the pause:

```yaml
auth:
  tokens:
    refresh_ttl: 180d
```

A change to `access_ttl` or `refresh_ttl` applies to tokens issued from then on; tokens already issued keep their expiry.

`refresh_reuse_window` covers a client that never received the answer to a refresh and sends the same refresh token again ([Tokens, lifetimes and rotation](identity.md#tokens-lifetimes-and-rotation)). It can be at most `5m`.

By default, a key issued without `--expires` never expires. With `max_key_ttl` set, such a key gets that lifetime, and a request for a longer one is refused. Keys issued before the setting keep what they have:

```yaml
auth:
  tokens:
    max_key_ttl: 365d
```

### `auth.local`

Built-in accounts: a password for each person, set through a link that an admin copies and sends. [Built-in accounts](identity.md#built-in-accounts) explains how they work.

The section's keys:

| Key                   | Type     | Default                       | Meaning                                              |
| --------------------- | -------- | ----------------------------- | ---------------------------------------------------- |
| `enabled`             | bool     | `true` without `auth.oidc`, `false` with it | Whether people sign in with a built-in account. |
| `min_password_length` | int      | `15`                          | The shortest password accepted, in characters.       |
| `invite_ttl`          | duration | `7d`                          | How long an invite link works.                       |
| `reset_ttl`           | duration | `1d`                          | How long a password reset link works.                |

#### Turning built-in accounts off

By default, built-in accounts are on when the file has no `auth.oidc` section and off when it has one. Leave the key out unless you want an instance without an identity provider to sign people in by API key alone:

```yaml
auth:
  local:
    enabled: false
```

Built-in accounts and an identity provider never run together, so `enabled: true` beside `auth.oidc` is refused at startup.

#### Password length

By default, a password has at least 15 characters. The value can be from 8 to 256; 256 characters is also the longest password Kazari accepts. Length is the only rule a password has to meet.

A change applies to passwords set from then on. Passwords already set keep working.

#### Link lifetimes

By default, an invite link works for 7 days and a reset link for 24 hours. Both must be greater than 0. A change applies to links made from then on.

The first-run setup link always works for 1 hour.

### `auth.oidc`

With this section, users sign in through an OpenID Connect provider. Without it, they sign in with a [built-in account](identity.md#built-in-accounts), or with an API key when `auth.local.enabled` is `false`. [Sign-in and identity](identity.md) walks through setting up a provider.

The section's keys:

| Key               | Type        | Default | Meaning                                                                                            |
| ----------------- | ----------- | ------- | -------------------------------------------------------------------------------------------------- |
| `issuer`          | string      | none    | The provider's issuer URL, exactly as its discovery document states it. Must be `https`. Required. |
| `client_id`       | string      | none    | Client ID of the app registered at the provider. Required.                                         |
| `client_secret`   | secret      | none    | Client secret. Required.                                                                           |
| `allowed_domains` | string list | `[]`    | Verified email domains that can sign in.                                                           |
| `auto_provision`  | bool        | `true`  | Create a member on first sign-in from an allowed domain.                                           |
| `admin_emails`    | string list | `[]`    | Emails provisioned with the admin role, and the only existing admins who may link by email.        |

### `admin`

The admin socket for this host, an optional separate listener for the admin host, and the [dashboard](dashboard.md):

| Key            | Type   | Default      | Meaning                                                                                       |
| -------------- | ------ | ------------ | --------------------------------------------------------------------------------------------- |
| `socket`       | string | `admin.sock` | Unix socket serving the admin API without a credential; relative to `data_dir` unless absolute. `""` disables it. |
| `listen`       | string | `""`         | Optional plain-HTTP listener for the admin host: the admin API, the dashboard, `/health` and `/metrics`, such as `127.0.0.1:9443`. |
| `dashboard`    | bool   | `true`       | Serve the dashboard at the root of the admin host.                                            |
| `public_url`   | string | `""`         | The address a browser uses for the admin host. Empty means `https://admin.<base_domain>`.     |
| `update_check` | bool   | `true`       | Ask GitHub once a day for the newest release.                                                 |

#### The admin socket

The socket is created with mode `0600`, so only the service user and root can use it. The `kazari` command on the same host finds it through the server's configuration (`$KAZARI_CONFIG`, `/etc/kazari/kazari.yaml` or `$KAZARI_DATA_DIR`) and uses it for every `admin` command with no token ([admin commands](cli.md#admin-commands)). Requests over it act as the `admin` principal, with no rate limit.

#### The separate listener

The listener serves `/v1`, the dashboard, `/health` and `/metrics` whatever the `Host` header, `/metrics` with no credential. When it is set, `admin.<base>` is no longer served on the public listener and answers `404` there, so point a remote admin CLI at this address and set `admin.public_url` for the dashboard.

#### Turning the dashboard off

By default, the admin host serves the dashboard at `/`, beside the admin API under `/v1`. To serve the API alone, turn it off:

```yaml
admin:
  dashboard: false
```

#### The dashboard's address

By default, the dashboard's address is `https://admin.<base_domain>`, built like every other public URL from `external_scheme` and `external_port`. Sign-in for the dashboard returns to this address, and every link the dashboard shows starts with it.

With `admin.listen` set, Kazari cannot know how a browser reaches that listener, so the dashboard is off until `admin.public_url` says. For a listener reached through an SSH tunnel, the settings are:

```yaml
admin:
  listen: "127.0.0.1:9443"
  public_url: http://localhost:9443
```

The value is an origin: a scheme, a host and an optional port, with no path. Plain `http` is accepted only for `localhost`, a name under it, or a loopback address. Any other address must be `https`, which a TLS proxy in front of `admin.listen` provides. [Reaching the dashboard](dashboard.md#reaching-the-dashboard) has the steps.

The same rule holds for the default address. If `external_scheme` is `http` and the base domain is not `localhost`, the default is plain `http` on a public name, and the dashboard is off. The start-up log says so; the admin API is served as before.

#### The update check

By default, the `update-check` job asks GitHub once a day for the newest release, so the dashboard can say when the instance is behind. [Scheduled jobs](ops.md#the-update-check) says what the request carries. To turn it off:

```yaml
admin:
  update_check: false
```

### `analytics`

Page views of artifacts, counted for the [dashboard](dashboard.md#page-views):

| Key         | Type     | Default | Meaning                                                              |
| ----------- | -------- | ------- | -------------------------------------------------------------------- |
| `enabled`   | bool     | `true`  | Count page views of artifacts.                                       |
| `readers`   | bool     | `true`  | Record which members opened a members page, and when.                |
| `retention` | duration | `8784h` | How long daily counts are kept: 366 days. At least `24h`.            |

Kazari counts views in memory and writes them once a minute as daily totals per page and per file. It keeps no request log, IP address or user agent. [Page views](ops.md#page-views) says what counts as a view.

#### Turning analytics off

To count nothing, turn analytics off:

```yaml
analytics:
  enabled: false
```

The dashboard then says that analytics is off, and the counts already stored stay until they pass the retention or analytics is turned on again and the `views-prune` job removes them.

#### Counting without names

On a `members` page Kazari knows who signed in, and by default records each member's first and last view. To keep the counts but not the names, turn readers off:

```yaml
analytics:
  readers: false
```

### `log`

Log level and format:

| Key      | Type   | Default | Meaning                              |
| -------- | ------ | ------- | ------------------------------------ |
| `level`  | string | `info`  | `debug`, `info`, `warn` or `error`.  |
| `format` | string | `text`  | `text` or `json` (slog handlers).    |

### `telemetry`

OpenTelemetry: the Prometheus metrics endpoint, and the export of traces, metrics and logs over OTLP to a collector or vendor.

A key set here wins over the matching `OTEL_*` variable. A key left unset (`""`, `0s`, an empty map or no value) falls back to that variable, then to the OpenTelemetry SDK's default, so Kazari also follows an environment that an operator or a platform sets for every service. The SDK also reads the variables Kazari has no key for, such as `OTEL_RESOURCE_ATTRIBUTES` and the span and batch limits.

| Key            | Type   | Default | Meaning                                                                                   |
| -------------- | ------ | ------- | ----------------------------------------------------------------------------------------- |
| `service_name` | string | `""`    | The resource's `service.name`. Unset: `OTEL_SERVICE_NAME`, else `kazari`.                 |
| `environment`  | string | `""`    | The resource's `deployment.environment.name`, such as `production`. Unset: `OTEL_RESOURCE_ATTRIBUTES`. |

`service.version` is always the release. Kazari logs a warning at startup for a signal switched on with no endpoint, and for an endpoint that sends telemetry in plain text to a host outside the private network.

### `telemetry.otlp`

The one OTLP destination every exported signal shares:

| Key           | Type     | Default | Meaning                                                                                       |
| ------------- | -------- | ------- | --------------------------------------------------------------------------------------------- |
| `endpoint`    | string   | `""`    | The collector's or vendor's base URL, such as `http://otel-collector:4318`. Unset: `OTEL_EXPORTER_OTLP_ENDPOINT`; with neither, nothing is exported. |
| `protocol`    | string   | `""`    | `http/protobuf`, usually on port 4318, or `grpc`, usually on port 4317. Unset: `OTEL_EXPORTER_OTLP_PROTOCOL`, else `http/protobuf`. |
| `headers`     | map      | `{}`    | Headers sent with every export, such as a vendor's API token. A secret. Unset: `OTEL_EXPORTER_OTLP_HEADERS`. |
| `compression` | string   | `""`    | `gzip` or `none`. Unset: `OTEL_EXPORTER_OTLP_COMPRESSION`, else `none`.                       |
| `timeout`     | duration | `0s`    | The longest one export attempt may take. Unset: `OTEL_EXPORTER_OTLP_TIMEOUT`, else `10s`. Failed exports are retried for up to 30 seconds. |
| `ca_file`     | string   | `""`    | PEM roots that verify an `https` endpoint. Unset: `OTEL_EXPORTER_OTLP_CERTIFICATE`, else the system roots. |
| `cert_file`   | string   | `""`    | Client certificate for mutual TLS, reread when it changes. Set with `key_file`. Unset: `OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE`. |
| `key_file`    | string   | `""`    | The client certificate's key. Unset: `OTEL_EXPORTER_OTLP_CLIENT_KEY`.                        |

With `http/protobuf`, Kazari appends the signal's path to the endpoint's, so `https://otlp.example.com/otlp` receives traces at `https://otlp.example.com/otlp/v1/traces`. With `grpc`, the endpoint has no path; `http` connects in plain text and `https` with TLS. The per-signal variables, such as `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`, apply as the OpenTelemetry specification describes: a signal's own endpoint is used as it is.

To keep a vendor's token out of the file, reference it from the dotenv file:

```yaml
telemetry:
  otlp:
    endpoint: https://otlp.example.com
    headers:
      authorization: "Bearer ${KAZARI_OTLP_TOKEN}"
```

As an environment variable, `KAZARI_TELEMETRY_OTLP_HEADERS` takes the form `name=value,name=value`, with the values percent-encoded: `authorization=Bearer%20<token>`.

### `telemetry.traces`

Which requests are traced and whether spans are exported:

| Key                 | Type   | Default | Meaning                                                                                              |
| ------------------- | ------ | ------- | ---------------------------------------------------------------------------------------------------- |
| `otlp`              | bool   | `true`  | Export spans whenever an endpoint is set.                                                            |
| `sample_ratio`      | number | unset   | Share of new traces kept on `api.`, `mcp.`, `admin.`, the admin socket and the account pages, from 0 to 1. |
| `site_sample_ratio` | number | unset   | Share of new traces kept on artifact hosts and the base domain's own page, from 0 to 1.             |

With both ratios unset, `OTEL_TRACES_SAMPLER` decides when it is set; otherwise Kazari keeps every control-plane trace and one page view in a hundred (`1.0` and `0.01`). Setting either ratio uses Kazari's sampler, with the default for the other. A request that arrives on the control plane with a `traceparent` header follows the caller's decision, so a trace is kept or dropped whole.

### `telemetry.metrics`

The metrics endpoint and the optional push of metrics over OTLP:

| Key          | Type     | Default | Meaning                                                                                                 |
| ------------ | -------- | ------- | ------------------------------------------------------------------------------------------------------- |
| `prometheus` | bool     | `true`  | Serve `/metrics` on the admin host ([metrics](ops.md#metrics)).                                         |
| `token`      | string   | `""`    | The bearer token `/metrics` needs on the public listener; while it is empty, `/metrics` is not served there. A secret. |
| `otlp`       | bool     | `false` | Also push metrics to the endpoint.                                                                      |
| `interval`   | duration | `0s`    | How often metrics are pushed, at least `5s`. Unset: `OTEL_METRIC_EXPORT_INTERVAL`, else `60s`.         |

### `telemetry.logs`

The optional copy of the log that goes to the endpoint ([exporting logs](ops.md#exporting-logs-over-otlp)):

| Key     | Type   | Default | Meaning                                                                          |
| ------- | ------ | ------- | -------------------------------------------------------------------------------- |
| `otlp`  | bool   | `false` | Also export each log record to the endpoint. stderr is unchanged.               |
| `level` | string | `""`    | The lowest level exported: `debug`, `info`, `warn` or `error`. `""` follows `log.level`. |

## Validation rules

Kazari checks all of these together at startup. The error lists every failure as `key: problem`, and the server exits with status 2:

- `base_domain` is required, lower-case, without scheme, port or trailing dot, and made of labels of letters, digits and hyphens. It is not `example.com`, `example.org`, `example.net` or a name under one.
- `data_dir` is not empty.
- `listen.http`, `listen.https` and `admin.listen`, when set, are `host:port`.
- `external_scheme` is `http` or `https`, and `external_port` is 0 to 65535.
- Outside proxy mode `listen.https` is required; in proxy mode `listen.http` is required.
- `tls.mode` is one of the three modes. Outside proxy mode:
  - `acme` needs a valid `tls.ca`, a known `tls.dns_provider` and, except for `route53`, `tls.dns.api_token`; `namecheap` also needs `tls.dns.user`.
- `tls.email`, when set, is an email address outside the placeholder domains.
  - `files` needs `tls.cert_file` and `tls.key_file`.
- Every number and duration in `limits`, `lifecycle` and `fetch` is greater than 0, and `fetch.budget` is at least `fetch.timeout`.
- `limits.max_file_bytes` and `limits.max_chunk_bytes` do not exceed `limits.max_version_bytes`.
- `serve.default_visibility` is `public`, `unlisted` or `members`, and `serve.share_link_max_ttl` is greater than 0.
- `auth.write_policy` is `shared` or `owner`.
- `auth.tokens.access_ttl` is greater than 0 and `auth.tokens.refresh_ttl` is at least as long. `auth.tokens.refresh_reuse_window` is between 0 and `5m`, and `auth.tokens.max_key_ttl` is not negative.
- `auth.admin_token`, when set, is at least 32 characters, unless `base_domain` is `localhost` or ends in `.localhost`: a development instance that only its own machine can reach.
- `auth.local.min_password_length` is between 8 and 256, and `auth.local.invite_ttl` and `auth.local.reset_ttl` are greater than 0. `auth.local.enabled` is not `true` while `auth.oidc` is present.
- With `auth.oidc`, `issuer` is an `https` URL, and `client_id` and `client_secret` are set.
- `admin.public_url`, when set, is an `http` or `https` origin with no path, query or credentials, and `http` only for `localhost`, a name under it or a loopback address.
- `analytics.retention` is at least `24h`.
- `log.level` and `log.format` are among the listed values.
- `telemetry.otlp.protocol`, when set, is `http/protobuf` or `grpc`, and `telemetry.otlp.compression` is `gzip` or `none`.
- `telemetry.otlp.endpoint`, when set, is an absolute `http` or `https` URL with no credentials, query or fragment, and with no path under `grpc`. Credentials belong in `telemetry.otlp.headers`, because the endpoint appears in `kazari check`, `kazari admin settings` and error messages.
- `telemetry.otlp.headers` are valid header names and values, `telemetry.otlp.timeout` is not negative, and `ca_file`, `cert_file` and `key_file` can be read, with `cert_file` and `key_file` set together.
- `telemetry.traces.sample_ratio` and `site_sample_ratio`, when set, are between 0 and 1.
- `telemetry.logs.level`, when set, is one of the `log.level` values.
- `telemetry.metrics.token`, when set, is at least 32 characters, with the same `localhost` exception as `auth.admin_token`, and `telemetry.metrics.interval` is `0s` or at least `5s`.
- An `OTEL_EXPORTER_OTLP_*_ENDPOINT` that Kazari uses must be an `http` or `https` URL; otherwise the server does not start.

Because every number in `limits` must be greater than 0, `write_rate_per_minute` cannot be set to 0 to turn off rate limiting, and `default_user_quota` cannot be 0. To give one user an unlimited quota, set it through the admin API instead:

```sh
kazari admin users update peter --quota 0
```
