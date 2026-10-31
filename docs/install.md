# Installing Kazari

This guide takes a fresh server to a first published page. It covers the DNS records, running Kazari with Docker Compose, from a package or from the binary, the preflight check, the first admin user and key, and a first publish. To try Kazari on your own machine first, the [quick start](quickstart.md) needs none of this.

The examples use `art.example.com` as the base domain. Replace it with yours.

## Choosing how to run it

Every way of running Kazari uses the same two files: `kazari.yaml`, which names the domain, the contact email and the DNS provider, and a dotenv file with the secrets. Pick the path that fits the host:

| Path                                                  | Best for                                                     | Upgrade                                     |
| ----------------------------------------------------- | ------------------------------------------------------------ | ------------------------------------------- |
| [Docker Compose](#running-with-docker-compose)        | Any Linux host with Docker                                   | `docker compose pull && docker compose up -d` |
| [Package](#installing-the-package)                    | Debian, Ubuntu, Fedora, RHEL and their relatives             | Install the next `.deb` or `.rpm`           |
| [Binary](#installing-the-binary)                      | Other Linux distributions, macOS, or no package manager      | Run the install script again               |
| [Behind your own proxy](#running-behind-a-reverse-proxy) | A Caddy, nginx or Traefik that already holds the certificate | Any of the above, in proxy mode          |

With the first three, Kazari obtains the wildcard certificate itself through the DNS provider's API. Behind a proxy, the proxy holds the certificate and Kazari serves plain HTTP.

## Before you start

You need:

- **A domain used only for Kazari.** Every artifact becomes a subdomain of it, so nothing else should live under `art.example.com`.
- **Two DNS records** pointing at the host, and the matching `AAAA` records only if the host really has IPv6:

  ```text
  art.example.com.      A     203.0.113.10
  *.art.example.com.    A     203.0.113.10
  ```

  The wildcard covers `api.`, `mcp.`, `admin.`, every `<slug>.` and every `<slug>--v<N>.` host. [DNS records](certificates.md#dns-records) explains the records and their pitfalls.
- **An API token for the DNS provider**, which is how a wildcard certificate is obtained from Let's Encrypt. Kazari works with Cloudflare, Route 53, Hetzner, DigitalOcean, Gandi, OVH, Namecheap and DuckDNS ([providers](certificates.md#providers)); for any other, issue the certificate with another tool ([using another DNS provider](certificates.md#using-another-dns-provider)). Behind your own proxy you need none.
- **Ports 80 and 443** reachable, or a reverse proxy in front.
- **A supported platform**: Linux on amd64 or arm64, or macOS on arm64. Kazari needs no CGO and no external database.

## Running with Docker Compose

The Compose file runs the published image with the two configuration files beside it. In an empty directory, fetch the three files:

```sh
curl -fsSLO https://kazari.io/deploy/compose.yml
curl -fsSLO https://kazari.io/deploy/kazari.yaml
curl -fsSL https://kazari.io/deploy/kazari.env.example -o .env
```

Edit `kazari.yaml` with your domain, contact email and DNS provider, and put the provider's token in `.env`. Then start Kazari:

```sh
docker compose up -d
docker compose logs -f
```

The log shows `kazari started`, then `serving` with the addresses, then `no users yet` with the next command, and within a minute `certificate obtained` for `*.art.example.com`. Compose waits for the health check, which asks the server over its socket.

To check the DNS records and the token from inside the running container, run the [preflight check](#checking-the-setup):

```sh
docker compose exec kazari kazari check
```

The container runs as the `nonroot` user, keeps its data in the volume `kazari_data`, and serves `/etc/kazari/kazari.yaml`. The image is tagged with each release version and `latest`; pin a version with `KAZARI_VERSION` in `.env`. Then go on to [creating the first admin user and key](#creating-the-first-admin-user-and-key).

## Installing the package

Each release carries `.deb` and `.rpm` packages for amd64 and arm64. They install the binary at `/usr/bin/kazari`, the systemd unit, the two configuration files under `/etc/kazari`, which upgrades leave alone, and the reference files under `/usr/share/doc/kazari`. Download the package for your platform from the [releases page](https://github.com/PeterBooker/kazari/releases) and install it:

```sh
sudo apt install ./kazari_<version>_amd64.deb      # Debian and Ubuntu
sudo dnf install ./kazari-<version>.x86_64.rpm     # Fedora and RHEL
```

The package prints the remaining steps. Edit `/etc/kazari/kazari.yaml` with your domain, contact email and DNS provider, put the provider's token in `/etc/kazari/kazari.env`, then check and start:

```sh
sudo kazari check
sudo systemctl enable --now kazari
journalctl -u kazari -f
```

`kazari check` reports on the configuration, the DNS records, the token and the ports before anything runs ([checking the setup](#checking-the-setup)). The log then shows the same lines as under Compose. Go on to [creating the first admin user and key](#creating-the-first-admin-user-and-key).

## Installing the binary

The binary is the server and the client in one. Install it with the script, or by hand with the release verified, then set it up under systemd.

### With the install script

The script installs the latest release into `/usr/local/bin`, after checking the download against the release's `checksums.txt`:

```sh
curl -fsSL https://kazari.io/install.sh | sh
```

It prints the installed version. To install a particular release or into another directory, set `KAZARI_VERSION` or `KAZARI_BINDIR` before `sh`. To read the script first, open <https://kazari.io/install.sh>.

### By hand, verifying the release

Each release on GitHub carries archives named `kazari_<version>_<os>_<arch>.tar.gz`, each holding the binary and the files in `deploy/`, with an SBOM each, a `checksums.txt`, and `checksums.txt.sigstore.json`, a Sigstore bundle signed with keyless cosign. Set the version you want, then download the archive, the checksums and their signature:

```sh
VERSION=0.3.0
curl -LO https://github.com/PeterBooker/kazari/releases/download/$VERSION/kazari_${VERSION}_linux_amd64.tar.gz
curl -LO https://github.com/PeterBooker/kazari/releases/download/$VERSION/checksums.txt
curl -LO https://github.com/PeterBooker/kazari/releases/download/$VERSION/checksums.txt.sigstore.json
```

Verify the signature of the checksums, then the archive against them:

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

### Running under systemd

The unit runs the server as a confined, unprivileged user. Put the unit and the two configuration files in place, from the archive's `deploy/` directory or from the site:

```sh
sudo mkdir -p /etc/kazari
sudo curl -fsSL https://kazari.io/deploy/kazari.yaml -o /etc/kazari/kazari.yaml
sudo curl -fsSL https://kazari.io/deploy/kazari.env.example -o /etc/kazari/kazari.env
sudo chmod 0600 /etc/kazari/kazari.env
sudo curl -fsSL https://kazari.io/deploy/kazari.service -o /etc/systemd/system/kazari.service
```

Edit `/etc/kazari/kazari.yaml` with your domain, contact email and DNS provider, and put the provider's token in `/etc/kazari/kazari.env`. Then check, enable and start the service:

```sh
sudo kazari check
sudo systemctl daemon-reload
sudo systemctl enable --now kazari
journalctl -u kazari -f
```

The unit does these things:

- `ExecStart=kazari serve --config /etc/kazari/kazari.yaml` starts the server, finding the binary in `/usr/local/bin` or `/usr/bin`.
- `EnvironmentFile=/etc/kazari/kazari.env` supplies the secrets. systemd reads it as root before dropping privileges, so mode `0600` is right.
- `DynamicUser=yes` runs it as a transient unprivileged user. `StateDirectory=kazari` gives that user `/var/lib/kazari` (stored under `/var/lib/private/kazari`), and `KAZARI_DATA_DIR` points there.
- `AmbientCapabilities=CAP_NET_BIND_SERVICE` lets it bind ports 80 and 443 without root.
- `ProtectSystem=strict`, `ProtectHome=yes`, `PrivateTmp=yes` and a system-call filter confine it to its state directory and `/tmp`.

Because the service user is dynamic, `/etc/kazari/kazari.yaml` must be world-readable (`0644`). That is one more reason to keep secrets in the environment file.

## Running behind a reverse proxy

If something else already terminates TLS on the host, run Kazari in *proxy mode*. It then serves plain HTTP on `listen.http` only, and takes the client IP from the last `X-Forwarded-For` entry, the one the proxy in front added, and then from `X-Real-IP`.

Turn on proxy mode and bind the plain listener to the loopback address, in place of the `tls` section:

```yaml
base_domain: art.example.com
data_dir: /var/lib/kazari

listen:
  proxy_mode: true
  http: "127.0.0.1:8080"
```

In proxy mode Kazari needs no DNS provider token. Public URLs still use `https`, since `external_scheme` defaults to `https`. The proxy must hold a certificate for `art.example.com` and `*.art.example.com` and forward every host to Kazari with the `Host` header intact; [proxy examples](certificates.md#proxy-examples) has Caddy, nginx and Traefik configurations.

> [!WARNING]
> Use proxy mode behind exactly one proxy, and do not expose the plain port. A client that reaches the port directly can set its own `X-Forwarded-For` and pick its own IP. Behind a chain of two proxies, such as a CDN and then a proxy, the per-IP limits apply to the outer proxy's address.

## Checking the setup

`kazari check` reads the configuration as `serve` would and reports on what the first start depends on, so a wrong record or a token without permission shows up before Let's Encrypt is asked. Run it on the server, before and after starting:

```sh
sudo kazari check
```

It prints one line per check and a summary:

```text
config       ok    /etc/kazari/kazari.yaml, base domain art.example.com, mode acme via cloudflare, data in /var/lib/kazari
data_dir     ok    /var/lib/kazari is writable
dns          ok    art.example.com -> 203.0.113.10 (this host)
dns          ok    api.art.example.com -> 203.0.113.10 (this host)
dns          ok    check-7f3a9c01.art.example.com -> 203.0.113.10 (this host)
dns token    ok    cloudflare wrote and removed a TXT record in zone example.com.
ports        ok    :443 is free
ports        ok    :80 is free
admin socket ok    /var/lib/kazari/admin.sock (no server listening yet)
metrics      warn  /metrics is not served on admin.art.example.com without telemetry.metrics.token; set it, or scrape admin.listen on a private address
telemetry    skip  no OTLP endpoint is set; nothing is exported

all checks passed, 1 warning(s)
```

The random host proves the wildcard record works. The token check writes a `_kazari-check` TXT record through the provider's API and removes it again, which is what issuance does. A `warn` line is worth reading but does not stop you: a host behind NAT or a proxy resolves to another address, a port is in use while Kazari runs, and the metrics warning only matters once Prometheus scrapes the instance ([metrics](ops.md#metrics)). A `fail` line exits with status 1; an invalid configuration exits with status 2 and lists every problem.

## Creating the first admin user and key

A fresh instance has no users. Create the first admin and issue it a key on the server, where the `kazari` command reaches the admin API over a Unix socket in the data directory and needs no token. Under systemd, run it as root:

```sh
sudo kazari admin users create peter --email peter@example.com --role admin
sudo kazari admin keys issue peter --label laptop        # prints kzk_... once
```

Under Compose, the same commands run inside the container:

```sh
docker compose exec kazari kazari admin users create peter --email peter@example.com --role admin
docker compose exec kazari kazari admin keys issue peter --label laptop
```

The key is shown exactly once, so copy it now. A key issued without `--scopes` carries the member scopes (`artifacts:read`, `artifacts:write`, `artifacts:delete`) and acts as a member, even for an admin user.

### Administering from another machine

To run admin commands from your laptop, issue the admin user a key that also holds the `admin` scope, and point the CLI at the admin host with it:

```sh
sudo kazari admin keys issue peter --label admin --scopes artifacts:read,artifacts:write,artifacts:delete,admin
```

```sh
export KAZARI_ADMIN_URL=https://admin.art.example.com KAZARI_ADMIN_TOKEN=kzk_...
kazari admin users list
```

An instance can also accept a fixed admin token, `KAZARI_AUTH_ADMIN_TOKEN` in the dotenv file, for scripts that run before any user exists; [the admin token](configuration.md#the-admin-token) covers it. If you set `admin.listen`, the admin host is served only on that address, and a remote CLI reaches it through an SSH tunnel.

## Verifying with a first publish

Publish a one-page site with your key to check the whole path, from the API to the wildcard certificate:

```sh
export KAZARI_URL=https://api.art.example.com
export KAZARI_TOKEN=kzk_...

mkdir -p site && echo '<!doctype html><title>Hello</title><h1>Hello</h1>' > site/index.html
kazari me
kazari publish ./site --slug hello        # prints https://hello.art.example.com/
curl -sI https://hello.art.example.com/
```

The `curl` shows the published page's response headers, served under the wildcard certificate. If the handshake fails, the certificate has not been issued yet: [the certificate is not issued](certificates.md#the-certificate-is-not-issued) says where to look.

## Next steps

- [MCP server](mcp.md): connect your agent to the instance.
- [Sign-in and identity](identity.md): let people sign in with their organisation account through an identity provider.
- [DNS and certificates](certificates.md): the records, the providers, other providers and proxy configurations.
- [Configuration](configuration.md): every key, its default and how to set it from the environment.
- [Operations](ops.md): backups, upgrades, logs, metrics and troubleshooting.
