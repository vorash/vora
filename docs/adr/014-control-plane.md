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

- **External interface: HTTP/JSON.** Sandbox lifecycle, exec (streamed NDJSON),
  stats, logs (events/execs), plus `/v1/runners` and `/v1/keys`.
- **Internal interface: gRPC `RunnerRegistry`.** Runners **dial out** to the API
  (`Register`, `Heartbeat`), so they can sit behind NAT. A public registry
  requires mTLS and a join token. The runner ID must match the client
  certificate's DNS SAN on register and heartbeat. The runner reads its ID from
  that `run_<ULID>` SAN, so the ID is fixed when the certificate is issued and
  survives restarts (without one it generates an ID per start). The API records the runner's
  advertised gRPC address, backends, and capacity.
- **Caller auth: static API keys.** `Authorization: Bearer vora_…`. Keys are
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
  HTTP API. `vora runners` and `vora keys` manage the control plane.
- **TLS/mTLS:** three independent channels, no aliases for the retired
  `VORA_API_TLS_*` variables:
  - `VORA_API_HTTP_TLS_{CERT,KEY,CA}` for the external HTTP API and CLI client;
  - `VORA_API_REGISTRY_TLS_{CERT,KEY,CA}` for the registry gRPC server and runner client;
  - `VORA_RUNNER_TLS_{CERT,KEY,CA}` for the runner TCP gRPC server and API client.
  The runner TCP endpoint requires all three files (mTLS), including a client
  certificate with DNS SAN `vora-api` (override with
  `VORA_RUNNER_CLIENT_IDENTITY`). Plaintext requires `VORA_RUNNER_INSECURE=1`
  and a loopback IP bind. The Unix socket is unaffected. Other channels keep
  their own TLS policy.
- **Tenancy:** tenant-scoped keys may access only sandboxes and keys in their
  own tenant. Routes store `sandbox_id`, `runner_id`, and `tenant_id`. Empty
  tenant IDs are global-admin-only. Non-admin keys cannot be created without a
  `tenant_id`. Runner inventory requires a global administrator (`admin` scope
  and empty tenant).

## Consequences

- The local workflow is unchanged: without `--api` the CLI still talks to the
  runner Unix socket.
- One `runtime.RunnerService` implementation is reused for both transports. The
  CLI shares one `client.API` interface across the gRPC and HTTP clients.
- **Runner TCP and public runner registry are root-of-trust interfaces.**
  Registry credentials bind the declared runner ID to its certificate.
  A shared join token alone does not authorize a runner-ID takeover.
  Runner TCP without mTLS is disabled
  except for an explicitly opted-in loopback development endpoint. The client
  certificate on runner TCP must identify the API. Registry and HTTP TLS remain separately
  configurable. The join token still gates registration, not runner RPCs.
  `VORA_API_TLS_*` is a breaking removal.
- Sandbox create is compensating: start or route-persistence failure destroys
  the runner sandbox on a detached timeout so request cancellation cannot leave
  an unrouteable workload.
- **API to runner works for NAT'd runners (ADR 022).** A runner normally needs a
  reachable gRPC address. With `VORA_API_TUNNEL_ADDR` it instead connects out and
  the API multiplexes RunnerService over a reverse tunnel (yamux), so NAT'd
  runners can be placed and driven.
- **Scheduling is capacity-aware (ADR 021):** runners are filtered by backend
  and network capability and by committed capacity, then the least-loaded is
  chosen. The reverse tunnel for fully-hosted/NAT'd runners remains a follow-up.
- The in-memory store does not persist keys or runners across restarts. Run with
  `VORA_API_DSN` for anything durable.
- The deploy track (see the v0.3 plan, §14) will codify `vora-api` and `vora-runner`
  service installation in Ansible.
