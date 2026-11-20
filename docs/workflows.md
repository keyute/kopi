# Workflows

A workflow is a JavaScript script the main agent writes to start agents and combine their results while it goes on with other work. The script runs in the background; when it ends, the agent is told the value it returned.

## Turning it on

- Type the whole word `ultracode` (any case) in a prompt to the main agent in the fullscreen interface. That prompt makes the `Workflow` tool callable, and it stays callable for the rest of the session.
- It does not count inside an `@` reference, a skill command or a control command such as `/plan ultracode`. `ultracoder` and `myultracode` do not match; `Ultracode.` does.
- A session started with `/new` or resumed with `/resume` starts with it off: type the word again.
- The word is highlighted in the input as a skill name is, and in sent messages, in the main view only. A prompt typed in a [subagent's view](subagents.md#views) does not turn it on.
- Print mode (`kopi -p`) never offers `Workflow`, and a bare `Workflow` rule in `deny` removes it, and the highlight with it.

## The script

- The script is the body of an async function: `await` and `return` work. What it returns, as JSON, is the workflow's value; a throw fails the workflow.
- It can call:
  - `agent(prompt, {agentType, schema, label})`, which starts an agent that sees only `prompt` and returns a promise of its final message.
  - `parallel(thunks)`, which calls the functions together and returns a promise of their results in order, `null` for one that threw.
  - `pipeline(items, ...stages)`, which passes every item through the stages in order, each called as `stage(previous, item, index)`, the items together; it returns a promise of the last stage's results in order, `null` for an item whose stage threw.
- `schema` is a JSON Schema with `"type": "object"`. The agent gets one more tool, `StructuredOutput`, whose parameters are the schema; the arguments of its last call are the result of `agent()`. An agent that ends without calling it, a schema of another type, or a `deny` rule for `StructuredOutput`, makes `agent()` reject.
- `label` names the agent in approval dialogs and in the task's output.
- The script has no files, network, timers or imports; it reaches Kopi only through `agent()`.
- A script's memory is limited to 128 MiB, and its `agent()` calls to 1000: the 1001st fails the workflow, even when the script catches the rejection.

## Workflow agents

- A workflow agent is not a [subagent](subagents.md). Each `agent()` call opens a session, sends it the one prompt, takes the reply and closes the session. It has no task of its own, cannot be sent a message, does not restart, ends with no notice and no row, and does not count towards the 20 running subagents.
- Without `agentType` the agent has every tool a subagent can have, the instruction files and the default effort. With one, it has that [definition](subagents.md#format)'s effort, tools, `instructions` setting, `mode` and prompt. An unknown name makes `agent()` reject. Workflow agents work with no definitions.
- An agent never gets `Workflow`, `Agent`, `SendMessage`, `AskUserQuestion`, `ExitPlanMode`, `TaskOutput` or `TaskStop`.
- Its `Read` and `Grep` reach the session's [saved output](tools.md#saved-output), as a subagent's do.
- Its model is the definition's `model`, else [`workflows.model`](configuration.md#workflows), else the session's model.
- Its preamble says it is a workflow agent in place of the subagent sentence.

## Configuration

| Key | Meaning |
| --- | --- |
| `workflows.model` | `provider/id` of the model of workflow agents without a definition `model`; default: the session's model. Checked at start like `smallModel` |
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

- The call shows as `• Workflow › <description>` with `↳ launched`, and returns a task id such as `t3`.
- While the main agent is idle and a workflow runs, the row above the input reads `Waiting on 1 workflow… <elapsed>`; the status line's `agents` segment does not count it.
- The end shows as `• workflow finished · <description> · 6m 3s` (`failed` or `stopped` in the error colour).
- `/tasks` lists the workflow next to other tasks as `workflow › <description>`, shows its output, one line when each agent starts and ends such as `#3 explorer › auth: completed · 41s`, and stops it with `x` (`stop-agent`) or from its dialog.
- When the workflow completes or fails, the agent is told with the value or the error, and a new turn starts if none runs, as for a subagent. `TaskStop` by the agent gives no notice; a stop from `/tasks` is reported with the next prompt without starting a turn.
- The agents' cost shows only in the status line's `total`, added as each agent ends.
- `/new`, `/resume` and exit stop running workflows and their agents; `/login` is refused while one runs.

## Limitations

- Agents share the working directory with nothing isolating concurrent writers.
- A result is not checked beyond its schema.
- The value a script returns reaches the model as it is, with no length limit.
- Each agent's transcript is saved under the session's directory and does not appear in `/resume`; it cannot be opened in a view.
- A workflow's task and output live only in memory and do not survive a restart.
- Stopping a workflow waits for its agents' current model requests and tool calls to stop.

## Not implemented

- A footer row and a progress view.
- Print mode (`kopi -p`).
- Resuming a workflow, or a session's running workflows.
- Saved workflows.
- Pausing a workflow.
- Isolation between agents, such as separate working directories.
- Nesting: an agent cannot start a workflow.
- Budgets.
- A cost per workflow.
- A model or effort per call.
