# ADR 014 — Control plane (`vora-api`) and runner registry

- Status: accepted (v0.3)
- Date: 2026-09-17

## Context

Through v0.2 the only way to drive Vora was `vora` talking gRPC to a runner's
**Unix socket** on the same host. That is fine for local development but there
is no external API, no way to authenticate a caller, and no way for a remote
runner to join. The hosted direction (a runner on a Hetzner/OVH box, driven from
elsewhere) needs a control plane.

## Decision

Add a separate **`vora-api`** process (`cmd/vora-api`):

- **External surface: HTTP/JSON.** Sandbox lifecycle, exec (streamed NDJSON),
  stats, logs (events/execs), plus `/v1/runners` and `/v1/keys`.
- **Internal surface: gRPC `RunnerRegistry`.** Runners **dial out** to the API
  (`Register`, `Heartbeat`), so they can sit behind NAT. Registration is
  authenticated with a **join token** (bearer); the API records the runner's
  advertised gRPC address, backends, and capacity.
- **Caller auth: static API keys.** `Authorization: Bearer vora_…`; keys are
  stored **SHA-256-hashed** with a tenant and scopes (`admin`, `sandbox`). The
  plaintext is shown once at creation. A `VORA_API_BOOTSTRAP_KEY` bootstraps the
  first admin key.
- **Placement:** the API routes a sandbox to the **first healthy runner** (by
  heartbeat) and stores a `sandbox → runner` route so later ops find it.
  Multi-runner scheduling is intentionally **deferred**.
- **Data:** a `Store` interface with an in-memory implementation (dev/tests) and
  a **PostgreSQL** implementation (`api_keys`, `runners`, `sandbox_routes`),
  selected by `VORA_API_DSN`.
- **Runner:** gains an optional TCP gRPC endpoint (`VORA_RUNNER_ADDR`) for the
  API to reach, and a registration/heartbeat loop (`VORA_API_GRPC`,
  `VORA_API_JOIN_TOKEN`, `VORA_RUNNER_ADVERTISE_ADDR`).
- **CLI:** `--api`/`--api-key` switch the client from the local socket to the
  HTTP API; `vora runners` and `vora keys` manage the control plane.

## Consequences

- The local workflow is unchanged: without `--api` the CLI still talks to the
  runner Unix socket.
- One `runtime.RunnerService` implementation is reused for both transports; the
  CLI shares one `client.API` interface across the gRPC and HTTP clients.
- **Runner ↔ API traffic is plaintext gRPC by default.** The join token gates
  registration, but TLS/mTLS for the runner channel is a follow-up; deploy the
  API and runners on a private network (or a mesh) until then.
- **API → runner requires the runner's gRPC address to be reachable.** This is
  NAT-friendly for registration but not for command delivery; a reverse tunnel
  (runner-initiated command stream) is a follow-up for the fully-hosted case.
- **Scheduling is deferred:** with more than one runner, placement is
  first-healthy, not capacity-aware.
- The in-memory store does not persist keys or runners across restarts; run with
  `VORA_API_DSN` for anything durable.
- The deploy track (see the v0.3 plan, §14) will codify `vora-api` + `vora-runner`
  service installation in Ansible.
