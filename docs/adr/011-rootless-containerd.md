# ADR 011 — Rootless containerd (dev) and rootful runner (prod)

- Status: accepted
- Date: 2026-03-24

## Context

The system containerd is root-only on the development host, and rootless gVisor
(ADR 008) requires the runner to live inside a persistent `rootlesskit` user
namespace. Production is a Hetzner bare-metal host where the runner can be a
root system service.

## Decision

- **Development (unprivileged):**
  - a Vora/`containerd-rootless` **user** containerd (installed via
    `containerd-rootless-setuptool.sh`), used as the OCI image store;
  - `vora-runner` as a **user systemd unit wrapped in `rootlesskit`**, so
    `runsc` Method 2 works (ADR 008).
- **Production (Hetzner, root):** `vora-runner` as a **system** systemd service;
  `runsc` runs directly (no `rootlesskit`), system containerd (or the rootless
  one) provides images.
- The same runner binary and config keys (`VORA_CONTAINERD_ADDRESS`,
  `VORA_CONTAINERD_NAMESPACE=vora`, `VORA_GVISOR_PLATFORM`) serve both; the only
  difference is whether the process is wrapped in `rootlesskit`.

## Consequences

- Local development stays unprivileged and matches the container-rootless
  pattern the ecosystem uses.
- Two process topologies to test (`rootlesskit`-wrapped and plain root); the
  integration suite exercises both where the host allows.
- The system containerd's `660 root:root` socket is never a problem because the
  runner uses a containerd it can reach (rootless, or system-as-root).