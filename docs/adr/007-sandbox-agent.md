# ADR 007 — Sandbox agent and image contract

- Status: accepted
- Date: 2026-09-16

## Context

Bubblewrap cannot join an existing sandbox: each invocation creates fresh
namespaces. A long-lived sandbox with repeatable `Exec` therefore needs a
process inside it to accept commands. Two smaller constraints shaped the
design:

1. A read-only root fs (`--ro-bind rootfs /`) cannot create mount points, so
   `--bind` destinations must already exist in the image.
2. Sandbox binaries must run in minimal images such as Alpine, which has no
   glibc.

## Decision

- Run a small statically linked agent, `vora-init`, as the sandbox command.
  It serves `AgentService` (`Ping`, `Exec`, `Signal`, `Shutdown`) over a Unix
  socket and reaps reparented children.
- Ship the agent **inside the ipc directory** (`<sandbox>/ipc/vora-init`),
  which is bound read-write at `/vora`. This avoids a separate file bind (which
  a read-only root cannot create) and keeps the root fs genuinely read-only.
- Build all `cmd/*` binaries with `CGO_ENABLED=0`.
- Require images to contain `/workspace` and `/vora`; `make images` builds them
  in for `vora/base`, `python`, and `node`.
- Replace `--die-with-parent` with an agent watchdog that exits when the runner
  stops contacting it, preserving teardown when the runner dies.

## Consequences

- Repeated `Exec` sees live process state and a persistent workspace.
- A sandbox is a real unit that can survive runner restarts briefly, then
  self-terminates; startup reconciliation cleans up remaining units.
- The agent binary lives in a writable directory, so a sandbox could tamper
  with it. This grants no capability the sandbox does not already have, since it
  fully controls its own processes; gVisor (v0.2) removes the agent entirely.
- Arbitrary images are still supported, but must pre-create the mount points.
  This is documented in the README and image Dockerfiles.
