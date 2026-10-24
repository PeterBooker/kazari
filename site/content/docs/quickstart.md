---
title: Quick start
description: Run Agalma on your own machine with Docker, create a user and a key, and publish a first page from Claude Code, curl or the CLI.
---

# Quick start

Run Agalma on your own machine and publish a first page in about five minutes. You need no domain,
DNS record or certificate: browsers resolve every `*.localhost` name to your own computer, so pages
appear at addresses like `http://hello.localhost:8080/`.

You need [Docker](https://docs.docker.com/get-docker/) with Compose and `git`. Claude Code is
optional, and so is [Go](https://go.dev/dl/) (1.27 or later), which runs the CLI from source.

## 1. Start the server

```sh
git clone https://github.com/PeterBooker/agalma.git
cd agalma
docker compose up --build -d
```

This builds the image and starts the development server on port 8080 with the configuration in
`deploy/dev.yaml`: plain HTTP, the base domain `localhost`, and the admin token `dev-admin-token`.
`docker compose ps` shows it as healthy once it is ready.

> [!TIP]
> No Docker? With Go installed, `go run ./cmd/agalma serve --config deploy/dev.yaml` runs the same
> server and keeps its data in `./.data`.

## 2. Create a user and an API key

The image contains the `agalma` CLI, so you can run the admin commands inside the container:

```sh
docker compose exec agalma /usr/local/bin/agalma \
  --admin-url http://admin.localhost:8080 --admin-token dev-admin-token \
  admin users create ana

docker compose exec agalma /usr/local/bin/agalma \
  --admin-url http://admin.localhost:8080 --admin-token dev-admin-token \
  admin keys issue ana --label laptop
```

The second command prints a key that starts with `agk_`. It is shown once, so copy it now. With Go
installed you can run the same commands on your machine as `go run ./cmd/agalma ...`.

## 3. Publish a page

Pick whichever way suits you.

### With Claude Code

Add the MCP server with your key:

```sh
claude mcp add --transport http agalma http://mcp.localhost:8080/ \
  --header "Authorization: Bearer agk_..."
```

Start Claude Code and ask for something:

> Make a one-page "hello" site with a big friendly heading and publish it to Agalma with the slug hello.

Claude calls the `create_artifact` tool and replies with the address,
`http://hello.localhost:8080/`.

### With curl

```sh
curl -sS http://api.localhost:8080/v1/artifacts \
  -H "Authorization: Bearer agk_..." \
  -H 'Content-Type: application/json' \
  -d '{"slug": "hello", "files": [{"path": "index.html", "content": "<!doctype html><title>Hello</title><h1>Hello</h1>"}]}'
```

The response carries the page's `url` and its `version`.

### With the CLI

```sh
mkdir -p site && echo '<!doctype html><title>Hello</title><h1>Hello</h1>' > site/index.html
export AGALMA_URL=http://api.localhost:8080 AGALMA_TOKEN=agk_...
go run ./cmd/agalma publish ./site --slug hello
```

## 4. Look around

- Open `http://hello.localhost:8080/` for the published version and `http://hello--v1.localhost:8080/`
  for version 1, which never changes.
- Publish again (ask Claude to change the heading, or run `go run ./cmd/agalma update hello ./site`)
  and a `--v2` address appears while `--v1` stays as it was.
- Ask Claude to "roll hello back to version 1", or, with `AGALMA_URL` and `AGALMA_TOKEN` exported
  as above, run `go run ./cmd/agalma get hello` to list the versions and
  `go run ./cmd/agalma rollback hello 1` to serve version 1 again.

> [!NOTE]
> If a tool cannot resolve `*.localhost` (browsers and systemd-resolved can; some other resolvers
> cannot), add the names to `/etc/hosts`:
> `127.0.0.1 api.localhost admin.localhost mcp.localhost hello.localhost`.

When you are finished, `docker compose down` stops the server; `docker compose down -v` also deletes
its data.

## Next steps

- [Publish with Claude](publish-with-claude.md): what to ask for, and how to change, share and retire pages.
- [Publish from code](publish-from-code.md): the CLI, preview deployments from CI, and the REST API.
- [Install](install.md): run Agalma on a real domain with TLS, so claude.ai and your team can reach it.
  claude.ai connects from the internet, so it needs a public `https` address rather than `localhost`.
