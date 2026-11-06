# agent-tool-runner — give an LLM a `run_code` tool without executing anything on your host

**The problem.** The single most useful tool to give a coding agent is "run
this code and tell me what happened". It is also the one tool that turns model
output into **arbitrary code execution on your machine**. Sandboxing the LLM
is not enough. The *tool implementation* is the trust boundary.

**The move.** The `run_code` tool executes in a Vora sandbox, never on the
host. The agent loop here is a plain OpenAI-compatible function-calling loop of
about a hundred lines, so you can see exactly where the boundary sits. It works
with any endpoint, for example OpenAI or a local vLLM or Ollama gateway. Each
tool call gets a fresh, budgeted sandbox, and results go back to the model as
text.

For production agent frameworks, Vora ships ready-made adapters:
[`integrations/openai-agents`](../integrations/openai-agents/) and the
[`sdk/mcp`](../sdk/mcp/) server. They do this same wiring inside those
frameworks.

## Run it

```bash
cd examples && npm install
export OPENAI_API_KEY=...            # any OpenAI-compatible endpoint
export OPENAI_BASE_URL=https://api.openai.com/v1   # optional
node agent-tool-runner/tool-server.mjs "What is the 40th Fibonacci number? Compute it."
```

The model calls `run_code`. You see the tool calls and their results as they
happen, then the final answer.

## What to notice

- **The host executes nothing.** The only place `exec` happens is inside the
  sandbox. The loop itself only passes JSON around.
- Each call runs with `network: "none"`, `pids: 32`, `memory: 128Mi`, and a 15s
  timeout. A runaway generated program returns `TimeoutExceeded` or `OOMKilled`
  to the model as a tool result instead of taking your service down. That result
  is a useful signal for the agent.
- Tool results are capped and *text*: the model sees output, never files or
  processes from your host.
- The same pattern works for `browse_url` (restricted egress and an allowlist),
  `run_sql` (sandboxed sqlite), and `use_api` (brokered credentials). Every
  capability gets its own sandbox profile.
