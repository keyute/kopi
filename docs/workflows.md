# Workflows

A workflow is a JavaScript script the main agent writes to start agents and combine their results while it goes on with other work. The script runs in the background; when it ends, the agent is told the value it returned.

## Turning it on

- Type the whole word `ultracode` (any case) in a prompt to the main agent. That makes `Workflow` callable for the rest of the session, and the status line shows `ultracode`. The tool's description tells the agent you typed it to have the task done by a script whose agents check what other agents found.
- It does not count inside an `@` reference, a skill command or a control command such as `/plan ultracode`. `ultracoder` does not match; `Ultracode.` does.
- `/new` and `/resume` start with it off: type the word again.
- The word is highlighted in the input and in sent messages, in the main view only. A prompt typed in a [subagent's view](subagents.md#views) does not turn it on.
- Print mode (`kopi -p`) never offers `Workflow`. A bare `Workflow` rule in `deny` removes it and the highlight.

## The script

- The script is an async function body. Its returned value, as JSON, is the workflow's value; a throw fails the workflow.
- It can call:
  - `agent(prompt, {agentType, schema, label, phase})`: starts an agent that sees only `prompt`; returns a promise of its final message.
  - `phase(title)`: sets the [progress dialog](#progress-dialog) heading for agents started after it.
  - `parallel(thunks)`: calls the functions together; returns a promise of their results in order, `null` for one that threw.
  - `pipeline(items, ...stages)`: passes every item through the stages in order, each called as `stage(previous, item, index)`, the items together; returns a promise of the last stage's results in order, `null` for an item whose stage threw.
- `schema` is a JSON Schema with `"type": "object"`. The agent gets one more tool, `StructuredOutput`, whose parameters are the schema; its last call's arguments are `agent()`'s result. In the agent's view that call reads `Returned result` and opens to the value. `agent()` rejects if the agent ends without calling it, the schema is of another type, or a `deny` rule covers `StructuredOutput`.
- `label` names the agent in approval dialogs, the task's output and the progress dialog; `phase` sets its heading, over `phase()`'s.
- The script has no files, network, timers or imports; it reaches Kopi only through `agent()`.
- A script's memory is limited to 128 MiB and its `agent()` calls to 1000: the 1001st fails the workflow, even if the script catches the rejection.

## Workflow agents

- A workflow agent is not a [subagent](subagents.md): each `agent()` call opens a session, sends it the one prompt, takes the reply and closes it. It has no task or row, cannot be messaged, does not restart and is not among the 20 subagents.
- Without `agentType` it has every tool a subagent can have, the instruction files and the default effort. With one it has that [definition](subagents.md#format)'s effort, tools, `instructions`, `mode` (`plan` keeps it in plan mode) and prompt. An unknown name makes `agent()` reject. No definitions are needed.
- It never gets `Workflow`, `Agent`, `SendMessage`, `AskUserQuestion`, `ExitPlanMode` or `TaskStop`; naming one in a definition's `tools` fails at start as an unknown tool.
- Its `Read` and `Grep` reach the session's [saved output](tools.md#saved-output). Its model is the definition's `model`, else [`workflows.model`](#configuration), else the session's model.

## Configuration

| Key | Meaning |
| --- | --- |
| `workflows.model` | `provider/id` of the model of agents without a definition `model`; default: the session's model. Checked at start as [any model key](configuration.md#model) is, its model and credential only in the fullscreen interface with no rule denying `Workflow`; the first-run login rule applies |
| `workflows.maxWorkflows` | Workflows running at once, default 4, at least 1; a further `Workflow` call is refused with an error naming the limit |
| `workflows.maxAgents` | Agents of one workflow running at once, default 16, at least 1; the rest wait |

The 1000-call and memory limits are fixed.

## Permissions and approvals

- A workflow agent's calls follow the main agent's [permission rules](permissions.md#rules), workspace confinement, [sandbox](sandbox.md) and mode, as a subagent's do.
- `Workflow` runs without asking unless a rule asks or denies it.
- An approval dialog names the agent: `workflow #2 auth t3 › Bash npm test` (agent number, label, task id); requests queue.
- `No` without a reason stops that agent only: its `agent()` rejects and the script goes on. Stopping the workflow ends its pending requests.
- `Yes, allow all edits this session` covers workflow agents too.

## What you see

- The call shows as `• Workflow › <description>` with `↳ launched`, returns a task id such as `t3`, and opens to the script.
- While the main agent is idle and a workflow runs, the row above the input reads `1 workflow still running`; the `workflows` status segment shows the count.
- The end shows as `• workflow finished · <description> · 6m 3s` and `↳ 9 agents · 1 failed` ([Transcript](usage.md#transcript)), opening to the returned value.
- `/tasks` lists it as `workflow › <description>`, stops it with `x` (`stop-agent`) and opens its progress dialog with Enter; dismissing returns to the list.
- When it completes or fails, the agent is told the value or error and a new turn starts if none runs, as for a subagent. `TaskStop` by the agent gives no notice; a stop from `/tasks` is reported with the next prompt, without a new turn.
- The agents' cost shows only in the status line's `total`, added as each agent ends.
- `/new`, `/resume` and exit stop workflows; `/login` is refused while one runs.

### Footer row

- While a workflow runs, a row shows under the status line (hidden [while a dialog is open](configuration.md#status-line)) with the [agent rows](subagents.md#agent-rows): `○ workflow › <description>`, a progress bar, completed/total, `N failed` when any call ended without completing, the latest running call's phase and elapsed time. Its format is fixed: [`subagentline`](configuration.md#subagent-rows) and `statusline` do not configure it.
- Down from the composer reaches it; Enter opens the progress dialog; `x` (`stop-agent`) stops the workflow.

### Progress dialog

- It opens from the footer row or `/tasks`; there is no `/workflows` command.
- It shows the description, progress and elapsed time, then the agents grouped by phase as `Phase · completed/total`, each as `<agent type> › <label>` with state, elapsed time and context tokens.
- Up and Down select an agent; Enter views it; `x` (`stop-agent`) stops the selected queued or running agent only: its `agent()` rejects, and `parallel()` and `pipeline()` give `null` for it. Esc closes the dialog.
- A prompt that needs an answer (an approval, a question, a plan) covers the dialog, which returns afterwards.
- It stays available after the workflow ends, until `/new`, `/resume` or exit.

### Viewing an agent

- The view is watch-only: nothing can be typed, sent or queued, and Esc returns to the dialog.
- A running agent's view follows it live; an ended agent's is read from its saved transcript. An agent that never started has none: Enter shows why.

## Limitations

- Agents share the working directory with nothing isolating concurrent writers.
- Results are checked only against the schema.
- The value a script returns reaches the model as is, with no length limit.
- Agent transcripts are saved under the session's directory, not listed in `/resume`. A workflow's task and output live only in memory and do not survive a restart.
- Stopping a workflow waits for its agents' current requests and tool calls to stop, and counts agents still queued or running as failed in its end row.

## Not implemented

- Resuming a workflow, or a session's running workflows.
- Saved workflows.
- Pausing a workflow.
- Isolation between agents, such as separate working directories.
- Budgets.
- A model or effort per call.
