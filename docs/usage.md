# Usage

## Running

```sh
kopi
kopi -c    # continue the latest session in this directory
kopi -r <id>
kopi -p "explain main.go"
kopi --mode execute --allowed-tools "Edit(docs/**)" -p "fix the typos in docs"
git diff | kopi -p "review this"
```

- `--model provider/id` picks the model for one run; see [`model`](configuration.md#model).
- With stdin and stdout both terminals and no `-p`, Kopi opens the fullscreen interface; otherwise it prints the response to one prompt and exits.
- The prompt is the argument, piped stdin, or both (stdin, a blank line, then the argument).
- A prompt is always sent as a prompt, never run as a command; a [skill](skills.md) it names is loaded.
- Print mode prints only the final message of the reply.
- Ctrl+C interrupts the turn. A failed or interrupted turn exits with status 1.
- `-c` and `-r` work with `-p`.
- Resuming (`-c`, `-r` or `/resume`) redraws the earlier conversation in the fullscreen interface, with its tool activity folded. With `-c` and `-r` the session is open at once, while the [MCP servers](mcp.md#startup) connect.
- A session resumed with `-c` or `-r` runs on that run's [session model](configuration.md#model), not the one it last ran on.
- On exit from a session with messages, the fullscreen interface prints the command to resume it.
- `--add-dir <path>` adds a directory to the [workspace](tools.md#file-tools) as [`/add-dir`](#commands) does. It is repeatable, and a path that cannot be added stops the start. In print mode a folder no [trust](trust.md#added-directories) decision covers is added with file access only, and nothing is saved.
- `--mode` and `--allowed-tools` are described in [tools](tools.md#modes) and [permissions](permissions.md#rules). `kopi mcp list` is in [MCP](mcp.md#kopi-mcp-list).

## Commands

Commands in the fullscreen interface:

| Command | Effect |
| --- | --- |
| `/plan` | Switch to plan mode |
| `/plan open` | Open the session's [plan file](tools.md#exitplanmode) in `$VISUAL`, else `$EDITOR` |
| `/execute` | Switch to execute mode |
| `/effort <level>` | Set the main agent's reasoning effort for this session; never saved to `config.json`. A resumed session keeps the level it was set to, and `/model` keeps the level |
| `/new` | Start a new session |
| `/resume <id>` | Resume a session |
| `/model <provider/id>` | Switch the model until Kopi exits; `config.json` is untouched and `/new` and `/resume` keep the choice, and running subagents keep their model. A running workflow's later `agent()` calls, and the `WebFetch`, `WebSearch` and classifier calls of running agents, use the new model when no key pins one. The effort set with `/effort` carries over, clamped to the new model's levels, `medium` when none was set; `WebSearch` is offered or withdrawn with the new model's hosted search when `smallModel` is unset. Refused while a turn runs; it returns at once, and a provider without a credential or an unknown model is reported as an error, with a pointer to `/login` for the former, while no turn starts until the change ends |
| `/add-dir <path>` | Add a directory to the [workspace](tools.md#file-tools) until Kopi exits; see [what it refuses](tools.md#file-tools) and [trust](trust.md#added-directories) for what it asks. Allowed while a turn runs |
| `/login <provider>` | Log in to a provider |
| `/logout <provider>` | Remove a provider's stored credential |
| `/tasks` | List background tasks, [subagents](subagents.md) and [workflows](workflows.md), show one's output, open a subagent's view or a workflow's [progress dialog](workflows.md#progress-dialog) and stop them |
| `/usage` | Show the context and what holds it, the [plan limits](providers.md#plan-limits), the tokens and estimated cost by model, the prompt cache, compaction and time, [as a dialog](#usage-1) |
| `/skill:<name>` | Load a skill with the prompt ([skills](skills.md)) |
| `/quit` | Exit |
| `//text` | Send a prompt that starts with `/` |

- A session whose transcript was last run on a model that does not reason, resumed on one that does, comes back at effort `off`; `/model` keeps that level like one set with `/effort`, and `/effort <level>` sets it again.

### ultrathink

The whole word `ultrathink`, in any case, in a prompt to the main agent raises its reasoning effort to the highest level the model supports. The raise starts with the request that carries the prompt, whether it opens the turn or joins a running one, and lasts to the end of that turn; the previous level is then put back, unless `/effort` was used during the turn, in which case that level stands. The prompt text goes to the model unchanged.

- Works in the fullscreen interface and with `-p`.
- Not matched inside an `@` reference, a skill command or a control command, and not typed to a subagent's view.
- Ctrl+Enter keeps the raise, as it continues the turn. A turn that starts because a task ended or queued input followed a completed turn is not raised unless its own prompt says `ultrathink`.
- No effect, and no highlight, on a model that does not reason.
- A crash mid-turn leaves the raised level in the session.

### /usage

```text
  Usage

  Context     ▰▰▰▱▱▱▱▱▱▱▱▱▱▱▱▱  21%  84.2k / 400.0k
  5h          ▰▰▰▰▱▱▱▱▱▱▱▱▱▱▱▱  23%  resets 15:40
  wk          ▰▰▰▰▰▰▰▰▱▱▱▱▱▱▱▱  52%  resets Thu 09:00

  System prompt    1.9k    0.5%
  Tools            8.1k    2.0%
  Instructions     6.2k    1.6%
  Skills           1.8k    0.5%
  Agents           1.3k    0.3%
  MCP              3.0k    0.8%
  Messages        61.9k   15.5%

  openai-codex/gpt-5-codex     1.2m   $3.90
  openai-codex/gpt-5-mini    310.0k   $0.41
  Total (estimate)             1.5m   $4.31

  Tokens      502.0k in · 58.0k out · 940.0k read · 12.0k write
  Cache       91% hit · warm until 15:42 · 2 misses $0.42
  Compaction  2 done · next at 383.6k
  Time        12m 40s model · 3m 5s tools
  Session     0195c2d4-7b1e-7c3a-9f2d-5e8a1b6c4d70 · 42 messages · 128 tool calls

  esc to close
```

- Esc closes it. It stays open while turns run and shows the new figures when a tool call or turn ends; a change of session closes it. PgUp/PgDn scroll it when it is taller than a third of the screen.
- `Context` is the main session's context; it is absent while the context's size or the model's window is unknown, as before the first request and right after a compaction. `5h`, `wk` and the other plan windows are the share of the plan used, in the warning colour from 90%, with when each resets (as in [Plan limits](providers.md#plan-limits)). They show only for a provider Kopi reads plan limits from, and only when it is the main session's model's provider.
- The rows under the bars are the main session's context by what holds it, each with its share of the model's window, so the shares add up to about `Context`'s (a share under a tenth of a percent shows as `<0.1%`):
  - `System prompt` is Kopi's own text, `Instructions` the [instruction files](trust.md), the home's and a trusted folder's.
  - `Tools` is the definitions of the tools the model can call, apart from the `Skill` tool's, which carries the skill list and is `Skills`, and the `Agent` tool's, which carries the subagent list and is `Agents`.
  - `MCP` is what MCP servers report about themselves and the definitions of their tools that have been loaded.
  - `Messages` is the rest: prompts, replies and tool results.
  - Every row but `Messages` is an estimate from the text's length; `Messages` is what they leave of the context the provider reports, so the rows add up to `Context`. Where the estimates come to more than that, they are scaled down to it.
  - They are absent whenever `Context` is.
- Each model row is the tokens and the cost of the requests the provider answered as that model. The cost is an estimate from the model's listed price; on a subscription sign-in it is not what you are billed. `summaries` is what compaction's own requests cost.
- The total covers the main session, its subagents, the workflow agents that have ended, and the classifier, `WebFetch`, `WebSearch` and title requests. `Tokens` is the same sum by kind.
- `Cache` is the main session's. `hit` is the share of the last request's prompt read from the provider's cache. A miss is prompt that had been cached and was paid for again; its dollar figure is what that cost over the cache-read price. `warm until` or `expired` appears only when the main session's model made the last request and declares a cache lifetime, so it is gone after switching model until the new model's first request. It is judged by time alone, so compacting can still rebuild a cache shown as warm.
- `Compaction` is how many times the main session's context has been compacted, and the context size above which the next one happens: the model's window less 16384 tokens (see [models](providers.md#models)).
- `Time` is how long the main session's requests to the model took and how long its tool calls ran, over the session's whole log. Waiting for an approval is in neither, and a part under a second is left out.
- `Session` is the session id and the messages and tool calls in the session's whole log. A row, or a group, with nothing to show is left out.

## Keys

Defaults; change them with [`keys`](configuration.md#keys).

- Enter sends the prompt or command.
  - While a turn runs, it queues the prompt for that turn.
  - Before the first session is open, it queues the prompt for that session.
  - Queued prompts and your [commands](#your-own-commands) show above the working row in the order typed: consecutive prompts share one shaded block, each command has its own, and `↳ queued · ctrl+enter to send now · up to edit` sits under the last prompt block.
  - These work in a [subagent's view](subagents.md#views) too, for that subagent.
- Ctrl+Enter sends now: it stops the current response and continues the turn with the queued messages plus the draft. It does not put a prompt ahead of a running [command of yours](#your-own-commands).
- Alt+Enter, Shift+Enter or Ctrl+J inserts a newline. Ctrl+Enter and Shift+Enter need a terminal that reports modified Enter.
- Up at the top of the input pulls queued prompts back into it for editing (not your commands); with nothing queued it browses input history, and Down goes back.
- Down from the last line of the input, with no newer history entry, moves into the [agent rows](subagents.md#agent-rows) under the status line: Up and Down move, Enter opens that session's view, `x` stops the selected subagent or workflow (a workflow's row opens its [progress dialog](workflows.md#progress-dialog)), Esc or typing returns to the input.
- Esc interrupts the running turn of the session whose view is open: the main agent's in the main view, only that subagent's run in a [subagent's view](subagents.md#views). It never acts on another session. It leaves your [commands](#your-own-commands) alone; with no turn running it stops the command that runs.
- The row above the input, in priority order: `Waiting for you…` while an approval, question or plan dialog is open; `<Verb>… <elapsed>` (`Brewing… 12s`) while the open view's turn runs; in the main view, Kopi's own activity such as `Connecting to MCP servers…` or `Logging in to <provider>…`; `2 agents, 1 workflow, 1 shell still running` in the main view while the main agent is idle and background tasks, subagents or [workflows](workflows.md) run.
- A turn ends with `κ Brewed for 41s`, `κ Interrupted after 12s` or `κ Failed after 12s` followed by the error.
- Each turn has its own row and time, including a turn started by a task's end. The time is wall time and includes time spent waiting on an approval.
- After an interrupt, a task's end starts no turn until the next prompt; see [background tasks](tools.md#background-tasks).
- Ctrl+C with a dialog open dismisses it as Esc does. Otherwise it interrupts the open view's running turn as Esc does, or stops your running command when no turn runs, else clears the draft, else a second press within two seconds exits.
- After an interrupt, queued messages return to the input; `/new`, `/resume` and exiting discard them.
- Ctrl+D exits when the input is empty.
- Shift+Tab switches between plan and execute.
- Ctrl+B moves the running foreground `Bash` command to the background, where it keeps running as a [task](tools.md#background-tasks). It moves your running [command](#your-own-commands) the same way.
  - While one runs, the working row shows `ctrl+b to background`.
  - With none running, Ctrl+B keeps its meaning in the input.
- Ctrl+G, while an [`ExitPlanMode`](tools.md#exitplanmode) plan is open, opens the plan file in `$VISUAL`, else `$EDITOR`.
- Ctrl+V pastes text or an image from the clipboard.
- Mouse wheel or PgUp/PgDn scrolls the transcript; while the open dialog's body is cut (an [approval](permissions.md#approval-dialog) preview, a `/tasks` output, an [`AskUserQuestion`](tools.md#askuserquestion) preview or review) they scroll that instead. PgUp/PgDn never move the cursor in a dialog's text field.
- Keys while the model asks you a question are listed under [`AskUserQuestion`](tools.md#askuserquestion).
- Ctrl+O opens every group and every row that opens, and every capped failure, until pressed again, including every agent's prompt and every message to an agent; see [Transcript](#transcript).

## Transcript

Each tool call is a row. It names the call by the tool while it runs, while it waits for approval and when it did not succeed, and by a past-tense verb once it succeeded. A path shows relative to the start directory, the session's plan file as `plan`, and a saved output file as `output <name>`.

```text
• Ran go test ./...
  ↳ 84 lines
• Edited internal/app/app.go
  ↳ +3 -1
• Read cmd/root.go:40-89
  ↳ 50 lines
```

- A row's bullet is at the left edge and its title two columns in. The `↳` line starts two columns in, with its text four in. An opened body is two columns past the text of the line above it: six in under a `↳` line, four in under a title alone. A call in a group is a `↳` line, and its opened body is six in. Wrapped lines hang under their text.

```text
• Ran ls
  ↳ 2 lines
      a.go
      b.go
▾ Read 1 file, ran 1 command
  ↳ Read a.go:98-101 · 4 lines
       98  func a() {
       99      b()
      100  }
      101
  ↳ Ran ls · 2 lines
      a.go
      b.go
• Edited a.go
  ↳ +1 -1
      - old line
      + new line
```

- A run of successful calls folds into a group, `Read 3 files, ran 2 commands`; click it or press Ctrl+O to list its calls. A call that did not succeed, and the question and plan-approval rows below, are never in a group.
- A group clause uses its rows' verb and a noun that covers every row in it: a `Read` of the plan and of a saved output file count as `read the plan` and `read 1 output file`, apart from `read 3 files`; a `Skill` call that reads a file of a skill (`Read skill <name> <file>`) counts as `read 1 skill file`.
- Some clauses are not counted: `searched the web`, `searched tools`, `read the plan`, `wrote the plan`, `edited the plan`. Repeats read `searched the web 3 times`, `edited the plan 3 times`.
- A clause counts a file, an agent, a shell or a workflow once however many calls touched it: seven edits of two files read `edited 2 files`, three messages to one agent `messaged 1 agent`. Commands, searches and launches count each call.
- A background command counts as `started 1 shell`, a message to an agent as `messaged 1 agent`, a `TaskStop` on an agent, shell or workflow as `stopped 1 shell` (on a task that was already over as `checked on 1 agent`), and a `WebFetch` that only got a redirect as `hit 1 redirect`.
- A group counts the commands that ran outside the [sandbox](sandbox.md#outside-the-sandbox): `ran 9 commands (1 unsandboxed)`. A succeeded command's own row ends `· unsandboxed`.
- A [command of yours](#your-own-commands) is one block, never in a group, drawn again on resume.
- A question you answered (`Asked about …`) and a plan decision (`Plan approval`) stay visible, with your answer or decision on the line under them.

Rows for background work name the agent (`researcher › survey peers`), the shell command (`shell npm run dev`) or the workflow (`workflow › review`), not a task id:

```text
• Started npm run dev in background
• Stopped shell npm run dev
• Messaged researcher › survey peers
  ↳ queued
• shell finished · npm run dev · 12s
```

A row says what happened instead of the tool's usual verb when that verb would be untrue:

```text
• Moved go test ./... to background
• TaskStop researcher › survey peers
  ↳ already completed (exit code 0)
• WebFetch example.com
  ↳ redirected
```

- `Moved <command> to background` is a command you moved with the `background` key, or that its timeout moved; the latter shows the time the call waited under it (`↳ after 2m`, or `↳ after 1s` when the model set a short timeout).
- A `TaskStop` on a task that was already over keeps the tool's name and says how it ended: `↳ already completed (exit code 0)`, `↳ already failed (exit code 1)` or `↳ already stopped`.
- A `WebFetch` that only got a redirect keeps the tool's name.
- A task row that is running, waiting for approval, failed or denied shows the task id (`TaskStop t3`).
- A session saved before a task's name was recorded in its result shows each as its tool does: `Stopped task t3`, `TaskStop t3` for a task that was already over, or `Messaged t3`.

A call that did not succeed is one of:

| Kind | Example | Meaning |
| --- | --- | --- |
| Rejected | `↳ rejected` | You answered `No` in the [approval dialog](permissions.md#approval-dialog); your reason shows under it |
| Denied | `↳ denied · plan mode` | A rule, the mode, the classifier or a run that cannot ask refused it |
| Interrupted | `↳ interrupted` | The turn was interrupted while it ran |
| Failed | `↳ exit code 1 · 84 lines` | It ran and returned an error; a one-line error shows itself, any other shows `failed` |

The bullet is green for a call that succeeded, yellow for one you or a rule refused, interrupted or stopped, and red for one that failed. A row that did not succeed colours its `↳` line like its bullet; a successful row's `↳` line is dim.

- A failed command's output shows under its line: in full up to 5 lines, otherwise its first two and last two lines around `… N more lines`. Click the row or press Ctrl+O for all of it.
- A successful row opens to the result as the model received it, except where this list says otherwise. An `Edit` or `Write` row shows `+3 -1` or `+40` and opens to its removed lines and its added lines.
- A `Bash` row whose title cut the command (a second line, or more than 80 characters) opens to the whole command after `$ `, above its output, whatever the outcome; a rejected, denied or interrupted row opens to it under its summary, and a cut command with no output still opens.
- Lines of an opened `Edit` or `Write` longer than 2000 characters end ` [line truncated]`.
- An agent's launch row opens to the prompt the main agent gave it, as a workflow's row opens to its script; a message row opens to the message.
- An agent's end row opens to its final reply, and a failed one shows its reason on the `↳` line, after the agent count of a workflow (`↳ 2 agents · 2 failed · <reason>`). A shell's end row shows its reason (`exit code 1`) when it failed and opens to the last lines of its output that the model was sent.
- A row that has nothing to add to its title and summary does not open: a command with no output, a file that is empty, a search with no match. A question's answers are on its row.
- A plan decision opens to the plan as it was presented, under your feedback for a revision.
- A plan you sent back or dismissed and a question you dismissed read as rows that did not succeed: a yellow bullet and `↳` line, the tool's title (`Plan approval`, `AskUserQuestion`), and your feedback shown under the plan row without opening it.
- An opened result is the model's text, so it includes notices written to the model, such as `use offset=2001 to continue`. A `Read` row shows its line numbers right-aligned to the widest, without the padding the model receives.
- An edit's lines show without surrounding context or line numbers.
- Colour in command output is not kept.
- Only the row's title line opens and closes it; scroll up to close a long body.

## Menus and highlighting

- `/` at the start of the input lists commands and skills; a skill is filled in rather than run.
- A space after a command lists its arguments: `/plan` lists `open`; `/effort`, `/model`, `/login` and `/logout` list their values.
- `/add-dir` lists directories only, one level at a time, each ending in `/`:
  - Nothing typed, or text with no `/` in it other than `~`, completes among the start directory's sibling folders: `pi` lists `../pig/`. Text with a `/`, and `~` alone, completes inside the folder typed so far.
  - A name matches on its start, in any letter case.
  - Hidden folders are listed once a `.` is typed.
  - A folder typed with its `/` is listed first, then the folders in it.
  - Accepting a folder from the menu fills it in and lists what is in it; it does not add it.
  - Enter on a folder typed in full, with its `/`, adds it. Without the `/`, one Enter completes it and a second adds it.
  - A folder that cannot be added is not listed. At most 50 are listed.
- Picking `/resume` from the menu lists this directory's sessions, which you filter by date or first message.
- `@` at the start of the input or after a space lists files and directories under the start directory, leaving out:
  - `.git`
  - anything a `.gitignore` in the start directory or below ignores
  - Kopi's home
- While a menu shows:
  - Up and Down choose, Tab fills in, Esc closes the menu.
  - Enter fills in and runs a command; a command that needs an argument, or a file reference, is only filled in.
  - Enter sends the input as typed when it already runs as it stands: `/plan ` with nothing after it, or an argument typed in full. No row is highlighted then; Up, Down or Tab chooses one.
- Known commands, skill commands (wherever they appear in the input) and `@` references are highlighted in the input. Sent and queued messages highlight the same skill commands and references.
- An `@path` is sent as written; a space in an inserted path is written `\ `.

## Your own commands

An input that starts with `!` runs the rest as a shell command of yours. A leading space makes it an ordinary prompt.

- It runs in the session's working directory, with the same shell as the [`Bash`](tools.md#bash) tool, outside the [sandbox](sandbox.md#outside-the-sandbox) and the [permission rules](permissions.md#limitations), in both modes, with no time limit.
- Only the fullscreen interface runs them. While you watch a subagent, a `!` command still runs for the main session and shows there.
- It runs at once, even while the agent works. Commands run one at a time in the order typed; a later one waits (`queued`).
- What you type reaches the agent in the order you typed it. A prompt typed behind a running command waits for that command.
- The agent reads a command's result at its next step when it is working, or at once when it is idle, and then replies to it. It reads the command, its exit status and time, and the same capped output a `Bash` result carries; long output is [saved](tools.md#saved-output) under the session's output folder.
- No reply is made for a command you stopped, for one moved to the background, or for one that finishes after you interrupted the agent, until you type something.
- Until the agent has read it, a command sits above the input with what else is queued, in the order typed. Once read, it is in the [transcript](#transcript) at that point.
- Ctrl+B moves the running command to the background, where it is a background shell like any other: listed and stoppable in `/tasks`, its output saved to a file the agent can read, the agent told when it ends. The next queued command starts.
- Esc and Ctrl+C interrupt the agent's turn when one runs and leave your commands alone. With no turn running they stop your running command; commands still queued behind it are dropped, and prompts waiting behind it return to the input.
- `/new`, `/resume` and exit stop your running command and drop the queued ones. A result the agent had not read yet is lost.

A command is a shaded block: `!` and the command, a status line under it, then the last lines of its output, 5 while above the input and 10 in the transcript. Above the input the output is only the last lines; click the status line or press Ctrl+O in the transcript to see all of the stored output.

| Status line | Meaning |
| --- | --- |
| `queued` | Waiting for an earlier command |
| `running 12s · ctrl+b to background · esc to stop` | Running |
| `exit 0 · 12s` | Finished; `· unread` is added while it is above the input |
| `stopped after 12s` | You stopped it; `· unread` is added while it is above the input |
| `moved to background after 12s` | You moved it to the background |

## Mouse

- Click a folded group to open it, and a row that [opens](#transcript) to open or close it. Only the row's title line takes the click.
- Drag in the transcript to select text; it is copied on release. Double-click selects a word, triple-click a line.
- Drag in the input to select text; it is copied on release. A click places the cursor. Double-click selects a word, triple-click the whole line across wraps.
- Dragging after a double- or triple-click extends the selection by words or lines.
- Copies go to the terminal (OSC 52) and the system clipboard.
- The terminal's own modifier-drag selection (Shift, or Option in some terminals) still works.

## Paste and images

- A paste over 10 lines or 1000 characters collapses to `[paste #N +L lines]` or `[paste #N C chars]` and is sent in full. Pasting the same text again right after expands it in place.
- An absolute path to an image file pasted or dropped into the input, or a clipboard image pasted with Ctrl+V, becomes `[image #N]` and is sent as an attachment: PNG, JPEG, GIF or WebP, up to 20 MiB.
- Backspace or Delete next to a paste or image block removes it whole.
- Clipboard images need `osascript` (macOS), `wl-paste` (Wayland) or `xclip` (X11).

## Terminal

Fullscreen only; print mode sends none of this. Kopi derives one state for the whole session, whichever session is shown:

- Waiting on you: an approval, a plan, a question or a login is open. (`/tasks` and a workflow's [progress dialog](workflows.md#progress-dialog) are not.)
- Working: the main turn runs, or a subagent or workflow does. Background shells do not count; one may be a server that never ends.
- Done or failed: the session is at rest and the last main turn completed or failed. It holds until work starts again or the session changes. An interrupted turn is neither.
- Idle: otherwise, including before any turn and after a session change.

What Kopi sends:

- **Program status** (OSC 7501): `app=kopi` and the state: working, waiting on you with its kind (`permission` for an approval or a plan, `question`, `auth` for a login), done, failed or idle. Terminals that read it show it per tab. Each running subagent and each running workflow also has a record beneath it: its task id, `working`, titled `<agent> › <description>`, removed when it ends. A workflow is one record whatever agents it starts; shells have none. All are cleared when Kopi exits.
- **Tab title**: `κ`, then the session's title followed by ` · ` and the workspace folder (`κ Fix flaky test · kopi.osc`), else the folder alone. Kopi asks the terminal once at start whether it reads OSC 7501; until it answers, or where it never does, a glyph for the state (`◐` working, `✋` waiting, `κ` otherwise) replaces the `κ`; `✓` done and `✗` failed show only for work that ended while the terminal was not focused, until it is focused again or work starts. A session gets its title from [`smallModel`](configuration.md#smallmodel), else the session model, with the first prompt it is given while it has none, asked once per run: the first 2000 bytes of the prompt go to that model, and the title is stored with the session so a resumed one shows it again. With `terminal.title` off, Kopi leaves the title alone and calls no model.
- **Progress bar** (OSC 9;4): an indeterminate bar while working, repeated every 10 seconds because Ghostty removes a bar 15 seconds after its last report. Off by default (`terminal.progress`); older iTerm2 and Kitty show the sequence as a notification.
- **Notification**: once, when the session starts waiting for an approval, a plan or an answer, or finishes or fails, while the terminal is not focused. It reads `Waiting for you: <title>`, `Finished: <title>` or `Failed: <title>`; a login sends none. The channel is `terminal.notifications`, see [configuration](configuration.md#terminal).

Limits:

- Focus comes from the terminal's focus reports. A terminal that sends none is taken as focused: no `✓` or `✗` glyph and no notification.
- tmux needs `set -g focus-events on`, and gets the bell. Nothing is wrapped for tmux passthrough.
- On exit the tab title is cleared, not restored.

## Not implemented

- `/usage` output in print mode, and a `kopi usage` subcommand.
- A split of each model's tokens into input, output and cache.
- The context by item (each instruction file, MCP server, skill or subagent), and a subagent's context, compactions and time in `/usage`.
- The session's wall-clock duration in `/usage`.
- Counting a running workflow agent in `/usage`: it is counted when it ends.
- After a resume, `/usage` keeps only the main session's earlier usage, not what subagents, workflow agents and those requests used then.
- Keeping the prompt cache warm.
- A status record per workflow agent, or a state other than working on a subagent's or workflow's record.
- A progress percentage.
- Naming or renaming a session by hand.
- A session title that follows a change of topic.
- Session titles in the resume list.
- Paste blocks in recalled history and re-edited queued messages: they show their full text.
- Keeping a command's output from the agent.
- A time limit for your commands.
- Starting a command in the background.
- A `cd` that lasts beyond its command.
- Completing from earlier commands.
- `!` in print mode: `kopi -p` sends it as text.
- Aliases and functions from your shell's startup files: your commands do not read them.
- Release packaging and self-update.
- Restoring added directories after Kopi restarts: pass `--add-dir` again when resuming. The resumed conversation still mentions the directory.
- A list of the added directories, and removing one short of quitting.
- Loading skills and `.mcp.json` from an added directory.
- `@` completion in an added directory: it completes only the start directory.
- The status line's branch, changes and directory segments for an added directory: they stay the start directory's.
