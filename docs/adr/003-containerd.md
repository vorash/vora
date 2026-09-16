# ADR 003 — containerd for image and sandbox management (gVisor)

- Status: accepted
- Date: 2026-09-16

## Context

The gVisor backend needs OCI image pull and unpack, rootfs/snapshot
management, and a container lifecycle with exec and stats. Implementing these
by hand would duplicate a large, security-sensitive surface. containerd is
already running on the development host (v2.3.5) and manages runc today.

## Decision

For the gVisor backend (v0.2), use containerd through its Go client
(`github.com/containerd/containerd/v2/client`) with `runsc` registered as a
runtime. The runner drives pull → unpack → create → start → exec → stop →
destroy and reads stats from containerd.

v0.1 does **not** depend on containerd: the Bubblewrap backend uses a simple
read-only rootfs cache built by `scripts/build-images.sh` (Docker `export`).
This keeps the first milestone rootless and dependency-light.

## Consequences

- v0.2 inherits battle-tested image pull/unpack and snapshot handling instead of
  reimplementing it.
- containerd owns container cgroups, so the gVisor backend reports
  `Capabilities.OwnsCgroups=true`; the runner's `cgroup.Manager` is used only by
  backends that do not (Bubblewrap).
- `runsc` must be installed and registered in the containerd configuration;
  this is a documented deployment step.
- v0.1 and v0.2 use different image paths. This is accepted because the shared
  contract is `SandboxSpec.Image`, not the storage mechanism underneath it.
- Using containerd does not imply Kubernetes; containerd is used directly, which
  keeps the Linux primitives visible.
