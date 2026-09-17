# ADR 012 — Egress gateway (NetworkRestricted)

- Status: accepted (bwrap only; gVisor is a documented limitation)
- Date: 2026-09-17 (amended 2026-09-17 for v0.4: gateway DNS, broker port,
  gVisor status)

## Context

ADR 004 decided that `NetworkRestricted` must force **all** egress through a
`vora-egress` proxy that allows a domain allowlist using **TLS SNI inspection
with passthrough** (no termination/MITM). v0.2 only implemented `NetworkNone`.

Enforcing this means the sandbox must not be able to reach anything except the
proxy, even if the workload ignores proxy environment variables. That requires
control of a network namespace the runner owns.

## Decision

A **`network.Manager`** prepares, per sandbox:

1. a network-namespace holder (`unshare --net sleep infinity`), rootless,
   inside the runner's rootlesskit user namespace. Startup reap only SIGKILLs a
   recorded PID when `/proc/<pid>/cmdline` still looks like that holder, so a
   recycled PID is not killed;
2. a point-to-point **veth** pair — host end in the runner (`10.7.x.1/30`),
   sandbox end moved into the holder (`10.7.x.2/30`) with a default route via
   the gateway;
3. **nftables** in the sandbox netns:
   - NAT: `dnat to <gateway>:8443` for non-loopback TCP not already addressed
     to the gateway;
   - filter: output hook **policy drop**, accepting only loopback and TCP to
     the exact gateway proxy port. Direct gateway services, UDP, and other
     protocols are denied even if a listener exists.

`redirect` was rejected: in an output NAT chain it targets the *local* netns, so
the proxy would have to live inside the sandbox. `dnat` keeps the proxy outside
and lets it recover the destination from the TLS ClientHello.

The **`egress.Proxy`** listens on the sandbox's gateway address. It reads the
ClientHello SNI, checks the sandbox's allowlist (exact or subdomain), passes
bytes through untouched, and denies non-TLS traffic and any upstream that
resolves to a loopback/private/link-local/CGNAT address (rejected in
`Dialer.Control` before connect). Transient accept errors are logged and retried;
ClientHello failures are audited with distinct reasons (`not_tls`, `no_sni`,
`handshake_timeout`, `handshake_failed`). Concurrent connections
are capped (`VORA_EGRESS_MAX_CONNECTIONS`, default 128) and idle connections
time out (`VORA_EGRESS_IDLE_TIMEOUT`, default 5m).

Every allow and deny is persisted through `audit.Store` (`egress.jsonl` or
`sandbox_egress`) and exposed as `GetSandboxEgress` / `GET /v1/sandboxes/{id}/egress`
and `vora logs --egress`. An allow is recorded **before** sandbox bytes are
forwarded; if that write fails the connection is denied (`audit_failed`).

`NetworkRestricted` is **bwrap only**. Bubblewrap is launched by the runner
(not `systemd-run`) so it can join the prepared netns via
`nsenter -t <holder> -n bwrap --share-net`; cgroup limits come from the same
delegated `cgroup.Subtree` gVisor already uses. `NetworkNone` keeps the existing
`systemd-run` path unchanged.

## Consequences

- The sandbox can only reach the proxy; allow/deny is enforced regardless of
  application configuration. Decisions are persisted and metered
  (`vora_network_decisions_total`, `vora_network_denied_total`,
  `vora_policy_denied_total`, `vora_audit_write_failed_total`).
- The proxy passes TLS through, so no CA distribution or trust-store change is
  required. Non-TLS/other-port traffic is denied by default.
- **gVisor is not covered, by decision.** `runsc --network=host` resolves to the
  *runner's* netns (not the prepared one), and joining the netns through the OCI
  spec's network namespace path makes egress time out because gVisor's netstack
  bypasses the host-ns DNAT. gVisor therefore advertises `NetworkNone` only;
  auto-select falls back to bwrap for restricted specs. This is a **permanent
  limitation of the current design**, not pending work: the userspace-netstack
  gateway that would close it is out of scope for v0.x.
- **DNS, decided for v0.4.** v0.3 DNATs only TCP, so a restricted workload must
  use literal addresses. v0.4 adds an **allowlist-only resolver on the gateway
  address**: the resolver answers only names matching the sandbox's
  `AllowedDomains` (everything else returns NXDOMAIN/REFUSED), refuses answers
  that map to a private/loopback/link-local/CGNAT address (reusing the same
  `blockedIP` check that guards upstreams), and audits every query as an egress
  decision. `nftRuleset` accepts UDP/53 and TCP/53 (truncated answers) to that
  exact gateway endpoint, and the runner points the sandbox's resolver
  configuration at it, so no image change is needed. Optional hardening, off by
  default: DNAT `udp dport 53` from any destination to the gateway resolver so a
  workload with a hardcoded resolver still gets fail-closed answers instead of a
  silent drop.
- **The broker port is a second gateway service.** v0.4 adds the credential
  broker on the same gateway address (`:8080`, ADR 013). The `nft` filter
  accepts the proxy, broker, and DNS endpoints only; every other destination and
  protocol is still dropped, so a sandbox that speaks plaintext HTTP directly to
  an arbitrary host remains denied.
- The manager records live networks under `<state>/netns/` and reaps leftovers
  on runner startup (fail-closed restart), matching the runner's other
  reconcile behaviour.
