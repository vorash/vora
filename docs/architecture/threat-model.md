# Threat Model

Status: draft · Applies to: v0.7

## Security objective

No workload inside a sandbox may affect the confidentiality, integrity or
availability of the host, the runner, or any other sandbox, beyond the resources
explicitly granted to it.

## Assets

| Asset | Why it matters |
| ----- | -------------- |
| Host filesystem | contains other tenants' data and system credentials |
| Host kernel | shared by every sandbox; a kernel exploit defeats all isolation |
| Other sandboxes' workspaces and processes | tenant isolation |
| Runner control socket | grants create/exec/destroy of sandboxes |
| Credentials | API keys, join tokens, runner TLS material, and brokered third-party credentials (held encrypted by `vora-api`, never by a sandbox) |
| Host resources (CPU, memory, PIDs, disk) | availability / denial of service |
| Audit trail | non-repudiation of executed code |

## Trust boundaries

1. **Sandbox ↔ runner** — the sandbox is untrusted; the runner is trusted.
2. **Runner ↔ host kernel** — the runner relies on namespace, cgroup and
   syscall-filtering primitives provided by the kernel.
3. **Runner ↔ image cache** — images are read-only inputs; a malicious image
   must not affect the host.
4. **Sandbox ↔ sandbox** — mutually untrusted.
5. **Sandbox ↔ network** — deny by default (`NetworkNone`), or allowlisted TLS
   via the egress proxy plus an allowlist-only resolver and an optional
   credential broker (`NetworkRestricted`, both backends —
   [ADR 012](../adr/012-egress-gateway.md),
   [ADR 017](../adr/017-gvisor-restricted-egress.md));
   `NetworkFull`/`NetworkPrivate` are bwrap-only
   ([ADR 016](../adr/016-network-modes.md)).
6. **Caller ↔ control plane** — API keys authenticate; tenant identity authorizes sandbox and key access, and an agent-bound key is confined to its own agent ([ADR 025](../adr/025-agent-identity.md)).
7. **Control plane ↔ runner** — runner TCP requires mTLS and the API client's DNS SAN (or an explicit loopback-only development override). Public registry and tunnel listeners require mTLS, binding the declared runner ID to the runner certificate's DNS SAN. A join token is separate and does not authenticate runner RPCs. Plaintext tunnel use requires an explicit loopback-only override and refuses brokered credentials by default.

## Adversary model

The workload is assumed to be actively malicious and able to:

- run arbitrary code, including exploits;
- inspect `/proc`, `/sys` and environment variables;
- fork, allocate memory, write to disk, and open sockets;
- attempt to escape namespaces, attack other sandboxes, and exfiltrate data.

The host, kernel, runner and image cache are assumed trusted but *not*
infallible; the design minimises the trusted surface.

## Threats and mitigations

| Threat | Mitigation | Boundary |
| ------ | ----------------- | -------- |
| Host filesystem read | read-only bind of a minimal rootfs; host paths not mounted | filesystem |
| Host filesystem write | rootfs read-only; only `/workspace`, `/root`, `/tmp` writable | filesystem |
| Cross-sandbox read/write | distinct mount namespaces; no shared writable paths | filesystem |
| Process inspection / signalling | PID namespace; only own processes visible | PID |
| SysV/POSIX IPC abuse | IPC namespace | IPC |
| Hostname / domain leakage | UTS namespace | UTS |
| Privilege escalation | unprivileged user namespace; no capabilities from the host; bwrap disables nested user namespaces and seccomp rejects namespace clone flags, clone3, and x32 ABI calls | user |
| Fork bomb | bwrap: `pids.max` via cgroup. gVisor: `memory.max`, as `pids.max` counts the Sentry's host threads and not guest tasks | cgroup |
| Memory bomb / OOM | `memory.max`, `memory.swap.max=0` | cgroup |
| CPU monopolisation | `cpu.max` quota | cgroup |
| Disk exhaustion | runner resource ceilings and default `DiskBytes` budget, filesystem project quota with periodic-scan fallback; per-snapshot archive cap and fork restore size bound ([ADR 015](../adr/015-disk-quota.md), [ADR 023](../adr/023-snapshot-fork.md)) | storage |
| Metadata service access | unrestricted network is never granted; `--unshare-net`, or `NetworkRestricted` filter+DNAT to the egress proxy that denies non-TLS and private IPs | network |
| Private-network probing | no network namespace route exists, or non-proxy traffic is dropped and private/link-local/CGNAT destinations are blocked; full/private modes also drop host-local input and full mode drops IPv6 | network |
| DNS exfiltration / rebinding | the only resolver is the gateway's: it answers allowlisted names only, refuses loopback/private/link-local/CGNAT answers (so a rebinding name cannot reach the metadata service), refuses non-address query types, and audits every query | network |
| Reverse shell / exfiltration | no network egress, or egress restricted to an allowlisted TLS SNI set with durable audit | network |
| Gateway / UDP bypass | nftables output filter default-drop; only loopback, TCP to the proxy/broker ports, and DNS to the resolver are accepted | network |
| Proxy resource exhaustion | per-sandbox connection cap (default 128), idle timeout (default 5m), and a bounded, cached DNS resolver | network |
| Cross-tenant API access | tenant-scoped keys can only see their own sandboxes and keys; unknown IDs return 404; callers cannot choose sandbox IDs and the runner validates canonical IDs before persisting records | control plane |
| Missing audit of allowed egress | allow decisions are persisted before bytes are forwarded; persistence failure or a full JSONL audit log denies the connection (proxy, broker, and DNS). Denial floods are rate-limited and the JSONL log is capped with an `audit_truncated` marker | audit |
| Erasing the audit trail by destroying the sandbox | destroy, TTL expiry, quota enforcement and reconcile leave `logs/` in place, and `vora-api` keeps the destroyed sandbox's route so the trail stays readable through the API; only the reaper can delete a trail, and only when `VORA_AUDIT_RETENTION` is set (default: keep forever). A configured `VORA_AUDIT_DSN` that cannot be opened stops the runner rather than falling back to local JSONL. Lost exec-record and lifecycle-event writes are counted in `vora_audit_write_failed_total` ([ADR 026](../adr/026-audit-retention.md)) | audit |
| Hiding a command by reusing an execution ID | the runner assigns every `execution_id` (a caller-supplied value is refused with 400/`InvalidArgument`); stores validate it as a canonical `exe_` ULID before it becomes a filename or key, and exec records are append-only (JSONL links without replacing, Postgres plain `INSERT`), so a conflict is an error, never an update | audit |
| Host environment disclosure through tenant `env` | bwrap `NetworkNone` runs under a transient systemd service, which expands `${VAR}` in its arguments; `systemd-run` is invoked with `--expand-environment=no` (systemd ≥ 254) so tenant values reach the sandbox byte for byte and the service manager's environment is never substituted in | runtime |
| Runner or service manager environment read from `/proc/<pid>/environ` | bwrap stays the sandbox's PID 1 and vora-init its PID 2, at the workload's uid, so both start with nothing inherited: the netns path runs with an empty environment, and the unit path runs bwrap through `env -i`. They and the workload hold only `HOME`, `PATH`, `PWD` and the spec env | runtime |
| Audit write amplification by allowed egress | allowed decisions are audited individually only up to a small budget per sandbox, host, alias and reason per window (10 per 10 s), and 100 per sandbox per window across all hosts, so fresh subdomains of an allowlisted wildcard-DNS domain do not each get a new budget (past it they are counted under host `*`); the rest are counted and written as one "N additional allowed decisions aggregated" row when the window ends, on a periodic flush, or at destroy, so every decision stays accounted for but one sandbox cannot drive unbounded synchronous inserts into a shared audit database. An exec's egress counts are computed by the store for just its time window. Counts held in memory are lost if the runner crashes before the flush (at most one window) | audit |
| Cleartext or guessable control-plane credentials | `vora-api` refuses a non-loopback HTTP bind without TLS (unless `VORA_API_HTTP_INSECURE=1`, for a TLS proxy) and a `VORA_API_BOOTSTRAP_KEY` shorter than 32 characters; a runner registry with neither a join token nor a client CA logs a startup warning because any local process could register as a runner | control plane |
| Command outliving its exec timeout (gVisor) | a timeout or cancel kills the command inside the sandbox with `runsc kill --pid` using the PID from `runsc exec -internal-pid-file`, not just the `runsc exec` client, so the `TIMEOUT_EXCEEDED` record is true and the command cannot keep using CPU, PIDs or egress (bwrap already kills the process group) | runtime |
| Ambient runner capabilities reaching bwrap | the system unit keeps `CAP_SYS_ADMIN`/`CAP_NET_ADMIN` ambient for `nsenter`, `ip` and `nft` (and `CAP_NET_BIND_SERVICE` for the port-53 resolver); the netns bwrap launch clears the ambient and inheritable sets with `setpriv` between `nsenter` and `bwrap`, which would otherwise abort ("Unexpected capabilities but not setuid") | runtime |
| Compromised runner reading or changing host state (non-root system unit) | the runner is the `vora` user with only `CAP_SYS_ADMIN`/`CAP_NET_ADMIN`/`CAP_NET_BIND_SERVICE`, `ProtectSystem=strict` and a `ReadWritePaths` allowlist. Two directives are relaxed so sandboxes can start (`#102`): `ProtectKernelTunables` is off, because its `/proc` overmounts make a fresh sandbox `/proc` mount "too revealing" to the kernel, and `ProtectHome=read-only` replaces `yes`, because `vora`'s user bus and rootless containerd socket live under `/run/user`. Residual: the runner can read `/proc/sys` and `/run/user`; writes stay blocked because those files are root- or owner-owned and the unit has no `CAP_DAC_OVERRIDE` | runtime |
| Credential theft from env/disk | runner secrets never injected into sandbox | credentials |
| Third-party credential exfiltration | credentials are stored encrypted by `vora-api`, resolved per create, and attached by the gateway broker; the value never reaches the sandbox environment, filesystem, `/proc`, the persisted record, or the audit trail (alias only) | credentials |
| Credential reflected back by the upstream | the broker forwards only `GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS`; `TRACE`, `TRACK`, `CONNECT` and anything else get 405 and an audited `broker_method_not_allowed` denial before the credential is attached | credentials |
| Credential replay across hosts | the broker re-originates only to the alias's bound, allowlisted host and never follows a redirect | credentials |
| Credential exposure on the control-plane channel | brokered create/fork is refused when the selected runner channel is plaintext, unless `VORA_ALLOW_INSECURE_BROKER=1`; the runner refuses both create and fork credentials over plaintext direct and reverse-tunnel endpoints | credentials |
| Sandbox survives runner death | in-sandbox agent exits when the runner disappears; reconciler reaps orphans | lifecycle |
| Malicious image | references are parsed before constructing cache paths or pulling; the rootfs is mounted read-only. In enforce mode the runner verifies and pulls the same digest-pinned reference and refuses cache fallback | supply chain |
| Workload-created special files in files API | reads and writes open with `O_NONBLOCK` and reject non-regular files so a FIFO cannot hold runner workers indefinitely | filesystem |
| Hostile snapshot archive | capture opens workload entries through confined `os.Root` handles and refuses raced symlinks; restore refuses traversal and special-file entries and never follows symlinks (ADR 023) | storage |
| Malicious CLI plugin on `$PATH` | `vora <name>` dispatches only to a strictly named `vora-<name>` resolved with `exec.LookPath`: no shell, no config-supplied command line, built-ins and Vora's own companion binaries always win, and `VORA_DISABLE_PLUGINS=1` opts out. Same trust model as `git`/`kubectl` — a writable early `$PATH` already implies code execution. | CLI |

Credential brokering is delivered for bwrap (ADR 013) and its decisions appear in
the same audit trail. Restricted egress and the gateway resolver are delivered
for both backends (ADR 012, closed for gVisor by ADR 017);
`NetworkFull`/`NetworkPrivate` remain bwrap-only (ADR 016).

## Assumptions

- User namespaces are enabled on the host (`kernel.unprivileged_userns_clone=1`
  for the local backend).
- The kernel, Bubblewrap, and containerd/gVisor are kept patched.
- The operator's `$PATH` is under their control; a writable entry ahead of the
  real tools already implies code execution as that operator.
- The runner process and its socket are protected by filesystem permissions.
- Images come from trusted references; content is verified only when
  `VORA_IMAGE_POLICY=enforce` requires a signature
  ([ADR 020](../adr/020-image-signing.md)).

## Out of scope

- Kernel exploits and side channels (Spectre-class).
- Physical access and host-level compromise.
- GPU isolation, browser isolation, Docker-in-Docker.
- Deep multi-tenant network segmentation beyond `NetworkPrivate`'s per-tenant
  `/24` (ADR 016).
- Cryptographic attestation of agent identity (image signatures cover image
  provenance only — ADR 020). The identity itself is ADR 025's derived URI and
  the authz subject; an SVID issuer can assert it later without policy changes.
- Transparent (TLS-terminating) credential brokering; the broker route is
  explicit, and non-HTTP protocols are not brokered.
- The gateway resolver sees the query *names* it is asked for; query content is
  not hidden from the runner.

## Verification

Each threat above maps to a test in `tests/security/`. Tests are required to
fail if the corresponding boundary is removed; see
[security-boundaries.md](security-boundaries.md).
