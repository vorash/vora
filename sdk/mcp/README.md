# @vorash/mcp

An [MCP](https://modelcontextprotocol.io) server that lets coding agents and
assistants provision and drive [Vora](../typescript) sandboxes directly.

It wraps the same HTTP API as [`@vorash/sdk`](../typescript) — no separate auth
path, no new privileges. Point it at a `vora-api` and an API key, and the host
gets six tools.

## Tools

| Tool | Purpose |
| --- | --- |
| `sandbox_create` | Create and start a sandbox; returns the `sandbox_id` |
| `sandbox_exec` | Run a command (via `/bin/sh -c`) and return stdout, stderr, exit code |
| `sandbox_read_file` | Read a file (UTF-8, or base64 for binary) |
| `sandbox_write_file` | Write a file, creating parent directories |
| `sandbox_stats` | Read memory, CPU, PID and OOM usage |
| `sandbox_destroy` | Destroy the sandbox |

Every tool takes a `sandbox_id` except `sandbox_create`, which returns one.

## Setup

Build the packages once from the repository root:

```sh
npm install
npm run build          # builds @vorash/sdk, then @vorash/mcp
```

Then register the server with your host, pointing at `sdk/mcp/dist/index.js`.

The server reads two environment variables:

| Variable | Meaning |
| --- | --- |
| `VORA_API_URL` | Base URL of `vora-api`, e.g. `http://127.0.0.1:9300` (defaults to that) |
| `VORA_API_KEY` | Bearer API key. Required — `vora-api` authenticates every request |

### Claude Code

```sh
claude mcp add vora --scope user \
  --env VORA_API_URL=http://127.0.0.1:9300 \
  --env VORA_API_KEY=vora_... \
  -- node /absolute/path/to/vora/sdk/mcp/dist/index.js
```

### Claude Desktop

Add to `claude_desktop_config.json` (`~/Library/Application Support/Claude/` on
macOS, `~/.config/Claude/` on Linux):

```json
{
  "mcpServers": {
    "vora": {
      "command": "node",
      "args": ["/absolute/path/to/vora/sdk/mcp/dist/index.js"],
      "env": {
        "VORA_API_URL": "http://127.0.0.1:9300",
        "VORA_API_KEY": "vora_..."
      }
    }
  }
}
```

Restart the host afterwards. Any host that speaks stdio MCP takes the same
`command`/`args`/`env` triple.

## Example session

```text
sandbox_create { "image": "vora/python", "memory": "512Mi", "network": "none" }
  -> Created sandbox sbx_01M2NVHE1KRD8JT3D6QG6R0EW3 (RUNNING).

sandbox_write_file { "sandbox_id": "sbx_01…", "path": "/workspace/main.py", "content": "print('hi')" }
  -> Wrote 11 bytes to /workspace/main.py.

sandbox_exec { "sandbox_id": "sbx_01…", "command": "python /workspace/main.py" }
  -> exit 0
     stdout:
     hi

sandbox_destroy { "sandbox_id": "sbx_01…" }
  -> Destroyed sbx_01M2NVHE1KRD8JT3D6QG6R0EW3.
```

## Notes

- **Failures are returned, not thrown.** An API error comes back as a tool
  result with `isError: true` and the message, so the agent can react to it.
- **stdout is the protocol.** The server writes JSON-RPC to stdout and any
  diagnostics to stderr; never log to stdout in this package.
- **`sandbox_exec` runs through a shell.** The command string is passed to
  `/bin/sh -c`, so quoting and pipes work, and the sandbox's own isolation is
  what constrains it.
- **Publishing is pending.** The package is not on npm yet, so hosts point at
  the built path. Once `@vorash/sdk` and `@vorash/mcp` are published, the same
  config works with `npx -y @vorash/mcp`.

## Development

```sh
npm install            # from the repository root
npm run build          # @vorash/sdk must be built before @vorash/mcp
npm test --workspace @vorash/mcp
```

The tests drive a real MCP client over an in-memory transport against a
stand-in `vora-api`, so tool schemas, argument encoding and error mapping are
all exercised without a live control plane.