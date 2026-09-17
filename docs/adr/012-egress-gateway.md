# ADR 012 — Egress gateway (NetworkRestricted)

- Status: accepted (bwrap only; gVisor deferred)
- Date: 2026-09-17

## Context

ADR 004 decided that `NetworkRestricted` must force **all** egress through a
`vora-egress` proxy that allows a domain allowlist using **TLS SNI inspection
with passthrough** (no termination/MITM). v0.2 only implemented `NetworkNone`.

Enforcing this means the sandbox must not be able to reach anything except the
proxy, even if the workload ignores proxy environment variables. That requires
control of a network namespace the runner owns.

## Decision

A **`network.Manager`** prepares, per sandbox:

1. a network-namespace holder (`unshare --net sleep`), rootless, inside the
   runner's rootlesskit user namespace;
2. a point-to-point **veth** pair — host end in the runner (`10.7.x.1/30`),
   sandbox end moved into the holder (`10.7.x.2/30`) with a default route via
   the gateway;
3. **nftables** in the sandbox netns: `dnat to <gateway>:8443` for every
   outbound TCP connection not already addressed to the gateway.

`redirect` was rejected: in an output NAT chain it targets the *local* netns, so
the proxy would have to live inside the sandbox. `dnat` keeps the proxy outside
and lets it recover the destination from the TLS ClientHello.

The **`egress.Proxy`** listens on the sandbox's gateway address. It reads the
ClientHello SNI, checks the sandbox's allowlist (exact or subdomain), passes
bytes through untouched, and denies non-TLS traffic and any upstream that
resolves to a loopback/private/link-local/CGNAT address.

`NetworkRestricted` is **bwrap only**. Bubblewrap is launched by the runner
(not `systemd-run`) so it can join the prepared netns via
`nsenter -t <holder> -n bwrap --share-net`; cgroup limits come from the same
delegated `cgroup.Subtree` gVisor already uses. `NetworkNone` keeps the existing
`systemd-run` path unchanged.

## Consequences

- The sandbox can only reach the proxy; allow/deny is enforced regardless of
  application configuration, and every decision is logged and metered
  (`vora_network_denied_total`, `vora_policy_denied_total`).
- The proxy passes TLS through, so no CA distribution or trust-store change is
  required. Non-TLS/other-port traffic is denied by default.
- **gVisor is not covered.** `runsc --network=host` resolves to the *runner's*
  netns (not the prepared one), and joining the netns through the OCI spec's
  network namespace path makes egress time out because gVisor's netstack
  bypasses the host-ns DNAT. gVisor therefore still advertises `NetworkNone`
  only; auto-select falls back to bwrap for restricted specs.
- **DNS is not routed.** Only TCP is DNAT'd, so workloads must resolve names
  themselves (or be given literal addresses). A gateway DNS resolver is a
  follow-up.
- A sandbox that only speaks plaintext HTTP cannot use the gateway yet; that is
  a deliberate default-deny.
- The manager records live networks under `<state>/netns/` and reaps leftovers
  on runner startup (fail-closed restart), matching the runner's other
  reconcile behaviour.
