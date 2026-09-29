# ADR 011 — Rootless containerd, and a non-root system runner

- Status: accepted (amended 2026-09 for V-13, `vorash/vora#102`)
- Date: 2026-03-24

## Context

The system containerd is root-only on the development host, and rootless gVisor
(ADR 008) requires the runner to live inside a persistent `rootlesskit` user
namespace. The first production plan was a root system service.

V-13 (`vorash/vora#71`) replaced the root system runner: it now runs as a
dedicated `vora` user with only ambient `CAP_SYS_ADMIN`/`CAP_NET_ADMIN`
(`CAP_NET_BIND_SERVICE` was later added for the port-53 resolver), and must
not reach the host-root containerd socket, which would restore host-root
authority. It therefore uses the `vora` user's rootless containerd too. That
unit shipped without ever booting; `#102` records what it needed to work.

## Decision

- **Development (unprivileged):**
  - a `containerd-rootless` **user** containerd (installed via
    `containerd-rootless-setuptool.sh`), used as the OCI image store;
  - `vora-runner` as a **user systemd unit wrapped in `rootlesskit`**
    (`scripts/vora-rootless.sh`), so `runsc` Method 2 works (ADR 008).
- **Production (system unit, `deploy/systemd/vora-runner.system.service`):**
  - `vora-runner` runs as the `vora` user with ambient `CAP_SYS_ADMIN`,
    `CAP_NET_ADMIN` and `CAP_NET_BIND_SERVICE` (restricted sandboxes' resolver
    listens on port 53 of their gateway); bwrap is launched with those cleared
    (V-31);
  - images come from `vora`'s rootless containerd at
    `/run/user/<vora-uid>/containerd/containerd.sock`;
  - sandbox cgroups come from `vora`'s lingering user manager
    (`VORA_SYSTEMD_MODE=user`). The unit cannot name that user's runtime
    directory (`%U` is the manager's uid, 0), so a non-root runner defaults
    `XDG_RUNTIME_DIR` to `/run/user/<euid>`, and the unit keeps `/run/user`
    readable (`ProtectHome=read-only`);
  - rootless containerd reports snapshot paths inside its own mount namespace
    (`/var/lib/containerd/…`, bind-mounted from `$XDG_DATA_HOME/containerd`).
    The runner maps them to the host path under its own `$XDG_DATA_HOME`,
    because sandboxes start in two mount namespaces: `none` through the user
    manager, the netns modes as runner children. A bind mount in the unit would
    only exist in the second;
  - **gVisor is not offered.** `runsc` needs Method 2 or host root; the backend
    refuses to register when the runner is not uid 0;
  - project quotas follow `CAP_SYS_ADMIN`, not uid 0 (ADR 015).
- The same runner binary and config keys (`VORA_CONTAINERD_ADDRESS`,
  `VORA_CONTAINERD_NAMESPACE=vora`, `VORA_GVISOR_PLATFORM`) serve every
  topology.

## Consequences

- Two process topologies to test. `scripts/test-system-unit.sh` boots the system
  unit on Ubuntu 26.04 in CI (the `System unit` workflow): a sandbox from each
  released image in each network mode, a kernel-enforced disk limit, and a
  restart that keeps subordinate-id file ownership.
- The system containerd's `660 root:root` socket is never used by the system
  runner.
- Rootless containerd keeps its store under `/var/lib/vora`, the unit's
  `StateDirectory=`. systemd chowns that tree recursively whenever its top
  directory is not owned by `vora`, which would rewrite the subordinate-id
  owners of unpacked image files. Hosts must chown `/var/lib/vora` to `vora`
  before setting up containerd (docs/release.md, Pitfalls).
- gVisor on a production host needs a root runner or a rootlesskit-wrapped one;
  neither is the shipped system unit.
