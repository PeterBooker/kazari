# Developing Kazari

This page is for working on Kazari itself: running the server from source, running it from the Docker image, the checks CI runs, releasing, and building the website. Operating an instance is the [installation guide](install.md).

You need [Go](https://go.dev/dl/) at the version in `go.mod`, and Docker for the Compose setup.

## Running the server from source

`serve --local` runs the server with no DNS, certificate or token: plain HTTP on `127.0.0.1:8080`, the base domain `kazari.localhost`, which browsers resolve to the local machine, and the data in `./.data`. Start it from the repository root:

```sh
go run ./cmd/kazari serve --local
```

Pages appear at `http://<slug>.kazari.localhost:8080/`, the API at `http://api.kazari.localhost:8080/`, the MCP server at `http://mcp.kazari.localhost:8080/`, the [dashboard](dashboard.md) at `http://admin.kazari.localhost:8080/` and the account page at `http://kazari.localhost:8080/account/`. `KAZARI_*` variables still apply on top of the preset. To start again from nothing, delete `./.data`.

### Signing in to the dashboard

A fresh `./.data` has no users, so the server prints a setup link when it starts:

```text
Kazari has no users yet. Open this link to create the first admin:

    http://kazari.localhost:8080/account/setup?token=kzl_...
```

Open it, choose a handle and a password of at least 15 characters, and sign in on the page that follows. The link works for an hour; while the instance has no user, `go run ./cmd/kazari admin setup-link` prints a fresh one ([creating the first admin](install.md#creating-the-first-admin)).

### Trying passkeys

Passkeys work under `serve --local`, because browsers treat `*.localhost` as a secure origin and `kazari.localhost` is a name they make passkeys for. To add one, open the account page, enter your password under **Passkeys** and choose **Add a passkey**. After you sign out, the sign-in page offers **Sign in with a passkey** ([passkeys](identity.md#passkeys)).

To test without a real device, open Chromium's DevTools, choose **More tools** then **WebAuthn**, and add a virtual authenticator with resident key and user verification support. The browser then makes and uses passkeys without a prompt.

### Publishing with an API key

The admin commands find the server's socket in `./.data`, so they need no token. In a second terminal, issue a key for your user and publish with it:

```sh
go run ./cmd/kazari admin keys issue peter --label laptop

KAZARI_URL=http://api.kazari.localhost:8080 KAZARI_TOKEN=kzk_... go run ./cmd/kazari publish ./site --slug demo
```

Open `http://demo.kazari.localhost:8080/` to see the page. **Make the key** on the account page issues a key the same way. To script a fresh instance without a browser, create the user with `go run ./cmd/kazari admin users create peter --role admin` instead of the setup link. That user has no password, so they sign in to the dashboard with **Use an API key instead**.

`serve --local` reads `./.env` for its `KAZARI_*` overrides. The client commands do not, but `KAZARI_URL` and `KAZARI_TOKEN` can live in a file you name with `KAZARI_ENV_FILE` instead of on the command line ([the rule](cli.md#environment-variables-and-env)). Keep `.env` out of version control. The `kazari-dev` MCP server in `.mcp.json` connects Claude Code to this server with a key exported as `KAZARI_DEV_TOKEN`.

> [!NOTE]
> Most browsers and systemd-resolved resolve `*.localhost`. If a tool on your system does not, add the hosts you need to `/etc/hosts`: `127.0.0.1 kazari.localhost api.kazari.localhost admin.kazari.localhost mcp.kazari.localhost demo.kazari.localhost`.

## Running the Docker image

`compose.yml` at the repository root builds `deploy/Dockerfile` and runs the image with `serve --local`, so the addresses and commands above are unchanged. The data lives in the named volume `kazari_data`, the port is published on loopback only, and `./.env` is passed into the container when it exists. Start it and follow the log:

```sh
docker compose up --build -d
docker compose logs -f
```

On a fresh volume the log shows the setup link. The admin commands run inside the container, over the socket in the volume:

```sh
docker compose exec kazari kazari admin setup-link
docker compose exec kazari kazari admin keys issue peter --label laptop
```

`docker compose watch` rebuilds and restarts the container when the source changes, and `docker compose down` stops it; add `-v` to discard the data as well. `deploy/compose.yml` is the Compose file for a real server.

### Watching traces and metrics

The `telemetry` profile adds [grafana/otel-lgtm](https://github.com/grafana/docker-otel-lgtm): an OpenTelemetry Collector with Tempo, Prometheus and Loki behind Grafana. To start it with Kazari exporting all three signals to it, run:

```sh
export KAZARI_TELEMETRY_OTLP_ENDPOINT=http://lgtm:4318 KAZARI_TELEMETRY_METRICS_OTLP=true KAZARI_TELEMETRY_LOGS_OTLP=true
docker compose --profile telemetry up --build -d
```

Grafana answers at <http://localhost:3000/>. Under Explore, Tempo shows each request's trace, Prometheus the metrics and Loki the log lines, which link to their trace. The local preset samples one page view in a hundred, as a server does; to trace every one, also export `KAZARI_TELEMETRY_TRACES_SITE_SAMPLE_RATIO=1`.

## Building, testing and releasing

These commands build a binary, run the same checks as CI and make a local release dry run:

```sh
# build, stamped with the version from git
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/PeterBooker/kazari/internal/buildinfo.version=$(git describe --tags --always --dirty)" -o kazari ./cmd/kazari

go test -race ./...                                       # tests with the race detector
test -z "$(gofmt -l .)"                                   # formatting check
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@latest ./...    # lint
go run golang.org/x/vuln/cmd/govulncheck@latest ./...     # known vulnerabilities
shellcheck site/static/install.sh deploy/packaging/*.sh   # the install and package scripts
go run github.com/goreleaser/goreleaser/v2@latest check   # the release configuration

goreleaser release --snapshot --clean --skip=publish,sign,sbom   # local release dry run
```

On pull requests and pushes to `main`, GitHub Actions runs these checks, `go mod tidy -diff`, the cross-builds, and `install.sh` against the latest release.

To release, push a tag named for the version without a `v`, such as `0.3.0`. The workflow runs goreleaser, which publishes the archives with SBOMs and a cosign-signed `checksums.txt`, the `.deb` and `.rpm` packages, and the signed image at `ghcr.io/peterbooker/kazari`.

## Building the website

The website at <https://kazari.io/> is a [Hugo](https://gohugo.io/) site in `site/`. It mounts the Markdown files in `docs/` as its reference pages, and the files in `deploy/` under `/deploy/`, so a change to either directory is a change to the site. The guides live in `site/content/docs/`, the landing page is `site/layouts/home.html`, and the install script is `site/static/install.sh`.

Building the site needs only the standard edition of Hugo, at the version set as `HUGO_VERSION` in `.github/workflows/pages.yml`: no Node and no theme. Install it, preview the site and build it as CI does:

```sh
go install github.com/gohugoio/hugo@v0.168.0   # Hugo at the version CI uses
hugo server --source site                      # preview at http://localhost:1313/, reloads on save
hugo --source site --gc --minify               # build into site/public/ as CI does
```

The build must print no `WARN` or `ERROR`. Before changing the site or the docs, know that:

- Pages in `docs/` stay plain Markdown that reads well on GitHub: no front matter, the first `# ` heading is the title, and links between pages are relative `.md` links (`certificates.md#dns-records`). The build fails on a link to a page that does not exist.
- A page appears in the sidebar, the previous and next links and the search once it is listed in `site/data/nav.toml`.
- The fonts are self-hosted and subset to the characters the site uses. After adding Japanese text, run `site/subset-fonts.sh` (it needs `curl` and fonttools with brotli) and commit the regenerated files in `site/static/fonts/`.
- `.github/workflows/pages.yml` builds the site for pull requests that touch `site/`, `docs/` or `deploy/`, and deploys it from `main`.
