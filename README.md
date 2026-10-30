# Kazari

Kazari is a self-hosted artifact host for pages written by LLMs. It lets a team's AI agents, in the cloud or self-hosted, publish HTML pages, dashboards and small sites on the team's own domain, signed in through its own identity provider, without handing the content to a third party.

One Go binary serves every artifact as its own static site at `<slug>.<base>` under a single wildcard certificate. It takes new artifacts and versions over a REST API, an MCP server and a CLI. Pages nobody needs any more are retired through a reversible lifecycle: stale, then soft-deleted, then purged.

> *Kazari* (飾り) is Japanese for a decoration set out on display, like the dolls and New Year pieces that are brought out for a season and then put away. Kazari does the same for pages: it puts them on show for as long as they are wanted, then retires them.

The documentation and guides are at <https://kazari.io/docs/>.

## Features

- **One origin per artifact.** `demo.art.example.com` serves the published version, and `demo--v3.art.example.com` pins version 3 forever. Each artifact is its own browser origin, so one page cannot read another's storage.
- **Immutable, content-addressed versions.** Every publish is a new version, and files are stored once by SHA-256. Roll back or forward with one call; `expected_version` and `If-Match` stop lost updates.
- **Three ways in, one service layer.** REST (`api.<base>/v1`), MCP over Streamable HTTP (`mcp.<base>/`, nineteen tools) and the `kazari` CLI all call the same code and return the same JSON.
- **Edits in place.** Agents change a page with exact or line-anchored edits instead of resending whole files. Whitespace-tolerant matches are reported, and stale anchors are refused before anything is written.
- **Copies of existing pages.** `import_page` fetches a page with its stylesheets, fonts and images onto the instance, removes its scripts and reports an outline of its sections for the edits that follow, so an agent whose own fetch returns text can start from a real page. Any publish can name a URL for a file instead of sending its bytes.
- **A skill per instance.** The server generates an Agent Skill with its own limits and CSP. It serves the skill over MCP (`skill://kazari/SKILL.md`, the Skills extension), through a tool, and as `kazari skill`. Hosts that support MCP Apps show published pages as a preview card.
- **Members-only pages and share links.** A `members` page opens only for people who sign in to the instance in their browser, on every version's host too. A share link with an expiry opens it for someone outside, and one call revokes it. Each host gets its own `HttpOnly` cookie through a single-use handshake with the sign-in host, so no cookie spans artifacts.
- **Large bundles.** Inline JSON for small pages, `multipart` with a `tar.gz` bundle for sites, chunked uploads for MCP clients with no shell, and a one-time archive URL for agents with a shell but no key.
- **Lifecycle.** Mark an artifact stale with a grace period. When the grace period lapses the artifact is soft-deleted, it is purged after the retention period, and its slug is freed after the tombstone period. Every step is reversible until the purge and lands in the audit log.
- **Sign-in through your identity provider.** An OAuth 2.1 authorization server for MCP clients (PKCE, Dynamic Client Registration, Client ID Metadata Documents, refresh-token rotation) federates to one OpenID Connect provider: Microsoft Entra ID, Google Workspace, Okta, Keycloak, Authentik and others. No passwords are stored, and API keys work everywhere too.
- **Operations built in.** Wildcard certificates by ACME DNS-01 through Cloudflare, Route 53, Hetzner, DigitalOcean, Gandi, OVH, Namecheap or DuckDNS, certificate files with hot reload, or a reverse proxy in front. `kazari check` tests the DNS records, the DNS token and the ports before the first start. SQLite and a blob directory are the only state. Prometheus metrics, structured logs, an admin API, quotas and rate limits are included.

## Quick start

To try Kazari on your own machine, with no domain or certificate, run the image with `serve --local` and create a user over the admin socket inside the container:

```sh
docker run -d --name kazari -p 127.0.0.1:8080:8080 -e KAZARI_LISTEN_HTTP=:8080 \
  ghcr.io/peterbooker/kazari:latest serve --local
docker exec kazari kazari admin users create ana --role admin
docker exec kazari kazari admin keys issue ana --label laptop      # prints kzk_... once
```

Pages appear at `http://<slug>.localhost:8080/`, the API at `http://api.localhost:8080/` and the MCP server at `http://mcp.localhost:8080/`. The [quick start](https://kazari.io/docs/quickstart/) goes on to publish a first page from an agent, curl or the CLI.

To run it for a team on `art.example.com`, you need the DNS records `art.example.com` and `*.art.example.com` pointing at the host and an API token for the DNS provider. Two short files configure it, whether under Docker Compose, from a `.deb`/`.rpm` package or from the binary:

```sh
curl -fsSLO https://kazari.io/deploy/compose.yml
curl -fsSLO https://kazari.io/deploy/kazari.yaml                     # edit: your domain, email and DNS provider
curl -fsSL https://kazari.io/deploy/kazari.env.example -o .env       # edit: the DNS provider's token
docker compose up -d
docker compose exec kazari kazari admin users create peter --role admin
```

The [installation guide](docs/install.md) has every path, the preflight check and the first publish, and [DNS and certificates](docs/certificates.md) the records, the providers and the proxy configurations.

## Configuration at a glance

`kazari.yaml` names the domain, the contact email and the DNS provider; everything else has a default. A file for ACME certificates through Cloudflare and sign-in through Microsoft Entra ID looks like this:

```yaml
base_domain: art.example.com
data_dir: /var/lib/kazari

tls:
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

Secrets stay out of the file. Kazari replaces each `${NAME}` with the environment variable `NAME`, read from the process environment or from a dotenv file (`/etc/kazari/kazari.env` under systemd, `.env` beside `compose.yml`), and a secret can also come from a file through `KAZARI_<KEY>_FILE`.

Every key can also be set directly as `KAZARI_<SECTION>_<KEY>`. The [configuration reference](docs/configuration.md) lists every key, and `deploy/kazari.reference.yaml` shows every default.

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

The [MCP server reference](docs/mcp.md) covers the tools, resources and authoring conventions, and [sign-in and identity](docs/identity.md) covers sign-in. For agents that read skills, `examples/kazari/SKILL.md` is the skill an instance with the default settings serves.

## CLI

The `kazari` binary is also the client. Install it on a laptop with `curl -fsSL https://kazari.io/install.sh | sh`. The commands you need most often:

```sh
kazari publish ./site --slug q3-report --name "Q3 report"   # create version 1
kazari update q3-report ./site --label "fix chart"            # add files as version 2
kazari get q3-report                                          # record and versions
kazari rollback q3-report 1                                   # serve version 1 again
kazari stale q3-report --grace 72h                            # retire in three days
kazari list --mine any --stale exclude                        # what you created or updated, not stale
kazari --json me                                              # your account, as JSON
```

On the server, `sudo kazari admin ...` administers the instance over its socket with no token. The [CLI reference](docs/cli.md) lists every command and flag.

## Developing Kazari

To run the development server, the tests and the checks CI runs:

```sh
go run ./cmd/kazari serve --local                       # http://<slug>.localhost:8080/, data in ./.data
go run ./cmd/kazari admin users create peter --role admin   # over the socket in ./.data, no token
docker compose up --build                               # the same server from the Docker image
go test -race ./...                                     # tests with the race detector
test -z "$(gofmt -l .)" && go vet ./...                 # formatting and vet
go run honnef.co/go/tools/cmd/staticcheck@latest ./...  # lint
```

[Developing Kazari](docs/development.md) has the rest: building a stamped binary, releasing, and building the website.

## Documentation

The documentation, with guides for publishing with an agent and from code, is published at <https://kazari.io/docs/>. Its reference pages are the files in `docs/`. The decision records are only here, not on the website:

| Page                                         | Contents                                                       |
| -------------------------------------------- | -------------------------------------------------------------- |
| [Installing Kazari](docs/install.md)         | Compose, packages, the binary, the preflight check, the first user |
| [DNS and certificates](docs/certificates.md) | DNS records, DNS-01 providers, other providers, proxy examples |
| [Configuration](docs/configuration.md)       | YAML keys, `${}` references, dotenv files, overrides, validation |
| [Operating Kazari](docs/ops.md)              | Data layout, backup, upgrades, logs, metrics, jobs, abuse      |
| [REST API](docs/api.md)                      | REST reference for the user and admin APIs                     |
| [MCP server](docs/mcp.md)                    | MCP tools, resources, prompt and chunked uploads               |
| [CLI](docs/cli.md)                           | The `kazari` server and client commands                        |
| [Identity and sign-in](docs/identity.md)     | OAuth, OpenID Connect providers and user provisioning          |
| [Developing Kazari](docs/development.md)     | Running from source, tests, releases, the website              |
>>>

| [Decision records](docs/adr/)                | Architecture decision records                                  |

## Status

v1 is in development. The server, REST API, MCP server, OAuth and CLI are implemented. Expect breaking changes until v1.0.0 is tagged.

## Licence

Kazari is released under the [MIT licence](LICENSE).
