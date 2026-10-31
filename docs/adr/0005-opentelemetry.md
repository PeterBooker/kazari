# ADR 0005: OpenTelemetry for traces, metrics and logs

Status: accepted (2026-10-31). Amends ADR 0001's dependency list.

## Context

Kazari's observability stopped at the request line: five Prometheus metrics by surface, structured logs with request ids, no traces and no per-tool numbers. An operator could see that an agent's request failed, but not which tool it called, what it changed, where the time went or why it failed.

Operators already run tracing and metrics backends (Grafana with Tempo, Prometheus and Loki, Honeycomb, Datadog, an OpenTelemetry Collector in front of any of them). OpenTelemetry is the one interface all of them accept, through OTLP over HTTP/protobuf or gRPC.

The 0.3.0 roadmap proposed OTLP over HTTP only, to keep `google.golang.org/grpc` out of the binary. The OTLP/HTTP exporters import it anyway (`otlptracehttp/internal/otlpconfig`), so that reason does not hold.

## Decision

- **OpenTelemetry carries traces, metrics and logs**, exported over OTLP to one endpoint (`telemetry.otlp`) with HTTP/protobuf or gRPC (`telemetry.otlp.protocol`). Each signal has its own switch; with no endpoint nothing is exported and the tracer and logger providers are no-ops.
- **Prometheus stays**: the OpenTelemetry metrics SDK with the Prometheus exporter serves `/metrics` on the admin host. On the public listener it now needs a bearer token (`telemetry.metrics.token`), because the new gauges and exemplars disclose far more than request counts.
- **Semantic-convention names throughout**, pinned to `semconv/v1.40.0`. The five 0.2 metrics are renamed (`http.server.request.duration`, `http.server.active_requests`, `kazari.job.duration`), `target_info` replaces a build-info gauge, and Go runtime metrics come from `contrib/instrumentation/runtime`. A semconv upgrade is a deliberate change whose renames golden files show.
- **Kazari's keys are the only configuration.** Every SDK option that would fall back to an `OTEL_*` variable is set explicitly, and a test sets each such variable to a hostile value.
- **The API reaches packages through `Options`**, as `Now` and `Logger` do. A leaf package, `internal/instrument`, holds the providers and helpers; nothing calls `otel.GetTracerProvider()`. The only global touched is `otel.SetErrorHandler`, once per process.
- **No caller identity leaves the process**: no handle, credential id, client IP, email or OIDC subject on spans, metrics or exported log records. The local audit and access logs keep who did what.
- **slog stays the logging API**, stderr its primary sink. Lines gain `trace_id` and `span_id`; an optional OTLP copy (`telemetry.logs.otlp`) drops identity keys.
- **No `otelhttp`.** The server side needs the post-match route, per-surface propagation and sampling, redacted queries and one response writer shared with the access log; the client side needs query redaction and per-purpose metrics. Kazari's own middleware emits the names and attributes `otelhttp` would.

ADR 0001's dependency list gains, under `go.opentelemetry.io/`: `otel` with its `trace`, `metric` and `log` APIs and `semconv/v1.40.0`; `otel/sdk`, `sdk/metric` and `sdk/log`; the OTLP exporters `otlptracehttp`, `otlptracegrpc`, `otlpmetrichttp`, `otlpmetricgrpc`, `otlploghttp` and `otlploggrpc`; `otel/exporters/prometheus`; `contrib/bridges/otelslog` and `contrib/instrumentation/runtime`. `prometheus/client_golang` stays for the registry, the process collector and `promhttp`.

## Consequences

- gRPC costs three exporter modules and a second test matrix (an in-process gRPC receiver beside the `httptest` OTLP/HTTP one), not a new transitive dependency. It dials only with `protocol: grpc`.
- 0.3.0 breaks Prometheus scrapers of `https://admin.<base>/metrics` (the token) and dashboards (the renames) in one release. The release notes and `docs/ops.md` carry the rename table and the scrape configuration; `admin.listen` and the admin socket stay open as before.
- Every dependency is pure Go, so `CGO_ENABLED=0` holds; `govulncheck` covers the new modules.
- Development-status conventions (MCP, GenAI, `db.client.connection.*`, `otel.sdk.*`, Go runtime) may be renamed by a later semconv release; the pin and the golden files turn that into a reviewed change.
- Off costs little: with telemetry off and Prometheus on, a request pays the metrics middleware alone.
