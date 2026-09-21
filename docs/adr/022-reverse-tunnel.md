# ADR 022 — NAT reverse tunnel

- Status: accepted (v0.6)
- Date: 2026-09-21

## Context

A runner dials **out** to `vora-api` to register and heartbeat, so it can sit
behind NAT for registration — but the API then has to **dial back** to operate a
sandbox (create/exec/destroy). A runner behind NAT therefore cannot be driven at
all. This is part B of issue #25; ADR 021 covers placement.

## Decision

**1. A runner-initiated multiplexed tunnel.**
The runner opens one long-lived TCP connection to the API's tunnel endpoint
(`VORA_API_TUNNEL_ADDR`) and authenticates with a one-line JSON hello
(`runner_id` + `join_token`, constant-time compared against the API's join
token; mTLS applies where the registry already uses it). The API terminates it
and multiplexes with **yamux**: the API is the yamux client and the runner is
the yamux server, one stream per API→runner gRPC connection.

**2. Transparent to the existing dial path.**
The API's `dialRunner` returns a `client.DialTunnel` over a virtual connection
instead of a TCP dial when the runner is tunnelled, so `placeRunner`,
`runnerFor` and exec streaming are unchanged. Exec works because the stream is a
real gRPC connection (HTTP/2) over the multiplexed link.

**3. A tunnelled runner registers with no address.**
When `VORA_API_TUNNEL_ADDR` is set the runner registers `address=""` and the
API marks it `tunneled`. Registration with an empty address is refused unless a
tunnel is configured, so an unroutable runner is never registered. One live
session per runner; a new session supersedes the old.

**4. Only the RunnerService is tunnelled.**
The registry (`Register`/`Heartbeat`) and metrics already flow runner→API and
need no tunnel. The tunnel carries the API→runner direction only.

## Consequences

- A runner behind NAT can be placed and driven end-to-end, closing the v0.3
  "reverse tunnel is a follow-up" note in ADR 014.
- `hashicorp/yamux` is a new (small, pure-Go) dependency; the alternative — a
  custom command stream — was rejected because it re-implements gRPC streaming,
  backpressure and cancellation and duplicates the generated `RunnerService`.
- The tunnel connection is not yet re-established automatically if it drops:
  the runner serves the streams of its one session, and a drop ends that
  session. Reconnection is a follow-up.
- The tunnel is plaintext unless TLS is added at the listener; the join token
  authenticates but does not encrypt. A future change should wrap the listener
  in TLS/mTLS.

## Refs

- Issue vorash/vora#25 (part B); ADR 014 (control plane), ADR 021 (placement).
- `internal/tunnel`, `internal/client.DialTunnel`, `internal/api` (`dialRunner`),
  `cmd/vora-api`, `cmd/vora-runner`.