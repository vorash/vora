# ADR 005 — Workspace storage

- Status: accepted
- Date: 2026-09-16

## Context

Sandboxes need a writable area and an executable root filesystem. Copying a
full rootfs per sandbox is slow and wasteful; sharing a writable rootfs across
sandboxes would break tenant isolation. Distributed storage is out of scope for
the POC.

## Decision

- **Read-only shared rootfs.** Images are unpacked once into
  `/var/lib/vora/images/<name>/rootfs` and mounted read-only
  (`--ro-bind`) into every sandbox that uses them. Because it is read-only, it
  can be shared with no isolation risk and no per-sandbox copy.
- **Per-sandbox mutable state only.** Each sandbox gets
  `sandbox/workspace` (rw, mounted `/workspace`), `sandbox/home` (rw, mounted
  `/root`), and a `tmpfs` at `/tmp`. All persistence is confined to these paths.
- **Local filesystem only** in v0.1; no object storage, no distributed
  filesystem, no overlay-on-create.

## Consequences

- Sandbox creation is fast: it creates directories and a bind mount, not a
  filesystem copy. Benchmarks should reflect this.
- Isolation is preserved because the shared rootfs is read-only and contains no
  tenant data.
- Toolchains that require writes outside `/workspace` (e.g. system package
  installs) fail closed. This is intentional; images should be prepared with the
  needed tools at build time.
- Later, snapshots/forks can be added with copy-on-write layers (overlayfs,
  reflink, or containerd snapshotters) without changing the `WorkspaceSpec`
  contract. Bubblewrap's `--overlay` support is noted as an option.
- Hetzner hosts may not use btrfs; the design does not rely on reflink, so it
  works on ext4 as well.
