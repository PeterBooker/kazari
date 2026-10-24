# Kazari

Self-hosted artifact host: one Go binary serves each artifact as a static site at `<slug>.<base-domain>` under a wildcard certificate, plus a REST API (`api.`), an MCP server with OAuth 2.1 (`mcp.`), an admin API (`admin.`) and a CLI. `PLAN.md` is the design (large: search it, don't read it whole), `docs/` the operator documentation, `docs/adr/` the decisions, `site/` the Hugo website on GitHub Pages, which also publishes `docs/`.

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

# local server: http://*.localhost:8080 in proxy mode, data in ./.data, admin token dev-admin-token.
# First user, key and publish: the header of compose.yml. The kazari-dev server in .mcp.json
# needs such a key exported as KAZARI_DEV_TOKEN before Claude Code starts.
go run ./cmd/kazari serve --config deploy/dev.yaml
docker compose up --build -d    # the same from the Docker image, data in the volume kazari_data

# release dry run (real releases run from a v* tag in GitHub Actions)
goreleaser release --snapshot --clean --skip=publish,sign,sbom
```

## Configuration

- YAML: `deploy/kazari.yaml` is the annotated example, `deploy/dev.yaml` the local one. Found by `serve --config PATH`, else `$KAZARI_CONFIG`, else `/etc/kazari/kazari.yaml`, else environment only.
- No secrets in the file: `${NAME}` and `${NAME:-default}` read the environment. A `.env` file (`--env-file`, `$KAZARI_ENV_FILE`, else `./.env`) loads first; real environment variables win. `deploy/kazari.env.example` lists the secrets.
- Every key is overridable as `KAZARI_<SECTION>_<KEY>` (lists comma-separated).

## Conventions

- `cmd/kazari` is a thin main: `serve` builds `internal/app`, everything else is `internal/cli`.
- Only `internal/app` knows every package; it wires constructors from `config.Config`. Packages export `New(deps..., Options)` and never read globals or config files.
- Packages are named for what they provide (`artifact`, `site`, `sqlite`, `upload`), never for a layer. Interfaces are small and owned by the consumer (`artifact.Store`, `site.Resolver`, `blob.Store`, `oauth.IdentityProvider`); `sqlite` implements them and passes the conformance suites `artifact/storetest` and `blob/blobtest`.
- `internal/api` holds wire types only; `internal/present` converts domain types to them and maps errors to HTTP statuses in one place, for both `httpapi` and `mcpserver`.
- Errors carry a code (`artifact.Error`); compare with `errors.Is` against the sentinels.
- Time is injected (`Options.Now`). Tests use a fixed clock and the scheduler's `Trigger`, never sleeps; they run against real SQLite in `t.TempDir()`, a real `blob.FS` and `httptest`.
- Standard library first: no web framework, ORM, DI container or assertion library. `go-cmp` is fine in tests.
- SQLite timestamps are INTEGER unix nanoseconds. Migrations are `internal/sqlite/migrations/NNNN_*.sql`, applied at start. Users are referenced by id internally and by handle externally.
- A bug fix adds a test at the lowest layer that reproduces it.
- A change in behaviour (commands, flags, limits, tools, defaults) updates every file that states it: `docs/`, `site/content/docs/`, `site/layouts/home.html`, `README.md`, `deploy/kazari.yaml`, `skills/kazari/SKILL.md`. After any change under `docs/` or `site/`, `hugo --source site --gc --minify` must print no WARN or ERROR.
