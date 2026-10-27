---
title: Quick start
description: Run Kazari on your own machine from the Docker image, create a user and a key, and publish a first page from an AI agent, curl or the CLI.
---

# Quick start

Run Kazari on your own machine and publish a first page in about five minutes. You need no domain, DNS record or certificate: browsers resolve every `*.localhost` name to your own computer, so pages appear at addresses like `http://hello.localhost:8080/`.

You need [Docker](https://docs.docker.com/get-docker/). An AI agent that can use MCP servers is optional, and so is the `kazari` command on your machine ([installing the client](cli.md#installing-the-client)).

## 1. Start the server

Start the published image with `serve --local`, which serves plain HTTP on port 8080 under the base domain `localhost`:

```sh
docker run -d --name kazari -p 127.0.0.1:8080:8080 -e KAZARI_LISTEN_HTTP=:8080 \
  ghcr.io/peterbooker/kazari:latest serve --local
```

The log, from `docker logs kazari`, ends with a `serving` line that names the addresses and a `no users yet` line that says what to do next. `KAZARI_LISTEN_HTTP` makes the server listen on every interface inside the container; the `-p` keeps the published port on your own machine.

> [!NOTE]
> No Docker? `curl -fsSL https://kazari.io/install.sh | sh` installs the binary, and `kazari serve --local` runs the same server with its data in `./.data`. Run the admin commands below as `kazari admin ...` from the same directory, without `docker exec`.

## 2. Create a user and an API key

The image contains the `kazari` command, and inside the container it reaches the admin API over a socket, so it needs no token. Create a user, here called `ana`:

```sh
docker exec kazari kazari admin users create ana --role admin
```

Then issue that user an API key:

```sh
docker exec kazari kazari admin keys issue ana --label laptop
```

The command prints a key that starts with `kzk_`. It is shown once, so copy it now.

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

With `kazari` installed on your machine, write a one-line page into `site/`, point the CLI at the server with your key, and publish the directory:

```sh
mkdir -p site && echo '<!doctype html><title>Hello</title><h1>Hello</h1>' > site/index.html
export KAZARI_URL=http://api.localhost:8080 KAZARI_TOKEN=kzk_...
kazari publish ./site --slug hello
```

The CLI prints the page's address, `http://hello.localhost:8080/`.

## 4. Look around

With the page published, try its versions:

- Open `http://hello.localhost:8080/` for the published version, and `http://hello--v1.localhost:8080/` for version 1, which never changes.
- Publish again, by asking your agent to change the heading or by running `kazari update hello ./site`. A `--v2` address appears, and `--v1` stays as it was.
- Roll back by asking your agent to "roll hello back to version 1". With the CLI, and `KAZARI_URL` and `KAZARI_TOKEN` exported as above, `kazari get hello` lists the versions and `kazari rollback hello 1` serves version 1 again.

> [!NOTE]
> Browsers and systemd-resolved resolve `*.localhost`, but some other resolvers cannot. If a tool cannot, add the names to `/etc/hosts`: `127.0.0.1 api.localhost admin.localhost mcp.localhost hello.localhost`.

## 5. Stop the server

When you are finished, stop and remove the container:

```sh
docker rm -f kazari
```

The pages live in the container's own volume; `docker rm -fv kazari` removes that too.

## Next steps

- [Publish with an agent](publish-with-an-agent.md): what to ask for, and how to change, share and retire pages.
- [Publish from code](publish-from-code.md): the CLI, preview deployments from CI, and the REST API.
- [Install](install.md): running Kazari on a real domain with TLS, so cloud agents and your team can reach it. A cloud agent connects from the internet, so it needs a public `https` address rather than `localhost`.
