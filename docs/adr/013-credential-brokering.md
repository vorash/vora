# ADR 013 — Credential brokering

- Status: accepted (v0.4)
- Date: 2026-09-17

## Context

Master plan §18 sets the goal: a sandbox must be able to call a third-party API
without its token ever existing in the sandbox's environment, filesystem, or
`/proc`. ADR 004 decided credentials are **brokered, not injected**, and
sketched a broker route `http://egress/creds/<alias>/…`; v0.3 deferred the
implementation.

The blocker was mechanical. The v0.3 egress gateway (ADR 012) is a **passthrough
SNI proxy**: it inspects the TLS ClientHello, then forwards raw bytes. It never
terminates TLS, so it cannot attach or rewrite an `Authorization` header on a
connection aimed straight at `api.github.com`. Attaching a credential to an
unmodified `curl https://api.github.com/user` requires either a broker route the
workload calls explicitly, or full TLS termination (a CA plus a trust-store
change). ADR 004 already rejected the MITM route for v0.x.

## Decision

**1. Broker route on the gateway (no TLS termination).**
The runner starts a plaintext HTTP broker listener on the sandbox's gateway
address inside the per-sandbox network namespace (default `:8080`,
`VORA_EGRESS_BROKER_PORT`), next to the SNI proxy. A sandbox calls:

```
http://<gateway>:8080/creds/<alias>/<path>
```

The broker resolves `<alias>` to a bound upstream `{host, scheme, secret}`,
drops the sandbox's own `Authorization`/`Cookie`/`Host` headers, attaches the
credential, and re-originates to the upstream **over TLS**. The
sandbox→gateway hop never leaves the netns; the gateway→upstream hop is
TLS. `VORA_BROKER_URL` (non-secret) is injected into the sandbox environment so
the workload does not guess the address.

**2. Aliases are bound to a host and a secret.**
An alias maps to exactly one upstream host and one credential. A brokered
request is only served when the bound host is also on the sandbox's
`AllowedDomains`; otherwise it is denied (`broker_host_not_allowed`) and
audited. A granted alias can never be used to reach an unlisted host.

**3. No credential is ever forwarded across a redirect.**
The broker re-originates only to the alias's bound host. A `3xx` is relayed to
the sandbox verbatim and the sandbox follows it through the normal SNI proxy,
where the allowlist applies. This satisfies the master-plan criterion
"redirect handling does not leak credentials".

**4. `vora-api` owns secrets; the runner never does.**
Secrets live in Postgres (`vora_secrets`,
`id, tenant_id, alias, host, scheme, nonce, ciphertext, created_at, updated_at`)
encrypted with AES-256-GCM under `VORA_MASTER_KEY`, which only `vora-api`
holds. Storage and CRUD mirror the API-key pattern in
`internal/api`: writes are tenant-scoped, values are write-only, and
`vora secrets list` returns metadata only.

At sandbox creation the API resolves each requested alias for the caller's
tenant and rejects unknown aliases or aliases whose host is not allowlisted
*before* it dials a runner. The resolved secrets travel in a dedicated,
non-persisted field of the create request and are consumed by the runner to
build the broker. They are **not** part of `SandboxSpec`: the runner's
persisted record (`state/sandboxes/<id>/config.json`) stores only the granted
aliases.

**5. Consequences of an API-owned model.**
- Brokering requires `--api`. A local-socket `vora sandbox create --broker …`
  cannot resolve a secret and fails with an explicit error rather than
  silently creating an unbrokered sandbox.
- The API→runner create call carries plaintext secrets. The runner refuses to
  accept them unless TLS is configured on that channel (`VORA_RUNNER_TLS_*`,
  via `internal/tlsutil`), unless an explicit insecure override is set; the
  insecure case is logged loudly at startup.

**6. Audit.**
Every brokered decision is an `EgressRecord` carrying the **alias**, the bound
host, the decision, and a reason — never the secret. Allowed decisions are
persisted before any upstream byte is forwarded; a persistence failure denies
the connection (`audit_failed`). Records flow through the existing
JSONL/Postgres stores, `GetSandboxEgress`,
`GET /v1/sandboxes/{id}/egress`, and `vora logs --egress`.

## Consequences

- The sandbox still never sees the secret: not in its environment, not in
  `/proc/self/environ`, not on disk, and not in the audit trail. Only the
  credential user sees it, once, when setting it.
- The workload must target the broker URL rather than the upstream API
  directly. The master-plan demo becomes
  `curl http://$VORA_BROKER_URL/creds/github/user` instead of
  `curl https://api.github.com/user`.
- Brokering is **bwrap-only**, because it rides the `NetworkRestricted` path;
  gVisor sandboxes have neither restricted egress nor brokering.
- No CA distribution, no image trust-store changes, no pinning breakage.
- The broker is HTTP-only by design: non-HTTP protocols through the broker
  route are out of scope, and direct non-TLS egress remains denied.
- A future "transparent" variant that terminates TLS for aliased hosts (so
  unmodified `curl https://api.github.com` works) remains possible; it would
  supersede this ADR and reintroduce CA lifecycle concerns.
