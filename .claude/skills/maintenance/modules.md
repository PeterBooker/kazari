# Go modules

Direct requirements are deliberate.

## Check

Updates within the current major, for the modules `go.mod` requires (`all` would also list modules of the build graph that are never built), then vulnerabilities:

```sh
go list -m -u -f '{{.Path}} {{.Version}}{{with .Update}} -> {{.Version}}{{end}}{{if .Indirect}} (indirect){{end}}' \
  $(go mod edit -json | jq -r '.Require[].Path') | grep -e '->'
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

`go list -m -u` does not report a new major version, which has its own module path. Ask for the next path: `go list -m -versions github.com/libdns/libdns/v2` prints versions after the path if a v2 exists, the path alone if not.

## Update

```sh
go get -u -t ./...    # latest minor/patch of every requirement, tests included
go mod tidy
git diff go.mod
```

One module: `go get example.com/mod@latest`, or `@vX.Y.Z` to fix a govulncheck finding at the fixed version.

Read the changelog of every direct requirement that moved. These need a closer look:

- **`github.com/modelcontextprotocol/go-sdk`**: Kazari accepts exactly `mcp.SupportedProtocolVersions()`, and `docs/mcp.md` lists them ("Supported: ..."). Compare the two after an update and fix the docs if they differ:

  ```sh
  grep -A8 '^var supportedProtocolVersions' "$(go list -m -f '{{.Dir}}' github.com/modelcontextprotocol/go-sdk)"/mcp/*.go
  grep -A1 'Supported: ' docs/mcp.md
  ```

  If the first `grep` finds nothing, the SDK moved the list: look for `func SupportedProtocolVersions` in the same directory. Also check changes to the stateless Streamable HTTP handler, tool schemas generated from `jsonschema` tags and resource templates; the `internal/mcpserver` tests cover them.
- **`github.com/caddyserver/certmagic` with `github.com/libdns/*`**: the DNS providers must implement the libdns API version CertMagic expects. Update `libdns/libdns`, the `cloudflare`, `hetzner` and `route53` providers and CertMagic together; if a provider has not caught up with a new libdns release, hold them all back. CertMagic logs through zap (`internal/certs/zaplog.go`), which is why `go.uber.org/zap` is a direct requirement.
- **`github.com/ncruces/go-sqlite3`** moves together with `github.com/ncruces/go-sqlite3-wasm/v6`, which carries the SQLite build. A new SQLite version is fine. Run the `internal/sqlite` tests and the conformance suites `internal/artifact/storetest` and `internal/blob/blobtest`.
- **`github.com/coreos/go-oidc/v3`, `golang.org/x/oauth2`**: run the `internal/oidc` and `internal/oauth` tests; check that ID token verification options and PKCE support are unchanged.
- **`go.opentelemetry.io/otel` and `go.opentelemetry.io/contrib`** move together: the SDK, the OTLP exporters, the Prometheus exporter, `contrib/instrumentation/runtime` and `contrib/bridges/otelslog`. After an update:
  - Run `go test ./internal/app -run TestMetricNames`. A failure means a rename, from the Prometheus exporter's translation or the runtime instrumentation's semantic conventions: release-note it, update `docs/ops.md`, then rerun with `-update-metrics`.
  - Kazari's own names stay on `go.opentelemetry.io/otel/semconv/v1.40.0`. Moving to a newer semconv package is its own change, never part of an update: it renames attributes on spans and metrics.
  - The SDK reads the standard `OTEL_*` variables wherever Kazari leaves a setting unset, and `internal/telemetry/otlp.go` resolves the OTLP transport variables itself because Kazari builds the HTTP client and gRPC connection. Grep the new exporters for variables it does not handle yet: `grep -rhoE '"[A-Z_]+"' "$(go list -m -f '{{.Dir}}' go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp)"/internal/otlpconfig/envconfig.go | sort -u`, and compare with `otlpEnv`'s callers.
- **`github.com/prometheus/client_golang`** serves `/metrics` and the process collector; `TestMetricNames` covers both.
- **`gopkg.in/yaml.v3`** (`internal/config`, `internal/cli`) is archived upstream; the YAML organisation maintains it as `go.yaml.in/yaml/v3`. Moving is an import path change: propose it, don't make it unasked.
- **AWS SDK modules** are indirect, through `libdns/route53`; letting `go get -u` raise them is fine.

## Verify

`SKILL.md`'s verification.
