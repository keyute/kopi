# MCP servers

## Configuration

`mcpServers` in `config.json` declares MCP servers; its value has the shape of the `mcpServers` object of a `.mcp.json`:

```json
{
  "version": 1,
  "mcpServers": {
    "context7": { "command": "npx", "args": ["-y", "@upstash/context7-mcp"], "env": { "CONTEXT7_API_KEY": "${CONTEXT7_API_KEY}" } },
    "github": { "type": "http", "url": "https://api.githubcopilot.com/mcp/", "headers": { "Authorization": "Bearer ${GITHUB_TOKEN}" } }
  }
}
```

- A stdio server has `command`, optional `args` and `env` (added to Kopi's environment), and `type` absent or `"stdio"`.
- A remote server has `"type": "http"` (streamable HTTP), `url` and optional `headers`.
- `${VAR}` and `${VAR:-default}` expand in `command`, `args`, `env` values, `url` and `headers` values. An unset variable with no default fails that server only.

## `.mcp.json`

- A `.mcp.json` in the start directory adds its `mcpServers`, only once you [trust the folder](trust.md#folder-trust).
- A server there replaces the `config.json` server of the same name whole.
- Its unknown keys are ignored, and a server of an unsupported type (`sse`, `ws`) fails alone.

## Startup

- Servers start at startup, and each has 30 seconds to connect.
- The fullscreen interface does not wait for them: the session opens at once, with "Connecting to MCP servers…" above the input until the last server has connected or failed.
  - A server joins the open session when it connects: its tools can be loaded from the agent's next request on, in a turn that is already running too.
  - A prompt sent while servers are still connecting waits for them until 2 seconds after Kopi started, then runs with the servers that have connected.
  - A subagent or workflow agent has the servers that had connected when it started.
- Print mode waits for the servers, at most 10 seconds, before it runs; a server that has not connected by then is left out.
- A server that fails or runs out of time is reported and its tools are absent for the run. The fullscreen interface shows the reason in the transcript when it happens, print mode on stderr.
- Server instructions are added to the system prompt, at most 2 KB each.
- Results are text, capped like other tool output, with the whole of a longer result [saved to a file](tools.md#saved-output); images and other content show as placeholders.

## Tool names

- A server's tools are named `mcp__<server>__<tool>`, with characters outside `A-Z`, `a-z`, `0-9`, `_` and `-` turned into `_`.
- A call's row and its approval read `<server>/<tool>` with the names spelled as in the tool's name, whether or not the server is connected.
- The normalized server component must be nonempty, must not contain `__`, and must not end in `_`, so a whole-server permission prefix cannot match another server.
- Invalid server names fail before connecting.
- A name over 64 characters, or one that two tools share, is skipped and reported.

## Plan mode and rules

- A tool the server marks read-only (`readOnlyHint`) works in plan mode; any other works only in execute mode.
- Read-only or not, an MCP call asks by default, see [permissions](permissions.md#precedence). Print mode denies it without an allow rule, unless the [classifier](permissions.md#classifier) allows it.
- A rule names one tool, `mcp__context7__resolve-library-id`, or ends in `*` to cover a prefix: `mcp__context7__*` for a whole server, `mcp__*` for every server.
- A prefix must be `mcp__` or run past the server name's `__`, so one that ends inside a server name, such as `mcp__context*`, is rejected at start.
- MCP rules take no path, and are accepted for servers that are not configured here.

## ToolSearch

- The system prompt lists each connected server's tool names; a server that connects later is added to the list.
- `ToolSearch` is there whenever a server is configured. A search that finds nothing names the servers still connecting.
- A tool's schema reaches the model only after the model loads it with `ToolSearch`, by keywords or by exact names, at most 20 per keyword search.
- `ToolSearch` works in both modes and, with no rule matching, never asks.
- Each new or resumed session starts with no MCP tool loaded.

## `kopi mcp list`

- Starts the servers this directory uses, lists each with where it is declared, its state and its tools (`read-only` or `execute`), and exits.
- It never asks: undecided and untrusted folders' `.mcp.json` servers show as not trusted and are not started.

## Limitations

- MCP servers run outside the [sandbox](sandbox.md), with your permissions and Kopi's environment, including provider API keys.
- An MCP tool an allow rule covers can change `.mcp.json` and `.git` files without asking, as MCP rules take no path.

## Not implemented

- OAuth for remote servers.
- The SSE transport.
- Resources and prompts.
- `tools/list_changed`.
- Image results.
- Sandboxing servers or giving them a restricted environment.
