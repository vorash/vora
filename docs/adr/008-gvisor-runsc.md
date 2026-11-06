# ADR 008 — gVisor via direct `runsc` (Method 2)

- Status: accepted
- Date: 2026-03-24

## Context

v0.1's Bubblewrap backend isolates namespaces but shares the host kernel. v0.2
adds gVisor for untrusted workloads. The obvious route, containerd with
`io.containerd.runsc.v1`, proved unstable on this stack:

- **rootless:** runsc selects its systemd cgroup manager and its D-Bus call is
  denied by polkit inside the user namespace;
- **rootful:** the container and sandbox processes appear but the task never
  starts and `ctr run` hangs.

Two gVisor facts settle the design:

- `runsc --rootless` (Method 1) **does not support `create`**. It is good only
  for one-shot `runsc do`. Our earlier "it works" was a false positive.
- gVisor's documented way to run **persistent** rootless containers is
  **Method 2**: `rootlesskit` owns a persistent user namespace and `runsc` runs
  as UID 0 *inside* it, using its normal (rootful) code paths.

## Decision

The gVisor backend drives **`runsc` directly**. containerd is not in the runtime
path.

- The runner (and therefore `runsc`) runs inside a **persistent `rootlesskit`
  user namespace**, mirroring `containerd-rootless.sh`.
- `runsc` is invoked **without `--rootless`** and **without** an OCI user
  namespace or `uidMappings` (Method 2 maps UIDs via `rootlesskit`/`subuid`).
- Flags: `runsc --root=<state> --platform=kvm --network=none --ignore-cgroups`.
  `--ignore-cgroups` is a **flag**. The `/etc/runsc/config.toml` file is not
  honoured on the shim path and is not relied on.
- `runsc spec` generates the OCI bundle (it is not hand-written), with
  `terminal: false`.
- The sandbox init is **Vora's own static binary** (`vora-init pause`, copied
  into the ipc directory bound at `/vora`), never a program from the image.
  gVisor needs only a process to keep the sandbox alive, because commands run
  with `runsc exec`. This binary is what lets a **shell-less (distroless) image**
  boot. Before it, the hardcoded `/bin/sh -c "sleep 86400"` failed with
  `failed to load /bin/sh` (issue #40).
- The backend implements `runtime.Backend` with `NativeExec=true` (no
  in-sandbox agent). runsc does **not** apply resource limits: the runner
  creates a delegated cgroup v2 subtree under `vora-runner.service`
  (`Delegate=yes`) and starts `runsc create`/`start` in that cgroup via
  `CLONE_INTO_CGROUP`. `--ignore-cgroups` stays so runsc never talks to
  systemd. `EnforcesResourceLimits` is true only when that subtree is writable.
  Otherwise Start fails rather than running unlimited.
- KVM is the default platform (the Hetzner host is bare metal). systrap is a
  config fallback.

Proven 2026-03-24 inside one persistent `rootlesskit` userns:
`create`, `start`, `exec` (prints `hi`), `kill`, `delete`, with no leftover
`runsc-gofer`/`runsc-sandbox` processes.

## Consequences

- No containerd shim, no systemd cgroups, no polkit interaction on the runtime
  path.
- The runner must be started inside a `rootlesskit` userns for rootless use. In
  production (root) it can run `runsc` directly as a system service. The same
  backend code serves both.
- `runsc`'s CLI is the integration point, so version drift is a real risk. The
  gVisor release is pinned and its flags are verified in CI.
- Containerd is still used, but only as an OCI image store (ADR 009).