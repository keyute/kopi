# Kopi

Kopi is a coding-agent CLI built on [PiG](https://github.com/MichaelKinsy/PiG).
It is early: one read-only tool, a fullscreen terminal interface, and a print mode that answers one prompt.

## Build and run

Needs Go (version in `go.mod`) and [Task](https://taskfile.dev/) to build; the built binary needs nothing else at runtime.

```sh
task build
bin/kopi login <provider>          # list providers with: bin/kopi login
bin/kopi --model provider/id
bin/kopi --model provider/id -c    # continue the latest session in this directory
bin/kopi --model provider/id -r <id>
bin/kopi --model provider/id -p "explain main.go"
git diff | bin/kopi --model provider/id -p "review this"
```

Commands in the fullscreen interface:

| Command | Effect |
| --- | --- |
| `/plan` | Switch to plan mode |
| `/execute` | Switch to execute mode |
| `/new` | Start a new session |
| `/resume [id]` | Resume a session, or list this directory's sessions |
| `/quit` | Exit |
| `//text` | Send a prompt that starts with `/` |

- When stdin and stdout are both terminals and `-p` is not given, Kopi opens a fullscreen interface; otherwise it prints the response to one prompt and exits.
- The prompt is the argument, piped stdin, or both (stdin, a blank line, then the argument). It is always sent as a prompt, never run as a command. Only the final message of the reply is printed.
- Ctrl+C interrupts the turn; a failed or interrupted turn exits with status 1. `-c` and `-r` work with `-p`.
- Resuming (`-c`, `-r` or `/resume`) redraws the earlier conversation in the fullscreen interface, with its tool activity folded.
- On exit from a session with messages, the fullscreen interface prints the command to resume it.

Keys in the fullscreen interface (defaults, see Configuration):

- Enter sends the prompt or command; while a turn runs it queues the prompt for that turn. Queued messages show in one shaded block above the working row, with `↳ queued · ctrl+enter to send now · up to edit` under it.
- Ctrl+Enter sends now: it stops the current response and continues the turn with the queued messages plus the draft.
- Alt+Enter, Shift+Enter or Ctrl+J inserts a newline. Ctrl+Enter and Shift+Enter need a terminal that reports modified Enter.
- Up at the top of the input pulls queued messages back into it for editing; with nothing queued it browses input history, and Down goes back.
- `/` at the start of the input lists commands; a space after `/resume` lists this directory's sessions.
- `@` at the start of the input or after a space lists files and directories under the start directory, leaving out `.git`, anything `.gitignore` ignores and the protected paths.
- While that menu shows, Up and Down choose, Tab fills in, Enter fills in and runs a command (a file reference is only filled in), and Esc closes the menu.
- Known commands and `@` references are highlighted in the input. An `@path` is sent as written; a space in an inserted path is written `\ `.
- Esc interrupts the running turn.
- Ctrl+C interrupts the running turn, else clears the draft, else a second press within two seconds exits.
- After an interrupt, queued messages return to the input; `/new`, `/resume` and exiting discard them.
- Ctrl+D exits when the input is empty.
- Shift+Tab switches between plan and execute.
- Ctrl+V pastes text or an image from the clipboard.
- Mouse wheel or PgUp/PgDn scrolls the transcript.
- Ctrl+O expands every group and call output until pressed again.

Mouse:

- Click a folded group to open it, and a call to show its output.
- Drag in the transcript to select text; it is copied on release. Double-click selects a word, triple-click a line.
- Drag in the input to select text; it is copied on release. A click places the cursor. Double-click selects a word, triple-click the whole line across wraps.
- Dragging after a double- or triple-click extends the selection by words or lines.
- Copies go to the terminal (OSC 52) and the system clipboard. The terminal's own modifier-drag selection (Shift, or Option in some terminals) still works.

Paste and images:

- A paste over 10 lines or 1000 characters collapses to `[paste #N +L lines]` or `[paste #N C chars]` and is sent in full. Pasting the same text again right after expands it in place.
- An absolute path to an image file pasted or dropped into the input, or a clipboard image pasted with Ctrl+V, becomes `[image #N]` and is sent as an attachment: PNG, JPEG, GIF or WebP, up to 20 MiB.
- Backspace or Delete next to a paste or image block removes it whole.
- Clipboard images need `osascript` (macOS), `wl-paste` (Wayland) or `xclip` (X11).

## Configuration

Kopi keeps all its state in its home: `$KOPI_HOME`, default `~/.kopi`.

```json
{ "version": 1, "model": "openai-codex/gpt-5.6-luna" }
```

- `config.json` in the home; precedence is default < config file < flags.
- `kopi config` shows each value and its origin; unknown keys are rejected.
- The fullscreen interface logs to `kopi.log` in the home; print mode logs to stderr.
- `history.jsonl` holds input history per working directory, written by the fullscreen interface; the last 100 entries are recalled.
- `images/` holds pasted clipboard images, removed after a day.
- `agent/` is PiG's agent directory, created on first run: its credential file (`auth.json`), session logs (`sessions/`), other PiG state and an optional `models.json`.

`keys` maps each fullscreen action to a list of key strings, and `statusline` lists the status line's `left` and `right` segments from `model`, `context`, `branch`, `mode` and `notice`, which shows transient hints such as `Copied` (default left `model, context, branch`, right `notice, mode`):

```json
{
  "version": 1,
  "keys": { "send-now": ["ctrl+s"] },
  "statusline": { "left": ["model", "branch"], "right": ["notice", "context", "mode"] }
}
```

| Action | Default |
| --- | --- |
| `submit` | `enter` |
| `newline` | `shift+enter`, `alt+enter`, `ctrl+j` |
| `send-now` | `ctrl+enter` |
| `interrupt` | `esc` |
| `cancel` | `ctrl+c` |
| `quit` | `ctrl+d` |
| `toggle-mode` | `shift+tab` |
| `paste` | `ctrl+v` |
| `history-prev`, `history-next` | `up`, `down` |
| `queue-edit` | `up` |
| `complete` | `tab` |
| `menu-up`, `menu-down` | `up`, `down` |
| `menu-close` | `esc` |
| `expand-all` | `ctrl+o` |
| `page-up`, `page-down` | `pgup`, `pgdown` |

- Setting one action replaces that action's list and leaves the others at their defaults.
- An unknown action or segment, an empty list or a blank key string is rejected at start; a key string that names no real key never matches.

## Modes and tools

- Sessions are in plan or execute mode; only the user switches, with `/plan`, `/execute` or Shift+Tab.
- Every session starts in plan, including a resumed one.
- `Read` reads text files inside the start directory: up to 2000 lines, 2000 characters per line and 100 KiB per call; binary files and directories are refused.
- `Read` refuses Kopi's home and `~/.ssh`, `~/.aws`, `~/.gnupg`, `~/.config/gcloud`, `~/.config/gh`, `~/.netrc`, `~/.git-credentials`, `~/.npmrc`, `~/.docker`, `~/.kube`, `~/.azure`, `~/.pi`, `~/.pig`, `~/.claude`, `~/.claude.json` and `~/.codex`.
- Execute mode unlocks nothing yet: there is no shell or file-modifying tool.

## Limitations

- Kopi is not an OS sandbox.
- `Read` protection is by file identity, re-checked on every read: a hard link into a protected store or a credential copied elsewhere is readable, whether a store exists is observable, and if a protected location cannot be inspected all reads are refused.
- `Read` detects binary files from their first 8 KiB and refuses absolute symlinks, even ones pointing inside the start directory.
- PiG's built-in providers still use ambient credentials: API keys in the environment, Google application-default credentials and the AWS profile and credential chain.
- Importing PiG changes the TLS settings of Go's process-wide default HTTP transport.
- `agent/models.json` is trusted configuration: PiG reads it, and its `apiKey` can run a shell command.
- Provider requests identify as PiG, not Kopi.

## Development

```sh
task test   # with -race
task lint   # needs golangci-lint
task fmt
```

Tests are mock-backed (PiG's faux provider and a local HTTP mock server); none use a live provider.

Manual live check: `kopi login openai-codex`, run with a model, ask it to read a file, then try `/execute`, `/plan`, Ctrl+C and `--continue`; then `kopi -p` with an argument and with piped input.

## Not implemented

Project-level config and trust, loading project instruction files, sandboxed shell and file-modifying tools, approval classification, delegation and workflows, background jobs, MCP, a Kopi provider identity, release packaging and self-update, and login for providers that need an installation device ID (`openai`).

Recalled history and re-edited queued messages show their full text rather than paste blocks.

## Contributing

Kopi is opinionated and built around how I work, so I don't accept feature requests or feature PRs. Bug reports are welcome as issues, and bug fixes are the only contributions I consider.

## Credits

Built on [PiG](https://github.com/MichaelKinsy/PiG) by Michael Kinsy (MIT). Other dependencies are listed in `go.mod`.

## License

MIT, see `LICENSE`.
