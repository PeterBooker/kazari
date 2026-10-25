---
title: Quick start
description: Run Kazari on your own machine with Docker, create a user and a key, and publish a first page from an AI agent, curl or the CLI.
---

# Quick start

Run Kazari on your own machine and publish a first page in about five minutes. You need no domain, DNS record or certificate: browsers resolve every `*.localhost` name to your own computer, so pages appear at addresses like `http://hello.localhost:8080/`.

You need [Docker](https://docs.docker.com/get-docker/) with Compose and `git`. An AI agent that can use MCP servers is optional, and so is [Go](https://go.dev/dl/) (1.27 or later), which runs the CLI from source.

## 1. Start the server

```sh
git clone https://github.com/PeterBooker/kazari.git
cd kazari
docker compose up --build -d
```

This builds the image and starts the development server on port 8080 with the configuration in `deploy/dev.yaml`: plain HTTP, the base domain `localhost`, and the admin token `dev-admin-token`. `docker compose ps` shows it as healthy once it is ready.

> [!TIP]
> No Docker? With Go installed, `go run ./cmd/kazari serve --config deploy/dev.yaml` runs the same server and keeps its data in `./.data`.

## 2. Create a user and an API key

The image contains the `kazari` CLI, so you can run the admin commands inside the container:

```sh
docker compose exec kazari /usr/local/bin/kazari \
  --admin-url http://admin.localhost:8080 --admin-token dev-admin-token \
  admin users create ana

docker compose exec kazari /usr/local/bin/kazari \
  --admin-url http://admin.localhost:8080 --admin-token dev-admin-token \
  admin keys issue ana --label laptop
```

The second command prints a key that starts with `kzk_`. It is shown once, so copy it now. With Go installed you can run the same commands on your machine as `go run ./cmd/kazari ...`.

## 3. Publish a page

Pick whichever way suits you.

### With an agent

Use an agent that runs on your machine: a cloud agent cannot reach `localhost`. Add the MCP server `http://mcp.localhost:8080/` to its configuration with your key. Many agents take a JSON entry like this; check your agent's documentation for where it goes:

```json
{
  "mcpServers": {
    "kazari": {
      "url": "http://mcp.localhost:8080/",
      "headers": { "Authorization": "Bearer kzk_..." }
    }
  }
}
```

Claude Code, for example, adds it with one command:

```sh
claude mcp add --transport http kazari http://mcp.localhost:8080/ \
  --header "Authorization: Bearer kzk_..."
```

Then ask your agent for something:

> Make a one-page "hello" site with a big friendly heading and publish it to Kazari with the slug hello.

The agent calls the `create_artifact` tool and replies with the address, `http://hello.localhost:8080/`.

### With curl

```sh
curl -sS http://api.localhost:8080/v1/artifacts \
  -H "Authorization: Bearer kzk_..." \
  -H 'Content-Type: application/json' \
  -d '{"slug": "hello", "files": [{"path": "index.html", "content": "<!doctype html><title>Hello</title><h1>Hello</h1>"}]}'
```

The response carries the page's `url` and its `version`.

### With the CLI

```sh
mkdir -p site && echo '<!doctype html><title>Hello</title><h1>Hello</h1>' > site/index.html
export KAZARI_URL=http://api.localhost:8080 KAZARI_TOKEN=kzk_...
go run ./cmd/kazari publish ./site --slug hello
```

## 4. Look around

- Open `http://hello.localhost:8080/` for the published version and `http://hello--v1.localhost:8080/` for version 1, which never changes.
- Publish again (ask your agent to change the heading, or run `go run ./cmd/kazari update hello ./site`) and a `--v2` address appears while `--v1` stays as it was.
- Ask your agent to "roll hello back to version 1", or, with `KAZARI_URL` and `KAZARI_TOKEN` exported as above, run `go run ./cmd/kazari get hello` to list the versions and `go run ./cmd/kazari rollback hello 1` to serve version 1 again.

> [!NOTE]
> If a tool cannot resolve `*.localhost` (browsers and systemd-resolved can; some other resolvers cannot), add the names to `/etc/hosts`: `127.0.0.1 api.localhost admin.localhost mcp.localhost hello.localhost`.

When you are finished, `docker compose down` stops the server; `docker compose down -v` also deletes its data.

## Next steps

- [Publish with an agent](publish-with-an-agent.md): what to ask for, and how to change, share and retire pages.
- [Publish from code](publish-from-code.md): the CLI, preview deployments from CI, and the REST API.
- [Install](install.md): run Kazari on a real domain with TLS, so cloud agents and your team can reach it. A cloud agent connects from the internet, so it needs a public `https` address rather than `localhost`.
