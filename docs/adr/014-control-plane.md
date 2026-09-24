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
- **TLS/mTLS:** three independent channels, no aliases for the retired
  `VORA_API_TLS_*` variables:
  - `VORA_API_HTTP_TLS_{CERT,KEY,CA}` — external HTTP API and CLI client;
  - `VORA_API_REGISTRY_TLS_{CERT,KEY,CA}` — registry gRPC server and runner client;
  - `VORA_RUNNER_TLS_{CERT,KEY,CA}` — runner TCP gRPC server and API client.
  When a CA is set on the server side, client certificates are required and
  verified. The runner TCP endpoint requires all three files (mTLS) and the API
  client certificate must carry DNS SAN `vora-api` (override with
  `VORA_RUNNER_CLIENT_IDENTITY`); plaintext requires `VORA_RUNNER_INSECURE=1`
  and a loopback IP bind. Other channels keep their own TLS policy, and the
  Unix socket is unaffected.
- **Tenancy:** tenant-scoped keys may only access sandboxes and keys in their
  own tenant. Routes store `sandbox_id`, `runner_id`, and `tenant_id`; empty
  tenant IDs are global-admin-only. Non-admin keys cannot be created without a
  `tenant_id`. Runner inventory requires a global administrator (`admin` scope
  and empty tenant).

## Consequences

- The local workflow is unchanged: without `--api` the CLI still talks to the
  runner Unix socket.
- One `runtime.RunnerService` implementation is reused for both transports; the
  CLI shares one `client.API` interface across the gRPC and HTTP clients.
- **HTTP, registry, and API → runner channels support TLS/mTLS** from the
  channel-specific files above. TLS is **off by default** (plaintext), so run
  the API and runners on a private network unless configured. The join token
  still gates registration. `VORA_API_TLS_*` is a breaking removal.
- Sandbox create is compensating: start or route-persistence failure destroys
  the runner sandbox on a detached timeout so request cancellation cannot leave
  an unrouteable workload.
- **API → runner works for NAT'd runners (ADR 022).** A runner normally needs a
  reachable gRPC address; with `VORA_API_TUNNEL_ADDR` it instead connects out and
  the API multiplexes RunnerService over a reverse tunnel (yamux), so NAT'd
  runners can be placed and driven.
- **Scheduling is capacity-aware (ADR 021):** runners are filtered by backend
  and network capability and by committed capacity, then the least-loaded is
  chosen. The reverse tunnel for fully-hosted/NAT'd runners remains a follow-up.
- The in-memory store does not persist keys or runners across restarts; run with
  `VORA_API_DSN` for anything durable.
- The deploy track (see the v0.3 plan, §14) will codify `vora-api` + `vora-runner`
  service installation in Ansible.
