# @vorash/openai-agents

Run [OpenAI Agents](https://openai.github.io/openai-agents-js/) code inside
[Vora](https://github.com/vorash/vora) sandboxes.

This is an **integration**, not part of the Vora core: it depends on
[`@vorash/sdk`](../typescript) and the Agents SDK, and nothing in Vora depends
on it.

## What it gives an agent

One tool, `run_code`:

```text
run_code { code: "print(sum(range(10)))", language: "python" }
  -> exit 0

     stdout:
     45
```

It executes the snippet inside a disposable Linux sandbox and returns stdout,
stderr and the exit code. The sandbox is created on first use, reused across
calls (so files and installed packages persist between tool calls), and
destroyed on `dispose()`.

## Install

```sh
npm install @vorash/openai-agents @openai/agents zod
```

`@openai/agents` and `zod` are peer dependencies — your agent already has them.

## Use

```ts
import { Agent, run } from "@openai/agents";
import { VoraSandboxClient } from "@vorash/openai-agents";

const sandbox = new VoraSandboxClient({ image: "vora/python", network: "none" });

const agent = new Agent({
  name: "Analyst",
  instructions: "Compute with run_code; do not guess.",
  tools: [sandbox.tool()],
});

try {
  const result = await run(agent, "Print the first 10 Fibonacci numbers.");
  console.log(result.finalOutput);
} finally {
  await sandbox.dispose();
}
```

The sandbox is provisioned lazily, so constructing the client is cheap. A
complete runnable version is in [`examples/agent.ts`](examples/agent.ts).

## Configuration

`VoraSandboxClient` takes a pre-built `Vora` client or the create options:

| Option | Default | Meaning |
| --- | --- | --- |
| `client` | `new Vora()` | Reuse an existing client; otherwise `VORA_API_URL` / `VORA_API_KEY` are read |
| `image` | `vora/python` | Rootfs image; the default matches the default language |
| `memory`, `cpu`, `pids`, `disk` | — | Resource limits (`"512Mi"`, `1`, `"500m"`) |
| `network` | — | `"none"` for default-deny egress |
| `env`, `ttl`, `backend` | — | Passed through to `sandbox create` |
| `workdir` | `/workspace` | Working directory for `run_code` |
| `sandboxId` | — | Run against an existing sandbox instead of creating one |

### Languages

`run_code` accepts `language: "python" | "javascript" | "shell"` (default
`python`) and runs it with `python3`, `node` or `sh`, feeding the snippet on
stdin. **The image must contain the interpreter**: use `vora/python` for
Python, `vora/node` for JavaScript, `vora/base` for shell.

### Lifecycle

- `start()` provisions the sandbox once; concurrent calls share one creation.
- `dispose()` destroys a sandbox the client created and is safe to call twice.
- A sandbox passed via `sandboxId` is **adopted**: `dispose()` leaves it
  running, because the client does not own it.

### Errors

`run_code` returns a string beginning with `error:` when the call fails, rather
than throwing, so the model can read the message and adapt. Programmatic
callers using `runCode()` get the exception (`VoraError` from the SDK).

## Development

```sh
npm install            # from the repository root
npm run build          # @vorash/sdk must be built before this package
npm test --workspace @vorash/openai-agents
node --experimental-strip-types examples/agent.ts   # needs OPENAI_API_KEY
```

The tests drive the tool through the Agents SDK's own `FunctionTool.invoke`
against a stand-in `vora-api`, so the model-facing path (schema, invocation,
error text) is covered without calling OpenAI.