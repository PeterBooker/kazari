# Kazari

Self-hosted artifact host: one Go binary serves each artifact as a static site at `<slug>.<base-domain>` under a wildcard certificate, plus a REST API (`api.`), an MCP server with OAuth 2.1 and the sign-in pages (`mcp.`), an admin API and a web dashboard (`admin.`, and a Unix socket in the data directory for the CLI on the same host), account pages on the apex (`/account/`) and a CLI. `docs/` is the operator documentation, `site/` the Hugo website at <https://kazari.io/>, which also publishes `docs/` and the files in `deploy/` under `/deploy/`.

## Commands

No Makefile; run from the repository root.

```sh
# build (pure Go, no CGO; release builds also stamp internal/buildinfo.version)
CGO_ENABLED=0 go build -o kazari ./cmd/kazari

# tests, always with the race detector
go test -race ./...

# what CI runs on every pull request (.github/workflows/ci.yml, which pins the tools' versions in its env)
test -z "$(gofmt -l .)"
go mod tidy -diff
go fix -diff ./...
go vet ./...                    # also with GOOS/GOARCH set to linux/arm64 and darwin/arm64
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest run    # the linters in .golangci.yml, staticcheck among them
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
shellcheck site/static/install.sh deploy/packaging/*.sh .github/scripts/*.sh
go run github.com/goreleaser/goreleaser/v2@latest check
go test -race -count=1 -shuffle=on -timeout=5m ./...
CGO_ENABLED=0 go test -count=1 -shuffle=on -timeout=5m ./...      # the configuration that ships
CGO_ENABLED=0 go test -run='^$' -bench=. -benchtime=1x ./...      # every benchmark once
.github/scripts/smoke.sh ./kazari                                 # the built binary: serve --local, publish, fetch, pull, deploy

# what scheduled.yml adds daily: every fuzz target for a count of executions, and the sign-in pages
# in a real browser (skipped unless the variable names a Chromium or Chrome command)
.github/scripts/fuzz.sh 20000
KAZARI_TEST_CHROMIUM=chromium go test -count=1 -run '^TestChromiumSignIn$' ./internal/app

# only when .github/ changed (workflows.yml)
pipx run zizmor==1.32.0 .
go run github.com/rhysd/actionlint/cmd/actionlint@latest
gh actions-lock --verify

# only when deploy/, go.mod, go.sum or .goreleaser.yaml changed (package.yml): the release dry run
# below, and the image started with .github/scripts/smoke-image.sh
docker build --file deploy/Dockerfile --tag kazari:dev . && .github/scripts/smoke-image.sh kazari:dev

# local server: http://*.kazari.localhost:8080 in proxy mode, data in ./.data, no token. A fresh
# ./.data prints a setup link on stderr that creates the first admin with a password; then the
# dashboard is http://admin.kazari.localhost:8080/ and the account page (passkeys, keys)
# http://kazari.localhost:8080/account/. Passkeys work here, not on a bare localhost base domain.
# The admin commands find its socket in ./.data: go run ./cmd/kazari admin setup-link, or
# admin users create peter --role admin, then admin keys issue peter. The kazari-dev server
# in .mcp.json needs such a key exported as KAZARI_DEV_TOKEN before Claude Code starts. If
# port 8080 is taken, move the local server and point .mcp.json at it, also before Claude Code starts:
#   export KAZARI_LISTEN_HTTP=127.0.0.1:18080 KAZARI_DEV_URL=http://mcp.kazari.localhost:18080/
go run ./cmd/kazari serve --local
docker compose up --build -d    # the same from the Docker image (serve --local), data in the volume kazari_data;
                                # restarts with Docker until docker compose stop/down, and holds port 8080
go run ./cmd/kazari check       # preflight: config, DNS, DNS token, ports (--local for the preset)

# release dry run (real releases run in GitHub Actions from a version tag without a v, such as 0.3.0)
goreleaser release --snapshot --clean --skip=publish,sign,sbom

# before tagging a release (the release skill prepares the pull request): print the release notes the tag will
# publish from CHANGELOG.md, and record the schema, which TestMigrateFromEveryRelease migrates from then on
.github/scripts/release-notes.sh 0.8.0
go test ./internal/sqlite -run TestMigrateFromEveryRelease -dump-schema 0.8.0
```

## Configuration

- YAML: `deploy/kazari.yaml` is the short file an instance starts from (the required keys), `deploy/kazari.reference.yaml` every key with its default. Found by `serve --config PATH`, else `$KAZARI_CONFIG`, else `/etc/kazari/kazari.yaml`, else environment only; `serve --local` uses the preset `config.Local()` instead.
- No secrets in the file: `${NAME}` and `${NAME:-default}` read the environment. A dotenv file (`--env-file`, `$KAZARI_ENV_FILE`, else `./.env`) loads first; real environment variables win. Beside a config file an unnamed `./.env` fills `${NAME}` only, not `KAZARI_*` overrides; the client commands read only `$KAZARI_ENV_FILE`, never `./.env`. `deploy/kazari.env.example` lists the secrets. A secret key also reads `KAZARI_<KEY>_FILE`.
- Every key is overridable as `KAZARI_<SECTION>_<KEY>` (lists comma-separated).
- `deploy/compose.yml` is the production Compose file (published image, `kazari.yaml` and `.env` beside it); the root `compose.yml` is the development one.

## Conventions

- `cmd/kazari` is a thin main that calls `internal/cli`, where kong parses every command: `serve` builds `internal/app`, `check` runs `internal/preflight`, everything else is a client of the REST API. Only `internal/app` knows every package; it wires constructors from `config.Config`. Packages export `New(deps..., Options)` and never read globals or config files.
- Packages are named for what they provide (`artifact`, `site`, `sqlite`, `upload`), never for a layer. Interfaces are small and owned by the consumer (`artifact.Store`, `site.Resolver`, `blob.Store`, `oauth.IdentityProvider`); `sqlite` implements them and passes the conformance suites `artifact/storetest` and `blob/blobtest`.
- The pages Kazari renders itself (`dashboard` on the admin host, `account` on the apex, the sign-in pages in `oauth`, the `gate`) are `html/template` on `internal/theme`, with no inline style or script and no build step; `internal/session` is the browser session of the admin host and the apex. The dashboard does nothing the admin API and the CLI cannot: an operation it needs gets a route and a command too.
- `internal/api` holds wire types only; `internal/present` converts domain types to them and maps errors to HTTP statuses in one place, for both `httpapi` and `mcpserver`.
- Errors carry a code (`errcode.Error`; `artifact.Error` is the same type): a package declares its sentinels with `errcode.New`, `Text` or `Shown` beside the code and its message, and `present` gives each code its status. Compare with `errors.Is` against the sentinels, never by code: two sentinels with one code stay apart.
- Time is injected (`Options.Now`). Tests use a fixed clock and the scheduler's `Trigger`, never sleeps; they run against real SQLite in `t.TempDir()`, a real `blob.FS` and `httptest`.
- Standard library first: no web framework, ORM, DI container or assertion library. kong parses the command line, `go-cmp` is fine in tests.
- SQLite timestamps are INTEGER unix nanoseconds. Migrations are `internal/sqlite/migrations/NNNN_*.sql`, applied at start. Users are referenced by id internally and by handle externally.
- A bug fix adds a test at the lowest layer that reproduces it, seen to fail without the fix.
- A new package gets a row in `internal/app/deps_test.go`, which holds the import rules above; the row is added, the rule is not loosened. The same goes for every test that walks a registry (routes, tools, templates, store methods, error codes).
- A change in behaviour (commands, flags, limits, tools, defaults) updates every file that states it: `docs/`, `site/content/docs/`, `site/layouts/home.html`, `README.md`, `deploy/kazari.yaml`, `deploy/kazari.reference.yaml`, `deploy/compose.yml`. `examples/kazari/SKILL.md` is generated, never edited: after a change to the skill text, the tool descriptions or the defaults, run `go test ./internal/mcpserver -run TestExampleSkill -update`. After any change under `docs/`, `site/` or `deploy/`, `hugo --source site --gc --minify` must print no WARN or ERROR.

## Tests

- Tests are in `package x_test` and go through the public API; what a test needs from inside is exported in `export_test.go`. Assert what the test is about, at the layer where the rule lives.
- A failure names the function, the input, what it got and what it wanted: `ParseHost(%q) = %q, want %q`. Structures are compared with `cmp.Diff` and printed as `(-want +got)`; errors with `errors.Is`, by their text only where the text is the product (a configuration message, CLI output, a tool's error).
- `t.Error` and carry on; `t.Fatal` only when the rest cannot run, and never from a goroutine the test started.
- A table is a slice of structs with a `name`, for cases that share their logic. Subtest names are short phrases.
- A helper that can fail takes `t` first and starts with `t.Helper()`; one that checks returns a value or an error and the test reports it. Teardown goes in `t.Cleanup`. New tests use `t.Context()`, `t.TempDir()`, `t.Setenv()` and `t.Chdir()`.
- Hermetic: loopback only, no wall clock, no home directory, no keyring, no fixed port. A test that needs more is gated by an environment variable and skips with a message that names it, as `TestChromiumSignIn` does.
- Pure timer and goroutine logic runs in a `testing/synctest` bubble; a wait on real I/O is a poll with a deadline that says what it waited for. Every goroutine a test starts is stopped and waited for: `internal/app` and `internal/scheduler` fail on a leaked one (`leaktest.Main`).
- A function that parses text from outside the process has a fuzz target in `fuzz_test.go` that asserts properties (no panic, round trip, idempotence). A failing input is committed under `testdata/fuzz/` with the fix.
- No retries and no skipped test without a reason. Coverage finds untested code and gates nothing.
- A lint finding is fixed, or silenced on its line with `//nolint:<linter> // why`; a linter is not switched off to pass.
