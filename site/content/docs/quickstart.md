---
title: Quick start
description: Run Kazari on your own machine with Docker, create a user and a key, and publish a first page from an AI agent, curl or the CLI.
---

# Quick start

Run Kazari on your own machine and publish a first page in about five minutes. You need no domain, DNS record or certificate: browsers resolve every `*.localhost` name to your own computer, so pages appear at addresses like `http://hello.localhost:8080/`.

You need [Docker](https://docs.docker.com/get-docker/) with Compose, and `git`. An AI agent that can use MCP servers is optional. So is [Go](https://go.dev/dl/) 1.27 or later, which runs the CLI from source.

## 1. Start the server

Clone the repository and start the development server with Docker Compose:

```sh
git clone https://github.com/PeterBooker/kazari.git
cd kazari
docker compose up --build -d
```

This builds the image and starts the server on port 8080 with the configuration in `deploy/dev.yaml`: plain HTTP, the base domain `localhost` and the admin token `dev-admin-token`. Once it is ready, `docker compose ps` shows it as healthy.

> [!NOTE]
> No Docker? With Go installed, `go run ./cmd/kazari serve --config deploy/dev.yaml` runs the same server and keeps its data in `./.data`.

## 2. Create a user and an API key

The image contains the `kazari` CLI, so you can run the admin commands inside the container. First create a user, here called `ana`:

```sh
docker compose exec kazari /usr/local/bin/kazari \
  --admin-url http://admin.localhost:8080 --admin-token dev-admin-token \
  admin users create ana
```

Then issue that user an API key:

```sh
docker compose exec kazari /usr/local/bin/kazari \
  --admin-url http://admin.localhost:8080 --admin-token dev-admin-token \
  admin keys issue ana --label laptop
```

The command prints a key that starts with `kzk_`. It is shown once, so copy it now.

If you have Go installed, you can run the same commands on your machine as `go run ./cmd/kazari ...`.

## 3. Publish a page

Publish with an agent, with curl or with the CLI, whichever suits you.

### With an agent

Use an agent that runs on your machine, because a cloud agent cannot reach `localhost`. Add the MCP server `http://mcp.localhost:8080/` to its configuration, with your key.

Many agents take a JSON entry like this one. Your agent's documentation says where it goes:

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

Claude Code, for example, adds the server with one command:

```sh
claude mcp add --transport http kazari http://mcp.localhost:8080/ \
  --header "Authorization: Bearer kzk_..."
```

Then ask your agent for something:

> Make a one-page "hello" site with a big friendly heading and publish it to Kazari with the slug hello.

The agent calls the `create_artifact` tool and replies with the address, `http://hello.localhost:8080/`.

### With curl

Send the page's HTML to the REST API, with your key in the `Authorization` header:

```sh
curl -sS http://api.localhost:8080/v1/artifacts \
  -H "Authorization: Bearer kzk_..." \
  -H 'Content-Type: application/json' \
  -d '{"slug": "hello", "files": [{"path": "index.html", "content": "<!doctype html><title>Hello</title><h1>Hello</h1>"}]}'
```

The response carries the page's `url` and its `version`.

### With the CLI

Write a one-line page into `site/`, point the CLI at the server with your key, and publish the directory:

```sh
mkdir -p site && echo '<!doctype html><title>Hello</title><h1>Hello</h1>' > site/index.html
export KAZARI_URL=http://api.localhost:8080 KAZARI_TOKEN=kzk_...
go run ./cmd/kazari publish ./site --slug hello
```

The CLI prints the page's address, `http://hello.localhost:8080/`.

## 4. Look around

With the page published, try its versions:

- Open `http://hello.localhost:8080/` for the published version, and `http://hello--v1.localhost:8080/` for version 1, which never changes.
- Publish again, by asking your agent to change the heading or by running `go run ./cmd/kazari update hello ./site`. A `--v2` address appears, and `--v1` stays as it was.
- Roll back by asking your agent to "roll hello back to version 1". With the CLI, and `KAZARI_URL` and `KAZARI_TOKEN` exported as above, `go run ./cmd/kazari get hello` lists the versions and `go run ./cmd/kazari rollback hello 1` serves version 1 again.

> [!NOTE]
> Browsers and systemd-resolved resolve `*.localhost`, but some other resolvers cannot. If a tool cannot, add the names to `/etc/hosts`: `127.0.0.1 api.localhost admin.localhost mcp.localhost hello.localhost`.

## 5. Stop the server

When you are finished, stop the server:

```sh
docker compose down
```

To delete its data as well, run `docker compose down -v` instead.

## Next steps

- [Publish with an agent](publish-with-an-agent.md): what to ask for, and how to change, share and retire pages.
- [Publish from code](publish-from-code.md): the CLI, preview deployments from CI, and the REST API.
- [Install](install.md): running Kazari on a real domain with TLS, so cloud agents and your team can reach it. A cloud agent connects from the internet, so it needs a public `https` address rather than `localhost`.
