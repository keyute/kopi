# MCP servers

## Configuration

`mcpServers` in `config.json` takes the shape of a `.mcp.json`'s `mcpServers`:

```json
{
  "version": 1,
  "mcpServers": {
    "context7": { "command": "npx", "args": ["-y", "@upstash/context7-mcp"], "env": { "KEY": "${KEY}" } },
    "github": { "type": "http", "url": "https://api.githubcopilot.com/mcp/", "headers": { "Authorization": "Bearer ${TOKEN}" } }
  }
}
```

- A stdio server has `command`, optional `args` and `env` (added to Kopi's environment); `type` is absent or `"stdio"`.
- A remote server has `"type": "http"` (streamable HTTP), `url` and optional `headers`.
- `${VAR}` and `${VAR:-default}` expand in `command`, `args`, `url` and the values of `env` and `headers`. An unset variable with no default fails that server only.

## `.mcp.json`

- The start directory's `.mcp.json` adds its `mcpServers` once you [trust the folder](trust.md#folder-trust); a server there replaces the `config.json` one of the same name whole.
- Its unknown keys are ignored; a server of an unsupported type (`sse`, `ws`) fails alone.

## Startup

- Servers start at startup, each with 30 seconds to connect; a tool call times out after 60 seconds.
- The fullscreen interface does not wait: the session opens at once, with "Connecting to MCP servers…" above the input from 2 seconds in until the last server has settled.
  - A connected server's tools can load from the agent's next request, mid-turn too.
  - The system prompt's server list updates once, when the last server has settled; a resumed session keeps its own until then.
  - A prompt sent while servers connect waits until 2 seconds after start, then runs with those connected.
  - A subagent or workflow agent has the servers connected when it started.
- Print mode waits at most 10 seconds; a server not connected by then is left out.
- A failed or timed-out server is reported (stderr in print mode) and its tools are absent for the run.
- Server instructions join the system prompt, at most 2 KB each.
- Results are text, capped like other tool output; a longer one is [saved to a file](tools.md#saved-output). Images and other content show as placeholders.

## Tool names

- A server's tools are named `mcp__<server>__<tool>`, characters outside `A-Z`, `a-z`, `0-9`, `_` and `-` becoming `_`. Rows and approvals read `<server>/<tool>`.
- A server name that normalizes to nothing, contains `__` or ends in `_` fails before connecting.
- A tool name over 64 characters or shared by two tools is skipped and reported.

## Plan mode and rules

- A tool the server marks read-only (`readOnlyHint`) works in plan mode; any other works only in execute mode.
- Every MCP call asks by default, read-only or not ([permissions](permissions.md#precedence)). Print mode denies it without an allow rule, unless the [classifier](permissions.md#classifier) allows it.
- A rule names one tool, `mcp__context7__resolve-library-id`, or ends in `*` for a prefix: `mcp__context7__*` for a server, `mcp__*` for all.
- A prefix must be `mcp__` or run past the server name's `__`; `mcp__context*` is rejected.
- MCP rules take no path and are accepted for servers not configured.

## ToolSearch

- The system prompt lists each connected server's tool names, updated once when the last server has settled.
- `ToolSearch` is present whenever a server is configured; a search that finds nothing names the servers still connecting.
- A tool's schema reaches the model only after it loads the tool with `ToolSearch`, by keywords or exact names, at most 20 per keyword search.
- It works in both modes and never asks without a matching rule. Each new or resumed session starts with no MCP tool loaded.

## `kopi mcp list`

- Starts the servers this directory uses, lists each with its source, state and tools (`read-only` or `execute`), and exits.
- It never asks: `.mcp.json` servers of undecided or untrusted folders show as not trusted and do not start.

## Limitations

- MCP servers run outside the [sandbox](sandbox.md), with your permissions and Kopi's environment, including provider API keys.
- An MCP tool an allow rule covers can change `.mcp.json` and `.git` files without asking, as MCP rules take no path.

## Not implemented

- OAuth for remote servers.
- The SSE transport.
- Resources and prompts.
- `tools/list_changed`.
- Sandboxing servers or giving them a restricted environment.
