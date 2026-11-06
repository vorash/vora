# ADR 017 — gVisor NetworkRestricted egress

- Status: accepted (v0.5)
- Date: 2026-09-21

## Context

ADR 012 decided that `NetworkRestricted` forces all egress through an
SNI-inspecting gateway, and implemented it for Bubblewrap by DNAT'ing the
sandbox's outbound TCP to the gateway proxy from the *sandbox* netns output
hook. It recorded gVisor as a permanent limitation: gVisor's userspace netstack
"bypasses the host-ns DNAT", so a gVisor sandbox could not egress, resolve
names, or use brokered credentials.

Two spikes (issue #18) established the boundary precisely:

- `runsc --network host` is not the deprecated host-kernel hostinet. gVisor
  still terminates TCP/IP in its userspace netstack. Commands it runs are
  netstack flows, not host sockets, so **kernel netfilter on the sandbox's
  output path never sees them**, which was the original ADR 012 finding.
- But netstack does emit real frames out the sandbox's veth, so they arrive in
  the **runner** netns and traverse its **prerouting** hook. A DNAT rule there
  captures them (verified locally under `rootlesskit` with a real `runsc`).
  This mirrors the bwrap rule at a different hook.

## Decision

**1. Enforce restricted egress in the runner netns for gVisor.**
`network.Policy` gains `Enforcement` (`sandbox` | `runner`). For `runner`,
`runnerRestrictedRules` installs, in the runner netns:
- nat prerouting: DNAT the sandbox veth's TCP (`ip daddr != gateway`) to the
  gateway proxy, and UDP/53 to the gateway resolver;
- filter input: accept only the gateway proxy/broker/DNS ports for that veth;
- filter forward: drop everything else that veth sends, so nothing is
  forwarded onward.

Each sandbox gets its own tables with `policy accept`, so one sandbox's drops
cannot affect another's traffic (the same reasoning as ADR 016).

**2. The sandbox runs in its own netns via `--network host`.**
The gVisor backend launches `runsc create`/`start` inside the sandbox's network
namespace (`nsenter -t <holder> -n runsc --network host …`). `--network host`
then means "this netns". The sandbox gets its own loopback and the veth, and
its only path out is the veth that the runner's rules police. It does not share the
runner's loopback.

**3. Reuse the egress components unchanged.**
The SNI proxy, allowlist resolver and credential broker listen on the gateway
address exactly as for bwrap. Audit, metrics, allowlist and broker logic are
shared. A host `resolv.conf` pointing at the gateway is bound over
`/etc/resolv.conf` in the OCI bundle. The broker base URL is injected when
aliases are granted.

**4. `--net-raw` stays off** (gVisor's default), so the sandbox cannot craft
packets to bypass the runner rules.

## Consequences

- gVisor `NetworkRestricted` reaches allowlisted hosts, resolves only
  allowlisted names, refuses private/metadata answers, and supports brokered
  credentials. That gives parity with bwrap and closes ADR 012's limitation.
- gVisor-only gaps remain: `NetworkFull` and `NetworkPrivate` are not implemented for
  gVisor (only `NetworkRestricted` is in scope here).
- The runner-side policy is keyed by the sandbox veth, so it must be re-applied
  if the veth is recreated. The manager persists the enforcement mode with the
  net state and cleans the runner tables on teardown and startup reap.
- Verification of the datapath belongs on a real gVisor host (the isolation
  suite runs under `rootlesskit`/root). The unit tests cover rule generation
  and the manager's enforcement selection.

## Refs

- Issue vorash/vora#18; ADR 012 (egress gateway), ADR 004 (network model),
  ADR 016 (network modes).
- `internal/network` (`runnerRestrictedRules`), `internal/runtime/gvisor`.