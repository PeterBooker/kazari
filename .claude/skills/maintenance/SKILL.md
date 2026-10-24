---
name: maintenance
description: Update Agalma's dependencies - the Go toolchain, Go modules, the Docker base images in deploy/, GitHub Actions, goreleaser and the Hugo version of the website. Use when asked to update, upgrade or bump dependencies, Go, modules, Dockerfiles, base images, actions or Hugo, to check what is out of date, or to fix a govulncheck finding.
---

# Maintenance: updating dependencies

Agalma pins versions in a handful of places. Update them by area, verify each area with the same
checks CI runs, and report what changed, what was held back and why.

## Where versions live

| Area | Where | Notes |
| ---- | ----- | ----- |
| Go toolchain | `go` line in `go.mod` | CI reads it (`go-version-file: go.mod`), so it sets the Go that CI and releases build with. |
| | `FROM golang:<major.minor>-alpine` in `deploy/Dockerfile` | Same minor as `go.mod`. The minor tag tracks patch releases. |
| | `site/content/docs/quickstart.md` | "Go (1.27 or later)": the minor only. |
| Go modules | `go.mod`, `go.sum` | Direct requirements are deliberate (see `docs/adr/0001-language.md`). |
| Runtime image | `gcr.io/distroless/static-debianNN:nonroot` in `deploy/Dockerfile` and `deploy/Dockerfile.goreleaser` | Keep both files on the same image. |
| busybox | `deploy/Dockerfile.goreleaser` | Only creates an empty data directory; any recent minor works. |
| GitHub Actions | `.github/workflows/ci.yml`, `release.yml`, `pages.yml` | Plus the example workflow in `site/content/docs/publish-from-code.md`. |
| goreleaser | `version: "~> v2"` in `release.yml`, `version: 2` in `.goreleaser.yaml` | The action major and the goreleaser major are separate. |
| Hugo | `HUGO_VERSION` in `.github/workflows/pages.yml` | Also the `go install github.com/gohugoio/hugo@v...` lines in `README.md` and `CLAUDE.md`. Use the same version locally. |
| Lint tools | staticcheck and govulncheck run at `@latest` | Nothing to pin. |

## 1. See what is out of date

If the request names one area ("bump Hugo"), check and update only that area. "Update the
dependencies" means every area below. These checks change nothing; run them from the repository
root. They need `curl`, `jq` and `git`.

**Go toolchain.** The pinned version, the image, and the latest stable release:

```sh
grep '^go ' go.mod
grep '^FROM golang' deploy/Dockerfile
curl -fsSL 'https://go.dev/dl/?mode=json' | jq -r '.[0].version'
```

**Go modules.** Updates within the current major, for the modules `go.mod` requires (`all` would
also list modules of the build graph that are never built):

```sh
go list -m -u -f '{{.Path}} {{.Version}}{{with .Update}} -> {{.Version}}{{end}}{{if .Indirect}} (indirect){{end}}' \
  $(go mod edit -json | jq -r '.Require[].Path') | grep -e '->'
```

`go list -m -u` does not report a new **major** version, which has its own module path. To check
whether one exists for a direct requirement, ask for the next path:

```sh
go list -m -versions github.com/libdns/libdns/v2    # versions after the path: a v2 exists; the path alone: none
```

**Vulnerabilities**, in the modules and in the standard library:

```sh
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

**MCP protocol versions.** The SDK's list and the one in `docs/mcp.md` must name the same dates:

```sh
grep -A8 '^var supportedProtocolVersions' "$(go list -m -f '{{.Dir}}' github.com/modelcontextprotocol/go-sdk)"/mcp/*.go
grep -A1 'Supported: ' docs/mcp.md
```

If the `grep` finds nothing, the SDK has moved the list: look for `func SupportedProtocolVersions`
in the same directory.

**Docker images.** What the Dockerfiles use, the newest Go alpine and busybox minor tags on Docker
Hub, and whether distroless publishes the next Debian release (replace `13` with the current
number plus one; prints `true` or `false`):

```sh
grep -n '^FROM' deploy/Dockerfile deploy/Dockerfile.goreleaser
curl -fsSL 'https://hub.docker.com/v2/repositories/library/golang/tags?page_size=100&name=-alpine' \
  | jq -r '.results[].name' | grep -E '^[0-9]+\.[0-9]+-alpine$' | sort -V | tail -n1
curl -fsSL 'https://hub.docker.com/v2/repositories/library/busybox/tags?page_size=100' \
  | jq -r '.results[].name' | grep -E '^[0-9]+\.[0-9]+$' | sort -V | tail -n1
curl -fsSL https://gcr.io/v2/distroless/static-debian13/tags/list | jq '.tags | index("nonroot") != null'
```

The registry answers a Debian release it does not have with an empty tag list rather than an
error, so check for the `nonroot` tag as above. A bare status code check would say yes.

**GitHub Actions, goreleaser and Hugo.** Every action in use with the major it is pinned to, then
the newest release tag of each repository. `git ls-remote` needs no token and has no rate limit,
unlike the GitHub API (60 unauthenticated requests an hour, which this loop can use up):

```sh
grep -rhoE 'uses: [^ ]+@[^ ]+' .github/workflows site/content | sort -u
grep -n 'version: "~>' .github/workflows/release.yml
grep -n 'HUGO_VERSION:' .github/workflows/pages.yml
for repo in $(grep -rhoE 'uses: [^@ ]+' .github/workflows site/content | cut -d' ' -f2 | cut -d/ -f1-2 | sort -u) \
    goreleaser/goreleaser gohugoio/hugo; do
  echo "$repo $(git ls-remote --tags --refs "https://github.com/$repo" | cut -d/ -f3 \
    | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -n1)"
done
```

`cut -d/ -f1-2` turns an action path such as `anchore/sbom-action/download-syft` into its
repository. Compare the local tools too: `go version`, `goreleaser --version`, `hugo version`.

Summarise the findings per area. Then, before changing anything, run the tests once
(`go test -race ./...`) so a failure that was already there is not blamed on an update.

## 2. Decide what to take

- **Patch and minor updates**: take them.
- **GitHub Actions majors**: take them after reading the release notes. The usual change is a
  newer Node runtime, but check for renamed or removed inputs that the workflows use.
- **Debian release of the distroless image** (`static-debian12` to `static-debian13`): take it. The
  image is static and has no libc, and the `nonroot` user stays UID 65532.
- **Go minor release** (1.27 to 1.28): take it, but read the release notes first (see below).
- **Go module majors, a goreleaser major, replacing or adding a direct dependency**: do not do
  these unprompted. They need code or config changes and are design decisions. Report them with
  a short note on what the change would involve, and ask.

## 3. Go toolchain

Patch release:

```sh
go get go@1.27.4      # updates the go line; do not add a toolchain line
go mod tidy
```

Minor release, in addition:

- change `golang:1.27-alpine` to `golang:1.28-alpine` in `deploy/Dockerfile`;
- change "Go (1.27 or later)" in `site/content/docs/quickstart.md`;
- read the release notes at <https://go.dev/doc/devel/release>. Raising the `go` line also switches
  `GODEBUG` defaults (the compatibility section of the notes lists them), which can change
  `net/http`, `crypto/tls` and `crypto/x509` behaviour. Look at what affects the TLS listener and
  CertMagic, the `ServeMux` host patterns, the OAuth and OIDC HTTP clients and `archive/tar`;
- `go vet` from a new release can report new findings. Fix them rather than silencing them.

A standard library vulnerability reported by govulncheck is fixed by moving to the patched Go
release, the same way.

## 4. Go modules

```sh
go get -u -t ./...    # latest minor/patch of every requirement, tests included
go mod tidy
git diff go.mod       # review what moved
```

To move only one module: `go get example.com/mod@latest`, or `@vX.Y.Z` to fix a govulncheck
finding at the fixed version.

Then read the changelog of every direct requirement that moved. Each of these needs a closer look:

- **`github.com/modelcontextprotocol/go-sdk`**: Agalma accepts exactly the protocol versions in
  `mcp.SupportedProtocolVersions()`, and `docs/mcp.md` lists them ("Supported: ..."). After an
  update, rerun the MCP protocol versions check from step 1. If the two lists differ, update the
  docs. Check for changes to the stateless Streamable HTTP handler, tool
  schemas generated from `jsonschema` tags, and resource templates. The tests in
  `internal/mcpserver` cover these.
- **`github.com/caddyserver/certmagic` with `github.com/libdns/*`**: the DNS providers must
  implement the libdns API version that CertMagic expects. Update `libdns/libdns`, the three
  providers (`cloudflare`, `hetzner`, `route53`) and CertMagic together. If a provider has not
  caught up with a new libdns release, hold all of them back. CertMagic logs through zap
  (`internal/certs/zaplog.go`), which is why `go.uber.org/zap` is a direct requirement.
- **`github.com/ncruces/go-sqlite3`**: moves together with `github.com/ncruces/go-sqlite3-wasm`,
  which carries the SQLite build. A new SQLite version is fine. Run the `internal/sqlite` tests and
  the store conformance suites (`internal/artifact/storetest`, `internal/blob/blobtest`).
- **`github.com/coreos/go-oidc/v3`, `golang.org/x/oauth2`**: run the `internal/oidc` and
  `internal/oauth` tests. Check that ID token verification options and PKCE support are unchanged.
- **`github.com/prometheus/client_golang`**: metric names and labels are documented in
  `docs/ops.md`. They must not change.
- **`gopkg.in/yaml.v3`** (`internal/config`, `internal/cli`): upstream archived it; the YAML
  organisation maintains it as `go.yaml.in/yaml/v3`. Moving is an import path change, so propose
  it rather than making it unasked.
- **AWS SDK modules** are indirect, through `libdns/route53`. Letting `go get -u` raise them is fine.

If a direct requirement is added or removed, update the dependency list in
`docs/adr/0001-language.md`.

## 5. Docker images

- `golang:<minor>-alpine`: keep it on the `go.mod` minor.
- `gcr.io/distroless/static-debianNN:nonroot`: change both Dockerfiles in one go.
- `busybox:<minor>`: the latest minor tag.

Verify:

```sh
docker build -f deploy/Dockerfile -t agalma:check .
docker compose up --build -d && docker compose ps      # must report healthy
goreleaser release --snapshot --clean --skip=publish,sign,sbom   # builds Dockerfile.goreleaser images
```

`docker compose up --build` rebuilds and restarts the user's local development server (its data
stays in the `agalma_data` volume). Say so before you run it if a stack is already up
(`docker compose ps`).

## 6. GitHub Actions and goreleaser

- Update every use of an action to the same major: `actions/checkout` and `actions/setup-go`
  appear several times in `ci.yml`, and once each in `release.yml`, `pages.yml` and the example
  in `site/content/docs/publish-from-code.md`.
- `anchore/sbom-action/download-syft` has only ever been `v0`. Keep `@v0` until a v1 exists.
- `sigstore/cosign-installer`: a new major can install a new cosign major. Check that the `signs`
  and `docker_signs` arguments in `.goreleaser.yaml` and the `cosign verify-blob` command in
  `docs/install.md` still work with it.
- `goreleaser/goreleaser-action`: its major is independent of goreleaser's. The
  `version: "~> v2"` input keeps goreleaser on v2. Moving goreleaser to v3 is a config migration:
  ask first.
- Lint the workflows and the release config:

  ```sh
  go run github.com/rhysd/actionlint/cmd/actionlint@latest
  goreleaser check
  ```

Workflows cannot be run locally. In the report, note that the changed workflows still need a run on
GitHub, and if the release workflow changed, a `--snapshot` dry run is the closest local check.

## 7. Hugo

- Set `HUGO_VERSION` in `.github/workflows/pages.yml` to the new release, and the version in the
  `go install github.com/gohugoio/hugo@v...` lines of `README.md` and `CLAUDE.md`. Make sure the release
  still ships `hugo_<version>_linux-amd64.tar.gz` and `hugo_<version>_checksums.txt`, the names
  the workflow downloads.
- Install the same version locally (`go install github.com/gohugoio/hugo@v<version>`, or the system
  package if it matches).
- `hugo --source site --gc --minify` must finish with no `WARN` and no `ERROR` lines. Hugo deprecates
  template functions and config keys often (`.Site.Data` became `hugo.Data`, `languageCode` became
  `locale`). Fix each deprecation the way its message says, then rebuild.
- Look at the result with `hugo server --source site`
  (<http://localhost:1313/agalma/>): the home page, one docs page, and a search.

## 8. Verify everything

What CI runs, plus the builds that CI does not cover:

```sh
test -z "$(gofmt -l .)"
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@latest ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
go mod tidy -diff
go test -race ./...
CGO_ENABLED=0 go build -trimpath -o /dev/null ./cmd/agalma
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o /dev/null ./cmd/agalma
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o /dev/null ./cmd/agalma
hugo --source site --gc --minify
```

Run the Docker and goreleaser checks from step 5 when images, Go or goreleaser changed. Then
repeat the step 1 checks for the areas you changed: everything you meant to take should now show
as current.

## 9. Report and commit

Report per area: old and new versions, anything held back with the reason, and changelog items
that affect Agalma.

Commit only when asked. If asked, make one commit per area, each a single short sentence and nothing
else: "Update Go to 1.27.4", "Update Go modules", "Update Docker base images",
"Update GitHub Actions", "Update Hugo to 0.168.0". Check the area's own verification before each
commit.
