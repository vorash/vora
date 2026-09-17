# Threat Model

Status: draft · Applies to: v0.4

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
5. **Sandbox ↔ network** — deny by default (`NetworkNone`), or allowlisted TLS via the egress proxy plus an allowlist-only resolver and an optional credential broker (`NetworkRestricted`, bwrap).
6. **Caller ↔ control plane** — API keys authenticate; tenant identity authorizes sandbox and key access.
7. **Control plane ↔ runner** — join token plus optional mTLS; runners are trusted execution hosts. Brokered credentials are refused on this channel without transport encryption.

## Adversary model

The workload is assumed to be actively malicious and able to:

- run arbitrary code, including exploits;
- inspect `/proc`, `/sys` and environment variables;
- fork, allocate memory, write to disk, and open sockets;
- attempt to escape namespaces, attack other sandboxes, and exfiltrate data.

The host, kernel, runner and image cache are assumed trusted but *not*
infallible; the design minimises the trusted surface.

## Threats and mitigations

| Threat | Mitigation (v0.3) | Boundary |
| ------ | ----------------- | -------- |
| Host filesystem read | read-only bind of a minimal rootfs; host paths not mounted | filesystem |
| Host filesystem write | rootfs read-only; only `/workspace`, `/root`, `/tmp` writable | filesystem |
| Cross-sandbox read/write | distinct mount namespaces; no shared writable paths | filesystem |
| Process inspection / signalling | PID namespace; only own processes visible | PID |
| SysV/POSIX IPC abuse | IPC namespace | IPC |
| Hostname / domain leakage | UTS namespace | UTS |
| Privilege escalation | unprivileged user namespace; no capabilities from the host | user |
| Fork bomb | `pids.max` via cgroup | cgroup |
| Memory bomb / OOM | `memory.max`, `memory.swap.max=0` | cgroup |
| CPU monopolisation | `cpu.max` quota | cgroup |
| Disk exhaustion | per-sandbox workspace only; **disk quota is not enforced** | storage |
| Metadata service access | unrestricted network is never granted; `--unshare-net`, or (bwrap `NetworkRestricted`) filter+DNAT to the egress proxy that denies non-TLS and private IPs | network |
| Private-network probing | no network namespace route exists, or non-proxy traffic is dropped and private/link-local/CGNAT destinations are blocked | network |
| DNS exfiltration / rebinding | the only resolver is the gateway's: it answers allowlisted names only, refuses loopback/private/link-local/CGNAT answers (so a rebinding name cannot reach the metadata service), refuses non-address query types, and audits every query | network |
| Reverse shell / exfiltration | no network egress, or egress restricted to an allowlisted TLS SNI set with durable audit | network |
| Gateway / UDP bypass | nftables output filter default-drop; only loopback, TCP to the proxy/broker ports, and DNS to the resolver are accepted | network |
| Proxy resource exhaustion | per-sandbox connection cap (default 128), idle timeout (default 5m), and a bounded, cached DNS resolver | network |
| Cross-tenant API access | tenant-scoped keys can only see their own sandboxes and keys; unknown IDs return 404 | control plane |
| Missing audit of allowed egress | allow decisions are persisted before bytes are forwarded; persistence failure denies the connection (proxy, broker, and DNS) | audit |
| Credential theft from env/disk | runner secrets never injected into sandbox | credentials |
| Third-party credential exfiltration | credentials are stored encrypted by `vora-api`, resolved per create, and attached by the gateway broker; the value never reaches the sandbox environment, filesystem, `/proc`, the persisted record, or the audit trail (alias only) | credentials |
| Credential replay across hosts | the broker re-originates only to the alias's bound, allowlisted host and never follows a redirect | credentials |
| Credential exposure on the control-plane channel | a brokered create is refused when the API-to-runner channel has no transport encryption, unless `VORA_ALLOW_INSECURE_BROKER=1`; the runner refuses the same request over a plaintext endpoint | credentials |
| Sandbox survives runner death | in-sandbox agent exits when the runner disappears; reconciler reaps orphans | lifecycle |
| Malicious image | image rootfs is data only, read-only, no execution at build time | supply chain |

Credential brokering is delivered for bwrap (ADR 013) and its decisions appear in
the same audit trail. Restricted egress and the gateway resolver are delivered
for bwrap only (ADR 012); gVisor remains `NetworkNone` by decision.

## Assumptions

- User namespaces are enabled on the host (`kernel.unprivileged_userns_clone=1`
  for the local backend).
- The kernel, Bubblewrap, and containerd/gVisor are kept patched.
- The runner process and its socket are protected by filesystem permissions.
- Images come from trusted references; content is not independently verified.

## Out of scope for v0.4

- Kernel exploits and side channels (Spectre-class).
- Physical access and host-level compromise.
- GPU isolation, browser isolation, Docker-in-Docker.
- Multi-tenant network segmentation (no network exists).
- Cryptographic attestation of images or agent identity.
- Transparent (TLS-terminating) credential brokering; the broker route is
  explicit, and non-HTTP protocols are not brokered.
- The gateway resolver sees the query *names* it is asked for; query content is
  not hidden from the runner.

## Verification

Each threat above maps to a test in `tests/security/`. Tests are required to
fail if the corresponding boundary is removed; see
[security-boundaries.md](security-boundaries.md).
