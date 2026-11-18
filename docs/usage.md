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
| `/execute` | Switch to execute mode |
| `/new` | Start a new session |
| `/resume <id>` | Resume a session |
| `/login <provider>` | Log in to a provider |
| `/logout <provider>` | Remove a provider's stored credential |
| `/tasks` | List background tasks, show one's output and stop it |
| `/skill:<name>` | Load a skill with the prompt ([skills](skills.md)) |
| `/quit` | Exit |
| `//text` | Send a prompt that starts with `/` |

## Keys

Defaults; change them with [`keys`](configuration.md#keys).

- Enter sends the prompt or command.
  - While a turn runs, it queues the prompt for that turn.
  - Before the first session is open, it queues the prompt for that session.
  - Queued messages show in one shaded block above the working row, with `↳ queued · ctrl+enter to send now · up to edit` under it.
- Ctrl+Enter sends now: it stops the current response and continues the turn with the queued messages plus the draft.
- Alt+Enter, Shift+Enter or Ctrl+J inserts a newline. Ctrl+Enter and Shift+Enter need a terminal that reports modified Enter.
- Up at the top of the input pulls queued messages back into it for editing; with nothing queued it browses input history, and Down goes back.
- Esc interrupts the running turn.
- Ctrl+C interrupts the running turn, else clears the draft, else a second press within two seconds exits.
- After an interrupt, queued messages return to the input; `/new`, `/resume` and exiting discard them.
- Ctrl+D exits when the input is empty.
- Shift+Tab switches between plan and execute.
- Ctrl+B moves the running foreground `Bash` command to the background, where it keeps running as a [task](tools.md#background-tasks).
  - While one runs, the working row shows `ctrl+b to background`.
  - With none running, Ctrl+B keeps its meaning in the input.
- Ctrl+V pastes text or an image from the clipboard.
- Mouse wheel or PgUp/PgDn scrolls the transcript.
- Ctrl+O expands every group and call output until pressed again.

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
- Known commands, skill commands (wherever they appear in the input) and `@` references are highlighted in the input.
- An `@path` is sent as written; a space in an inserted path is written `\ `.

## Mouse

- Click a folded group to open it, and a call to show its output.
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
