# ADR 024 — TypeSafe JEV as an agent integration

- Status: accepted
- Date: 2026-02 (v0.8)

## Context

`vorash/vora-probe` evaluates TypeSafe's Jev decision model as one arm of an
evidence-relation benchmark (`models/jev.go`) and established the contracts:
batched typed questions in one call, `confidence` as answer-distribution
concentration (never correctness probability), failed calls reported as failed
arms, and the provider-returned model recorded on every result. We want that
decision capability available to agents in general, not just the probe.

## Decision

**`integrations/typesafe-jev` (`@vorash/typesafe-jev`): an integration, not
core.** Like `@vorash/openai-agents`, it is a standalone npm package that
nothing in Vora depends on. It exposes JEV's three primitives (`choice`,
`score`, `noul`) behind one `jev_decide` tool and a `decide()` API.

**The official `@typesafe-ai/sdk` is the transport**, pinned to an exact version
(the SDK is pre-1.0). The probe's hand-rolled Go client exists because Go has no
official SDK; in TypeScript, owning an HTTP client would duplicate typed
questions, retries, and error classes for no gain.

**The probe's semantics contract is part of the tool surface.** The
confidence caveat ships verbatim in the tool description and README, `state` is
documented as data, and a response that drops or invents answers is rejected by
name. The probe's relation-labeling use case ships as an example recipe
(`examples/probe-relations.ts`), not as core API.

## Consequences

- Agents get calibrated structured decisions without Vora core gaining a model
  dependency or a TypeSafe credential path; keys stay run configuration
  (`TYPESAFE_API_KEY`).
- A breaking SDK release is absorbed at a reviewed version bump, not silently.
- The probe keeps its own Go arm; the TS example is pinned to parity by tests,
  so drift between the two is visible.
