# ADR 0001: Go as the implementation language

Status: accepted (2026-10-24). Dependency list amended: the configuration file is YAML, not TOML, and CertMagic's zap logger is bridged to `log/slog`; OpenTelemetry is added by [ADR 0005](0005-opentelemetry.md).

## Context

Kazari is one binary that terminates TLS for a wildcard domain, serves static artifacts, exposes a REST API and an MCP server, and runs a background scheduler. A one-to-two-week Rust-vs-Go spike was proposed before writing any code.

The deciding needs are:

- **MCP.** The official Go SDK (`github.com/modelcontextprotocol/go-sdk`) supports Streamable HTTP, a stateless mode that builds a server per request, bearer-token middleware and in-memory transports for tests.
- **Wildcard certificates.** A wildcard needs the ACME DNS-01 challenge. CertMagic plus the libdns providers (Cloudflare, Route 53, Hetzner) handle issuance, storage and renewal; adding a provider is an import.
- **Team fit.** The people maintaining Kazari write Go day to day.
- **Distribution.** Release binaries and a distroless Docker image; a static, CGO-free build keeps cross-compilation to linux/amd64, linux/arm64 and darwin/arm64 trivial.

## Decision

Write Kazari in Go and drop the spike. The needs above already decide it, and the spike would spend one to two weeks confirming them.

Keep the dependency set small and standard-library first: `net/http` with `ServeMux` host patterns, `log/slog`, `database/sql`, `archive/tar`, `embed`, `testing`. Third-party code only where the standard library has nothing: the MCP SDK, CertMagic and libdns, a pure-Go SQLite driver, `gopkg.in/yaml.v3` for the configuration file, `coreos/go-oidc` with `x/oauth2`, `x/time/rate`, `x/sync/errgroup`, the Prometheus client and `go-cmp` in tests. CertMagic logs through zap, so `go.uber.org/zap` is required too; `internal/certs` hands CertMagic a zap logger that writes to `slog`.

Deliberately not used: cobra, chi/gin/echo, gorm/sqlx/sqlc, testify, mockery, wire/fx, uuid, cron libraries, and zap or logrus for Kazari's own logging.

## Consequences

- Build with `CGO_ENABLED=0 go build -trimpath`; every dependency must be pure Go.
- Binary size target is 25 MB rather than 10 MB, because a pure-Go SQLite adds well over 10 MB. The idle RSS target (under 50 MB) stands.
- Protocol support follows the official MCP SDK's release cadence; a protocol revision the SDK lacks waits for it or is contributed upstream.
- Wiring is constructor injection in `internal/app`; no DI or web framework to learn or upgrade.
