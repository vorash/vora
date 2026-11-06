# Configuration

Every Vora binary is configured through environment variables. The system
install writes templates for them to `/etc/vora/*.env`
([`scripts/install-system.sh`](../scripts/install-system.sh)).

## Runner (`vora-runner`)

| Variable | Meaning |
| -------- | ------- |
| `VORA_STATE_DIR` | Sandbox state directory |
| `VORA_SOCKET` | gRPC Unix socket for the local CLI |
| `VORA_SYSTEMD_MODE` | `user` or `system` — which systemd manager runs sandbox units |
| `VORA_METRICS_ADDR` | Prometheus metrics and `/healthz` (default `127.0.0.1:9101`) |
| `VORA_INIT_BINARY` | Path to `vora-init`, the in-sandbox PID 1 |
| `VORA_BACKENDS` | Comma-separated isolation backends to register (`bwrap`, `gvisor`). Unset registers every backend that starts and skips the rest; set, a listed backend that fails to start stops the runner. Unknown names and duplicates are errors |
| `VORA_GVISOR_PLATFORM` | `kvm` or `systrap` |
| `VORA_GVISOR_ROOT`, `VORA_RUNSC` | `runsc` state root and binary |
| `VORA_CONTAINERD_ADDRESS` | containerd socket |
| `VORA_CONTAINERD_NAMESPACE` | containerd namespace (default `vora`) |
| `VORA_CONTAINERD_SNAPSHOTTER` | snapshotter (default `native`) |
| `VORA_IMAGES_MODE` | `auto`, `containerd` or `cache` |
| `VORA_IMAGE_POLICY` | `off` (default: trust every image, warn at startup) or `enforce` (digest-pinned, `cosign`-verified; [ADR 020](adr/020-image-signing.md)) |
| `VORA_AUDIT_DSN` | PostgreSQL/Supabase audit store. Unset → local JSONL; set but unreachable → the runner refuses to start |
| `VORA_AUDIT_RETENTION` | Duration after which audit trails are pruned. Unset keeps local trails forever ([ADR 026](adr/026-audit-retention.md)) |
| `VORA_POOL_UNSAFE_REUSE` | `allow` lets this runner host pools that skip the reset between runs; `deny` (default) refuses them, and a persisted one does not load ([ADR 028](adr/028-warm-pools.md)) |
| `VORA_EGRESS_MAX_CONNECTIONS` | Concurrent restricted-egress connections (default 128) |
| `VORA_EGRESS_IDLE_TIMEOUT` | Idle timeout for egress connections (default `5m`) |
| `VORA_EGRESS_BROKER_PORT` | Credential broker port inside the sandbox netns (default 8080) |

Resource ceilings, enforced on every create: `VORA_MAX_MEMORY` (bytes),
`VORA_MAX_CPU` (millicores), `VORA_MAX_PIDS`, `VORA_MAX_TTL` (duration),
`VORA_DEFAULT_DISK` (bytes) and `VORA_MAX_SNAPSHOT_BYTES` (bytes).

### Joining a control plane

| Variable | Meaning |
| -------- | ------- |
| `VORA_API_GRPC` | `vora-api` registry address the runner dials out to (`Register`/`Heartbeat`) |
| `VORA_API_JOIN_TOKEN` | Shared registration token |
| `VORA_RUNNER_ADDR` | TCP gRPC listen address, so the API can reach this runner |
| `VORA_RUNNER_ADVERTISE_ADDR` | Address announced to the API, when it differs from `VORA_RUNNER_ADDR` |
| `VORA_RUNNER_INSECURE` | `1` allows plaintext runner TCP — development only, loopback IP binds only |
| `VORA_API_TUNNEL_ADDR` | Connect out over a reverse tunnel instead of being dialled ([ADR 022](adr/022-reverse-tunnel.md)) |

## Control plane (`vora-api`)

| Variable | Meaning |
| -------- | ------- |
| `VORA_API_HTTP_ADDR` | HTTP/JSON listen address (default `127.0.0.1:9300`) |
| `VORA_API_GRPC_ADDR` | Runner registry listen address (default `127.0.0.1:9301`) |
| `VORA_API_DSN` | PostgreSQL store. Unset → in-memory; keys and runners do not survive a restart |
| `VORA_API_BOOTSTRAP_KEY` | Initial admin key, at least 32 characters. Never stored |
| `VORA_API_JOIN_TOKEN` | Runner registration token. Without it or a registry client CA, `vora-api` warns at startup, and a non-loopback registry is refused |
| `VORA_API_HTTP_INSECURE` | `1` allows plaintext HTTP on a non-loopback bind |
| `VORA_MASTER_KEY` | ≥32 bytes; enables `vora secrets` and credential brokering. Without it the secret endpoints answer `503` |
| `VORA_ALLOW_INSECURE_BROKER` | `1` allows brokered creates over a plaintext API→runner channel |

## TLS

TLS/mTLS is configured per channel:

| Channel | Variables |
| ------- | --------- |
| External HTTP / CLI | `VORA_API_HTTP_TLS_{CERT,KEY,CA}` |
| Runner registry | `VORA_API_REGISTRY_TLS_{CERT,KEY,CA}` (required for a non-loopback registry) |
| API → runner | `VORA_RUNNER_TLS_{CERT,KEY,CA}` |
| Reverse tunnel | `VORA_API_TUNNEL_TLS_{CERT,KEY,CA}` on both ends |

- A non-loopback HTTP bind without TLS is refused unless
  `VORA_API_HTTP_INSECURE=1`.
- Runner TCP requires all three runner TLS variables (mTLS). The API's client
  certificate must carry the DNS SAN `vora-api`, or the
  `VORA_RUNNER_CLIENT_IDENTITY` override. For local development only,
  `VORA_RUNNER_INSECURE=1` with a loopback IP bind allows plaintext.
- With a registry client CA, the API binds each runner to the `run_<ULID>` DNS
  SAN of its client certificate, and the runner takes its ID from that SAN (one
  per host, stable across restarts). Issue each runner's registry and tunnel
  client certificates with the same one; the tunnel certificate's SAN must match
  the runner ID.
- A tunnel bound to loopback may use `VORA_TUNNEL_INSECURE=1` for local
  development, but then brokered credentials are refused by default.
- Brokered creates require TLS on the API→runner channel unless
  `VORA_ALLOW_INSECURE_BROKER=1`.
- `VORA_API_TLS_*` is no longer read.

## Telemetry

OpenTelemetry tracing across API → runner → runtime is off unless
`OTEL_EXPORTER_OTLP_ENDPOINT` is set (`OTEL_EXPORTER_OTLP_INSECURE=true` for a
plaintext collector). Trace ids appear in logs and in the `X-Trace-Id` response
header ([ADR 019](adr/019-opentelemetry.md)).

## CLI (`vora`)

| Variable / flag | Meaning |
| --------------- | ------- |
| `--api`, `--api-key` | Talk to `vora-api` over HTTP instead of the local runner socket |
| `VORA_DISABLE_PLUGINS` | `1` turns off external `vora-<name>` plugin dispatch |

The SDKs and the MCP server read `VORA_API_URL` (default
`http://127.0.0.1:9300`) and `VORA_API_KEY`.
