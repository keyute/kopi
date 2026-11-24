# Workflows

A workflow is a JavaScript script the main agent writes to start agents and combine their results while it goes on with other work. The script runs in the background; when it ends, the agent is told the value it returned.

## Turning it on

- Type the whole word `ultracode` (any case) in a prompt to the main agent in the fullscreen interface. That prompt makes the `Workflow` tool callable, and it stays callable for the rest of the session; the status line shows `ultracode` while it is.
- It does not count inside an `@` reference, a skill command or a control command such as `/plan ultracode`. `ultracoder` and `myultracode` do not match; `Ultracode.` does.
- A session started with `/new` or resumed with `/resume` starts with it off: type the word again.
- The word is highlighted in the input as a skill name is, and in sent messages, in the main view only. A prompt typed in a [subagent's view](subagents.md#views) does not turn it on.
- Print mode (`kopi -p`) never offers `Workflow`, and a bare `Workflow` rule in `deny` removes it, and the highlight with it.

## The script

- The script is the body of an async function: `await` and `return` work. What it returns, as JSON, is the workflow's value; a throw fails the workflow.
- It can call:
  - `agent(prompt, {agentType, schema, label, phase})`, which starts an agent that sees only `prompt` and returns a promise of its final message.
  - `phase(title)`, which sets the heading the [progress dialog](#progress-dialog) shows the agents that `agent()` starts after it under.
  - `parallel(thunks)`, which calls the functions together and returns a promise of their results in order, `null` for one that threw.
  - `pipeline(items, ...stages)`, which passes every item through the stages in order, each called as `stage(previous, item, index)`, the items together; it returns a promise of the last stage's results in order, `null` for an item whose stage threw.
- `schema` is a JSON Schema with `"type": "object"`. The agent gets one more tool, `StructuredOutput`, whose parameters are the schema; the arguments of its last call are the result of `agent()`. In the agent's view that call's row reads `Returned result` and opens to the value. An agent that ends without calling it, a schema of another type, or a `deny` rule for `StructuredOutput`, makes `agent()` reject.
- `label` names the agent in approval dialogs, in the task's output and in the progress dialog. `phase` sets the heading for that one agent, over the one set by `phase()`.
- The script has no files, network, timers or imports; it reaches Kopi only through `agent()`.
- A script's memory is limited to 128 MiB, and its `agent()` calls to 1000: the 1001st fails the workflow, even when the script catches the rejection.

## Workflow agents

- A workflow agent is not a [subagent](subagents.md). Each `agent()` call opens a session, sends it the one prompt, takes the reply and closes the session. It has no task of its own, no row in the main transcript and no agent row, cannot be sent a message, does not restart, and does not count towards the 20 running subagents.
- Without `agentType` the agent has every tool a subagent can have, the instruction files and the default effort. With one, it has that [definition](subagents.md#format)'s effort, tools, `instructions` setting, `mode` and prompt. An unknown name makes `agent()` reject. Workflow agents work with no definitions.
- An agent never gets `Workflow`, `Agent`, `SendMessage`, `AskUserQuestion`, `ExitPlanMode` or `TaskStop`.
- Its `Read` and `Grep` reach the session's [saved output](tools.md#saved-output), as a subagent's do.
- Its model is the definition's `model`, else [`workflows.model`](configuration.md#workflows), else the session's model.
- Its preamble says it is a workflow agent in place of the subagent sentence.

## Configuration

| Key | Meaning |
| --- | --- |
| `workflows.model` | `provider/id` of the model of workflow agents without a definition `model`; default: the session's model. A value that is not `provider/id` fails at start in every mode; its model and credential are checked only in the fullscreen interface with no rule denying `Workflow` |
| `workflows.maxWorkflows` | How many workflows run at once, default 4, at least 1; a further `Workflow` call is refused with an error naming the limit |
| `workflows.maxAgents` | How many agents of one workflow run at once, default 16, at least 1; the rest wait |

The 1000 calls and the memory limit are fixed.

## Permissions and approvals

- A workflow agent's calls go through the same [permission rules](permissions.md#rules), workspace confinement, [sandbox](sandbox.md) and mode as the main agent's, and the session's mode applies to it as it does to subagents. A `mode: plan` definition keeps its agents in plan mode.
- `Workflow` itself only reads and runs without asking; a rule can ask or deny it.
- When an agent's call needs approval, the dialog names the agent: `workflow #2 auth t3 › Bash npm test` (the agent's number in the script, its label and the workflow's task id). Several requests queue and are shown one at a time.
- `No` without a reason stops that agent only: its `agent()` rejects and the script goes on. Stopping the workflow ends its pending requests.
- `Yes, allow all edits this session` covers workflow agents as it covers subagents.

## What you see

- The call shows as `• Workflow › <description>` with `↳ launched`, and returns a task id such as `t3`. Opening the row shows the script. A `TaskStop` row on it reads `Stopped workflow › <description>`.
- While the main agent is idle and a workflow runs, the row above the input reads `1 workflow still running`; the status line's `workflows` segment shows the count.
- The end shows as `• workflow finished · <description> · 6m 3s` (`failed` or `stopped` in place of `finished`; the bullet is red when it failed and yellow when it was stopped), followed by `↳ 9 agents · 1 failed` (the failed part only when non-zero). A failed workflow's reason joins the count on that line: `↳ 2 agents · 2 failed · <reason>`. Opening a finished row shows the value the script returned.
- `/tasks` lists the workflow next to other tasks as `workflow › <description>`, and stops it with `x` (`stop-agent`). Enter opens its progress dialog; dismissing it returns to the task list.
- When the workflow completes or fails, the agent is told with the value or the error, and a new turn starts if none runs, as for a subagent. `TaskStop` by the agent gives no notice; a stop from `/tasks` is reported with the next prompt without starting a turn.
- The agents' cost shows only in the status line's `total`, added as each agent ends.
- `/new`, `/resume` and exit stop running workflows and their agents; `/login` is refused while one runs.

### Footer row

- While a workflow runs, a row shows under the status line with the [agent rows](subagents.md#agent-rows): `○ workflow › <description>`, a 16-cell bar, completed/total, `N failed` when any call ended without completing (stopped ones included), the phase of the latest running call when there is one, and the run's elapsed time.
- Each `agent()` call has an equal share of the bar, coloured by state: completed, failed or stopped, running; queued calls are empty cells.
- When the row is too narrow the description shortens first, then the bar is left out. Its format is fixed: [`subagentline`](configuration.md#subagent-rows) and `statusline` do not configure it.
- Down from the composer reaches it like an agent row; Enter opens the progress dialog; `x` (`stop-agent`) stops the workflow. The row leaves when the workflow ends.

### Progress dialog

- Opened from the footer row or from `/tasks`. There is no `/workflows` command.
- It shows the description, the same bar and counts with the elapsed time (and `failed` or `stopped` for a run that ended that way), then the agents: those with no phase first, then each phase in the order it first appeared, as `Phase · completed/total`.
- Each agent shows as `<agent type> › <label>`, its state by colour, the state word when it is queued, failed or stopped, its elapsed time and its context tokens.
- Up and Down select an agent; Enter views it; `x` (`stop-agent`) stops the selected queued or running agent only: its `agent()` call rejects, and `parallel()` and `pipeline()` give `null` for it. Esc closes the dialog.
- It stays available after the workflow ends, until `/new`, `/resume` or exit, and reopens on the agent last selected.
- A prompt that needs an answer (an approval, a question, a plan) covers it, and it returns afterwards.

### Viewing an agent

- The transcript switches to that agent's session, drawn like any session. The view is watch-only: nothing can be typed, sent or queued to the agent, and Esc returns to the dialog on the same agent.
- The status line shows that agent's model and context while it is viewed.
- A running agent's view follows it live; an ended agent's is read from its saved transcript.
- An agent that never started (still queued, or failed before its session opened) has no transcript: Enter shows why instead.

## Limitations

- Agents share the working directory with nothing isolating concurrent writers.
- A result is not checked beyond its schema.
- The value a script returns reaches the model as it is, with no length limit.
- Each agent's transcript is saved under the session's directory and does not appear in `/resume`.
- The progress dialog's agent list is moved through with Up and Down only; page keys scroll the transcript behind it.
- An ended agent's view, read from its saved transcript, has no closing turn row.
- A workflow's task and output live only in memory and do not survive a restart.
- Every running workflow agent's output is processed for its view while it runs, whether or not it is viewed.
- Opening an ended agent's view reads its whole saved transcript first.
- Stopping a workflow waits for its agents' current model requests and tool calls to stop, and counts the agents still queued or running at that moment as failed in its end row.

## Not implemented

- Print mode (`kopi -p`).
- Resuming a workflow, or a session's running workflows.
- Saved workflows.
- Pausing a workflow.
- Isolation between agents, such as separate working directories.
- Nesting: an agent cannot start a workflow.
- Budgets.
- A cost per workflow.
- A model or effort per call.
