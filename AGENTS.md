# AGENTS.md

Guidance for coding agents working in this repository.

## What this is

Vora runs untrusted code (AI agents, generated scripts) in isolated Linux
sandboxes and keeps an auditable record of what it did. Written in Go; no
Kubernetes in the path.

- **Isolation backends:** Bubblewrap (namespaces + cgroup v2 via systemd) and
  gVisor (`runsc`), selected per sandbox behind `runtime.Backend`.
- **Rootfs:** read-only shared image cache from rootless containerd snapshots.
- **Networking:** default-deny; `NetworkRestricted` uses an SNI-inspecting
  egress gateway with allowlist-only DNS (ADR 012).
- **Credentials:** brokered server-side, never entering the sandbox (ADR 013).

Status: **v0.4, proof of concept** — implemented and tested, but not audited
and not ready for anything that matters. Read `README.md` before making
architectural claims.

## Layout

| Path | Contents |
| ---- | -------- |
| `cmd/` | binaries: `vora` (CLI), `vora-runner` (daemon), `vora-init` (in-sandbox PID 1), `vora-api` (control plane) |
| `internal/` | all packages; backend code lives in `internal/runtime/<backend>` |
| `api/proto/v1` | protobuf/gRPC definitions (generated Go checked in) |
| `tests/integration`, `tests/security`, `tests/benchmark` | build-tagged suites |
| `docs/adr/` | numbered architecture decision records |
| `docs/architecture/` | overview, sandbox lifecycle, security boundaries, threat model |
| `deploy/systemd`, `scripts/`, `images/` | units, tooling, sandbox rootfs images |
| `sdk/typescript/` | TypeScript SDK for the HTTP API; its own npm package and `vitest` suite |
| `sdk/mcp/` | MCP server exposing the sandbox tools to coding agents, built on the SDK |

## Build & test

```sh
make build              # compile all packages
make binaries           # build cmd/... into bin/
make test               # unit tests (go test ./...)
make test-integration   # real runner over a unix socket (tag: integration)
make test-security      # isolation security suite (tag: security)
make test-gvisor        # gVisor suite under rootlesskit
make vet                # go vet ./...
make fmt                # go fmt ./...
```

The integration and security suites need a working `bubblewrap`, `systemd --user`
and (for gVisor) `rootlesskit`. Some targets are skipped locally when the
tooling is absent. CI (`.github/workflows/ci.yml`) runs actionlint, shellcheck,
gofmt check, `go vet`, unit tests, then the integration + security suites.

Sandbox binaries are built with `CGO_ENABLED=0` so they run inside minimal
rootfs images — do not introduce cgo or dynamic linking into `vora-init`.

## Conventions

- **Match existing style.** Go code is `gofmt`-clean and `go vet`-clean; the CI
  `unit` job fails otherwise.
- **Isolation stays behind `runtime.Backend`.** Control-plane code must not
  depend on backend internals; add backend specifics under
  `internal/runtime/<backend>` and declare them via `Capabilities`.
- **Persist before observing.** Write state changes to disk before reporting
  them to a caller.
- **Fail loud.** Use semantic termination reasons (`OOMKilled`,
  `PidsLimitExceeded`, …), and surface uncertainty rather than hiding it.
- **Comments: only when they earn their place.** Explain a non-obvious *why*
  (a footgun, a cross-system contract), never narrate what the code already
  says. Default to none.
- **Document decisions as ADRs.** Add `docs/adr/NNN-<slug>.md` for architectural
  changes; cross-reference from the relevant architecture docs.
- **Don't commit plan files.** `plans/**`, `PLAN.md`, and scratch docs are local
  only; never `git add` them. Check the staged list before committing — a
  `git add -A` picks them up silently.

## Commit & PR workflow

- Commit as **iuliansafta** and **sign the commits** (GPG).
- Open a PR for every change; do not push to the default branch directly.
- Before opening any PR, review the code with **Plannotator**
  (`plannotator-review`).
- Commit subjects follow `<type>(<scope>): <summary>` (`feat`, `fix`, `docs`,
  `build`, `test`, `chore`). The body explains *why*, and may include the
  affected packages as a bullet list. Reference issues as `vorash/vora#NN`.
- Branches are `<type>/<ticket>` with the ticket lower-cased (e.g.
  `feat/dev-1991`). One ticket per branch; reuse the same branch name across
  repos when a change spans them.