// agent-tool-runner: an OpenAI-compatible agent loop whose `run_code` tool
// executes in a Vora sandbox — model output becomes arbitrary code execution
// somewhere, and that somewhere is never the host.
//
//   OPENAI_API_KEY=... node agent-tool-runner/tool-server.mjs "compute it"
import process from "node:process";

import { Vora } from "@vorash/sdk";

const vora = new Vora();
const baseUrl = (process.env.OPENAI_BASE_URL ?? "https://api.openai.com/v1").replace(/\/+$/, "");
const apiKey = process.env.OPENAI_API_KEY;
const model = process.env.OPENAI_MODEL ?? "gpt-4o-mini";

const TOOL = {
  type: "function",
  function: {
    name: "run_code",
    description: "Run a Python script and return its stdout, stderr and exit code.",
    parameters: {
      type: "object",
      properties: {
        code: { type: "string", description: "Python source to execute." },
      },
      required: ["code"],
    },
  },
};

/** Execute model-authored code in a fresh sandbox. Never on the host. */
async function runCode(code) {
  const sandbox = await vora.sandboxes.create({
    image: "vora/python",
    backend: process.env.VORA_BACKEND ?? "bwrap",
    cpu: 0.5,
    memory: "128Mi",
    pids: 32,
    ttl: "1m",
    network: "none",
  });
  try {
    const result = await sandbox.exec({
      command: "python3",
      args: ["-c", code],
      cwd: "/workspace",
      timeoutSeconds: 15,
    });
    // The model gets text and semantic outcomes (OOMKilled, TimeoutExceeded…)
    // — useful signal for retrying, and nothing else.
    return [
      `exit=${result.exitCode} reason=${result.reason ?? "EXITED"}`,
      `stdout: ${result.stdout.slice(0, 2000)}`,
      result.stderr === "" ? "" : `stderr: ${result.stderr.slice(0, 1000)}`,
    ]
      .filter(Boolean)
      .join("\n");
  } catch (error) {
    // Killed run (timeout / OOM / fork storm): tell the model, keep the
    // partial output, and never pretend the code succeeded.
    const partial = error.body ?? {};
    return `run killed: ${error.message}\nstdout: ${(partial.stdout ?? "").slice(0, 1000)}`;
  } finally {
    await sandbox.destroy();
  }
}

async function chat(messages) {
  const response = await fetch(`${baseUrl}/chat/completions`, {
    method: "POST",
    headers: { "Content-Type": "application/json", Authorization: `Bearer ${apiKey}` },
    body: JSON.stringify({ model, messages, tools: [TOOL] }),
  });
  if (!response.ok) {
    throw new Error(`${response.status} ${await response.text()}`);
  }
  return (await response.json()).choices[0].message;
}

async function agentLoop(task) {
  const messages = [
    { role: "system", content: "You solve problems with the run_code tool. Show the computation, not the answer from memory." },
    { role: "user", content: task },
  ];

  for (let turn = 0; turn < 8; turn += 1) {
    const message = await chat(messages);
    if (message.tool_calls === undefined || message.tool_calls.length === 0) {
      return message.content; // the final answer
    }

    messages.push(message);
    for (const call of message.tool_calls) {
      const { code } = JSON.parse(call.function.arguments);
      console.log(`--- run_code ---\n${code}\n----------------`);
      const result = await runCode(code ?? "");
      console.log(result);
      // Tool output goes back as a message, capped and text-only.
      messages.push({ role: "tool", tool_call_id: call.id, content: result });
    }
  }
  return "gave up after 8 turns";
}

const task = process.argv.slice(2).join(" ") ?? "";
if (task === "") {
  console.error('usage: node tool-server.mjs "your task"');
  process.exit(2);
}
console.log(await agentLoop(task));
