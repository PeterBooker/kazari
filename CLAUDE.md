# Kazari

Self-hosted artifact host: one Go binary serves each artifact as a static site at `<slug>.<base-domain>` under a wildcard certificate, plus a REST API (`api.`), an MCP server with OAuth 2.1 (`mcp.`), an admin API (`admin.`, and a Unix socket in the data directory for the CLI on the same host) and a CLI. `docs/` is the operator documentation, `docs/adr/` the decisions, `site/` the Hugo website at <https://kazari.io/>, which also publishes `docs/` (except `security.md` and `adr/`) and the files in `deploy/` under `/deploy/`.

## Commands

No Makefile; run from the repository root.

```sh
# build (pure Go, no CGO; release builds also stamp internal/buildinfo.version)
CGO_ENABLED=0 go build -o kazari ./cmd/kazari

# tests, always with the race detector
go test -race ./...

# what CI runs on every pull request
test -z "$(gofmt -l .)"
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@latest ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
go mod tidy -diff
shellcheck site/static/install.sh deploy/packaging/*.sh
go run github.com/goreleaser/goreleaser/v2@latest check

# local server: http://*.localhost:8080 in proxy mode, data in ./.data, no token. The admin
# commands find its socket in ./.data: go run ./cmd/kazari admin users create peter --role admin,
# then admin keys issue peter. The kazari-dev server in .mcp.json needs such a key exported as
# KAZARI_DEV_TOKEN before Claude Code starts.
go run ./cmd/kazari serve --local
docker compose up --build -d    # the same from the Docker image (serve --local), data in the volume kazari_data
go run ./cmd/kazari check       # preflight: config, DNS, DNS token, ports (--local for the preset)

# release dry run (real releases run in GitHub Actions from a version tag without a v, such as 0.3.0)
goreleaser release --snapshot --clean --skip=publish,sign,sbom
```

## Configuration

- YAML: `deploy/kazari.yaml` is the short file an instance starts from (the required keys), `deploy/kazari.reference.yaml` every key with its default. Found by `serve --config PATH`, else `$KAZARI_CONFIG`, else `/etc/kazari/kazari.yaml`, else environment only; `serve --local` uses the preset `config.Local()` instead.
- No secrets in the file: `${NAME}` and `${NAME:-default}` read the environment. A dotenv file (`--env-file`, `$KAZARI_ENV_FILE`, else `./.env`) loads first; real environment variables win. `deploy/kazari.env.example` lists the secrets. A secret key also reads `KAZARI_<KEY>_FILE`.
- Every key is overridable as `KAZARI_<SECTION>_<KEY>` (lists comma-separated).
- `deploy/compose.yml` is the production Compose file (published image, `kazari.yaml` and `.env` beside it); the root `compose.yml` is the development one.

## Conventions

- `cmd/kazari` is a thin main that calls `internal/cli`, where kong parses every command: `serve` builds `internal/app`, `check` runs `internal/preflight`, everything else is a client of the REST API. Only `internal/app` knows every package; it wires constructors from `config.Config`. Packages export `New(deps..., Options)` and never read globals or config files.
- Packages are named for what they provide (`artifact`, `site`, `sqlite`, `upload`), never for a layer. Interfaces are small and owned by the consumer (`artifact.Store`, `site.Resolver`, `blob.Store`, `oauth.IdentityProvider`); `sqlite` implements them and passes the conformance suites `artifact/storetest` and `blob/blobtest`.
- `internal/api` holds wire types only; `internal/present` converts domain types to them and maps errors to HTTP statuses in one place, for both `httpapi` and `mcpserver`.
- Errors carry a code (`artifact.Error`); compare with `errors.Is` against the sentinels.
- Time is injected (`Options.Now`). Tests use a fixed clock and the scheduler's `Trigger`, never sleeps; they run against real SQLite in `t.TempDir()`, a real `blob.FS` and `httptest`.
- Standard library first: no web framework, ORM, DI container or assertion library. kong parses the command line, `go-cmp` is fine in tests.
- SQLite timestamps are INTEGER unix nanoseconds. Migrations are `internal/sqlite/migrations/NNNN_*.sql`, applied at start. Users are referenced by id internally and by handle externally.
- A bug fix adds a test at the lowest layer that reproduces it.
- A change in behaviour (commands, flags, limits, tools, defaults) updates every file that states it: `docs/`, `site/content/docs/`, `site/layouts/home.html`, `README.md`, `deploy/kazari.yaml`, `deploy/kazari.reference.yaml`, `deploy/compose.yml`, `examples/kazari/SKILL.md`. After any change under `docs/`, `site/` or `deploy/`, `hugo --source site --gc --minify` must print no WARN or ERROR.
