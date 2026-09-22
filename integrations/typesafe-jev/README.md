# @vorash/typesafe-jev

Use [TypeSafe's JEV](https://docs.typesafe.ai) decision model from
[OpenAI Agents](https://openai.github.io/openai-agents-js/).

This is an **integration**, not part of the Vora core: it depends on
[`@typesafe-ai/sdk`](https://www.npmjs.com/package/@typesafe-ai/sdk) and the
Agents SDK, and nothing in Vora depends on it.

## What it gives an agent

One tool, `jev_decide`: send a piece of context (`state`) and any number of
narrow structured questions in a single call, get one typed answer per question.

```text
jev_decide {
  state: "Hi, I've been trying to connect my Stripe account for 3 days...",
  questions: {
    department: { type: "choice", instructions: "Which team should handle this",
                  criteria: { billing: "Payment or subscription issues",
                              technical: "Bugs or integration problems",
                              sales: "Pricing or account questions" } },
    frustration: { type: "score", instructions: "How frustrated the customer appears",
                   criteria: ["Calm, just stating facts", "Frustrated but civil", "Very angry"] },
    urgency: { type: "noul", instructions: "The message conveys urgency or time-sensitivity" }
  }
}
  -> { model: "jev-1.13.0", requestedModel: "jev-latest",
       answers: { department: { type: "choice", choice: "technical", confidence: 0.78,
                                probabilities: { technical: 0.85, billing: 0.15, sales: 0.0 } },
                  frustration: { type: "score", score: 1.0, confidence: 1.0, ... },
                  urgency: { type: "noul", noul: 0.9 } },
       usage: { input_tokens: 120, output_tokens: 30 } }
```

Everything goes in **one round trip** — N questions cost one call, so a batch of
decisions stays cheap and atomic.

## Install

```sh
npm install @vorash/typesafe-jev @openai/agents zod
```

`@openai/agents` and `zod` are peer dependencies — your agent already has them.

## Use

```ts
import { Agent, run } from "@openai/agents";
import { JevDecisionClient } from "@vorash/typesafe-jev";

const jev = new JevDecisionClient({ model: "jev-1.13.0" });

const agent = new Agent({
  name: "Support triage",
  instructions: "Classify with jev_decide; do not guess.",
  tools: [jev.tool()],
});

const result = await run(agent, "Triage this: my integration keeps failing.");
console.log(result.finalOutput);
```

A complete runnable version is in [`examples/agent.ts`](examples/agent.ts).

### Deciding without an agent

`decide()` is the programmatic path — typed answers, errors thrown (the tool
returns `error:` strings instead, so a model can adapt):

```ts
const result = await jev.decide(state, questions);
console.log(result.answers.department.choice, result.model, result.usage);
```

## Question types

| Type | `criteria` | Answer |
| --- | --- | --- |
| `choice` | map of label → description (`null` for undescribed labels) | selected `choice`, `probabilities` per label, `confidence` |
| `score` | ordered rubric, at least two levels | expected `score` (may fall between levels), `probabilities` per level, `confidence` |
| `noul` | optional `{ true, false }` outcome descriptions | `noul` — the probability that the answer is yes |

Instructions and criteria also accept JSON structure; see the
[TypeSafe primitives docs](https://docs.typesafe.ai/primitives.md) and the
`decide()` types (`EntryType`) for that advanced form.

## Confidence semantics

**`confidence` measures the concentration of the answer distribution, not the
probability that the answer is correct.** A confidence of 0.9 means Jev's answer
distribution was concentrated on one option — it is not a 90% guarantee that the
answer is right. Use it as a routing signal (act / ask a human / gather more
evidence), never as a correctness score. This wording is part of the tool
description, so the model sees it too.

Two more properties worth knowing: `state` content is data and cannot carry
instructions to the model, and a response that drops or invents an answer is
rejected by name rather than silently accepted.

## Recipe: evidence-relation labeling (vora-probe parity)

[`examples/probe-relations.ts`](examples/probe-relations.ts) reproduces the
classification that `vorash/vora-probe`'s Jev arm runs: one `choice` question per
piece of evidence over `supports` / `contradicts` / `neutral` /
`insufficient_context`, with shared rules against causal overreach, all batched
in one call. The exported `relationQuestions()` builds the questions; the test
suite pins the label set and definitions.

## Configuration

`JevDecisionClient` takes a pre-built `@typesafe-ai/sdk` client or the connection
options:

| Option | Default | Meaning |
| --- | --- | --- |
| `client` | — | Reuse an existing `TypeSafeClient`; when given, the options below are ignored |
| `apiKey` | `TYPESAFE_API_KEY` | TypeSafe API key |
| `baseUrl` | `https://api.typesafe.ai` | API root |
| `model` | `jev-latest` | Model to request; pin one (e.g. `jev-1.13.0`) for reproducibility. The provider's returned model is reported on every result |
| `timeoutMs` | SDK default | Timeout per attempt |
| `maxRetries` | SDK default (2) | Retries after the initial attempt |
| `fetch` | global `fetch` | Custom transport, for tests or instrumentation |

### Errors

`jev_decide` returns a string beginning with `error:` when the call fails —
including when a response leaves a question unanswered or answers with the wrong
type, naming the offending question — rather than throwing, so the model can
read the message and adapt. Programmatic callers using `decide()` get the
exception (`TypeSafeError` and friends from the SDK).

## Development

```sh
npm install            # from the repository root
npm run build          # then: node --experimental-strip-types examples/*.ts
npm test --workspace @vorash/typesafe-jev
```

The tests drive both paths against a fake `fetch` injected into the TypeSafe
client — request shape, the probe-parity label set, answer validation, and the
model-facing `FunctionTool.invoke` path — without calling the TypeSafe API.
