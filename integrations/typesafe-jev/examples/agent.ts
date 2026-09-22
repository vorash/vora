/**
 * An OpenAI Agent that delegates narrow classification to Jev.
 *
 *   export OPENAI_API_KEY=sk-...
 *   export TYPESAFE_API_KEY=tsk-...
 *   npm run build && node examples/agent.ts
 */
import { Agent, run } from "@openai/agents";

import { JevDecisionClient } from "../dist/index.js";

const jev = new JevDecisionClient({ model: "jev-1.13.0" });

const agent = new Agent({
  name: "Support triage",
  instructions:
    "Triage the customer message with one jev_decide call carrying three questions: " +
    "a choice `department` over billing/technical/sales, a score `frustration` over an " +
    "ordered rubric you define, and a noul `urgency`. Report the answers and the reported " +
    "confidence, then recommend the next step. Do not classify without the tool.",
  tools: [jev.tool()],
});

const result = await run(
  agent,
  "Hi, I've been trying to connect my Stripe account for 3 days and the integration keeps failing. " +
    "I'm losing sales. Please help ASAP.",
);
console.log(result.finalOutput);
