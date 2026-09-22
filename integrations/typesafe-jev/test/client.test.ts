import { RunContext } from "@openai/agents";
import { describe, expect, it } from "vitest";

import { CONFIDENCE_NOTE, JEV_DECIDE_TOOL_NAME, JevDecisionClient } from "../src/index.js";
import {
  RELATION_DEFINITIONS,
  RELATION_LABELS,
  relationQuestions,
} from "../examples/probe-relations.js";
import { clientFor, fakeFetch, firstRequest, usage } from "./helpers.js";

const STATE = "Hi, I've been trying to connect my Stripe account for 3 days and the integration keeps failing.";

function systemOne(answers: Record<string, unknown>, model = "jev-1.13.0"): unknown {
  return { model, answers, usage };
}

describe("decide", () => {
  it("asks every question in one request and carries its criteria", async () => {
    const fake = fakeFetch(() => ({
      json: systemOne({
        department: { type: "choice", choice: "technical", confidence: 0.78, probabilities: { technical: 0.85, billing: 0.15 } },
        frustration: { type: "score", score: 1, confidence: 1, legend: { "0": "calm", "1": "frustrated" }, probabilities: { "0": 0.0, "1": 1.0 } },
        urgency: { type: "noul", noul: 0.9 },
      }),
    }));
    const client = clientFor(fake, { model: "jev-1.13.0" });

    await client.decide(STATE, {
      department: {
        type: "choice",
        instructions: "Which team should handle this",
        criteria: { billing: "Payment or subscription issues", technical: "Bugs or integration problems" },
      },
      frustration: {
        type: "score",
        instructions: "How frustrated the customer appears",
        criteria: ["calm", "frustrated"],
      },
      urgency: { type: "noul", instructions: "The message conveys urgency" },
    });

    expect(fake.requests).toHaveLength(1);
    const request = firstRequest(fake);
    expect(request.url).toContain("/v1/systemone");
    expect(request.headers.authorization).toBe("Bearer jev_test");
    expect(request.body.state).toBe(STATE);
    expect(request.body.model).toBe("jev-1.13.0");
    const raw = JSON.stringify(request.body);
    expect(raw).toContain("Which team should handle this");
    expect(raw).toContain("Payment or subscription issues");
    expect(raw).toContain("frustrated");
    expect(Object.keys(request.body.questions as Record<string, unknown>)).toEqual([
      "department",
      "frustration",
      "urgency",
    ]);
  });

  it("reproduces the vora-probe relation labels in one batched request", async () => {
    const fake = fakeFetch(() => ({
      json: systemOne({
        "relation:OBS-0001": { type: "choice", choice: "supports", confidence: 0.9, probabilities: { supports: 0.9, contradicts: 0.0, neutral: 0.05, insufficient_context: 0.05 } },
        "relation:OBS-0002": { type: "choice", choice: "insufficient_context", confidence: 0.7, probabilities: { supports: 0.0, contradicts: 0.0, neutral: 0.3, insufficient_context: 0.7 } },
      }),
    }));
    const client = clientFor(fake, { model: "jev-1.13.0" });

    await client.decide(STATE, relationQuestions(["OBS-0001", "OBS-0002"]));

    const request = firstRequest(fake);
    expect(Object.keys(request.body.questions as Record<string, unknown>)).toEqual([
      "relation:OBS-0001",
      "relation:OBS-0002",
    ]);
    const raw = JSON.stringify(request.body);
    for (const label of RELATION_LABELS) {
      expect(raw).toContain(label);
    }
    for (const definition of Object.values(RELATION_DEFINITIONS)) {
      expect(raw).toContain(definition);
    }
  });

  it("passes answers through untouched and records both models", async () => {
    const answer = {
      type: "choice",
      choice: "contradicts",
      confidence: 0.82,
      probabilities: { supports: 0.05, contradicts: 0.82, neutral: 0.08, insufficient_context: 0.05 },
    };
    const fake = fakeFetch(() => ({ json: systemOne({ "relation:OBS-0001": answer }) }));
    const client = clientFor(fake);

    const result = await client.decide(STATE, relationQuestions(["OBS-0001"]));

    expect(result.answers["relation:OBS-0001"]).toEqual(answer);
    expect(result.requestedModel).toBe("jev-latest");
    expect(result.model).toBe("jev-1.13.0");
    expect(result.usage).toEqual(usage);
  });

  it("names the questions a partial response left unanswered", async () => {
    const fake = fakeFetch(() => ({
      json: systemOne({ first: { type: "noul", noul: 0.5 } }),
    }));
    const client = clientFor(fake);

    await expect(
      client.decide(STATE, {
        first: { type: "noul", instructions: "one" },
        second: { type: "noul", instructions: "two" },
      }),
    ).rejects.toThrow(/no answer for second/);
  });

  it("rejects an answer whose type does not match the question", async () => {
    const fake = fakeFetch(() => ({
      json: systemOne({ first: { type: "choice", choice: "a", confidence: 1, probabilities: { a: 1 } } }),
    }));
    const client = clientFor(fake);

    await expect(
      client.decide(STATE, { first: { type: "noul", instructions: "one" } }),
    ).rejects.toThrow(/first answered choice, asked noul/);
  });

  it("names answers that were not asked for", async () => {
    const fake = fakeFetch(() => ({
      json: systemOne({
        first: { type: "noul", noul: 0.5 },
        ghost: { type: "noul", noul: 0.5 },
      }),
    }));
    const client = clientFor(fake);

    await expect(
      client.decide(STATE, { first: { type: "noul", instructions: "one" } }),
    ).rejects.toThrow(/not asked for: ghost/);
  });

  it("builds the SDK client from options and sends the API key", async () => {
    const fake = fakeFetch(() => ({
      json: systemOne({ first: { type: "noul", noul: 0.5 } }),
    }));
    const client = new JevDecisionClient({
      apiKey: "jevkey",
      baseUrl: "https://api.typesafe.ai",
      model: "jev-1.13.0",
      maxRetries: 0,
      fetch: fake.fetch,
    });

    await client.decide(STATE, { first: { type: "noul", instructions: "one" } });

    expect(firstRequest(fake).headers.authorization).toBe("Bearer jevkey");
    expect(firstRequest(fake).body.model).toBe("jev-1.13.0");
  });
});

describe("jev_decide", () => {
  const args = JSON.stringify({
    state: STATE,
    questions: { urgency: { type: "noul", instructions: "Does this message express urgency?" } },
  });

  it("returns the decision when the model invokes it", async () => {
    const fake = fakeFetch(() => ({
      json: systemOne({ urgency: { type: "noul", noul: 0.9 } }),
    }));

    const output = await clientFor(fake).tool().invoke(new RunContext(), args);

    const parsed = JSON.parse(String(output)) as Record<string, unknown>;
    expect(parsed.model).toBe("jev-1.13.0");
    expect(parsed.requestedModel).toBe("jev-latest");
    expect(parsed.answers).toEqual({ urgency: { type: "noul", noul: 0.9 } });
  });

  it("names a question the response left unanswered instead of guessing", async () => {
    const fake = fakeFetch(() => ({ json: systemOne({}) }));

    const output = await clientFor(fake).tool().invoke(new RunContext(), args);

    expect(String(output)).toMatch(/^error: .*urgency/);
  });

  it("returns an error string instead of throwing", async () => {
    const fake = fakeFetch(() => ({ status: 400, json: { error: "bad request" } }));

    const output = await clientFor(fake).tool().invoke(new RunContext(), args);

    expect(String(output)).toMatch(/^error: /);
  });

  it("is named jev_decide and carries the confidence caveat", () => {
    const tool_ = clientFor(fakeFetch(() => ({ json: systemOne({}) }))).tool();

    expect(tool_.name).toBe(JEV_DECIDE_TOOL_NAME);
    expect(tool_.description).toContain(CONFIDENCE_NOTE);
  });
});
