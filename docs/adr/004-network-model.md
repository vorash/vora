# ADR 004 — Network model

- Status: accepted
- Date: 2026-09-16

## Context

Untrusted code must not reach the metadata service, private networks, internal
services, or arbitrary hosts. At the same time, agents legitimately need to
reach a small set of public endpoints (package registries, APIs). Domain-based
policy and credential brokering are core product features, but they are the
hardest primitives to get right.

## Decision

- **v0.1: `NetworkNone` only.** Sandboxes run with `--unshare-net`; there are no
  interfaces and no routes. Not even loopback egress beyond the sandbox's own
  namespace. Unsupported modes return an explicit error.
- **v0.3: `NetworkRestricted`.** All egress is forced through a `vora-egress`
  proxy that applies an allowlist using **TLS SNI inspection with passthrough**
  (no TLS termination / MITM).
- **Credentials are brokered, not injected.** The sandbox calls a broker route
  (for example `http://egress/creds/<alias>/…`); the proxy maps it to the
  upstream host and attaches the secret. The private key or token never exists
  in the sandbox environment or filesystem.
- **Default deny for private ranges.** `127.0.0.0/8`, `169.254.0.0/16`
  (including `169.254.169.254`), `10.0.0.0/8`, `172.16.0.0/12`,
  `192.168.0.0/16`, and internal Vora services are blocked unless explicitly
  permitted.

## Consequences

- v0.1 is trivially safe on the network: no network exists.
- SNI-based allowlisting requires no CA distribution and no trust-store
  changes, which keeps the demo robust; the trade-off is that HTTP (non-TLS)
  traffic cannot be inspected the same way and must be handled explicitly.
- Credential brokering through a broker route is slightly less transparent than
  header injection but avoids MITM certificate lifecycle and pinning problems.
- Denials are first-class events and must appear in the audit trail with the
  attempted host and reason.
- `NetworkFull`/`NetworkPrivate` were reserved modes through v0.4 and are
  implemented in v0.5 ([ADR 016](016-network-modes.md)).
