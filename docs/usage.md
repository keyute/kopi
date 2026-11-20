# Usage

## Running

```sh
bin/kopi
bin/kopi -c    # continue the latest session in this directory
bin/kopi -r <id>
bin/kopi -p "explain main.go"
bin/kopi --mode execute --allowed-tools "Edit(docs/**)" -p "fix the typos in docs"
git diff | bin/kopi -p "review this"
```

- `--model provider/id` picks the model for one run; see [`model`](configuration.md#model).
- With stdin and stdout both terminals and no `-p`, Kopi opens the fullscreen interface; otherwise it prints the response to one prompt and exits.
- The prompt is the argument, piped stdin, or both (stdin, a blank line, then the argument).
- A prompt is always sent as a prompt, never run as a command; a [skill](skills.md) it names is loaded.
- Print mode prints only the final message of the reply.
- Ctrl+C interrupts the turn. A failed or interrupted turn exits with status 1.
- `-c` and `-r` work with `-p`.
- Resuming (`-c`, `-r` or `/resume`) redraws the earlier conversation in the fullscreen interface, with its tool activity folded.
- On exit from a session with messages, the fullscreen interface prints the command to resume it.
- `--mode` and `--allowed-tools` are described in [tools](tools.md#modes) and [permissions](permissions.md#rules). `kopi mcp list` is in [MCP](mcp.md#kopi-mcp-list).

## Commands

Commands in the fullscreen interface:

| Command | Effect |
| --- | --- |
| `/plan` | Switch to plan mode |
| `/plan open` | Open the session's [plan file](tools.md#exitplanmode) in `$VISUAL`, else `$EDITOR` |
| `/execute` | Switch to execute mode |
| `/new` | Start a new session |
| `/resume <id>` | Resume a session |
| `/login <provider>` | Log in to a provider |
| `/logout <provider>` | Remove a provider's stored credential |
| `/tasks` | List background tasks, [subagents](subagents.md) and [workflows](workflows.md), show one's output, open a subagent's view and stop it |
| `/skill:<name>` | Load a skill with the prompt ([skills](skills.md)) |
| `/quit` | Exit |
| `//text` | Send a prompt that starts with `/` |

## Keys

Defaults; change them with [`keys`](configuration.md#keys).

- Enter sends the prompt or command.
  - While a turn runs, it queues the prompt for that turn.
  - Before the first session is open, it queues the prompt for that session.
  - Queued messages show in one shaded block above the working row, with `↳ queued · ctrl+enter to send now · up to edit` under it.
  - These work in a [subagent's view](subagents.md#views) too, for that subagent.
- Ctrl+Enter sends now: it stops the current response and continues the turn with the queued messages plus the draft.
- Alt+Enter, Shift+Enter or Ctrl+J inserts a newline. Ctrl+Enter and Shift+Enter need a terminal that reports modified Enter.
- Up at the top of the input pulls queued messages back into it for editing; with nothing queued it browses input history, and Down goes back.
- Down from the last line of the input, with no newer history entry, moves into the [agent rows](subagents.md#agent-rows) under the status line: Up and Down move, Enter opens that session's view, `x` stops the selected subagent, Esc or typing returns to the input.
- Esc interrupts the running turn of the session whose view is open: the main agent's in the main view, only that subagent's run in a [subagent's view](subagents.md#views). It never acts on another session.
- While the turn is idle and background tasks, subagents or [workflows](workflows.md) run, the row above the input reads `Waiting on 2 agents, 1 workflow, 1 shell… 1m 5s`.
- Ctrl+C with a dialog open dismisses it as Esc does. Otherwise it interrupts the open view's running turn as Esc does, else clears the draft, else a second press within two seconds exits.
- After an interrupt, queued messages return to the input; `/new`, `/resume` and exiting discard them.
- Ctrl+D exits when the input is empty.
- Shift+Tab switches between plan and execute.
- Ctrl+B moves the running foreground `Bash` command to the background, where it keeps running as a [task](tools.md#background-tasks).
  - While one runs, the working row shows `ctrl+b to background`.
  - With none running, Ctrl+B keeps its meaning in the input.
- Ctrl+G, while an [`ExitPlanMode`](tools.md#exitplanmode) plan is open, opens the plan file in `$VISUAL`, else `$EDITOR`.
- Ctrl+V pastes text or an image from the clipboard.
- Mouse wheel or PgUp/PgDn scrolls the transcript; while the open dialog's body is cut (an [approval](permissions.md#approval-dialog) preview, a `/tasks` output, an [`AskUserQuestion`](tools.md#askuserquestion) preview or review) they scroll that instead. PgUp/PgDn never move the cursor in a dialog's text field.
- Keys while the model asks you a question are listed under [`AskUserQuestion`](tools.md#askuserquestion).
- Ctrl+O opens every group and every row that opens, and every capped failure, until pressed again; see [Transcript](#transcript).

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

- A run of successful calls folds into a group, `Read 3 files, ran 2 commands`; click it or press Ctrl+O to list its calls. A call that did not succeed, and the question and plan-approval rows below, are never in a group.
- A group counts the commands that ran outside the [sandbox](sandbox.md#outside-the-sandbox): `ran 9 commands (1 unsandboxed)`. A succeeded command's own row ends `· unsandboxed`.
- A question you answered (`Asked about …`) and a plan decision (`Plan approval`) stay visible, with your answer or decision on the line under them.

A call that did not succeed is one of:

| Kind | Example | Meaning |
| --- | --- | --- |
| Rejected | `↳ rejected` | You answered `No` in the [approval dialog](permissions.md#approval-dialog); your reason shows under it |
| Denied | `↳ denied · plan mode` | A rule, the mode, the classifier or a run that cannot ask refused it |
| Interrupted | `↳ interrupted` | The turn was interrupted while it ran |
| Failed | `↳ exit code 1 · 84 lines` | It ran and returned an error; a one-line error shows itself, any other shows `failed` |

Rejected, denied and interrupted rows have a warning-coloured bullet, a failed row the error colour.

- A failed command's output shows under its line: in full up to 5 lines, otherwise its first two and last two lines around `… N more lines`. Click the row or press Ctrl+O for all of it.
- A successful row opens to the result as the model received it. An `Edit` or `Write` row shows `+3 -1` or `+40` and opens to its removed lines and its added lines.
- A `Bash` row whose title cut the command (a second line, or more than 80 characters) opens to the whole command after `$ `, above its output, whatever the outcome; a rejected, denied or interrupted row opens to it under its summary, and a cut command with no output still opens.
- Lines of an opened `Edit` or `Write` longer than 2000 characters end ` [line truncated]`.
- A row that has nothing to add to its title and summary does not open: a command with no output, a file that is empty, a search with no match, a launched agent. A question's answers are on its row.
- A plan revision you asked for opens to your feedback.
- An opened result is the model's text, so it includes notices written to the model, such as `use offset=2001 to continue`.
- An edit's lines show without surrounding context or line numbers.
- Colour in command output is not kept.
- Only the row's title line opens and closes it; scroll up to close a long body.

## Menus and highlighting

- `/` at the start of the input lists commands and skills; a skill is filled in rather than run.
- Picking `/resume` from the menu lists this directory's sessions, which you filter by date or first message.
- `@` at the start of the input or after a space lists files and directories under the start directory, leaving out:
  - `.git`
  - anything a `.gitignore` in the start directory or below ignores
  - Kopi's home
- While a menu shows:
  - Up and Down choose, Tab fills in, Esc closes the menu.
  - Enter fills in and runs a command; a command that takes an argument, or a file reference, is only filled in.
- Known commands, skill commands (wherever they appear in the input) and `@` references are highlighted in the input. Sent and queued messages highlight the same skill commands and references.
- An `@path` is sent as written; a space in an inserted path is written `\ `.

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

## Not implemented

- Paste blocks in recalled history and re-edited queued messages: they show their full text.
- Running commands from the input.
- Release packaging and self-update.
