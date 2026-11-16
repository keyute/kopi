# Kopi

Kopi is a coding-agent CLI built on [PiG](https://github.com/MichaelKinsy/PiG).
It is early: tools to read, write and edit files, a fullscreen terminal interface, and a print mode that answers one prompt.

## Build and run

Needs Go (version in `go.mod`) and [Task](https://taskfile.dev/) to build; the built binary needs nothing else at runtime.

```sh
task build
bin/kopi login <provider>          # list providers with: bin/kopi login
bin/kopi logout <provider>
bin/kopi --model provider/id
bin/kopi --model provider/id -c    # continue the latest session in this directory
bin/kopi --model provider/id -r <id>
bin/kopi --model provider/id -p "explain main.go"
bin/kopi --model provider/id --mode execute --allowed-tools "Edit(docs/**)" -p "fix the typos in docs"
git diff | bin/kopi --model provider/id -p "review this"
```

Commands in the fullscreen interface:

| Command | Effect |
| --- | --- |
| `/plan` | Switch to plan mode |
| `/execute` | Switch to execute mode |
| `/new` | Start a new session |
| `/resume <id>` | Resume a session |
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
- `/` at the start of the input lists commands; picking `/resume` from the menu lists this directory's sessions, which you filter by date or first message.
- `@` at the start of the input or after a space lists files and directories under the start directory, leaving out `.git`, anything a `.gitignore` in the start directory or below ignores and Kopi's home.
- While that menu shows, Up and Down choose, Tab fills in, Enter fills in and runs a command (a command that takes an argument, or a file reference, is only filled in), and Esc closes the menu.
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
- `installation_id` holds the installation's UUID, created the first time a sign-in asks for it.
- `agent/` is PiG's agent directory, created on first run: its credential file (`auth.json`), session logs (`sessions/`), other PiG state and an optional `models.json`.

`mode` is the mode sessions start in: `plan` (default) or `execute`.

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

## Providers

- `kopi login` lists the providers that offer a login, with their methods and the ones Kopi holds a credential for; `kopi login <provider>` runs one.
- A provider offers an account sign-in, a stored API key or both; with both, Kopi asks which. An API key is read hidden on a terminal, or as a line from piped stdin.
- A sign-in prints its URL and opens it in the browser; when the browser does not open, use the printed URL.
- `kopi logout <provider>` removes the provider's credential from `agent/auth.json`.
- A credential in the environment works without a login; `kopi login` marks only stored credentials as logged in.
- After an `anthropic` account sign-in Kopi notes that Anthropic bills third-party tools per token as extra usage, not from Claude plan limits.
- Sign-in introduces the app as `Kopi` to OpenAI's Sign in with ChatGPT (`openai`), which also receives `installation_id`, and as `kopi` to other sign-ins that take a name.
- Requests carry no PiG attribution headers.
- Still PiG's: the page a browser sign-in returns to shows pi's logo, the `radius` sign-in URL names `pi-gateway` on `radius.pi.dev`, and xAI's sign-in sends `referrer: pig`.

## Modes and tools

- Sessions are in plan or execute mode; only the user switches, with `/plan`, `/execute` or Shift+Tab. Plan mode allows only `Read`.
- Sessions start and resume in the configured `mode` (default plan), including ones opened with `/new` and `/resume`; `--mode` overrides the config file.
- `Read` reads text files inside the start directory: up to 2000 lines, 2000 characters per line and 100 KiB per call; binary files and directories are refused.
- `Write` creates a file or replaces its whole content; `Edit` replaces a string that must match exactly once unless `replace_all` is set, in a text file of up to 10 MiB. Both work inside the start directory, replace the file atomically and keep its mode.
- All three refuse Kopi's home, including paths there that do not exist yet.

Permission rules in `config.json` allow, ask about or deny calls:

```json
{
  "permissions": {
    "allow": ["Edit(docs/**)"],
    "ask": ["Edit(**/*.sql)"],
    "deny": ["*(.env*)", "Read(!.env.example)", "*(~/.ssh)", "Write(/etc/**)"]
  }
}
```

- A rule is `Name` or `Name(path)`. The name is a tool name (`Read`, `Write`, `Edit`) or `*` for every tool; an unknown name or a malformed rule is rejected at start.
- `/abs` is absolute, `~/x` is under the home directory, and anything else is relative to the start directory. `*` and `**` glob. A name with no slash (`.env`) matches at any depth, and a directory covers everything in it. Paths match case-insensitively on macOS and Windows.
- `Name(!path)` exempts the calls it matches from the other rules in its own list, from the same config layer or flag; it never reaches rules from anywhere else.
- A bare `Name` in `deny` removes the tool from the model.
- Deny beats ask, and ask beats allow. With no rule matching, `Read` is allowed and any other call asks.
- Built in: `Edit` and `Write` under any `.git` directory always ask.
- `--allowed-tools` and `--disallowed-tools` add allow and deny rules to the config's; each is repeatable and takes comma-separated rules (commas inside `()` or `{}` do not split).
- A rule's path counts as a match on either the path as written or its resolved target when it denies, asks or exempts from an allow, and only on both when it allows or exempts from a deny or ask.

The fullscreen interface asks before a call that needs approval, showing the change: `Yes`, `Yes, allow all edits this session` (for `Write` and `Edit` when no rule asked), or `No`, which stops the turn. The `menu-up` and `menu-down` keys choose and `submit` answers, a digit answers with that option, and `interrupt` answers `No`. Allowing all edits never overrides a deny or ask rule and lasts until the session ends. Print mode cannot ask, so it denies those calls, and the refusal says to add an allow rule.

## Limitations

- Kopi is not an OS sandbox, and permission rules are not an OS boundary: they match the path a call names.
- Symlinks in rule paths, the start directory and the home directory are resolved once, at start.
- `Edit` detects identity or content changes at its pinned destination immediately before replacement and refuses detected conflicts. This is best-effort, not atomic compare-and-swap: reads can race and another writer can change the file between the check and rename. `Write` replaces unconditionally.
- `Write` and `Edit` pin and check parent-directory handles before making changes, so replacing a parent name with a symlink cannot redirect those changes. Another process can still move a checked directory, including into Kopi's home; later changes follow its handle. Permission checks and these protections are not an OS boundary.
- `Read` protection is by file identity, re-checked on every read: a hard link into Kopi's home or a credential copied elsewhere is readable, whether Kopi's home exists is observable, and if Kopi's home cannot be inspected all reads are refused.
- Kopi refuses no credential locations outside its home; deny them with rules, such as `*(~/.ssh)`.
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

Manual live check: `kopi login openai-codex`, run with a model, ask it to read a file, then try `/execute`, `/plan`, Ctrl+C and `--continue`; then `kopi -p` with an argument and with piped input. For sign-in, with a scratch `KOPI_HOME`: `kopi login` lists `openai` and `openai-codex`; `kopi login openai-codex` opens the browser with `originator=kopi` in the URL; `kopi login openai` has `agent_name_hint=Kopi` and `ext_agent_host_id=urn:uuid:<installation_id>` in the URL and OpenAI's consent screen names Kopi; `kopi logout openai-codex`, then a run suggests `kopi login`.

## Not implemented

Project-level config and trust, loading project instruction files, a sandboxed shell, saved approvals, a `/permissions` command, approval classification, delegation and workflows, background jobs, MCP, a Kopi provider identity, and release packaging and self-update.

Recalled history and re-edited queued messages show their full text rather than paste blocks.

## Contributing

Kopi is opinionated and built around how I work, so I don't accept feature requests or feature PRs. Bug reports are welcome as issues, and bug fixes are the only contributions I consider.

## Credits

Built on [PiG](https://github.com/MichaelKinsy/PiG) by Michael Kinsy (MIT). Other dependencies are listed in `go.mod`.

## License

MIT, see `LICENSE`.
