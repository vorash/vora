# ADR 025 — Agent identity and agent-bound API keys

- Status: accepted
- Date: 2026-02 (v0.8)

## Context

`vorash/vora#32` (master plan M30, §37 P3): identifiers existed (`tenant_id`,
`project_id`, `agent_id`, `sandbox_id`, `execution_id`) but there was no
per-agent identity. Auth was tenant-scoped API keys, `agent_id` was unverified
metadata any tenant key could forge, and audit records attributed to nothing at
all — no tenant, no agent, no key. SPIFFE/SPIRE is explicitly not implemented
yet; the identifier and authz path are designed so it can be layered later.

## Decision

**Canonical identity: `spiffe://vora.sh/tenant/<t>/agent/<a>`.**
`internal/identity` owns the shape: the URI is *derived* from a tenant and an
agent handle and never stored or sent. Handles are validated at every entry
point (`[a-z0-9][a-z0-9_-]{0,63}`), segments are escaped, and `Parse` is strict
(canonical round-trip required) — the URI is an authz subject, so its parsing is
a security boundary. No SPIFFE protocol code exists anywhere.

**Authz path: agent-bound API keys.** `APIKey.AgentID` binds a key to one agent
(`vora keys create --agent <handle>`, sandbox scope only — admin keys are not
workload credentials). Like the existing tenant rule, the identity is *forced*
from the key at create (`spec.AgentId = key.AgentID`), never trusted from the
request. Object access composes on top of tenant scoping: a bound key reaches
only its own agent's sandboxes and snapshots, everything else reads as "not
found" (the existing cross-tenant indistinguishability). Unbound tenant keys
keep their legacy reach across the tenant, and their `agent_id` stays
*asserted*, not verified — visible in audit as attribution without a binding.

**Audit attribution on every record.** `Event`, `ExecRecord` and `EgressRecord`
carry `Actor{tenant_id, agent_identity, key_id}`. Tenant and agent identity are
derived by the runner from the persisted spec (authoritative after create);
`key_id` marks request-driven actions (start/exec/destroy/fork) and travels in
the runner RPC `Actor` message from the control plane, which stamps it from the
authenticated key. Machinery transitions (TTL expiry, reconcile, quota) carry no
`key_id` — they have no request. `audit/postgres` gains additive columns
(`0003_agent_identity`); `audit/jsonl` gains JSON fields.

**The SPIRE/SVID seam.** An SVID issuer later asserts the same
`identity.Agent` URI; policy code and audit records are unchanged. The trust
domain (`vora.sh`) and the derived-not-stored rule are what make that a drop-in.

## Consequences

- Agents of one tenant are now isolated from each other at the control plane;
  a leaked workload key exposes one agent, not the tenant.
- Every audit record answers "which agent, on whose key", across events, execs
  and egress decisions.
- Create writes no audit record (the record starts at the start transition), so
  the RUNNING transition is the earliest attributed record; this is the plan's
  "create carries it" landing where creation actually emits a record.
- Cross-agent enforcement lives entirely in `internal/api`; the runner never
  sees a foreign-agent request, so the isolation tests sit with the control
  plane rather than in the runtime `security` suite (which has no API fixture).
- Wire compatibility: proto `Actor` fields are additive; old runners ignore
  them and lose `key_id` attribution until upgraded.
