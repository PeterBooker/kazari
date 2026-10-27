# Developing Kazari

This page is for working on Kazari itself: running the server from source, running it from the Docker image, the checks CI runs, releasing, and building the website. Operating an instance is the [installation guide](install.md).

You need [Go](https://go.dev/dl/) at the version in `go.mod`, and Docker for the Compose setup.

## Running the server from source

`serve --local` runs the server with no DNS, certificate or token: plain HTTP on `127.0.0.1:8080`, the base domain `localhost`, which browsers resolve to the local machine, and the data in `./.data`. Start it from the repository root:

```sh
go run ./cmd/kazari serve --local
```

Pages appear at `http://<slug>.localhost:8080/`, the API at `http://api.localhost:8080/`, and the MCP server at `http://mcp.localhost:8080/`. `KAZARI_*` variables still apply on top of the preset.

In a second terminal, create a user and a key, then publish. The admin commands find the server's socket in `./.data`, so they need no token:

```sh
go run ./cmd/kazari admin users create peter --role admin
go run ./cmd/kazari admin keys issue peter --label laptop

KAZARI_URL=http://api.localhost:8080 KAZARI_TOKEN=kzk_... go run ./cmd/kazari publish ./site --slug demo
```

Open `http://demo.localhost:8080/` to see the page. To start again from nothing, delete `./.data`.

The CLI reads `./.env` as well, so `KAZARI_URL` and `KAZARI_TOKEN` can live there instead of on the command line. Keep `.env` out of version control. The `kazari-dev` MCP server in `.mcp.json` connects Claude Code to this server with a key exported as `KAZARI_DEV_TOKEN`.

> [!NOTE]
> Most browsers and systemd-resolved resolve `*.localhost`. If a tool on your system does not, add the hosts you need to `/etc/hosts`: `127.0.0.1 api.localhost admin.localhost mcp.localhost demo.localhost`.

## Running the Docker image

`compose.yml` at the repository root builds `deploy/Dockerfile` and runs the image with `serve --local`, so the addresses and commands above are unchanged. The data lives in the named volume `kazari_data`, the port is published on loopback only, and `./.env` is passed into the container when it exists. Start it and follow the log:

```sh
docker compose up --build -d
docker compose logs -f
```

The admin commands run inside the container, over the socket in the volume:

```sh
docker compose exec kazari kazari admin users create peter --role admin
docker compose exec kazari kazari admin keys issue peter --label laptop
```

`docker compose watch` rebuilds and restarts the container when the source changes, and `docker compose down` stops it; add `-v` to discard the data as well. `deploy/compose.yml` is the Compose file for a real server.

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

The website at <https://kazari.io/> is a [Hugo](https://gohugo.io/) site in `site/`. It mounts the Markdown files in `docs/` as its reference pages, except `security.md` and `adr/`, and the files in `deploy/` under `/deploy/`, so a change to either directory is a change to the site. The guides live in `site/content/docs/`, the landing page is `site/layouts/home.html`, and the install script is `site/static/install.sh`.

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
