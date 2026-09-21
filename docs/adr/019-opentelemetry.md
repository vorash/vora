# ADR 019 — OpenTelemetry tracing

- Status: accepted (v0.6)
- Date: 2026-09-21

## Context

Vora exposed Prometheus metrics but no distributed tracing: the OpenTelemetry
libraries were indirect dependencies and no spans were emitted. Debugging a slow
or failing `create`/`exec`/`destroy` meant correlating logs across `vora-api`
and `vora-runner` by hand.

## Decision

**1. Off by default, zero overhead when disabled.**
`telemetry.SetupTracing` installs a tracer provider only when an OTLP endpoint
is configured (`OTEL_EXPORTER_OTLP_ENDPOINT`, passed explicitly as
`TracingConfig.Endpoint`). With no endpoint the global provider stays the
default no-op, so spans cost nothing and no exporter is created. The W3C
propagator is installed either way, so trace context still flows between
processes once a collector exists.

**2. Spans across the whole path.**
- `vora-api` wraps its HTTP mux with `otelhttp`, so every request is a server
  span and inbound `traceparent` is honoured. The response carries `X-Trace-Id`
  on every request (including errors), so a caller can quote it.
- gRPC hops use `otelgrpc` — a server handler on the runner's gRPC server and
  the API's registry server, a client handler on the API→runner dial and the
  runner→API registry dial. This is what connects the API span to the runner.
- The runner opens child spans for the runtime work it drives
  (`runtime.start`, `runtime.exec`), so the trace reaches the runtime layer.

**3. Log correlation.**
`telemetry.ContextHandler` is a `slog.Handler` that adds `trace_id` and
`span_id` from the record's context. Callers use the `*Context` log methods; a
log line without a span is unchanged.

**4. Metrics are untouched.**
The `/metrics` endpoint and `telemetry.Metrics` stay as they are; tracing is
additive.

## Consequences

- A collector is optional and never on the hot path until configured.
- The `otelgrpc`/`otelhttp` instrumentation adds small per-call overhead even
  when disabled; it is bounded and measured against the no-op provider.
- Configuring an OTLP endpoint is a host concern (collector address, TLS). The
  runner and API read `OTEL_EXPORTER_OTLP_ENDPOINT` and use plaintext gRPC; TLS
  to the collector is not wired yet.

## Refs

- Issue vorash/vora#24; `internal/telemetry` (`tracing.go`), `cmd/vora-*`,
  `internal/api`, `internal/client`, `internal/registry`.