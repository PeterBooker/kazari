# Agalma

Self-hosted artifact host: one Go binary serves every artifact as a static site at
`<slug>.<base-domain>` under a wildcard certificate, with a REST API (`api.`), an MCP server plus
OAuth 2.1 (`mcp.`), an admin API (`admin.`) and a CLI. `PLAN.md` is the design; `docs/` is the
operator documentation; `docs/adr/` records decisions; `site/` is the Hugo website on GitHub Pages,
which also publishes `docs/`.

## Commands

There is no Makefile. Run these from the repository root.

```sh
# build (pure Go, no CGO)
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X agalma/internal/buildinfo.version=$(git describe --tags --always --dirty)" -o agalma ./cmd/agalma

# tests (always with the race detector)
go test -race ./...

# format, lint, vulnerabilities (what CI runs on every pull request)
test -z "$(gofmt -l .)"
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@latest ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
go mod tidy -diff

# run locally: http://*.localhost:8080 in proxy mode, data in ./.data, admin token dev-admin-token
go run ./cmd/agalma serve --config deploy/dev.yaml

# the same from the Docker image (compose.yml, deploy/Dockerfile), data in the named volume agalma_data
docker compose up --build -d

# first user and key against the local server, then publish a directory
go run ./cmd/agalma --admin-url http://admin.localhost:8080 --admin-token dev-admin-token admin users create peter
go run ./cmd/agalma --admin-url http://admin.localhost:8080 --admin-token dev-admin-token admin keys issue peter --label laptop
AGALMA_URL=http://api.localhost:8080 AGALMA_TOKEN=agk_... go run ./cmd/agalma publish ./site --slug demo

# release dry run (real releases run from a v* tag in GitHub Actions)
goreleaser release --snapshot --clean --skip=publish,sign,sbom

# website: Hugo standard edition at HUGO_VERSION from .github/workflows/pages.yml
go install github.com/gohugoio/hugo@v0.161.1
hugo server --source site               # http://localhost:1313/agalma/, live reload
hugo --source site --gc --minify        # build into site/public/ as CI does; must print no WARN or ERROR

# what is out of date (Go, modules, base images, actions, goreleaser, Hugo); read-only
.claude/skills/maintenance/check-updates.sh
```

Dependency updates follow the `maintenance` skill (`.claude/skills/maintenance/SKILL.md`).

## Configuration

- YAML file (`deploy/agalma.yaml` is the annotated example, `deploy/dev.yaml` the local one).
  `serve --config PATH`, else `$AGALMA_CONFIG`, else `/etc/agalma/agalma.yaml`, else environment only.
- Secrets never go in the file: `${NAME}` and `${NAME:-default}` in a value read the environment.
  A `.env` file (`--env-file`, `$AGALMA_ENV_FILE`, else `./.env`) is loaded first; real environment
  variables win over it. `deploy/agalma.env.example` lists the secret variables.
- Every key is also overridable as `AGALMA_<SECTION>_<KEY>` (lists comma-separated).

## Website (`site/`)

- Hugo, standard edition, no theme and no Node: templates in `site/layouts` (Hugo's current layout
  names: `baseof.html`, `home.html`, `docs/page.html`, `_partials/`, `_markup/`), plain CSS in
  `site/assets/css/main.css` (colour tokens with `light-dark()`), plain JS in `site/assets/js/main.js`
  (theme toggle, docs menu, copy buttons, tabs, search). Published by `.github/workflows/pages.yml`.
- `site/hugo.toml` mounts `../docs` as `content/docs`, so each `docs/<name>.md` is `/docs/<name>/`.
  Those files stay GitHub-readable Markdown: no front matter, the first `# ` heading is the title
  (`_partials/title.html`), links between them are relative `.md` links (`install.md#tls-modes`).
  `_markup/render-link.html` resolves them, sends `README.md` to GitHub, and fails the build on a
  missing page. It does not check `#fragments`: keep anchors matching GitHub-style heading ids.
- `site/content/` holds what is not in `docs/`: `_index.md` (front matter only; the landing page is
  `layouts/home.html`) and the guides in `site/content/docs/` (`_index.md`, `quickstart.md`,
  `publish-with-claude.md`, `publish-from-code.md`), which may use GitHub alerts (`> [!NOTE]`).
- `site/data/nav.toml` is the sidebar, previous/next order and search index (`layouts/home.json`
  becomes `index.json`). A new docs page goes there; an entry without a page fails the build.
- The site is served under `/agalma/`: link with `.RelPermalink`, `relURL` or `site.GetPage`,
  never a path starting with `/`.
- A change in behaviour (commands, flags, limits, tools, defaults) updates `docs/`, the guides in
  `site/content/docs/` and any claim on the landing page (`layouts/home.html`).

## Layout and conventions

- `cmd/agalma` is a thin main: `serve` builds `internal/app`; everything else is `internal/cli`.
- `internal/app` is the only package that knows every other package; it wires constructors from
  `config.Config`. Packages export `New(deps..., Options)`; nothing reads globals or config files.
- Packages are named for what they provide (`artifact`, `site`, `sqlite`, `upload`), never for a
  layer. Interfaces are small and owned by the consumer (`artifact.Store`, `site.Resolver`,
  `blob.Store`, `oauth.IdentityProvider`); `sqlite` implements them and passes the conformance
  suites in `artifact/storetest` and `blob/blobtest`.
- `internal/api` holds wire types only; `internal/present` converts domain types to them and maps
  errors to HTTP statuses in one place. Both `httpapi` and `mcpserver` go through it.
- Errors carry a code (`artifact.Error`); compare with `errors.Is` against the sentinels.
- Time is injected (`Options.Now`); tests use a fixed clock and the scheduler's `Trigger`, never
  sleeps. Tests run against real SQLite in `t.TempDir()`, a real `blob.FS` and `httptest`.
- Standard library first; no web framework, ORM, DI container or assertion library.
  `go-cmp` is fine in tests.
- SQLite timestamps are INTEGER unix nanoseconds; users are referenced by id internally and by
  handle externally; migrations are `internal/sqlite/migrations/NNNN_*.sql` applied at start.
- A bug fix adds a test at the lowest layer that reproduces it.
