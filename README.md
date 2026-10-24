# Agalma

Agalma is a self-hosted artifact host for pages written by LLMs. One Go binary serves every artifact as
its own static site at `<slug>.<base>` under a single wildcard certificate, takes new artifacts and
versions over a REST API, an MCP server and a CLI, and retires pages nobody needs any more through a
reversible stale, soft-delete and purge lifecycle. It exists so that a team can let Claude (or any
MCP client) publish HTML pages, dashboards and small sites on its own domain, signed in through its
own identity provider, without handing the content to a third party.

## Features

- **One origin per artifact.** `demo.art.example.com` serves the published version,
  `demo--v3.art.example.com` pins version 3 forever. Each artifact is its own browser origin, so one
  page cannot read another's storage.
- **Immutable, content-addressed versions.** Every publish is a new version; files are stored once
  by SHA-256. Roll back or forward with one call; `expected_version` and `If-Match` stop lost updates.
- **Three ways in, one service layer.** REST (`api.<base>/v1`), MCP over Streamable HTTP
  (`mcp.<base>/`, twelve tools) and the `agalma` CLI all call the same code and return the same JSON.
- **Large bundles.** Inline JSON for small pages, `multipart` with a `tar.gz` bundle for sites,
  chunked uploads for MCP clients with no shell.
- **Lifecycle.** Mark an artifact stale with a grace period; when it lapses the artifact is
  soft-deleted, then purged after the retention period, and its slug is freed after the tombstone
  period. Every step is reversible until the purge and lands in the audit log.
- **Sign-in through your identity provider.** An OAuth 2.1 authorization server for MCP clients
  (PKCE, Dynamic Client Registration, Client ID Metadata Documents, refresh-token rotation) that
  federates to one OpenID Connect provider: Microsoft Entra ID, Google Workspace, Okta, Keycloak,
  Authentik and others. No passwords are stored. API keys work everywhere too.
- **Operations built in.** Wildcard certificates by ACME DNS-01 (Cloudflare, Route 53, Hetzner),
  certificate files with hot reload, or a reverse proxy in front; SQLite and a blob directory as the
  only state; Prometheus metrics, structured logs, an admin API, quotas and rate limits.

## Quick start

### Docker

```sh
mkdir -p /srv/agalma/data && sudo chown 65532:65532 /srv/agalma/data
docker run -d --name agalma -p 80:80 -p 443:443 \
  -v /srv/agalma/data:/var/lib/agalma \
  -e AGALMA_BASE_DOMAIN=art.example.com \
  -e AGALMA_TLS_EMAIL=ops@example.com \
  -e AGALMA_TLS_DNS_PROVIDER=cloudflare \
  -e AGALMA_TLS_DNS_API_TOKEN=cf-token \
  -e AGALMA_AUTH_ADMIN_TOKEN="$(openssl rand -hex 32)" \
  ghcr.io/peterbooker/agalma:latest serve
```

### Binary

```sh
tar -xzf agalma_<version>_linux_amd64.tar.gz
sudo install -m 0755 agalma /usr/local/bin/agalma
sudo mkdir -p /etc/agalma
sudo install -m 0644 deploy/agalma.yaml /etc/agalma/agalma.yaml            # edit it
sudo install -m 0600 deploy/agalma.env.example /etc/agalma/agalma.env      # secrets
sudo cp deploy/agalma.service /etc/systemd/system/ && sudo systemctl enable --now agalma
```

Both need a wildcard DNS record (`*.art.example.com` and `art.example.com` pointing at the host).
[docs/install.md](docs/install.md) has the full walkthrough, including proxy mode behind Caddy.

Then create a user and a key, and publish:

```sh
agalma --admin-url https://admin.art.example.com --admin-token "$ADMIN_TOKEN" admin users create peter
agalma --admin-url https://admin.art.example.com --admin-token "$ADMIN_TOKEN" admin keys issue peter --label laptop
export AGALMA_URL=https://api.art.example.com AGALMA_TOKEN=agk_...
agalma publish ./site --slug demo        # prints https://demo.art.example.com/
```

## Configuration at a glance

```yaml
base_domain: art.example.com
data_dir: /var/lib/agalma

tls:
  mode: acme
  email: ops@example.com
  dns_provider: cloudflare
  dns:
    api_token: ${AGALMA_TLS_DNS_API_TOKEN}

auth:
  oidc:
    issuer: https://login.microsoftonline.com/<tenant-id>/v2.0
    client_id: "..."
    client_secret: ${AGALMA_AUTH_OIDC_CLIENT_SECRET}
```

Secrets stay out of the file: `${NAME}` is replaced by the environment variable `NAME`, read from the
process environment or a `.env` file (`/etc/agalma/agalma.env` under systemd). Every key can also be
set directly as `AGALMA_<SECTION>_<KEY>`. See [docs/configuration.md](docs/configuration.md).

## Connecting Claude

**Claude Code**, with an API key:

```sh
claude mcp add --transport http agalma https://mcp.art.example.com/ \
  --header "Authorization: Bearer agk_..."
```

Leave out `--header` to let Claude Code sign in through OAuth instead.

**claude.ai**: add a custom connector with the URL `https://mcp.art.example.com/`. claude.ai discovers
the authorization server, sends you to your organisation's identity provider and shows Agalma's
consent page. With no identity provider configured, the consent page asks for an API key instead.

Tools, resources and the authoring conventions are in [docs/mcp.md](docs/mcp.md); sign-in is in
[docs/identity.md](docs/identity.md). A Claude Code skill ships in `skills/agalma/SKILL.md`.

## CLI

```sh
agalma publish ./site --slug q3-report --name "Q3 report"   # create version 1
agalma update q3-report ./site --label "fix chart"            # add files as version 2
agalma get q3-report                                          # record and versions
agalma rollback q3-report 1                                   # serve version 1 again
agalma stale q3-report --grace 72h                            # retire in three days
agalma list --mine any --stale exclude
agalma --json me
```

See [docs/cli.md](docs/cli.md).

## Development

```sh
go run ./cmd/agalma serve --config deploy/dev.yaml      # http://<slug>.localhost:8080/, admin token dev-admin-token
docker compose up --build                               # the same server from the Docker image, data in a named volume
go test -race ./...
test -z "$(gofmt -l .)" && go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@latest ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X agalma/internal/buildinfo.version=$(git describe --tags --always --dirty)" -o agalma ./cmd/agalma
```

Releases are cut by pushing a `v*` tag; GitHub Actions runs goreleaser. The full walkthrough,
including a first publish against the development server, is in
[docs/install.md](docs/install.md#local-development).

## Documentation

| Document                                     | Contents                                                       |
| -------------------------------------------- | -------------------------------------------------------------- |
| [docs/install.md](docs/install.md)           | DNS, release binaries, systemd, Docker, Caddy, TLS, first user |
| [docs/configuration.md](docs/configuration.md) | YAML keys, `${}` references, `.env`, overrides, validation   |
| [docs/ops.md](docs/ops.md)                   | Data layout, backup, upgrades, logs, metrics, jobs, abuse      |
| [docs/api.md](docs/api.md)                   | REST reference for the user and admin APIs                     |
| [docs/mcp.md](docs/mcp.md)                   | MCP tools, resources, prompt and chunked uploads               |
| [docs/cli.md](docs/cli.md)                   | The `agalma` client commands                                   |
| [docs/identity.md](docs/identity.md)         | OAuth, OpenID Connect providers and user provisioning          |
| [docs/security.md](docs/security.md)         | Threat model and security design                               |
| [docs/adr/](docs/adr/)                       | Architecture decision records                                  |

## Status

v1 is in development. The server, REST API, MCP server, OAuth and CLI are implemented; expect
breaking changes before v1.0.0 is tagged.

## Licence

MIT. See [LICENSE](LICENSE).
