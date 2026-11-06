# ADR 021 — Capacity-aware placement

- Status: accepted (v0.6)
- Date: 2026-09-21

## Context

ADR 014 split the control plane out but kept placement trivial: `pickRunner`
returned the **first healthy runner**. A runner's declared `backends`, its
capacity, and its current load were recorded but never used, so a second runner
was never chosen and a full runner kept receiving sandboxes.

This is part A of issue #25. The NAT reverse tunnel (part B) is separate.

## Decision

**1. Filter, then pick the least-loaded.**
`placeRunner` filters healthy runners (heartbeat within TTL, already done by
`runners`) by:
- **backend**: `Runner.SupportsBackend(spec.Backend)`, where a named backend must
  be advertised and an empty backend (auto-select) needs any backend;
- **network mode**: `Runner.SupportsNetwork` against the runner's advertised
  `network_modes` (a new `RegisterRunnerRequest.network_modes` field, the union
  of its backends' `Capabilities.NetworkModes`);
- **capacity**: `running < max_sandboxes` and the requested CPU/memory fit within
  the runner's advertised totals.

Survivors are sorted by committed CPU, then committed memory, then committed
sandbox count, then id for determinism. The chosen runner and a **reason**
(`least-loaded` or `only-runner`) are logged, and the reject reason per runner
is logged when nothing fits.

**2. Committed capacity is the API's own, not a heartbeat.**
The API creates every sandbox, so `SandboxRoute` now carries the requested
`CPUMillis`/`MemoryBytes`. `committedByRunner` sums them per runner. Placement
therefore does not depend on heartbeat freshness for accounting, and a
repository-backed route table makes it survive an API restart. The runner's
heartbeat `sandboxes_running` is still kept as a cross-check (the larger of the
two is used for the count). A sandbox created directly on a runner (bypassing
the API) is visible only through that heartbeat.

**3. Observability.**
`GET /v1/runners` now reports `committed_cpu_millis`,
`committed_memory_bytes` and `committed_sandboxes`, alongside the existing
`healthy` flag, so placement is inspectable. The placement reason is logged on
the API. (The API has no Prometheus endpoint yet, so a
`vora_runner_placements_total{reason}` counter is deferred to when it does.)

## Consequences

- Adding a second runner now actually distributes load, and an incapable or full
  runner is skipped rather than handed the request.
- Placement is deterministic and testable without a real runner (unit tests
  cover the filter and the score, and the store backs the accounting).
- `network_modes` is a per-runner union across backends, not a per-backend
  matrix: auto-select picks a backend that supports the requested mode, and the
  runner validates the pair at start. A request for a mode only some backends
  support is therefore accepted by placement and settled by the runner.
- A runner that predates this change advertises no `network_modes`, so it is
  treated as `none`-only until upgraded. The API and runner ship together.

## Refs

- Issue vorash/vora#25 (part A); ADR 014 (control plane). Part B (reverse
  tunnel) will extend this ADR or add its own.
- `internal/api` (`placeRunner`, `SandboxRoute`), `api/proto/v1/control.proto`.