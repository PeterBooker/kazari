# Installing Agalma

This guide takes a fresh host to a first published artifact. The examples use `art.example.com` as
the base domain; replace it with yours.

## Requirements

- **A domain you control**, used only for Agalma. Every artifact becomes a subdomain of it.
- **Two DNS records** pointing at the host:

  ```text
  art.example.com.      A     203.0.113.10
  *.art.example.com.    A     203.0.113.10
  ```

  (and the matching `AAAA` records for IPv6). The wildcard covers `api.`, `mcp.`, `admin.`,
  every `<slug>.` and every `<slug>--v<N>.` host.
- **A DNS provider API token** for the ACME DNS-01 challenge, which is the only way to get a
  wildcard certificate from Let's Encrypt. Agalma supports:

  | Provider     | `tls.dns_provider` | Credential in `tls.dns.api_token`                                    |
  | ------------ | ------------------ | --------------------------------------------------------------------- |
  | Cloudflare   | `cloudflare`       | API token with `Zone.DNS:Edit` on the zone                            |
  | Hetzner DNS  | `hetzner`          | Hetzner DNS API token                                                 |
  | AWS Route 53 | `route53`          | Empty (AWS default credential chain) or `ACCESS_KEY_ID:SECRET_ACCESS_KEY` |

  You do not need a token if you bring your own certificate (`tls.mode = "files"`) or run behind a
  reverse proxy that holds the certificate (proxy mode).
- **Ports 80 and 443** reachable, or a reverse proxy in front.
- Linux on amd64 or arm64, or macOS on arm64. No CGO, no external database.

## Install the release binary

Each release on GitHub carries `agalma_<version>_<os>_<arch>.tar.gz` archives (the binary, the
licence, this README and the files in `deploy/`), a `checksums.txt`, an SBOM per archive, and a
cosign signature of the checksums (`checksums.txt.sig`, `checksums.txt.pem`).

```sh
VERSION=0.1.0
curl -LO https://github.com/PeterBooker/agalma/releases/download/v$VERSION/agalma_${VERSION}_linux_amd64.tar.gz
curl -LO https://github.com/PeterBooker/agalma/releases/download/v$VERSION/checksums.txt
curl -LO https://github.com/PeterBooker/agalma/releases/download/v$VERSION/checksums.txt.sig
curl -LO https://github.com/PeterBooker/agalma/releases/download/v$VERSION/checksums.txt.pem

# The checksums were signed by the release workflow (keyless cosign).
cosign verify-blob checksums.txt \
  --signature checksums.txt.sig --certificate checksums.txt.pem \
  --certificate-identity-regexp '^https://github.com/PeterBooker/agalma/\.github/workflows/release\.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

sha256sum --check --ignore-missing checksums.txt
tar -xzf agalma_${VERSION}_linux_amd64.tar.gz
sudo install -m 0755 agalma /usr/local/bin/agalma
agalma version
```

The same binary is the server (`agalma serve`) and the client (every other command, see
[cli.md](cli.md)).

## Run under systemd

The archive contains `deploy/agalma.service`, `deploy/agalma.toml` and `deploy/Caddyfile`.

```sh
sudo mkdir -p /etc/agalma
sudo install -m 0644 deploy/agalma.toml /etc/agalma/agalma.toml
sudo install -m 0600 /dev/null /etc/agalma/agalma.env
sudo install -m 0644 deploy/agalma.service /etc/systemd/system/agalma.service
```

Edit `/etc/agalma/agalma.toml`. The minimum for ACME is:

```toml
base_domain = "art.example.com"
data_dir    = "/var/lib/agalma"

[tls]
mode         = "acme"
email        = "ops@example.com"
dns_provider = "cloudflare"
```

Put the secrets in `/etc/agalma/agalma.env`, not in the TOML file. systemd reads this file as root
before it drops privileges, so mode `0600` is right:

```sh
AGALMA_TLS_DNS_API_TOKEN=your-cloudflare-token
AGALMA_AUTH_ADMIN_TOKEN=a-long-random-string        # openssl rand -hex 32
# AGALMA_AUTH_OIDC_CLIENT_SECRET=...                 # once you configure sign-in
```

What the unit does:

- `ExecStart=/usr/local/bin/agalma serve --config /etc/agalma/agalma.toml`.
- `DynamicUser=yes` runs it as a transient unprivileged user; `StateDirectory=agalma` gives that user
  `/var/lib/agalma` (stored under `/var/lib/private/agalma`), and `AGALMA_DATA_DIR` points there.
- `AmbientCapabilities=CAP_NET_BIND_SERVICE` lets it bind ports 80 and 443 without root.
- `ProtectSystem=strict`, `ProtectHome=yes`, `PrivateTmp=yes` and a system-call filter confine it to
  its state directory and `/tmp`.

Because the service user is dynamic, `/etc/agalma/agalma.toml` must be world-readable (`0644`), which
is another reason to keep secrets in the environment file.

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now agalma
journalctl -u agalma -f
```

Startup logs `agalma started` with the mode and the bound addresses. The first start then obtains
the wildcard certificate in the background; watch the log for CertMagic's
`certificate obtained successfully` line (see [ops.md](ops.md#certificate-issuance) if it does not
come).

### How `agalma serve` finds its configuration

1. `--config PATH`, if given (the file must exist);
2. else `$AGALMA_CONFIG`;
3. else `/etc/agalma/agalma.toml`, when that file exists;
4. else environment variables only.

Environment variables override the file in every case. See [configuration.md](configuration.md).
A configuration error exits with status 2 and lists every problem; a failure to start (a port in
use, an unreachable OIDC issuer, a database that cannot be opened) exits with status 1.

## Run with Docker

The image `ghcr.io/peterbooker/agalma` is distroless, runs as the `nonroot` user (UID 65532), keeps
its data in the volume `/var/lib/agalma` and exposes ports 80 and 443. Its default command is
`serve --config /etc/agalma/agalma.toml`, so either mount a config file there:

```sh
sudo mkdir -p /srv/agalma/data && sudo chown 65532:65532 /srv/agalma/data
docker run -d --name agalma --restart unless-stopped \
  -p 80:80 -p 443:443 \
  -v /srv/agalma/data:/var/lib/agalma \
  -v /etc/agalma/agalma.toml:/etc/agalma/agalma.toml:ro \
  --env-file /etc/agalma/agalma.env \
  ghcr.io/peterbooker/agalma:latest
```

or pass `serve` as the command and configure everything through the environment:

```sh
docker run -d --name agalma --restart unless-stopped \
  -p 80:80 -p 443:443 \
  -v /srv/agalma/data:/var/lib/agalma \
  -e AGALMA_BASE_DOMAIN=art.example.com \
  -e AGALMA_TLS_EMAIL=ops@example.com \
  -e AGALMA_TLS_DNS_PROVIDER=cloudflare \
  --env-file /etc/agalma/agalma.env \
  ghcr.io/peterbooker/agalma:latest serve
```

The data directory must be writable by UID 65532; a bind mount owned by root is not. Images are
tagged with the release version and `latest`, for linux/amd64 and linux/arm64, and signed with cosign.

## Run behind a reverse proxy (proxy mode)

If something else already terminates TLS on the host, run Agalma in proxy mode: it then serves plain
HTTP on `[listen].http` only and takes the client IP from the last `X-Forwarded-For` entry (the one
the proxy in front added), then `X-Real-IP`. Use proxy mode behind exactly one proxy, and do not
expose the plain port.

```toml
base_domain = "art.example.com"
data_dir    = "/var/lib/agalma"

[listen]
proxy_mode = true
http       = "127.0.0.1:8080"
```

In proxy mode Agalma terminates no TLS, so the `tls` settings beyond a valid `tls.mode` are not
required. Public URLs still use `https` (`external_scheme` defaults to `https`).

`deploy/Caddyfile` forwards the apex and every subdomain to Agalma, with Caddy holding the wildcard
certificate:

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

The `dns cloudflare` directive needs a Caddy build with the `caddy-dns/cloudflare` module
(`xcaddy build --with github.com/caddy-dns/cloudflare`); use the module for your DNS provider. Any
other proxy works the same way: forward every host unchanged (`Host` header intact) to Agalma.

## TLS modes

| `tls.mode`    | Certificates                                                                                               | Use for                     |
| ------------- | ---------------------------------------------------------------------------------------------------------- | --------------------------- |
| `acme`        | CertMagic obtains and renews one certificate for `art.example.com` and `*.art.example.com` by DNS-01, stored in `data_dir/certmagic/`. Needs `tls.email` and, except for Route 53, `tls.dns.api_token`. | Production (default)        |
| `files`       | PEM files at `tls.cert_file` and `tls.key_file`, checked every minute and reloaded when their size or modification time changes; a bad reload keeps the old certificate and logs a warning. | Your own CA or a cert manager |
| `self-signed` | An ECDSA certificate generated at start, valid for the base domain, its wildcard, `127.0.0.1` and `::1`. Browsers warn. | Development and tests       |

`tls.ca` picks the ACME directory: `letsencrypt` (default), `letsencrypt-staging` (use it for a dry
run, since production Let's Encrypt rate-limits failed attempts), or any `https` directory URL.

For `files`, the certificate must cover both `art.example.com` and `*.art.example.com`.

## Create the first admin user and key

The admin API lives at `https://admin.art.example.com/v1` and accepts the admin token from
`AGALMA_AUTH_ADMIN_TOKEN`. Use the CLI (from any machine):

```sh
export AGALMA_ADMIN_URL=https://admin.art.example.com
export AGALMA_ADMIN_TOKEN=the-admin-token

agalma admin health
agalma admin users create peter --email peter@example.com --role admin
agalma admin keys issue peter --label laptop        # prints agk_... once
```

The key is shown exactly once. A key issued without `--scopes` carries the member scopes
(`artifacts:read`, `artifacts:write`, `artifacts:delete`) and acts as a member even for an admin
user. To retire the config token, issue the admin user a key that also holds the `admin` scope and
use it on the admin host, then remove `AGALMA_AUTH_ADMIN_TOKEN`:

```sh
agalma admin keys issue peter --label admin \
  --scopes artifacts:read,artifacts:write,artifacts:delete,admin
```

## Verify with a first publish

```sh
export AGALMA_URL=https://api.art.example.com
export AGALMA_TOKEN=agk_...

mkdir -p site && echo '<!doctype html><title>Hello</title><h1>Hello</h1>' > site/index.html
agalma me
agalma publish ./site --slug hello        # prints https://hello.art.example.com/
curl -sI https://hello.art.example.com/
curl -s https://art.example.com/health
```

Then connect Claude ([README](../README.md#connecting-claude), [mcp.md](mcp.md)) and, if you want
claude.ai users to sign in with their organisation account, configure an identity provider
([identity.md](identity.md)).

## Local development

`make run-dev` builds the binary and runs it in proxy mode on port 8080 with `base_domain = localhost`,
data in `./.data` and the admin token `dev-admin-token`. Browsers resolve `*.localhost` to the
loopback address, so no DNS or certificate is needed.

```sh
make run-dev

# in another terminal
agalma --admin-url http://admin.localhost:8080 --admin-token dev-admin-token admin users create peter
agalma --admin-url http://admin.localhost:8080 --admin-token dev-admin-token admin keys issue peter --label laptop

AGALMA_URL=http://api.localhost:8080 AGALMA_TOKEN=agk_... agalma publish ./site --slug demo
```

Open `http://demo.localhost:8080/`. The MCP server is at `http://mcp.localhost:8080/`. `make clean`
removes the binary and `./.data`. If a tool on your system does not resolve `*.localhost` (most
browsers and systemd-resolved do), add the hosts you need to `/etc/hosts`:
`127.0.0.1 api.localhost admin.localhost mcp.localhost demo.localhost`.

Other targets: `make build`, `make test` (race detector), `make lint` (vet and staticcheck),
`make vuln` (govulncheck) and `make snapshot` (a local goreleaser build without publishing).
