# ADR 009 — Images via the containerd OCI store

- Status: accepted
- Date: 2026-03-24

## Context

v0.1 built a rootfs cache with `docker export`. v0.2 needs real OCI images
shared by two backends (bwrap and gVisor). containerd already provides the
content store and snapshotters, but on this host the system containerd is
root-only (`/run/containerd/containerd.sock` mode `660 root:root`,
`/var/lib/containerd` `0700`). A **rootless** containerd (`containerd-rootless`
via `rootlesskit`) works and produces **user-owned** snapshots.

## Decision

containerd is used purely as the **OCI content and snapshot store**. It is not
the container runtime (see ADR 008).

- Pull and unpack through the containerd client with `WithPullUnpack`.
- Resolve the image snapshot rootfs through the snapshotter (`fuse-overlayfs`,
  falling back to `native`), which yields a user-readable directory.
- **gVisor:** the `runsc` OCI bundle's `root.path` points at that snapshot rootfs
  (read-only).
- **bwrap:** `--ro-bind` the same snapshot rootfs as `/`.
- Our images (`vora/base`, `python`, `node`) become OCI refs. `vora/base` is
  built with Docker and imported (`docker save | ctr ... images import -`).
  `scripts/pull-images.sh` replaces `build-images.sh`.

## Consequences

- Both backends use one image pipeline, and snapshots are shared, not copied.
- Rootless snapshots are user-owned, so rootless bwrap can bind them.
- containerd is a dependency of the runner for images only. A containerd outage
  blocks new sandboxes but not running ones.
- If containerd is unavailable, the runner can fall back to a simple pull path
  (future work). v0.2 requires containerd.