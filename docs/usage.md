# Usage

## Running

```sh
kopi
kopi -c    # continue the latest session in this directory
kopi -r <id>
kopi -p "explain main.go"
kopi --mode execute --allowed-tools "Edit(docs/**)" -p "fix the typos in docs"
git diff | kopi -p "review this"
kopi version
```

- With stdin and stdout both terminals and no `-p`, Kopi opens the fullscreen interface; otherwise it prints one response and exits.
- The prompt is the argument, piped stdin, or both (stdin, a blank line, then the argument). It is never run as a command; a [skill](skills.md) it names is loaded.
- Print mode prints only the reply's final message. Ctrl+C interrupts the turn; a failed or interrupted turn exits with status 1.
- `--model provider/id` picks the model for one run; see [`model`](configuration.md#model).
- `-c` and `-r` work with `-p`; the session opens at once, while the [MCP servers](mcp.md#startup) connect.
- A resumed session runs on this run's [session model](configuration.md#model) and [effort](configuration.md#effort), not the ones it last ran with.
- Resuming redraws the earlier conversation, tool activity folded. On exit from a session with messages, Kopi prints the resume command.
- `--add-dir <path>` adds a directory to the [workspace](tools.md#file-tools) like [`/add-dir`](#commands). It is repeatable; a path that cannot be added stops the start. In print mode, a folder no [trust](trust.md#added-directories) decision covers gets file access only.
- `--log-level` sets the level of `kopi.log` and, in print mode, stderr (default `warn`).
- `--mode`, `--allowed-tools` and `--disallowed-tools` are in [tools](tools.md#modes) and [permissions](permissions.md#rules). `kopi mcp list` is in [MCP](mcp.md#kopi-mcp-list).

## Commands

| Command | Effect |
| --- | --- |
| `/plan` | Switch to plan mode |
| `/plan open` | Open the session's [plan file](tools.md#exitplanmode) in `$VISUAL`, else `$EDITOR` |
| `/execute` | Switch to execute mode |
| `/effort <level>` | Set the main agent's reasoning effort until Kopi exits; `config.json` is untouched and `/new`, `/resume` and `/model` keep the level, subject to the exception under [`effort`](configuration.md#effort) |
| `/new` | Start a new session |
| `/resume <id>` | Resume a session |
| `/model <provider/id>` | Switch the model until Kopi exits, [see below](#model) |
| `/add-dir <path>` | Add a directory to the [workspace](#add-dir-and-remove-dir) |
| `/remove-dir <path>` | Remove an added directory |
| `/login <provider>` | Log in to a provider |
| `/logout <provider>` | Remove a provider's stored credential |
| `/tasks` | List, inspect and stop background tasks, [subagents](subagents.md) and [workflows](workflows.md); open a subagent's view or a workflow's [progress dialog](workflows.md#progress-dialog) |
| `/btw <question>` | Ask about the session so far, [answered in a dialog](#btw) |
| `/usage` | Show context, [plan limits](providers.md#plan-limits), tokens and cost, [as a dialog](#usage-1) |
| `/copy` | Copy the last reply or one of its code blocks, [see below](#copy) |
| `/skill:<name>` | Load a skill with the prompt ([skills](skills.md)) |
| `/quit` | Exit |
| `//text` | Send a prompt that starts with `/` |

### /model

- `config.json` is untouched. `/new` and `/resume` keep the choice; running subagents keep their model. A running workflow's later `agent()` calls use the new model unless a key pins one.
- Effort carries over, clamped to the new model's levels. With `smallModel` unset, `WebSearch` follows the new model's hosted search.
- Refused while a turn runs. A provider without a credential or an unknown model is an error; the former points to `/login`.

### /copy

- Copies the last finished reply as markdown. When it has code blocks, a dialog picks the whole reply or one block, copied without its fence.
- A reply still being written, interrupted, or awaiting recovery after an error or output limit is skipped for the one before it. Earlier replies are not offered. Copies go where [Mouse](#mouse) says.

### /add-dir and /remove-dir

- A directory stays in the [workspace](tools.md#file-tools) until `/remove-dir` removes it or Kopi exits. [Trust](trust.md#added-directories) covers what `/add-dir` asks and what `/remove-dir` does not undo.
- `/remove-dir` takes the spelling the directory was added in, its real path, or a path resolving to it, never the start directory. It applies at once to the session, its subagents and workflow agents.
- Both work during a turn.

### ultrathink

The whole word `ultrathink`, in any case, in a prompt to the main agent raises its effort to the model's highest level from the request carrying it to the end of that turn. The previous level is then restored, unless `/effort` was used during the turn. The model gets the prompt unchanged.

- Works in the fullscreen interface and with `-p`. Not matched inside an `@` reference, skill command or control command, nor typed to a subagent's view.
- Ctrl+Enter keeps the raise. A turn started by a task's end or queued input is not raised unless its own prompt says so.
- No effect on a model that does not reason.

### /btw

```text
  Side question

  what was the name of that config file again?

  internal/config/ui.go. The key map is in Defaults(), lines 116-127.

  esc to close
```

`/btw <question>` shows an answer about the session so far in a dialog. Neither question nor answer joins the session.

- It works while a turn runs without interrupting it, and sees the main session except the reply being written.
- No tool runs. The answer is whole, not streamed, from the session's model at its effort.
- The whole context is sent once more, at the cache-read price while the cache is warm, and counts in `$total` and `/usage`.
- Esc cancels the request while it is answering, and closes the dialog after; a closed answer cannot be reopened. A failed request shows its error in place of the answer. An approval, question or plan covers it; it returns after. PgUp/PgDn scroll.
- A session with more images than the model takes may be refused.
- Not available: follow-ups, history, a subagent's transcript, print mode.

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

- It updates while turns run; Esc or a session change closes it.
- `Context` is the main session's, split by holder (system prompt, tools, instructions, skills, agents, MCP, messages) as shares of the model's window, estimated from text length. Both are absent while the size or the window is unknown: before the first request and right after a compaction.
- `Skills` and `Agents` are the `Skill` and `Agent` tool definitions, which carry the skill and subagent lists; `Tools` excludes them. `MCP` is what servers report plus their loaded tool definitions.
- `5h`, `wk` and other plan windows show the share of the plan used (warning colour from 90%) and when it resets, only when Kopi reads [plan limits](providers.md#plan-limits) for the main model's provider.
- Each model row is the tokens and cost of the requests answered as that model; the total adds the main session, subagents, ended workflow agents, and classifier, `WebFetch`, `WebSearch` and title requests. Cost is an estimate from the listed price, not a subscription's bill.
- `Cache` is the main session's: `hit` is the share of the last request's prompt read from cache, a miss is cached prompt paid for again (its cost over the cache-read price). `warm until` or `expired` appears only when the main model made the last request and declares a cache lifetime; it is judged by time alone, so compacting can rebuild a cache shown as warm.
- `Compaction` is the count and the context size above which the next happens: the window less 16384 tokens (see [models](providers.md#models)).
- `Tokens` is the total above, by kind. `Time` is how long the main session's model requests and tool calls took over its whole log, excluding approval waits. `Session` is the id and the log's message and tool-call counts.

## Keys

Defaults; see [`keys`](configuration.md#keys).

- Enter sends the prompt or command, or queues it while a turn runs or before the first session is open. Queued prompts and your [commands](#your-own-commands) show above the working row in order, with `↳ queued · ctrl+enter to send now · up to edit` under the last. This works in a [subagent's view](subagents.md#views) for that subagent.
- Ctrl+Enter stops the current response and continues the turn with the queued messages plus the draft. It does not go ahead of a running [command of yours](#your-own-commands).
- Alt+Enter, Shift+Enter or Ctrl+J inserts a newline. Ctrl+Enter and Shift+Enter need a terminal that reports modified Enter.
- Up at the top of the input pulls queued prompts back for editing (not your commands); with none queued it browses history.
- Down from the input's last line, with no newer history, moves into the [agent rows](subagents.md#agent-rows): Up and Down move, Enter opens a session's view (a workflow's [progress dialog](workflows.md#progress-dialog)), `x` stops it, Esc or typing returns.
- Esc interrupts the running turn of the open view: the main agent's, or only that subagent's. It leaves your [commands](#your-own-commands) alone; with no turn running it stops the command that runs.
- Ctrl+C dismisses an open dialog like Esc. Otherwise it interrupts the turn, else stops your running command, else clears the draft, else a second press within two seconds exits.
- After an interrupt, queued messages return to the input and a task's end starts no turn until the next prompt. `/new`, `/resume` and exit discard the queue.
- Ctrl+D exits when the input is empty. Shift+Tab switches between plan and execute. Ctrl+V pastes text or an image.
- Ctrl+B moves the running foreground `Bash` command, or your running [command](#your-own-commands), to the background as a [task](tools.md#background-tasks). The working row names the key once one has run 2 seconds.
- Ctrl+G, while an [`ExitPlanMode`](tools.md#exitplanmode) plan is open, opens the plan file in `$VISUAL`, else `$EDITOR`.
- Mouse wheel or PgUp/PgDn scrolls the transcript, or an open dialog's body while it is cut. [`AskUserQuestion`](tools.md#askuserquestion) lists its own keys.
- Ctrl+O opens every group, row that opens and capped failure until pressed again; see [Transcript](#transcript).

The row above the input shows, in priority order: `Waiting for you…` while an approval, question or plan dialog is open; `<Verb>… <elapsed>` (`Brewing… 12s`) while the open view's turn runs; in the main view, Kopi's own activity (`Connecting to MCP servers…`, `Logging in to <provider>…`) once it has lasted 2 seconds; then, with the main agent idle, `2 agents, 1 workflow, 1 shell still running`.

A turn ends with `κ Brewed for 41s`, `κ Interrupted after 12s` or `κ Failed after 12s` and the error; the time includes approval waits.

## Transcript

Each tool call is a row, named by the tool while it runs, waits for approval or did not succeed, and by a past-tense verb (`Ran go test ./...`) once it succeeded, with a `↳` summary line. Paths are relative to the start directory; the plan file shows as `plan`, a saved output file as `output <name>`.

```text
• Ran go test ./...
  ↳ 84 lines
• Edited internal/app/app.go
  ↳ +3 -1
• Read cmd/root.go:40-89
  ↳ 50 lines
```

- A run of successful calls folds into a group (`Read 3 files, ran 2 commands`); click it or press Ctrl+O to list them.
- A group counts a file, agent, shell or workflow once however many calls touched it; commands, searches and launches count each call, and commands outside the [sandbox](sandbox.md#outside-the-sandbox) are counted (`ran 9 commands (1 unsandboxed)`). A succeeded command's own row ends `· unsandboxed`.
- A [command of yours](#your-own-commands), an answered question and a plan decision (`↳ approved`, `↳ revision requested`, `↳ dismissed`) never join a group; the last two show your answer under them.
- Rows for background work name the agent, command or workflow, not a task id, unless the row is running, awaiting approval, failed or denied.
- A row says what happened when the usual verb would be untrue: `Moved go test ./... to background` (a timeout adds `↳ after 2m`), a `TaskStop` on a finished task (`↳ already completed (exit code 0)`), a `WebFetch` that only got a redirect (`↳ redirected`).
- A background command's end is a row: `• shell finished · npm run dev · 12s`, or `failed`, `stopped`.
- A `SendMessage` row ends `↳ queued`, `↳ continued` (it restarted an ended subagent) or `↳ ended` (the report is pending; not delivered).

A call that did not succeed is one of:

| Kind | Example | Meaning |
| --- | --- | --- |
| Rejected | `↳ rejected` | You answered `No` in the [approval dialog](permissions.md#approval-dialog); your reason shows under it |
| Denied | `↳ denied · plan mode` | A rule, the mode, the classifier or a run that cannot ask refused it |
| Interrupted | `↳ interrupted` | The turn was interrupted while it ran, or before it started |
| Failed | `↳ exit code 1 · 84 lines` | It ran and returned an error; a one-line error shows itself, any other shows `failed` |

The bullet is green for success, yellow for a call you or a rule refused, interrupted or stopped, red for a failure. The `↳` line takes its colour unless the row succeeded, when it is dim.

- A failed command's output shows under its line: up to 5 lines, otherwise the first two and last two around `… N more lines`. Click the row or press Ctrl+O for all of it.
- A successful row opens to the result as the model received it, notices included, without colour. `Edit` and `Write` rows open to the removed and added lines without context.
- A `Bash` row whose title cut the command (a second line, or over 80 characters) opens to the whole command above its output.
- An agent's launch row opens to its prompt, a workflow's to its script, a message row to the message, an end row to the final reply (a failed one shows its reason on the `↳` line), a shell's end row to the last output lines sent to the model, a plan decision to the plan as presented. Feedback on a plan sent back or dismissed, and on a dismissed question, shows under the row.

In a reply, HTML is shown as written, a line break inside a paragraph is kept, a link shows as its text with the URL in parentheses and an image as its text and URL. Footnotes are not supported: `[^1]` reads as a link reference. A table wider than the terminal wraps its cells; one too narrow for a cell per column shows as pipe text.

## Menus and highlighting

- `/` at the start lists commands and skills; a skill is filled in, not run.
- A space after a command lists its arguments: `open` for `/plan`; values for `/effort`, `/model`, `/login` and `/logout`; the added directories by real path for `/remove-dir`; this directory's sessions, filtered by date or first message, for `/resume`.
- `/add-dir` lists directories only, one level at a time, each ending in `/`, at most 50; hidden ones appear once a `.` is typed, and a folder that cannot be added never.
  - Text with no `/` (other than `~`) completes among the start directory's sibling folders: `pi` lists `../pig/`. Text with a `/`, and `~` alone, completes inside the folder typed so far. Names match on their start, in any case.
  - Accepting a folder fills it in and lists its contents; Enter on a folder typed in full adds it.
- `@` at the start or after a space lists files and directories under the start directory, leaving out `.git`, anything a `.gitignore` there or below ignores, and Kopi's home. An `@path` is sent as written; a space in an inserted path is written `\ `.
- Up and Down choose, Tab fills in, Esc closes the menu. Enter fills in and runs a command, but only fills in a command that needs an argument, or a file reference. Enter sends the input as typed when it already runs as it stands: `/plan ` with nothing after it, or an argument typed in full.
- Known commands, skill commands and `@` references are highlighted.

## Your own commands

An input that starts with `!` runs the rest as a shell command of yours. A leading space makes it a prompt.

- The `!` replaces the prompt's `❯`, in red. Backspace at the start turns it back into a prompt and keeps the text.
- It runs in the session's working directory with the [`Bash`](tools.md#bash) tool's shell, outside the [sandbox](sandbox.md#outside-the-sandbox) and the [permission rules](permissions.md#limitations), in both modes, with no time limit.
- Only the fullscreen interface runs them; from a subagent's view, one runs for the main session.
- It runs at once, even while the agent works. Commands run one at a time in order; a later one, and a prompt typed behind a running one, waits.
- The agent reads the result at its next step, or at once when idle, and replies. It gets the command, exit status, time and the same capped output a `Bash` result carries; long output is [saved](tools.md#saved-output).
- No reply is made for a command you stopped, moved to the background, or that finishes after you interrupted the agent, until you type something.
- Until the agent has read it, a command sits above the input with what else is queued; once read, it is in the [transcript](#transcript).
- Ctrl+B moves the running command to the background as a shell like any other: in `/tasks`, output saved to a file, the agent told when it ends. The next queued command starts.
- Esc and Ctrl+C interrupt the agent's turn when one runs; otherwise they stop your running command, drop those queued behind it and return waiting prompts to the input.
- `/new`, `/resume` and exit stop your running command and drop the queued ones; a result the agent had not read is lost.
- When a command will not work when the agent runs it, the agent gives you the `! command` line in a code block; its output goes to the agent once you run it.

A command is a shaded block: `!` and the command, a status line, then the last lines of output (5 above the input, 10 in the transcript). Click the status line or press Ctrl+O for all of it.

| Status line | Meaning |
| --- | --- |
| `queued` | Waiting for an earlier command |
| `running 12s · ctrl+b to background · esc to stop` | Running; the keys show once it has run 2 seconds |
| `exit 0 · 12s` | Finished; `· unread` is added while it is above the input |
| `stopped after 12s` | You stopped it; `· unread` is added while it is above the input |
| `moved to background after 12s` | You moved it to the background |

## Mouse

- Click a folded group, or a row that [opens](#transcript), on its title or `↳` line to open or close it.
- Drag in the transcript or the input to select text; it is copied on release, to the terminal (OSC 52) and the system clipboard. Double-click selects a word, triple-click a line; dragging after either extends by words or lines.
- The terminal's modifier-drag selection (Shift, or Option in some terminals) still works.

## Paste and images

- A paste over 10 lines or 1000 characters collapses to `[paste #N +L lines]` or `[paste #N C chars]` and is sent in full. Pasting it again right after expands it.
- An absolute path to an image file pasted or dropped into the input, or a clipboard image, becomes `[image #N]` and is sent as an attachment: PNG, JPEG, GIF or WebP, up to 20 MiB.
- Backspace or Delete next to a paste or image block removes it whole.
- Clipboard images need `osascript` (macOS), `wl-paste` (Wayland) or `xclip` (X11).

## Terminal

Fullscreen only. Kopi derives one state for the whole session, whichever view is shown:

- Waiting on you: an approval, plan, question or login is open (not `/tasks` or a workflow's [progress dialog](workflows.md#progress-dialog)).
- Working: the main turn, a subagent or a workflow runs. Background shells do not count.
- Done or failed: at rest after a main turn that completed or failed, until work starts or the session changes. An interrupted turn is neither.
- Idle: otherwise.

What Kopi sends:

- **Program status** (OSC 7501): `app=kopi` and the state; waiting on you carries its kind (`permission` for an approval or plan, `question`, `auth` for a login). Each running subagent and workflow also has a record, titled `<agent> › <description>`. All are cleared on exit.
- **Tab title**: `κ`, the session's title and the workspace folder (`κ Fix flaky test · kopi.osc`), else the folder alone. Where the terminal does not read OSC 7501, a glyph for the state replaces the `κ`. While a session has no title, its first prompt gives it one: the first 2000 bytes go to [`smallModel`](configuration.md#smallmodel), else the session model, and the title is stored with the session. With `terminal.title` off, Kopi leaves the title alone and calls no model. On exit the title is cleared, not restored.
- **Progress bar** (OSC 9;4): an indeterminate bar while working. Off by default (`terminal.progress`).
- **Notification**: once, while the terminal is not focused, when the session starts waiting for an approval, plan or answer (`Waiting for you: <title>`) or finishes or fails (`Finished: <title>`, `Failed: <title>`). A login sends none. The channel is `terminal.notifications`, see [configuration](configuration.md#terminal).

Focus comes from the terminal's focus reports; a terminal that sends none counts as focused and gets no notification. tmux needs `set -g focus-events on`, and gets the bell. Nothing is wrapped for tmux passthrough.

## Not implemented

- `/usage` in print mode, and a `kopi usage` subcommand.
- In `/usage`: a per-model split of tokens into input, output and cache, the context by item (each instruction file, MCP server, skill or subagent), a subagent's context, compactions and time, and wall-clock duration.
- After a resume, `/usage` keeps only the main session's earlier usage.
- Keeping the prompt cache warm, a progress percentage, and a status record per workflow agent.
- Naming a session by hand, a title that follows the topic, and titles in the resume list.
- A `cd` that outlasts one of your commands, completing from earlier commands, and your shell's aliases and functions.
- Starting a `!` command in the background, keeping its output from the agent, and self-update.
- Restoring added directories after a restart: pass `--add-dir` again. The resumed conversation still mentions the directory.
- From an added directory: skills, `.mcp.json`, `@` completion, and the status line's branch, changes and directory segments, which stay the start directory's.
