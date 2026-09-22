/**
 * An OpenAI Agent that runs its code inside a Vora sandbox.
 *
 *   export OPENAI_API_KEY=sk-...
 *   export VORA_API_URL=http://127.0.0.1:9300
 *   export VORA_API_KEY=vora_...
 *   npm run build && node examples/agent.ts
 */
import { Agent, run } from "@openai/agents";

import { VoraSandboxClient } from "../dist/index.js";

const sandbox = new VoraSandboxClient({ image: "vora/python", memory: "512Mi", network: "none" });

const agent = new Agent({
  name: "Sandboxed analyst",
  instructions:
    "You compute with the run_code tool, which executes Python inside a disposable Vora sandbox. " +
    "Always call the tool instead of guessing, then report the numbers it printed.",
  tools: [sandbox.tool()],
});

try {
  const result = await run(
    agent,
    "Write a Python program that prints the first 10 Fibonacci numbers, one per line, then summarise the sequence.",
  );
  console.log(result.finalOutput);
  console.log(`sandbox used: ${sandbox.sandbox?.id ?? "none"}`);
} finally {
  await sandbox.dispose();
}