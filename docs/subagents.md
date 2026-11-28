# Subagents

A subagent is another agent the main agent starts for one task. It has its own context, instructions, model and tools and sees only the prompt it is given. The main agent can message it while it works, unless a `deny` rule removes `SendMessage`. Only its final message returns, as its whole result.

## Format

- A definition is a Markdown file with YAML frontmatter, one subagent per `*.md` file directly in `$KOPI_HOME/agents/` (default `~/.kopi/agents/`). Symlinks are followed. The body is the subagent's system prompt.

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
| `effort` | `off`, `minimal`, `low`, `medium`, `high`, `xhigh` or `max`; default `medium`, or the nearest level the model has |
| `tools` | List of tool names; default: every tool a subagent can have. MCP tools go by their full `mcp__…` name |
| `mode` | `plan` keeps the subagent in plan mode whatever the session's mode; omitted, it follows the session's mode |
| `instructions` | `false` leaves the instruction files out of the subagent's prompt; default `true` |

## Loading

- Definitions are read once at startup.
- Kopi does not start, naming the file, on an unknown field, a missing or multi-line `name` or `description`, an unknown tool or empty `tools` list, a `mode` other than `plan`, a bad `model` or `effort`, or a duplicate name.
- With no definitions there is no `Agent` or `SendMessage` tool; Kopi ships none.

## Use

- The agent starts a subagent with `Agent`: a short description, the prompt and the subagent's name. Every subagent runs in the background; the call returns at once with a task id such as `t3`.
- When a subagent ends, the agent is told with its final message. No tool reads a running subagent; `TaskStop` stops it.
- `SendMessage` sends a further message by task id. A running subagent reads it when its current step ends; an ended or stopping one starts again with its earlier context and reports again.
- A message to a subagent whose end notice is still unread is not delivered; the result says it has ended.
- The end notice lists messages the subagent never read as undelivered; a `TaskStop` by the agent gives no notice and drops them.
- `/tasks` lists subagents, opens a view with Enter and stops the selected one with `x` (`stop-agent`).

## What a subagent gets

- Its prompt is Kopi's own text, the definition's body, the instruction files (unless `instructions: false`), then what its MCP servers report.
- Its tools are the session's, or the `tools` list, but never `Agent`, `SendMessage`, `Workflow`, `AskUserQuestion`, `ExitPlanMode` or `TaskStop`; naming one in `tools` fails at start as an unknown tool.
- It cannot reach the session's [plan file](tools.md#exitplanmode); its `Read` and `Grep` reach the session's [saved output](tools.md#saved-output), its own and others'.
- Directories added with [`/add-dir`](usage.md#commands) or `--add-dir` reach subagents and [workflow](workflows.md) agents, told with their first prompt. After [`/remove-dir`](usage.md#commands) their file tools lose the directory at once; a subagent is told with its next prompt.
- Its `Bash` runs in the foreground only, and a command still running at its timeout is stopped.

## Permissions

- A subagent's calls follow the main agent's [permission rules](permissions.md#rules), workspace confinement and [sandbox](sandbox.md), including `unsandboxed` `Bash` calls [outside the sandbox](sandbox.md#outside-the-sandbox).
- An approval dialog names the subagent and its task (`explorer t3 › …`); requests queue.
- `No` without a reason stops that subagent only; Esc on its dialog answers that way.
- `Yes, allow all edits this session` covers the main agent and its subagents.
- `kopi -p` cannot ask, so such a call is denied, as for the main agent.

## What you see

- A call shows as `• explorer › <description>` with `↳ launched`; its end as `• explorer finished · <description> · 2m 14s` (or `failed`, `stopped`), opening to the final reply. See [Transcript](usage.md#transcript).
- While the main agent is idle and work runs, the row above the input reads `2 agents, 1 shell still running`; the status line's [`agents`, `workflows` and `shells`](configuration.md#segments) segments show the counts.
- `/new`, `/resume` and exit stop subagents. `kopi -p` waits for them and prints the reply that follows their results. Their transcripts are saved under the session's directory, not listed in `/resume`.

### Agent rows

- While a subagent runs, rows show under the status line (hidden [while a dialog is open](configuration.md#status-line)): `main`, then one per running subagent, such as `○ explorer › Find the config loader · 22.1k tokens · gpt-5.6`. [`subagentline`](configuration.md#subagent-rows) sets the entries.
- At most five rows show. An ended subagent's row stays only while its view is open or it holds input handed back.
- Down from the last input line enters the rows at `main`.
  - `menu-up` and `menu-down` move; `submit` opens that session's view.
  - `x` (`stop-agent`) on a running subagent's row stops it.
  - Esc, Up on `main` or typing returns to the input.

### Views

- Opening a row replaces the transcript with that session's; open `main` to go back. `/new` and `/resume` drop subagent views.
- Input goes to the viewed subagent: a running one reads it when its current step ends; an ended one starts again with its earlier context, and its next end wakes the main agent, which does not see what you typed.
- Ctrl+Enter (`send-now`) stops the current step and continues with what is queued; Up (`queue-edit`) takes queued input back for editing.
- Slash commands act on the main session. Ctrl+B (`background`) moves nothing to the background in a subagent's view.
- Esc and Ctrl+C stop the viewed subagent's run; with a dialog open they dismiss it instead. The main agent is told at its next request and is not woken. Input still queued for a stopped session returns to the input box, or when its view is next opened.
- A stopped subagent continues on your input in its view or a `SendMessage`.
- The status line shows the viewed subagent's own `model`, `effort`, `context`, `tokens`, `tools` and `cost`; `total` stays the whole session's.

## Workflows

A [workflow](workflows.md)'s agents use these definitions by name but are not subagents: no task, row or notice, and not among the 20. Their sessions can be [viewed](workflows.md#viewing-an-agent) watch-only.

## Limitations

- A subagent cannot start subagents or run commands in the background.
- At most 20 subagents run at once.
- A subagent's turns and time are unlimited.
- An MCP tool named in `tools` is checked only for its `mcp__` prefix at startup; an unavailable one is left out without an error.
- Stopping a subagent and switching session wait for its current request or tool call to stop.
- `/login` is refused while a subagent or [workflow](workflows.md) runs.
- After Esc in the main view, a finishing subagent does not wake the agent until your next prompt.
- Subagents are not restored on resume, and their earlier task ids cannot be messaged.
- Definitions load only from Kopi's home, not from a project.
- A view cannot be opened by clicking a row.
- The result of a slash command typed in a subagent's view shows in the main view.
