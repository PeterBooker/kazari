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

The container restarts whenever Docker starts, such as after a reboot, until you stop it with `docker compose stop` or `docker compose down`. While it runs it holds port 8080, so stop it before running `go run ./cmd/kazari serve --local`.

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

go test -race -count=1 -shuffle=on -timeout=5m ./...      # tests with the race detector, in a random order
CGO_ENABLED=0 go test -count=1 -shuffle=on ./...          # the same tests as the binary ships, without cgo
CGO_ENABLED=0 go test -run='^$' -bench=. -benchtime=1x ./...   # every benchmark once
test -z "$(gofmt -l .)"                                   # formatting check
go mod tidy -diff                                         # go.mod and go.sum are tidy
go fix -diff ./...                                        # no pending modernizer fix
go vet ./...
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest run   # the linters in .golangci.yml
go run golang.org/x/vuln/cmd/govulncheck@latest ./...     # known vulnerabilities
shellcheck site/static/install.sh deploy/packaging/*.sh .github/scripts/*.sh
go run github.com/goreleaser/goreleaser/v2@latest check   # the release configuration
.github/scripts/smoke.sh ./kazari                         # start the binary, publish a page, fetch it

goreleaser release --snapshot --clean --skip=publish,sign,sbom   # local release dry run
```

A test that fails under `-shuffle=on` prints its seed; run it again with `-shuffle=<seed>`.

### Fuzzing the parsers

Every function that parses text from outside the process has a fuzz target in its package's `fuzz_test.go`. `go test` runs each target on its seeds and on the inputs under `testdata/fuzz/`, so the commands above already cover them. To search for new inputs, run every target for a number of executions each:

```sh
.github/scripts/fuzz.sh            # 200,000 executions a target
.github/scripts/fuzz.sh 20000      # a shorter run
```

To fuzz one target for longer, name its package and the target:

```sh
go test -run='^$' -fuzz='^FuzzParseHost$' -fuzztime=5000000x ./internal/site
```

A failing input is written to `testdata/fuzz/<target>/` in the package. Commit it with the fix: it then runs in every `go test`.

### Testing the sign-in pages in a browser

`TestChromiumSignIn` drives Chromium or Chrome through the first admin's setup, a password sign-in, adding a passkey and signing in with it, on the dashboard and on the page an MCP client opens. By default it is skipped. To run it, name the browser in `KAZARI_TEST_CHROMIUM`:

```sh
KAZARI_TEST_CHROMIUM=chromium go test -count=1 -run '^TestChromiumSignIn$' ./internal/app
```

The test starts its own server on a free port and its own browser profile with a virtual authenticator, so it needs no running server and no passkey device. The value can carry flags, such as `google-chrome --no-sandbox`.

To keep a screenshot of every step, add `-artifacts` and a directory that exists:

```sh
mkdir -p /tmp/kazari-browser
KAZARI_TEST_CHROMIUM=chromium go test -count=1 -run '^TestChromiumSignIn$' -artifacts -outputdir=/tmp/kazari-browser ./internal/app
```

The screenshots are in `_artifacts/` below that directory.

### What CI runs

GitHub Actions runs five workflows from `.github/workflows/`:

| Workflow | Runs | What it does |
| -------- | ---- | ------------ |
| `ci.yml` | Every pull request and push to `main`, and before a release | The commands above, with the build and `go vet` repeated for `linux/arm64` and `darwin/arm64`. The job **CI passed** succeeds only when every other job did, so it is the one check to require on `main` |
| `workflows.yml` | A change under `.github/` | [zizmor](https://docs.zizmor.sh/) and actionlint on the workflows, and `gh actions-lock --verify` on the lockfile |
| `package.yml` | A change to `deploy/`, `go.mod`, `go.sum` or `.goreleaser.yaml` | Builds `deploy/Dockerfile` and the release dry run, and starts both images with `.github/scripts/smoke-image.sh` |
| `install-script.yml` | A change to `site/static/install.sh` | Installs the latest release with the script |
| `scheduled.yml` | Daily, and by hand from the Actions tab | govulncheck, the race tests five times in a random order, every fuzz target, the browser test of the sign-in pages, the install script, and the coverage of the whole module in the run's summary |

`ci.yml` and `workflows.yml` name the version of each tool they run, so a new release of a linter cannot fail a pull request that did not change. CI builds golangci-lint from source, because a released binary refuses a module on a newer Go than the one that built it. Coverage is reported and gates nothing.

### Releasing

A release is a pull request, then a tag. The pull request adds the version's section to [`CHANGELOG.md`](../CHANGELOG.md) and records the release's database schema. In Claude Code, the `release` skill in `.claude/skills/` drafts both from the pull requests merged since the last tag and opens the pull request; edit the section there before merging.

Each section starts with the bare version as its heading, then the date, a short summary and the changes under **Upgrading**, **New**, **Changed** and **Fixed**:

```markdown
## 0.8.0

Released 2026-12-01.
```

To see the release notes a tag will publish, print its section:

```sh
.github/scripts/release-notes.sh 0.8.0
```

The schema of the release is recorded so that later migrations are tested against it. The command writes `internal/sqlite/testdata/schema/0.8.0.sql`; commit it in the same pull request:

```sh
go test ./internal/sqlite -run TestMigrateFromEveryRelease -dump-schema 0.8.0
```

`TestMigrateFromEveryRelease` migrates every recorded schema and fails when the result differs from a fresh database.

Once the pull request is merged, push a tag named for the version without a `v`, such as `0.8.0`, on the merge commit. `release.yml` refuses a tag that is not a version or has no section in `CHANGELOG.md`, runs `ci.yml` on the tagged commit, then runs goreleaser with that section as the release notes. goreleaser publishes the archives with SBOMs and a cosign-signed `checksums.txt`, the `.deb` and `.rpm` packages, and the signed image at `ghcr.io/peterbooker/kazari`, one image for `linux/amd64` and `linux/arm64` with an SBOM attached. `release.yml` then records a [build provenance attestation](https://docs.github.com/en/actions/concepts/security/artifact-attestations) for every file in `checksums.txt`.

Two builds of one commit give the same archives and packages: the binary and every file carry the commit's time, not the time of the build.

After publishing, the workflow checks what it published: the signatures, every file against `checksums.txt`, the provenance, that the image starts, and that `install.sh` installs the new version. A failure there means the release is out and needs a look.

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
