# Kazari

Kazari is a self-hosted artifact host for pages written by LLMs. It lets a team's AI agents, in the cloud or self-hosted, publish HTML pages, dashboards and small sites on the team's own domain, signed in through its own identity provider, without handing the content to a third party.

One Go binary serves every artifact as its own static site at `<slug>.<base>` under a single wildcard certificate. It takes new artifacts and versions over a REST API, an MCP server and a CLI. Pages nobody needs any more are retired through a reversible lifecycle: stale, then soft-deleted, then purged.

> *Kazari* (飾り) is Japanese for a decoration set out on display, like the dolls and New Year pieces that are brought out for a season and then put away. Kazari does the same for pages: it puts them on show for as long as they are wanted, then retires them.

The documentation and guides are at <https://kazari.io/docs/>.

## Features

- **One origin per artifact.** `demo.art.example.com` serves the published version, and `demo--v3.art.example.com` pins version 3 forever. Each artifact is its own browser origin, so one page cannot read another's storage.
- **Immutable, content-addressed versions.** Every publish is a new version, and files are stored once by SHA-256. Roll back or forward with one call; `expected_version` and `If-Match` stop lost updates.
- **Three ways in, one service layer.** REST (`api.<base>/v1`), MCP over Streamable HTTP (`mcp.<base>/`, seventeen tools) and the `kazari` CLI all call the same code and return the same JSON.
- **Edits in place.** Agents change a page with exact or line-anchored edits instead of resending whole files. Whitespace-tolerant matches are reported, and stale anchors are refused before anything is written.
- **A skill per instance.** The server generates an Agent Skill with its own limits and CSP. It serves the skill over MCP (`skill://kazari/SKILL.md`, the Skills extension), through a tool, and as `kazari skill`. Hosts that support MCP Apps show published pages as a preview card.
- **Members-only pages and share links.** A `members` page opens only for people who sign in to the instance in their browser, on every version's host too. A share link with an expiry opens it for someone outside, and one call revokes it. Each host gets its own `HttpOnly` cookie through a single-use handshake with the sign-in host, so no cookie spans artifacts.
- **Large bundles.** Inline JSON for small pages, `multipart` with a `tar.gz` bundle for sites, and chunked uploads for MCP clients with no shell.
- **Lifecycle.** Mark an artifact stale with a grace period. When the grace period lapses the artifact is soft-deleted, it is purged after the retention period, and its slug is freed after the tombstone period. Every step is reversible until the purge and lands in the audit log.
- **Sign-in through your identity provider.** An OAuth 2.1 authorization server for MCP clients (PKCE, Dynamic Client Registration, Client ID Metadata Documents, refresh-token rotation) federates to one OpenID Connect provider: Microsoft Entra ID, Google Workspace, Okta, Keycloak, Authentik and others. No passwords are stored, and API keys work everywhere too.
- **Operations built in.** Wildcard certificates by ACME DNS-01 (Cloudflare, Route 53, Hetzner), certificate files with hot reload, or a reverse proxy in front. SQLite and a blob directory are the only state. Prometheus metrics, structured logs, an admin API, quotas and rate limits are included.

## Quick start

Either way of running Kazari needs DNS records for `*.art.example.com` and `art.example.com` pointing at the host. The [installation guide](docs/install.md) has the full walkthrough, including proxy mode behind Caddy.

### Running with Docker

To run the image with everything configured through the environment:

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

The container runs as UID 65532, so the data directory must be writable by that user.

### Running the release binary

To install the binary from a release archive and run it under systemd:

```sh
tar -xzf kazari_<version>_linux_amd64.tar.gz
sudo install -m 0755 kazari /usr/local/bin/kazari
sudo mkdir -p /etc/kazari
sudo install -m 0644 deploy/kazari.yaml /etc/kazari/kazari.yaml            # edit it
sudo install -m 0600 deploy/kazari.env.example /etc/kazari/kazari.env      # secrets
sudo cp deploy/kazari.service /etc/systemd/system/ && sudo systemctl enable --now kazari
```

The configuration goes in `/etc/kazari/kazari.yaml` and its secrets in `/etc/kazari/kazari.env`.

### Publishing a first page

With the server running, use the admin token to create a user and an API key, then publish a directory with the key:

```sh
kazari --admin-url https://admin.art.example.com --admin-token "$ADMIN_TOKEN" admin users create peter
kazari --admin-url https://admin.art.example.com --admin-token "$ADMIN_TOKEN" admin keys issue peter --label laptop
export KAZARI_URL=https://api.art.example.com KAZARI_TOKEN=kzk_...
kazari publish ./site --slug demo        # prints https://demo.art.example.com/
```

The key (`kzk_...`) is shown only once, so copy it when `admin keys issue` prints it.

## Configuration at a glance

A configuration file for ACME certificates through Cloudflare and sign-in through Microsoft Entra ID looks like this:

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

Secrets stay out of the file. Kazari replaces each `${NAME}` with the environment variable `NAME`, read from the process environment or from a `.env` file (`/etc/kazari/kazari.env` under systemd).

Every key can also be set directly as `KAZARI_<SECTION>_<KEY>`. The [configuration reference](docs/configuration.md) lists every key.

## Connecting an agent

Kazari works with any agent that can use a remote MCP server over Streamable HTTP, whether it runs in the cloud or on your own machine. The server is at `https://mcp.art.example.com/`.

### Cloud agents

Add a custom connector with the URL `https://mcp.art.example.com/`. The agent discovers the authorization server, sends you to your organisation's identity provider and shows Kazari's consent page. With no identity provider configured, the consent page asks for an API key instead.

In claude.ai, for example, custom connectors are under **Settings → Connectors**.

### Self-hosted agents

Add the server to the agent's configuration with an API key. Many agents take a JSON entry along these lines:

```json
{
  "mcpServers": {
    "kazari": {
      "url": "https://mcp.art.example.com/",
      "headers": { "Authorization": "Bearer kzk_..." }
    }
  }
}
```

Leave out `headers` to sign in through OAuth instead, if your agent supports it.

Claude Code, for example, adds the server with one command:

```sh
claude mcp add --transport http kazari https://mcp.art.example.com/
```

To use a key instead of OAuth, add `--header "Authorization: Bearer kzk_..."` to that command.

The [MCP server reference](docs/mcp.md) covers the tools, resources and authoring conventions, and [sign-in and identity](docs/identity.md) covers sign-in. For agents that read skills, `examples/kazari/SKILL.md` is an example skill.

## CLI

The `kazari` binary is also the client. The commands you need most often:

```sh
kazari publish ./site --slug q3-report --name "Q3 report"   # create version 1
kazari update q3-report ./site --label "fix chart"            # add files as version 2
kazari get q3-report                                          # record and versions
kazari rollback q3-report 1                                   # serve version 1 again
kazari stale q3-report --grace 72h                            # retire in three days
kazari list --mine any --stale exclude                        # what you created or updated, not stale
kazari --json me                                              # your account, as JSON
```

The [CLI reference](docs/cli.md) lists every command and flag.

## Developing Kazari

To run the development server, the tests and most of the checks CI runs:

```sh
go run ./cmd/kazari serve --config deploy/dev.yaml      # http://<slug>.localhost:8080/, admin token dev-admin-token
docker compose up --build                               # the same server from the Docker image, data in a named volume
go test -race ./...                                     # tests with the race detector
test -z "$(gofmt -l .)" && go vet ./...                 # formatting and vet
go run honnef.co/go/tools/cmd/staticcheck@latest ./...  # lint
go run golang.org/x/vuln/cmd/govulncheck@latest ./...   # known vulnerabilities
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X kazari/internal/buildinfo.version=$(git describe --tags --always --dirty)" -o kazari ./cmd/kazari
```

The last command builds the binary, stamped with the version from git.

To release, push a version tag without a `v`, such as `0.2.0`, and GitHub Actions runs goreleaser. [Local development](docs/install.md#local-development) in the installation guide has the full walkthrough, including a first publish against the development server.

### Building the website

The website at <https://kazari.io/> is a [Hugo](https://gohugo.io/) site in `site/`. It mounts the Markdown files in `docs/` as its reference pages, except `security.md` and `adr/`, so a change to `docs/` is a change to the site.

The guides (quick start, publishing with an agent, publishing from code) live in `site/content/docs/`, and the landing page is `site/layouts/home.html`. Building the site needs only the standard edition of Hugo, at the version set as `HUGO_VERSION` in `.github/workflows/pages.yml`: no Node and no theme.

To install Hugo, preview the site and build it as CI does:

```sh
go install github.com/gohugoio/hugo@v0.168.0   # Hugo at the version CI uses
hugo server --source site                      # preview at http://localhost:1313/, reloads on save
hugo --source site --gc --minify               # build into site/public/ as CI does
```

Before changing the site or the docs, know that:

- Pages in `docs/` stay plain Markdown that reads well on GitHub: no front matter, the first `# ` heading is the title, and links between pages are relative `.md` links (`install.md#tls-modes`). The build fails on a link to a page that does not exist.
- A page appears in the sidebar, the previous and next links and the search once it is listed in `site/data/nav.toml`.
- The fonts are self-hosted and subset to the characters the site uses. After adding Japanese text, run `site/subset-fonts.sh` (it needs `curl` and fonttools with brotli) and commit the regenerated files in `site/static/fonts/`.
- `.github/workflows/pages.yml` builds the site for pull requests that touch `site/` or `docs/`, and deploys it from `main`. The repository's Pages source must be set to GitHub Actions.

## Documentation

The documentation, with guides for publishing with an agent and from code, is published at <https://kazari.io/docs/>. Its reference pages are the files in `docs/`. The security design and the decision records are only here, not on the website:

| Page                                         | Contents                                                       |
| -------------------------------------------- | -------------------------------------------------------------- |
| [Installing Kazari](docs/install.md)         | DNS, release binaries, systemd, Docker, Caddy, TLS, first user |
| [Configuration](docs/configuration.md)       | YAML keys, `${}` references, `.env`, overrides, validation     |
| [Operating Kazari](docs/ops.md)              | Data layout, backup, upgrades, logs, metrics, jobs, abuse      |
| [REST API](docs/api.md)                      | REST reference for the user and admin APIs                     |
| [MCP server](docs/mcp.md)                    | MCP tools, resources, prompt and chunked uploads               |
| [CLI](docs/cli.md)                           | The `kazari` client commands                                   |
| [Identity and sign-in](docs/identity.md)     | OAuth, OpenID Connect providers and user provisioning          |
| [Security](docs/security.md)                 | Threat model and security design                               |
| [Decision records](docs/adr/)                | Architecture decision records                                  |

## Status

v1 is in development. The server, REST API, MCP server, OAuth and CLI are implemented. Expect breaking changes until v1.0.0 is tagged.

## Licence

Kazari is released under the [MIT licence](LICENSE).
