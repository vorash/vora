# Security Boundaries

Status: draft · Applies to: v0.7

This document answers a single question for every resource type:

> What exactly prevents Sandbox A from affecting Sandbox B (or the host)?

Each boundary lists the mechanism, how it is enforced, and the Go test that
proves it. If the mechanism is removed, the test must fail.

## Sandbox A vs Sandbox B

| Resource | A cannot reach B because… | Mechanism | Test |
| -------- | ------------------------- | --------- | ---- |
| Files | A's mount namespace has no path to B's workspace or rootfs; root rootfs is shared read-only and holds no tenant data | mount namespace, `--ro-bind` / OCI readonly root, per-sandbox workspace bind | `TestFilesystemIsolation` |
| Sandbox identity | A cannot choose B's ID to collide with its workspace or route; only the runner assigns canonical sandbox IDs | API/runner create rejection and store validation | `TestCreateSandboxRejectsCallerID`, `TestCreateRejectsCallerIDsWithoutTouchingExistingWorkspace` |
| Processes | A's PID namespace contains only its own processes; B's PIDs are not namespaced into A | PID namespace | `TestPIDNamespaceIsolation` |
| Signals | A cannot `kill` a PID it cannot see | PID namespace | `TestCannotSignalHost` |
| IPC | A has a private SysV/POSIX IPC namespace; B's shared memory is invisible | IPC namespace | `TestIPCNamespaceIsolation` |
| Hostname / domain | A's UTS namespace is private (`hostname=vora`) | UTS namespace | `TestUTSNamespaceIsolation` |
| Users / capabilities | A runs in an unprivileged user namespace with no host capabilities (bwrap) | user namespace | `TestUnprivilegedUserNamespace` |
| Network | A's network namespace has no interfaces and no routes; B is unreachable | net namespace / `runsc --network none` | `TestNetworkNone` |
| Egress (bwrap, `NetworkRestricted`) | A's only route is the gateway; every TCP connection is DNAT'd to the SNI proxy, which allows listed domains and denies the rest | per-sandbox netns + veth + nft DNAT + SNI proxy (ADR 012) | `TestNetworkRestrictedDNAT` + `egress` unit tests |
| Name resolution (bwrap) | A can only resolve allowlisted names, through the gateway resolver; private/CGNAT answers and non-address types are refused | allowlist-only resolver on the gateway + nft accept (ADR 012) | `TestDNSThroughNetns` + `egress` DNS unit tests |
| Brokered credentials (bwrap) | A reaches an allowlisted API through the broker route without ever holding the credential; the broker binds each alias to one host and never follows a redirect | gateway broker + host/allowlist binding (ADR 013) | `TestNoSecretsLeakBroker` + `egress` broker unit tests |
| Egress (bwrap, `NetworkFull`) | A reaches public hosts through the runner, but private/CGNAT ranges, IPv6, host-local services and every other sandbox stay blocked | per-sandbox netns + veth + nft output filter + runner input/forward/NAT (ADR 016) | `TestNetworkFullBlocksPrivateRanges` + `TestFullAndPrivateRunnerRulesDropHostInput` |
| Peer network (bwrap, `NetworkPrivate`) | A reaches the other sandboxes of its tenant and nothing else; public egress, other tenants and host-local services are denied | per-sandbox netns + veth + per-tenant routing + nft input drop/default-deny (ADR 016) | `TestNetworkPrivateIsolatesTenants` + `TestFullAndPrivateRunnerRulesDropHostInput` |
| Memory | A's allocations are capped; exceeding the cap kills A, not B | `memory.max`, `memory.swap.max=0` | `TestMemoryBombKilled` |
| PIDs | A's fork attempts hit its own `pids.max`; B's process budget is untouched | `pids.max` | `TestForkBombContained` |
| Control socket | A's control socket lives in its own `ipc/` directory bound only into A; reconnects pin the socket inode and peer credentials and refuse symlink/replacement | per-sandbox bind mount + peer check | `TestControlSocketIsolation`, `TestReplacedControlSocketFailsClosed` |
| Metadata / secrets | A has no network and no runner credentials in env, files or `/proc/self/environ`; brokered credentials are attached outside the sandbox | no network; broker route (ADR 013) | `TestNoSecretsLeak` (+ broker case) |

Runner admission also caps sandbox memory/CPU/PIDs/disk/TTL, total sandboxes,
and per-tenant sandbox and snapshot counts before allocation. Tenant counts
are per runner, not cluster-wide.

CPU quota is set (`cpu.max` / systemd `CPUQuota`) and asserted by
`TestSystemdUnitLimitsAndTeardown` / the gVisor subtree writer. There is no
live-throttle security test (too flaky). Disk is bounded by `DiskBytes`: a
filesystem project quota when the host supports it, otherwise a periodic usage
scan (ADR 015); `TestDiskQuotaBoundsHostWrites` covers the hard path.

## Host protection

| Host resource | Protection |
| ------------- | ---------- |
| Filesystem | rootfs mounted read-only; host paths never bind-mounted; no host `/` exposure; the files API opens nonblocking and refuses special files; snapshot capture uses confined root handles and refuses raced symlinks; restore never writes outside a sandbox's writable roots and never follows a symlink ([ADR 023](../adr/023-snapshot-fork.md)) |
| Kernel | bwrap: unprivileged user namespace with nested userns disabled + a seccomp deny-list (including namespace clone flags and x32 ABI) installed by `vora-init` (ADR 018). gVisor: user-space kernel (KVM) |
| PID 1 / init | sandbox has its own PID namespace |
| Devices | minimal `/dev` from Bubblewrap / gVisor OCI spec |
| cgroup hierarchy | bwrap: transient systemd units. gVisor: delegated subtree under `vora-runner.service` (`Delegate=yes`) |
| Audit trail | lives outside the workload's mounts (`logs/` is never bound into a sandbox) and survives destroy, TTL expiry, quota enforcement and reconcile, and the API keeps a destroyed sandbox's route so it stays readable; only the retention reaper (`VORA_AUDIT_RETENTION`, off by default) removes it ([ADR 026](../adr/026-audit-retention.md)) |
| Audit record identity | execution IDs are runner-assigned and validated at the store; records are append-only, so no caller can replace or rewrite another execution's record, in its own or another sandbox |
| Availability | memory/PID caps prevent host exhaustion; CPU quota bounds utilisation |

## Residual risks (accepted)

- **Shared kernel on bwrap.** Bubblewrap isolates namespaces but shares the
  kernel; a kernel vulnerability could escape. gVisor reduces this surface.
- **bwrap runs under a seccomp deny-list (ADR 018).** It is hardening, not a
  containment boundary: namespaces and cgroup limits are the isolation, and
  gVisor's user-space kernel is stronger still where it is available.
- **Read-only rootfs assumptions.** Some toolchains expect writable paths
  outside `/workspace`; those calls fail closed rather than being permitted.
- **Disk quota is hard only under an enforcing project quota**
  ([ADR 015](../adr/015-disk-quota.md)). Without one, the periodic usage scan
  bounds the sandbox but not a burst that lands between scans.
- **Tenant `env` values sit on the bwrap command line.** They are visible in
  `/proc/<pid>/cmdline` and `ps` to other local users unless `/proc` is mounted
  with `hidepid`. Keeping env off argv (a `0600` file in the IPC dir applied by
  `vora-init`) is not done yet; do not share the host with untrusted local
  users, and prefer credential brokering over env-passed tokens.
- **Images are trusted** unless `VORA_IMAGE_POLICY=enforce` verifies a
  signature ([ADR 020](../adr/020-image-signing.md)).
- **The brokered hop inside the netns is plaintext HTTP.** The credential is
  attached by the gateway, so it still never enters the sandbox, but a workload
  that logs its own broker responses sees what the API returned.
- **Agent-bound keys share tenant credential aliases.** Agent binding isolates
  sandbox ownership but does not grant aliases individually: a leaked agent key
  can request any alias its tenant has stored if it also grants the bound host.
- **The gateway resolver sees query names.** Only allowlisted names are
  answered, but the runner learns which of them the sandbox asked for.
- **gVisor restricted egress is enforced in the runner netns (ADR 017).**
  gVisor `NetworkRestricted` uses the same gateway proxy/DNS/broker as bwrap,
  but its policy is applied on the runner's inbound path because netstack
  bypasses the sandbox output hook. `NetworkFull`/`NetworkPrivate` remain
  bwrap-only.

## Verification policy

`tests/security/` is a deliverable, not an afterthought. Every test:

1. creates a real sandbox through the normal runner path;
2. attempts the attack described above;
3. asserts the boundary holds;
4. documents *why* the boundary matters so that weakening it breaks the test.

## Backends

The matrix runs against **every available backend** as subtests:

| Backend | How it runs | Covered by the matrix |
| ------- | ----------- | --------------------- |
| `bwrap` | unprivileged, cgroups v2 via systemd | full table including fork/memory and user-ns |
| `gVisor` | `runsc` Method 2 inside a `rootlesskit` user namespace; limits via delegated cgroup subtree | full table except `TestUnprivilegedUserNamespace`; `NetworkRestricted` via runner-side policy (ADR 017) |

`make test-security` runs the bwrap leg (and skips the gVisor subtests when the
runner is unprivileged). `make test-security-all` additionally runs the gVisor
leg inside a `rootlesskit` userns.
