# Subagents

A subagent is another agent that the main agent starts for one task. It has its own context, instructions, model and tools, sees only the prompt it is given, and its final message is returned to the main agent.

## Format

- A definition is a Markdown file with YAML frontmatter in `$KOPI_HOME/agents/` (default `~/.kopi/agents/`): one subagent per `*.md` file directly in that directory. Symlinks are followed.
- The body is the subagent's system prompt.
- `~/.kopi/agent/` (singular) is unrelated: Kopi's runtime keeps sessions and sign-ins there.

```markdown
---
name: explorer
description: Searches the codebase and reports what it finds
effort: low
tools: [Read, Glob, Grep, LS]
mode: plan
---

You search the codebase to answer the question you are given.
Report file paths and line numbers.
```

| Field | Meaning |
| --- | --- |
| `name` | Required, one line |
| `description` | Required, one line; the main agent reads it to choose a subagent |
| `model` | `provider/id`; default: the session's model |
| `effort` | `off`, `minimal`, `low`, `medium`, `high`, `xhigh` or `max`; default: the model's own |
| `tools` | List of tool names; default: every tool a subagent can have. MCP tools go by their full `mcp__…` name |
| `mode` | `plan` keeps the subagent in plan mode whatever mode the session is in; omitted, it follows the session's mode |
| `instructions` | `false` leaves the instruction files out of the subagent's prompt; default `true` |

## Loading

- Definitions are read once at startup, in name order.
- Kopi does not start, with an error that names the file, on:
  - an unknown field
  - a missing or multi-line `name` or `description`
  - an unknown tool, or an empty `tools` list
  - a `mode` other than `plan`
  - a bad `model` or `effort`
  - two files with the same name
- With no definitions there is no `Agent` or `SendMessage` tool: Kopi ships no subagents of its own.

## Use

- The agent starts a subagent with the `Agent` tool: a short description, the prompt and the subagent's name.
- Every subagent runs in the background. The call returns at once with a task id such as `t3`; the agent can go on working or start more subagents.
- When a subagent ends, the agent is told with its final message and continues.
- `SendMessage` sends a further message to a subagent by task id. A running subagent reads it when its current step ends; one that has ended starts again with its earlier context and reports again.
- `TaskOutput` shows a subagent's activity so far and `TaskStop` stops it. `/tasks` lists subagents next to background commands and can stop them.

## What a subagent gets

- Its prompt is Kopi's own text, then the definition's body, then the instruction files (unless `instructions: false`), then what its MCP servers report.
- Its tools are the session's tools, or the `tools` list, but never `Agent`, `SendMessage`, `AskUserQuestion`, `ExitPlanMode`, `TaskOutput` or `TaskStop`, and it cannot reach the session's [plan file](tools.md#exitplanmode).
- Its `Bash` runs in the foreground only, and a command still running at its timeout is stopped.

## Permissions

- A subagent's calls go through the same [permission rules](permissions.md#rules), workspace confinement and [sandbox](sandbox.md) as the main agent's.
- When one needs approval, the dialog names the subagent and its task (`explorer t3 › …`). Several requests queue and are shown one at a time.
- `No` without a reason stops that subagent only; Esc on its dialog answers that way.
- `Yes, allow all edits this session` covers the main agent and its subagents.
- `kopi -p` cannot ask, so such a call is denied, as it is for the main agent.

## What you see

- In the transcript a call shows as `• explorer › <description>` with `↳ launched`, and its end as `• explorer finished · <description> · 2m 14s` (`failed` or `stopped` in the error colour).
- While the main agent is idle and work is running, the row above the input reads `Waiting on 2 agents, 1 shell… <elapsed>`. The status line's [`tasks`](configuration.md#status-line) segment shows the same counts.
- Esc interrupts the main agent only; subagents keep running until they finish or are stopped.
- `/new`, `/resume` and exit stop them.
- `kopi -p` waits for running subagents and prints the reply that follows their results.
- Subagent transcripts are saved under the session's own directory and do not appear in `/resume`.

## Limitations

- A subagent cannot start subagents.
- At most 20 subagents run at once.
- A subagent cannot run commands in the background.
- A subagent's turns and time are not limited.
- An MCP tool named in `tools` is checked only for its `mcp__` prefix at startup; one that is not available is left out without an error.
- Stopping a subagent (`TaskStop`, `/tasks`) and switching session wait for its current model request or tool call to stop.
- `/login` is refused while a subagent is running, as it is while a turn runs.
- After Esc, a subagent that finishes does not wake the agent until your next prompt.
- Subagents are not restored when a session is resumed, and their earlier task ids cannot be messaged.
- Definitions load only from Kopi's home, not from a project.
- A running subagent's transcript cannot be viewed beyond `/tasks`.
