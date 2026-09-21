# ADR 016 — NetworkFull and NetworkPrivate

- Status: accepted (v0.5)
- Date: 2026-09-21

## Context

ADR 004 defined the network model and reserved two modes. `NetworkNone` and
`NetworkRestricted` shipped; `NetworkFull` and `NetworkPrivate` stayed
unimplemented, so `runtime.NetworkMode` advertised values no backend honoured.

ADR 004 also set the governing rule: private ranges (`127.0.0.0/8`,
`169.254.0.0/16`, `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`) are denied
by default and only relaxed when a mode explicitly permits it.

## Decision

**1. Every non-None mode uses the same per-sandbox namespace + veth.**
The runner prepares a network namespace with a point-to-point veth (the
`NetworkRestricted` primitive, ADR 012). The mode only changes what the
namespaces may reach.

**2. NetworkFull routes and masquerades public egress, and still blocks private
ranges.** The sandbox's output chain accepts everything except the ADR 004
private destinations, which are dropped before routing. The runner enables IPv4
forwarding and installs per-sandbox forward + postrouting chains that drop the
same private destinations and masquerade the sandbox's source. A sandbox
therefore reaches the internet but not another sandbox, the host LAN, or the
metadata service.

**3. NetworkPrivate gives a tenant a shared /24 the runner routes between.**
Each tenant is assigned `10.9.<tenantIndex>.0/24`. Within it every sandbox gets
its own /30, so the runner owns a unique gateway address per veth. The sandbox's
output chain is default-deny and accepts only its tenant's /24; the runner's
forward chain accepts that /24 from the sandbox and drops everything else. No
NAT is installed, because peers are reached through the runner and the traffic
never leaves the namespace. Public egress and every other private range,
including other tenants, are denied.

**4. Per-sandbox nftables tables in the runner namespace.**
A shared table with `policy drop` would drop *every* sandbox's forwarded
traffic, because a packet traverses all base chains at a hook and any `drop`
wins. Each sandbox therefore gets `vora_fwd_<id>` and `vora_nat_<id>` tables
with `policy accept` that only emit drops for their own source address.

**5. Validate rejects combinations that make no sense.**
Allowed domains and broker aliases require `NetworkRestricted`; `NetworkPrivate`
requires a tenant. This is enforced in `SandboxSpec.Validate` so a bad spec
fails before any namespace is created.

## Consequences

- `NetworkPrivate` peer connectivity is by address; there is no per-tenant
  resolver, so peers are addressed directly (or by the caller's own discovery).
- One `NetworkPrivate` tenant is capped at 64 concurrent sandboxes (64 /30s in
  a /24) and the tenant space at 256. Exceeding either fails the create loudly.
- The tenant index is assigned per runner and not persisted; after a restart all
  leftovers are reaped and the index space is reused. Two runners serving the
  same tenant do not share an L2 or routing domain, so peers must be on the same
  runner.
- `NetworkFull` needs `net.ipv4.ip_forward` in the runner's network namespace,
  which `EnableForwarding` sets; a host that forbids it fails the start.
- gVisor still supports only `NetworkNone` (issue #18), so these modes are
  Bubblewrap-only.

## Refs

- Issue vorash/vora#19; master plan §5, §17.
- `internal/network`, `internal/runtime/bwrap`; ADR 004, ADR 012.