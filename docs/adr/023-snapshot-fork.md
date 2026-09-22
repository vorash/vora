# ADR 023 — Sandbox snapshots and forks

- Status: accepted
- Date: 2026-01 (v0.8)

## Context

Master plan §29: parallel agents want to fan out from one sandbox state —
"repo state → snapshot → agent A / agent B / agent C → evaluate". This needs
`sandbox → snapshot` and `snapshot → new sandbox`, cheap enough to fan out and
honest about what it preserves.

## Decision

**Scope: writable state only.** A snapshot captures the sandbox's `workspace/`
and `home/`. The rootfs is the shared read-only image and is not captured; the
snapshot record pins `spec.Image` and a fork re-resolves it like any create.
`/tmp` is a tmpfs on both backends and is excluded by design.

**Consistency: live and crash-consistent.** The copy runs while the sandbox is
up. Whole files as of the copy, torn writes possible, entries created or
removed mid-walk may be missed. A file that shrinks mid-copy is zero-padded to
keep the tar valid. No agent protocol, no quiescing — callers snapshot between
writes if they need more.

**Fork inherits the spec verbatim.** New sandbox ID; image, resources, network,
env and TTL are the source's (TTL re-armed at fork start). No overrides in this
iteration; the `WorkspaceSpec` stored in the record still points at the source
sandbox and is replaced by the fork's fresh layout.

**Snapshot data is untrusted input.** The workload authored every byte, so
restore refuses anything that could write outside the target directories:
malformed paths, symlinked path components, symlink targets resolving outside
their directory, and any non regular/dir/symlink entry. Nothing is silently
dropped — unsupported entries fail loudly. Only permission bits are restored
(no ownership, no setuid).

**Routing follows the data.** Snapshotting is a runner operation (the runner
owns the filesystem); `vora-api` tracks snapshot→runner the way it tracks
sandbox→runner, and a fork is routed to the runner holding the snapshot.

**No dedicated audit rows.** Snapshot and fork are data-copy operations and get
the same treatment as the file routes (which are not audited): the persisted
snapshot record is the trail, and a fork's sandbox lifecycle events are
recorded normally.

**Snapshots outlive sandboxes and have no TTL.** Forking an old snapshot after
its source is gone is valid; `snapshot rm` reclaims the space.

## Consequences

- Fan-out is cheap (writable dirs only) but a fork reproduces file state, not
  memory or process state.
- Snapshot storage is bounded only by the state disk; there is no quota on
  snapshot bytes yet (same as logs). Size is recorded per snapshot.
- A fork whose image is no longer resolvable fails at create time with the
  usual loud error, exactly like a normal create.
- SDK/MCP surfaces are not part of this change and follow separately.
