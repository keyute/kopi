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
- `SendMessage` sends a further message to a subagent by task id. A running subagent reads it when its current step ends; one that has ended, or whose run is being stopped, starts again with its earlier context and reports again. The call does not wait for the stopping run to end.
- The notice the agent receives when a subagent's run completes or fails, or when you stop it, lists the messages queued for it that it never read as undelivered. When the agent stops a subagent with `TaskStop` it receives no notice, and those messages are dropped.
- `TaskOutput` shows a subagent's activity so far and `TaskStop` stops it. `/tasks` lists subagents next to background commands, opens a subagent's view with Enter and stops the selected running subagent with `x` (`stop-agent`) from the list.

## What a subagent gets

- Its prompt is Kopi's own text, then the definition's body, then the instruction files (unless `instructions: false`), then what its MCP servers report.
- Its tools are the session's tools, or the `tools` list, but never `Agent`, `SendMessage`, `Workflow`, `AskUserQuestion`, `ExitPlanMode`, `TaskOutput` or `TaskStop`, and it cannot reach the session's [plan file](tools.md#exitplanmode).
- Its `Bash` runs in the foreground only, and a command still running at its timeout is stopped.

## Permissions

- A subagent's calls go through the same [permission rules](permissions.md#rules), workspace confinement and [sandbox](sandbox.md) as the main agent's, including `Bash` calls that set `unsandboxed` [outside the sandbox](sandbox.md#outside-the-sandbox).
- When one needs approval, the dialog names the subagent and its task (`explorer t3 › …`). Several requests queue and are shown one at a time.
- `No` without a reason stops that subagent only; Esc on its dialog answers that way.
- `Yes, allow all edits this session` covers the main agent and its subagents.
- `kopi -p` cannot ask, so such a call is denied, as it is for the main agent.

## What you see

- In the transcript a call shows as `• explorer › <description>` with `↳ launched` (the row does not open), and its end as `• explorer finished · <description> · 2m 14s` (`failed` or `stopped` in the error colour).
- While the main agent is idle and work is running, the row above the input reads `Waiting on 2 agents, 1 shell… <elapsed>`. The status line's [`agents` and `shells`](configuration.md#segments) segments show the same counts.
- `/new`, `/resume` and exit stop them.
- `kopi -p` waits for running subagents and prints the reply that follows their results.
- Subagent transcripts are saved under the session's own directory and do not appear in `/resume`.

### Agent rows

- While a subagent runs, rows show under the status line, two columns in: `main` first, then one row per running subagent in start order, such as `○ explorer › Find the config loader · 22.1k tokens · gpt-5.6`. The segments are set by [`agentline`](configuration.md#agent-rows).
- `●` marks the row of the session whose view is open, `○` the others.
- A subagent that has ended leaves the rows, unless its view is the one open or holds input handed back when it stopped; then `main` and that row stay until you switch away, or for held input until you open it. With no running subagent and the main view open there are no rows.
- At most five rows show; with more, `↑ N more` and `↓ N more` lines mark what is hidden. On a narrow terminal a row drops entries from the right end, then is cut.
  - The window of five scrolls only when the cursor, or with no cursor the open view's row, would leave it, so rows keep their place as the cursor moves.
- Down from the last line of the input, with no newer history entry, moves into the rows, starting at `main`; `❭` marks the cursor row, in the two columns left of `●`/`○`, and the row is drawn in the accent colour.
  - Up and Down (`menu-up`, `menu-down`) move; Enter (`submit`) opens that session's view and leaves the cursor on that row.
  - `x` (`stop-agent`) on a running subagent's row stops it; on the `main` row and on the open view's row it types an `x`.
  - Esc, Up on `main` or typing returns to the input; typing also types. Esc there interrupts nothing.
  - A dialog (approval, question, login, `/tasks`) takes keys first.

### Views

- Opening a row replaces the whole transcript with that session's. A subagent's view shows the prompt it was given, its messages and its tool calls as they happen, with its own scroll position and folds.
- To go back, open the `main` row. A view stays open after its subagent ends. `/new` and `/resume` drop subagent views and return to main.
- Input goes to the viewed subagent. Enter sends it: a running subagent reads it when its current step ends, and it shows as queued until then. An ended subagent starts again with its earlier context and reports to the main agent again when it ends; the main agent is woken by that end as by any subagent's end, and does not see what you typed.
- Ctrl+Enter (`send-now`) stops the subagent's current step and continues with what is queued; Up with queued input (`queue-edit`) takes it back for editing. Images and a named skill go with the message.
- Slash commands always act on the main session. Ctrl+B (`background`) moves nothing to the background in a subagent's view.
- Esc and Ctrl+C act on the session whose view is open, never on another: in a subagent's view they stop that subagent's run only. The main agent is told at its next request that you stopped the subagent, and is not woken for it.
- Input still queued for a stopped session returns to the input box; if its view is not open, when the view is next opened. A stopped subagent continues when you type in its view or the main agent uses `SendMessage`.
- The status line keeps its layout; `model`, `effort`, `context`, `tokens`, `tools` and `cost` show the viewed subagent's own. The row above the input shows the viewed session's activity; `Waiting on 2 agents, 1 shell…` shows only in the main view.
- Approvals are unchanged: a subagent's request shows as a dialog naming the subagent and its task whatever view is open.

## Workflows

A [workflow](workflows.md)'s agents use these definitions by name, but are not subagents: they have no task, row or notice of their own and are not among the 20.

## Limitations

- A subagent cannot start subagents.
- At most 20 subagents run at once.
- A subagent cannot run commands in the background.
- A subagent's turns and time are not limited.
- An MCP tool named in `tools` is checked only for its `mcp__` prefix at startup; one that is not available is left out without an error.
- Stopping a subagent (`TaskStop`, `/tasks`) and switching session wait for its current model request or tool call to stop.
- `/login` is refused while a subagent or a [workflow](workflows.md) is running, as it is while a turn runs.
- After Esc in the main view, a subagent that finishes does not wake the agent until your next prompt.
- Subagents are not restored when a session is resumed, and their earlier task ids cannot be messaged.
- Definitions load only from Kopi's home, not from a project.
- A view cannot be opened by clicking a row.
- The result of a slash command typed in a subagent's view shows in the main view.
