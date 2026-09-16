# Vora

**Secure execution infrastructure for autonomous agents.**

Vora runs untrusted AI agents and generated code inside isolated Linux
environments with resource controls, network policy, credential brokering, and
complete execution traces.

Built with Go, Linux namespaces, cgroups v2, Bubblewrap, gVisor, containerd,
and OpenTelemetry.

> **Status: early proof of concept.** The runtime is under active development.
> Nothing in this repository is production-ready, and none of the isolation
> boundaries have been independently audited. See [Roadmap](#roadmap).

## What problem this solves

Agents and LLM-generated code are untrusted by default. They are usually run
either directly on the host or inside containers that share the host kernel and
have no meaningful network or credential boundary. Vora's thesis is that
untrusted code should run behind a *policy-controlled execution boundary*:

- filesystem isolation
- PID / IPC / namespace isolation
- CPU, memory and process-count limits (cgroups v2)
- default-deny network policy
- credentials brokered outside the sandbox
- a full audit trail of every execution

## Architecture (target)

```text
Developer / Agent
      │
  CLI / SDK / MCP
      │
      ▼
  Vora API ──gRPC──▶ Vora Runner ──▶ runtime backend ──▶ sandbox
                                    ├─ bwrap (local/dev)
                                    └─ gVisor/containerd (hosted)
      │
      ▼
  Vora Egress (allowlist, credential injection, audit)
```

The control plane never depends on the isolation technology; every backend is
behind a small `runtime.Backend` interface.

## Roadmap

| Version | Scope |
| ------- | ----- |
| **v0.1** | Go runner, Bubblewrap backend, cgroups v2, in-sandbox exec agent, CLI, security test suite, benchmarks |
| v0.2 | gVisor/containerd backend, HTTP control plane, remote runner |
| v0.3 | Network policy, egress gateway, credential brokering, audit, SDK, MCP |

## Repository layout

```text
api/proto/v1/   protobuf contracts (runner + in-sandbox agent)
cmd/            vora (CLI), vora-runner (daemon), vora-init (sandbox agent)
internal/       runtime, cgroup, images, workspace, sandbox, cli, ...
docs/           architecture docs and ADRs
tests/          integration, security and benchmark suites
deploy/         systemd units and Ansible roles
```

## Development

Requirements: Go 1.27+, `protoc` with `protoc-gen-go` / `protoc-gen-go-grpc`,
and (for the local backend) Bubblewrap plus a systemd user session.

```bash
make build        # build all packages
make proto        # regenerate protobuf Go code
make test         # unit tests
make test-security# isolation security suite
make bench        # benchmark harness
make help         # all targets
```

## Security model

Vora assumes all sandboxed code is malicious. The threat model and the exact
boundaries are documented under `docs/architecture/`. The security test suite
under `tests/security/` is a first-class deliverable: each test encodes *why* a
boundary matters and is expected to fail if that boundary is removed.

## License

MIT — see [LICENSE](LICENSE).
