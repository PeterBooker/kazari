# Kazari

Kazari is a self-hosted artifact host for pages written by LLMs. One Go binary serves every artifact as its own static site at `<slug>.<base>` under a single wildcard certificate, takes new artifacts and versions over a REST API, an MCP server and a CLI, and retires pages nobody needs any more through a reversible stale, soft-delete and purge lifecycle. It exists so that a team can let Claude (or any MCP client) publish HTML pages, dashboards and small sites on its own domain, signed in through its own identity provider, without handing the content to a third party.

> *Kazari* (飾り) is Japanese for a decoration set out on display, like the dolls and New Year pieces that are brought out for a season and then put away. Kazari does the same for pages: it puts them on show for as long as they are wanted, then retires them.

Documentation and guides: <https://kazari.io/docs/>

## Features

- **One origin per artifact.** `demo.art.example.com` serves the published version, `demo--v3.art.example.com` pins version 3 forever. Each artifact is its own browser origin, so one page cannot read another's storage.
- **Immutable, content-addressed versions.** Every publish is a new version; files are stored once by SHA-256. Roll back or forward with one call; `expected_version` and `If-Match` stop lost updates.
- **Three ways in, one service layer.** REST (`api.<base>/v1`), MCP over Streamable HTTP (`mcp.<base>/`, twelve tools) and the `kazari` CLI all call the same code and return the same JSON.
- **Large bundles.** Inline JSON for small pages, `multipart` with a `tar.gz` bundle for sites, chunked uploads for MCP clients with no shell.
- **Lifecycle.** Mark an artifact stale with a grace period; when it lapses the artifact is soft-deleted, then purged after the retention period, and its slug is freed after the tombstone period. Every step is reversible until the purge and lands in the audit log.
- **Sign-in through your identity provider.** An OAuth 2.1 authorization server for MCP clients (PKCE, Dynamic Client Registration, Client ID Metadata Documents, refresh-token rotation) that federates to one OpenID Connect provider: Microsoft Entra ID, Google Workspace, Okta, Keycloak, Authentik and others. No passwords are stored. API keys work everywhere too.
- **Operations built in.** Wildcard certificates by ACME DNS-01 (Cloudflare, Route 53, Hetzner), certificate files with hot reload, or a reverse proxy in front; SQLite and a blob directory as the only state; Prometheus metrics, structured logs, an admin API, quotas and rate limits.

## Quick start

### Docker

```sh
mkdir -p /srv/kazari/data && sudo chown 65532:65532 /srv/kazari/data
docker run -d --name kazari -p 80:80 -p 443:443 \
  -v /srv/kazari/data:/var/lib/kazari \
  -e KAZARI_BASE_DOMAIN=art.example.com \
  -e KAZARI_TLS_EMAIL=ops@example.com \
  -e KAZARI_TLS_DNS_PROVIDER=cloudflare \
  -e KAZARI_TLS_DNS_API_TOKEN=cf-token \
  -e KAZARI_AUTH_ADMIN_TOKEN="$(openssl rand -hex 32)" \
  ghcr.io/peterbooker/kazari:latest serve
```

### Binary

```sh
tar -xzf kazari_<version>_linux_amd64.tar.gz
sudo install -m 0755 kazari /usr/local/bin/kazari
sudo mkdir -p /etc/kazari
sudo install -m 0644 deploy/kazari.yaml /etc/kazari/kazari.yaml            # edit it
sudo install -m 0600 deploy/kazari.env.example /etc/kazari/kazari.env      # secrets
sudo cp deploy/kazari.service /etc/systemd/system/ && sudo systemctl enable --now kazari
```

Both need a wildcard DNS record (`*.art.example.com` and `art.example.com` pointing at the host). [docs/install.md](docs/install.md) has the full walkthrough, including proxy mode behind Caddy.

Then create a user and a key, and publish:

```sh
kazari --admin-url https://admin.art.example.com --admin-token "$ADMIN_TOKEN" admin users create peter
kazari --admin-url https://admin.art.example.com --admin-token "$ADMIN_TOKEN" admin keys issue peter --label laptop
export KAZARI_URL=https://api.art.example.com KAZARI_TOKEN=kzk_...
kazari publish ./site --slug demo        # prints https://demo.art.example.com/
```

## Configuration at a glance

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
  oidc:
    issuer: https://login.microsoftonline.com/<tenant-id>/v2.0
    client_id: "..."
    client_secret: ${KAZARI_AUTH_OIDC_CLIENT_SECRET}
```

Secrets stay out of the file: `${NAME}` is replaced by the environment variable `NAME`, read from the process environment or a `.env` file (`/etc/kazari/kazari.env` under systemd). Every key can also be set directly as `KAZARI_<SECTION>_<KEY>`. See [docs/configuration.md](docs/configuration.md).

## Connecting Claude

**Claude Code**, with an API key:

```sh
claude mcp add --transport http kazari https://mcp.art.example.com/ \
  --header "Authorization: Bearer kzk_..."
```

Leave out `--header` to let Claude Code sign in through OAuth instead.

**claude.ai**: add a custom connector with the URL `https://mcp.art.example.com/`. claude.ai discovers the authorization server, sends you to your organisation's identity provider and shows Kazari's consent page. With no identity provider configured, the consent page asks for an API key instead.

Tools, resources and the authoring conventions are in [docs/mcp.md](docs/mcp.md); sign-in is in [docs/identity.md](docs/identity.md). A Claude Code skill ships in `skills/kazari/SKILL.md`.

## CLI

```sh
kazari publish ./site --slug q3-report --name "Q3 report"   # create version 1
kazari update q3-report ./site --label "fix chart"            # add files as version 2
kazari get q3-report                                          # record and versions
kazari rollback q3-report 1                                   # serve version 1 again
kazari stale q3-report --grace 72h                            # retire in three days
kazari list --mine any --stale exclude
kazari --json me
```

See [docs/cli.md](docs/cli.md).

## Development

```sh
go run ./cmd/kazari serve --config deploy/dev.yaml      # http://<slug>.localhost:8080/, admin token dev-admin-token
docker compose up --build                               # the same server from the Docker image, data in a named volume
go test -race ./...
test -z "$(gofmt -l .)" && go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@latest ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X kazari/internal/buildinfo.version=$(git describe --tags --always --dirty)" -o kazari ./cmd/kazari
```

Releases are cut by pushing a `v*` tag; GitHub Actions runs goreleaser. The full walkthrough, including a first publish against the development server, is in [docs/install.md](docs/install.md#local-development).

### Website

The website at <https://kazari.io/> is a [Hugo](https://gohugo.io/) site in `site/`. It mounts the Markdown files in `docs/` as its reference pages, so a change to `docs/` is a change to the site. The landing page and the guides (quick start, publishing with Claude, publishing from code) live in `site/content/`. Building it needs only the standard edition of Hugo, at the version set as `HUGO_VERSION` in `.github/workflows/pages.yml`: no Node, no theme.

```sh
go install github.com/gohugoio/hugo@v0.168.0   # Hugo at the version CI uses
hugo server --source site                      # preview at http://localhost:1313/, reloads on save
hugo --source site --gc --minify               # build into site/public/ as CI does
```

- Pages in `docs/` stay plain Markdown that reads well on GitHub: no front matter, the first `# ` heading is the title, and links between pages are relative `.md` links (`install.md#tls-modes`). The build fails on a link to a page that does not exist.
- A page appears in the sidebar, the previous and next links and the search once it is listed in `site/data/nav.toml`.
- The fonts are self-hosted and subset to the characters the site uses. After adding Japanese text, run `site/subset-fonts.sh` (needs `curl` and fonttools with brotli) and commit the regenerated files in `site/static/fonts/`.
- `.github/workflows/pages.yml` builds the site for pull requests that touch `site/` or `docs/` and deploys it from `main`. The repository's Pages source must be set to GitHub Actions.

## Documentation

The documentation is published, with guides for publishing with Claude and from code, at <https://kazari.io/docs/>. The reference pages are the files in `docs/`:

| Document                                     | Contents                                                       |
| -------------------------------------------- | -------------------------------------------------------------- |
| [docs/install.md](docs/install.md)           | DNS, release binaries, systemd, Docker, Caddy, TLS, first user |
| [docs/configuration.md](docs/configuration.md) | YAML keys, `${}` references, `.env`, overrides, validation   |
| [docs/ops.md](docs/ops.md)                   | Data layout, backup, upgrades, logs, metrics, jobs, abuse      |
| [docs/api.md](docs/api.md)                   | REST reference for the user and admin APIs                     |
| [docs/mcp.md](docs/mcp.md)                   | MCP tools, resources, prompt and chunked uploads               |
| [docs/cli.md](docs/cli.md)                   | The `kazari` client commands                                   |
| [docs/identity.md](docs/identity.md)         | OAuth, OpenID Connect providers and user provisioning          |
| [docs/security.md](docs/security.md)         | Threat model and security design                               |
| [docs/adr/](docs/adr/)                       | Architecture decision records                                  |

## Status

v1 is in development. The server, REST API, MCP server, OAuth and CLI are implemented; expect breaking changes before v1.0.0 is tagged.

## Licence

MIT. See [LICENSE](LICENSE).
