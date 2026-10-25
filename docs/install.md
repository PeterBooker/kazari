# Installing Kazari

This guide takes a fresh host to a first published artifact. It covers the DNS records and the wildcard certificate Kazari needs, the release binary under systemd or Docker, running behind a reverse proxy, and the first admin user and key. To run Kazari on your own machine for development instead, go to [Local development](#local-development).

The examples use `art.example.com` as the base domain. Replace it with yours.

## Requirements

Before you start, you need:

- **A domain you control**, used only for Kazari. Every artifact becomes a subdomain of it.
- **Two DNS records** pointing at the host, plus the matching `AAAA` records for IPv6:

  ```text
  art.example.com.      A     203.0.113.10
  *.art.example.com.    A     203.0.113.10
  ```

  The wildcard covers `api.`, `mcp.`, `admin.`, every `<slug>.` and every `<slug>--v<N>.` host.
- **A DNS provider API token** for the ACME DNS-01 challenge, which is the only way to get a wildcard certificate from Let's Encrypt. Kazari supports these providers:

  | Provider     | `tls.dns_provider` | Credential in `tls.dns.api_token`                                    |
  | ------------ | ------------------ | --------------------------------------------------------------------- |
  | Cloudflare   | `cloudflare`       | API token with `Zone.DNS:Edit` on the zone                            |
  | Hetzner DNS  | `hetzner`          | Hetzner DNS API token                                                 |
  | AWS Route 53 | `route53`          | Empty (AWS default credential chain) or `ACCESS_KEY_ID:SECRET_ACCESS_KEY` |

  You do not need a token if you bring your own certificate (`tls.mode: files`) or run behind a reverse proxy that holds the certificate (proxy mode).
- **Ports 80 and 443** reachable, or a reverse proxy in front.
- **A supported platform**: Linux on amd64 or arm64, or macOS on arm64. Kazari needs no CGO and no external database.

## Installing the release binary

Each release on GitHub carries these files:

- `kazari_<version>_<os>_<arch>.tar.gz` archives, each holding the binary, the licence, the README and the files in `deploy/`.
- An SBOM for each archive.
- `checksums.txt`, the checksums of the files.
- `checksums.txt.sigstore.json`, a Sigstore bundle for the checksums, signed with cosign.

Set the version you want, then download its archive, the checksums and their signature:

```sh
VERSION=0.1.0
curl -LO https://github.com/PeterBooker/kazari/releases/download/$VERSION/kazari_${VERSION}_linux_amd64.tar.gz
curl -LO https://github.com/PeterBooker/kazari/releases/download/$VERSION/checksums.txt
curl -LO https://github.com/PeterBooker/kazari/releases/download/$VERSION/checksums.txt.sigstore.json
```

The release workflow signs the checksums with keyless cosign. Verify that signature, then check the archive against the checksums:

```sh
cosign verify-blob checksums.txt \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github.com/PeterBooker/kazari/\.github/workflows/release\.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

sha256sum --check --ignore-missing checksums.txt
```

`sha256sum` prints the archive's name followed by `OK`. Unpack the archive and install the binary:

```sh
tar -xzf kazari_${VERSION}_linux_amd64.tar.gz
sudo install -m 0755 kazari /usr/local/bin/kazari
kazari version
```

The same binary is the server (`kazari serve`) and the client. Every other command is a client command, described in the [CLI reference](cli.md).

## Running under systemd

The archive's `deploy/` directory holds a systemd unit, `kazari.service`, with templates for its configuration and secrets. Copy them into place:

```sh
sudo mkdir -p /etc/kazari
sudo install -m 0644 deploy/kazari.yaml /etc/kazari/kazari.yaml
sudo install -m 0600 deploy/kazari.env.example /etc/kazari/kazari.env
sudo install -m 0644 deploy/kazari.service /etc/systemd/system/kazari.service
```

### Configuring the server

Edit `/etc/kazari/kazari.yaml`. For ACME, the settings that matter are:

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
```

Each `${NAME}` is replaced at startup by the environment variable `NAME`, so the file itself holds no secrets.

The unit passes `--config /etc/kazari/kazari.yaml`. Without that flag, `kazari serve` tries `$KAZARI_CONFIG`, then `/etc/kazari/kazari.yaml`, then runs on environment variables alone. It can also read a `.env` file, and `KAZARI_*` variables override the YAML file. The [configuration reference](configuration.md#where-the-file-comes-from) gives the order of each.

### Storing the secrets

Put the secret values in `/etc/kazari/kazari.env`, which the unit loads with `EnvironmentFile=`:

```sh
KAZARI_TLS_DNS_API_TOKEN=your-cloudflare-token
# openssl rand -hex 32
KAZARI_AUTH_ADMIN_TOKEN=a-long-random-string
# once you configure sign-in
# KAZARI_AUTH_OIDC_CLIENT_SECRET=...
```

systemd reads this file as root before it drops privileges, so mode `0600` is right.

A `${NAME}` reference to a variable that is not set stops startup with an error naming it. Define every variable the file refers to, as the template does, or delete the line from the YAML. The [environment references](configuration.md#environment-references) section has the full syntax.

### What the unit does

The unit runs the server as a confined, unprivileged user:

- `ExecStart=/usr/local/bin/kazari serve --config /etc/kazari/kazari.yaml` starts the server.
- `DynamicUser=yes` runs it as a transient unprivileged user. `StateDirectory=kazari` gives that user `/var/lib/kazari` (stored under `/var/lib/private/kazari`), and `KAZARI_DATA_DIR` points there.
- `AmbientCapabilities=CAP_NET_BIND_SERVICE` lets it bind ports 80 and 443 without root.
- `ProtectSystem=strict`, `ProtectHome=yes`, `PrivateTmp=yes` and a system-call filter confine it to its state directory and `/tmp`.

Because the service user is dynamic, `/etc/kazari/kazari.yaml` must be world-readable (`0644`). That is another reason to keep secrets in the environment file.

### Starting the service

Reload systemd, start the service and follow its log:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now kazari
journalctl -u kazari -f
```

Startup logs `kazari started` with the mode and the bound addresses. The first start then obtains the wildcard certificate in the background: watch the log for CertMagic's `certificate obtained successfully` line. If it does not come, see [certificate issuance](ops.md#the-certificate-is-not-issued).

If the configuration is invalid, the server exits with status 2 and lists every problem. Any other failure to start, such as a port in use, an unreachable OIDC issuer or a database that cannot be opened, exits with status 1.

## Running with Docker

The image `ghcr.io/peterbooker/kazari` is distroless and runs as the `nonroot` user (UID 65532). It keeps its data in the volume `/var/lib/kazari` and exposes ports 80 and 443. Images are tagged with the release version and `latest`, for linux/amd64 and linux/arm64, and signed with cosign.

The data directory must be writable by UID 65532. A named volume is initialised from the image and is writable as it is. A bind mount owned by root is not, so create the directory and hand it to that user first:

```sh
sudo mkdir -p /srv/kazari/data && sudo chown 65532:65532 /srv/kazari/data
```

### With a configuration file

The image's default command is `serve --config /etc/kazari/kazari.yaml`. To use a configuration file, mount it at that path:

```sh
docker run -d --name kazari --restart unless-stopped \
  -p 80:80 -p 443:443 \
  -v /srv/kazari/data:/var/lib/kazari \
  -v /etc/kazari/kazari.yaml:/etc/kazari/kazari.yaml:ro \
  --env-file /etc/kazari/kazari.env \
  ghcr.io/peterbooker/kazari:latest
```

### With environment variables only

To configure everything through the environment instead, pass `serve` as the command:

```sh
docker run -d --name kazari --restart unless-stopped \
  -p 80:80 -p 443:443 \
  -v /srv/kazari/data:/var/lib/kazari \
  -e KAZARI_BASE_DOMAIN=art.example.com \
  -e KAZARI_TLS_EMAIL=ops@example.com \
  -e KAZARI_TLS_DNS_PROVIDER=cloudflare \
  --env-file /etc/kazari/kazari.env \
  ghcr.io/peterbooker/kazari:latest serve
```

## Running behind a reverse proxy

If something else already terminates TLS on the host, run Kazari in *proxy mode*. It then serves plain HTTP on `listen.http` only. It takes the client IP from the last `X-Forwarded-For` entry, the one the proxy in front added, and then from `X-Real-IP`.

> [!WARNING]
> Use proxy mode behind exactly one proxy, and do not expose the plain port. A client that reaches the port directly can set its own `X-Forwarded-For` and pick its own IP. Behind a chain of two proxies, such as a CDN and then a proxy, the per-IP limits apply to the outer proxy's address.

### Configuring Kazari

Turn on proxy mode and bind the plain listener to the loopback address:

```yaml
base_domain: art.example.com
data_dir: /var/lib/kazari

listen:
  proxy_mode: true
  http: "127.0.0.1:8080"
```

In proxy mode Kazari terminates no TLS, so it needs no `tls` settings beyond a valid `tls.mode`. Public URLs still use `https`, since `external_scheme` defaults to `https`.

### Configuring the proxy

`deploy/Caddyfile` forwards the apex and every subdomain to Kazari, with Caddy holding the wildcard certificate:

```caddyfile
*.art.example.com, art.example.com {
	tls ops@example.com {
		dns cloudflare {env.CLOUDFLARE_API_TOKEN}
	}

	reverse_proxy 127.0.0.1:8080 {
		header_up X-Forwarded-Host {host}
		header_up X-Real-IP {remote_host}
	}
}
```

The `dns cloudflare` directive needs a Caddy build with the `caddy-dns/cloudflare` module, made with `xcaddy build --with github.com/caddy-dns/cloudflare`. For another DNS provider, use its module instead.

Any other proxy works the same way: forward every host to Kazari unchanged, with the `Host` header intact.

## TLS modes

`tls.mode` decides where the certificate comes from. Most instances use `acme`, the default. Use `files` when another tool manages the certificates, and `self-signed` for development and tests:

| `tls.mode`    | Certificates                                 | Use for                              |
| ------------- | -------------------------------------------- | ------------------------------------ |
| `acme`        | Obtained from an ACME CA by DNS-01           | Production (default)                 |
| `files`       | PEM files you provide                        | Your own CA or a certificate manager |
| `self-signed` | Generated at start                           | Development and tests                |

### `acme`

CertMagic obtains and renews one certificate for `art.example.com` and `*.art.example.com` by DNS-01, and stores it in `data_dir/certmagic/`. This mode needs `tls.email` and, except for Route 53, `tls.dns.api_token`.

`tls.ca` picks the ACME directory:

- `letsencrypt`, the default.
- `letsencrypt-staging`, for a dry run. Use it while you test, since production Let's Encrypt rate-limits failed attempts.
- Any `https` directory URL.

### `files`

Kazari reads the PEM files at `tls.cert_file` and `tls.key_file`. The certificate must cover both `art.example.com` and `*.art.example.com`.

Kazari checks the files every minute and reloads them when their size or modification time changes. A bad reload keeps the old certificate and logs a warning.

### `self-signed`

Kazari generates an ECDSA certificate at start, valid for the base domain, its wildcard, `127.0.0.1` and `::1`. Browsers warn about it.

## Creating the first admin user and key

The admin API lives at `https://admin.art.example.com/v1`. It accepts the admin token from `KAZARI_AUTH_ADMIN_TOKEN`, which must be at least 32 characters.

Use the CLI, from any machine, to check the server and create a user with a key:

```sh
export KAZARI_ADMIN_URL=https://admin.art.example.com
export KAZARI_ADMIN_TOKEN=the-admin-token

kazari admin health
kazari admin users create peter --email peter@example.com --role admin
kazari admin keys issue peter --label laptop        # prints kzk_... once
```

The key is shown exactly once, so copy it now. A key issued without `--scopes` carries the member scopes (`artifacts:read`, `artifacts:write`, `artifacts:delete`) and acts as a member, even for an admin user.

If you set `admin.listen`, the admin host is served only on that address. Run the CLI on the server with `KAZARI_ADMIN_URL=http://127.0.0.1:9443`, or through an SSH tunnel.

### Retiring the admin token

To stop using the token from the configuration, issue the admin user a key that also holds the `admin` scope:

```sh
kazari admin keys issue peter --label admin \
  --scopes artifacts:read,artifacts:write,artifacts:delete,admin
```

Use that key on the admin host, then remove `KAZARI_AUTH_ADMIN_TOKEN`, as [retiring the admin token](configuration.md#retiring-the-admin-token) describes.

## Verifying with a first publish

Publish a one-page site with your key to check the whole path, from the API to the wildcard certificate:

```sh
export KAZARI_URL=https://api.art.example.com
export KAZARI_TOKEN=kzk_...

mkdir -p site && echo '<!doctype html><title>Hello</title><h1>Hello</h1>' > site/index.html
kazari me
kazari publish ./site --slug hello        # prints https://hello.art.example.com/
curl -sI https://hello.art.example.com/
curl -s https://art.example.com/health
```

The first `curl` shows the published page's response headers, and the second checks the server's health endpoint.

## Local development

`deploy/dev.yaml` runs the server on your own machine, with no DNS or certificate: browsers resolve `*.localhost` to the loopback address. It uses proxy mode on `127.0.0.1:8080`, with `base_domain: localhost` and data in `./.data`.

The admin token is `dev-admin-token`, unless `KAZARI_AUTH_ADMIN_TOKEN` is set in the environment or in `./.env`. The server listens on loopback only, since that token is public knowledge.

Start the server, then create a user, issue a key and publish from a second terminal:

```sh
go run ./cmd/kazari serve --config deploy/dev.yaml

# in another terminal
go run ./cmd/kazari --admin-url http://admin.localhost:8080 --admin-token dev-admin-token admin users create peter
go run ./cmd/kazari --admin-url http://admin.localhost:8080 --admin-token dev-admin-token admin keys issue peter --label laptop

KAZARI_URL=http://api.localhost:8080 KAZARI_TOKEN=kzk_... go run ./cmd/kazari publish ./site --slug demo
```

Open `http://demo.localhost:8080/` to see the page. The MCP server is at `http://mcp.localhost:8080/`. To start again from nothing, delete `./.data`.

> [!NOTE]
> Most browsers and systemd-resolved resolve `*.localhost`. If a tool on your system does not, add the hosts you need to `/etc/hosts`: `127.0.0.1 api.localhost admin.localhost mcp.localhost demo.localhost`.

The CLI reads `./.env` as well, so `KAZARI_URL` and `KAZARI_TOKEN` can live there instead of on the command line. Keep `.env` out of version control.

### Docker Compose

`compose.yml` at the repository root runs the same development server from the Docker image, so the URLs, the admin token and the commands above are unchanged. The Compose file:

- builds `deploy/Dockerfile`;
- mounts `deploy/dev.yaml` as the configuration, listening on every interface inside the container through `KAZARI_LISTEN_HTTP`;
- publishes port 8080 on the loopback addresses only;
- keeps the data in the named volume `kazari_data`, since the image's `KAZARI_DATA_DIR=/var/lib/kazari` replaces `./.data`;
- passes `./.env` into the container when it exists.

Start and manage it with the usual Compose commands:

```sh
docker compose up --build -d      # build and start; docker compose ps shows it healthy
docker compose logs -f            # follow the server log
docker compose watch              # rebuild and restart on source changes
docker compose down               # stop; -v discards the data as well
```

### Building, testing and releasing

These commands build a binary, run the same checks as CI and make a local release dry run:

```sh
# build, stamped with the version from git
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X kazari/internal/buildinfo.version=$(git describe --tags --always --dirty)" -o kazari ./cmd/kazari

go test -race ./...                                       # tests with the race detector
test -z "$(gofmt -l .)"                                   # formatting check
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@latest ./...    # lint
go run golang.org/x/vuln/cmd/govulncheck@latest ./...     # known vulnerabilities

goreleaser release --snapshot --clean --skip=publish,sign,sbom   # local release dry run
```

On pull requests and pushes to `main`, GitHub Actions runs gofmt, go vet, staticcheck, govulncheck, `go mod tidy -diff`, `go test -race` and the cross-builds.

To release, push a tag named for the version without a `v`, such as `0.2.0`. The workflow runs goreleaser, which publishes the archives with SBOMs and a cosign-signed `checksums.txt`, and pushes the signed image to `ghcr.io/peterbooker/kazari`.

## Next steps

- [MCP server](mcp.md): connect your agent to the instance.
- [Sign-in and identity](identity.md): let people sign in with their organisation account through an identity provider.
- [Configuration](configuration.md): every key, its default and how to set it from the environment.
- [Operations](ops.md): backups, upgrades, logs, metrics and troubleshooting.
