# Kopi

Kopi is a coding-agent CLI built on [PiG](https://github.com/MichaelKinsy/PiG).
It is early: tools to read, write and edit files and to run commands, tools from MCP servers, a fullscreen terminal interface, and a print mode that answers one prompt.

## Build and run

Needs Go (version in `go.mod`) and [Task](https://taskfile.dev/) to build; the built binary needs nothing else at runtime.

```sh
task build
bin/kopi login <provider>          # list providers with: bin/kopi login
bin/kopi --model provider/id
bin/kopi --model provider/id -c    # continue the latest session in this directory
bin/kopi --model provider/id -r <id>
bin/kopi --model provider/id -p "explain main.go"
bin/kopi --model provider/id --mode execute --allowed-tools "Edit(docs/**)" -p "fix the typos in docs"
bin/kopi mcp list                  # the MCP servers this directory uses and their tools
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
- `trust.json` holds which folders you trust (see MCP servers); Kopi writes it and never writes `config.json`.
- `history.jsonl` holds input history per working directory, written by the fullscreen interface; the last 100 entries are recalled.
- `images/` holds pasted clipboard images, removed after a day.
- `agent/` is PiG's agent directory, created on first run: its credential file (`auth.json`), session logs (`sessions/`), other PiG state and an optional `models.json`.

`mode` is the mode sessions start in: `plan` (default) or `execute`.

`keys` maps each fullscreen action to a list of key strings, and `statusline` lists the status line's `left` and `right` segments from `model`, `context`, `branch`, `mode`, `notice`, which shows transient hints such as `Copied`, and `sandbox`, which shows `sandboxed` or `unsandboxed` when Bash is available (default left `model, context, branch`, right `notice, sandbox, mode`):

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

`sandbox` configures the OS sandbox `Bash` runs in (see Modes and tools):

```json
{
  "version": 1,
  "sandbox": {
    "allowWrite": ["~/Library/Caches/go-build"],
    "denyRead": ["~/.ssh"],
    "network": false
  }
}
```

- `sandbox.enabled`: `true` refuses to start when the sandbox is unavailable; `false` runs `Bash` without it; unset uses it when available and otherwise logs a warning and runs `Bash` without it.
- `sandbox.allowWrite` and `sandbox.denyRead` take plain paths, no globs: `/abs` is absolute, `~/x` is under the home directory, and anything else is relative to the start directory, so `.env` means only `<start directory>/.env`, unlike in a rule.
- A later layer's `sandbox.allowWrite` or `sandbox.denyRead` replaces an earlier one's whole.
- `sandbox.network` (default `false`) allows IP networking in the sandbox.

## Modes and tools

- Sessions are in plan or execute mode; only the user switches, with `/plan`, `/execute` or Shift+Tab. Plan mode allows `Read`, read-only MCP tools, and `Bash` only in the sandbox, where it cannot write the start directory; `Write`, `Edit` and other MCP tools need execute mode.
- Sessions start and resume in the configured `mode` (default plan), including ones opened with `/new` and `/resume`; `--mode` overrides the config file.
- `Read` reads text files inside the start directory: up to 2000 lines, 2000 characters per line and 100 KiB per call; binary files and directories are refused.
- `Write` creates a file or replaces its whole content; `Edit` replaces a string that must match exactly once unless `replace_all` is set, in a text file of up to 10 MiB. Both work inside the start directory, replace the file atomically and keep its mode.
- `Read`, `Write` and `Edit` refuse Kopi's home, including paths there that do not exist yet.
- `Bash` runs a command with `bash -c` (`/bin/sh` when `bash` is not on `PATH`) in the start directory, with Kopi's environment. Every call is a fresh shell with no input; `cd` and variables do not carry over.
- `Bash`'s `timeout` is in milliseconds, default 120000, at most 600000. On timeout or interrupt the command's whole process group is killed (on Windows, only the shell).
- `Bash` returns stdout and stderr combined, keeping the first and last 15,000 bytes. A non-zero exit is an error reporting the exit code.
- On macOS `Bash` runs inside an OS sandbox (`sandbox-exec`), configured with `sandbox` (see Configuration).
- In the sandbox a command can write only in the start directory, `/tmp`, the temp directory and the `sandbox.allowWrite` paths, and never the files of the start directory's `.git` that choose git's config and hooks (`hooks`, `config`, `config.worktree`, `commondir`, `worktrees`, `modules`); when the start directory has no `.git`, a command cannot write `HEAD` in it. In plan mode nothing in the start directory is writable, including `sandbox.allowWrite` paths inside it.
- In the sandbox a command cannot create, write, rename or remove a file named `.mcp.json`, in any letter case and in any directory, nor the file the start directory's `.mcp.json` links to at start; git operations that change a `.mcp.json` therefore fail in the sandbox.
- In the sandbox a command can read everything except Kopi's home and the `sandbox.denyRead` paths, which it also cannot write, rename or remove.
- In the sandbox a command has no network unless `sandbox.network` is true, which opens IP networking only; Unix sockets stay blocked.

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

- A rule is `Name` or `Name(path)`. The name is a tool name (`Read`, `Write`, `Edit`, `Bash`), an MCP tool name (see MCP servers) or `*` for every tool; an unknown name or a malformed rule is rejected at start.
- `/abs` is absolute, `~/x` is under the home directory, and anything else is relative to the start directory. `*` and `**` glob. A name with no slash (`.env`) matches at any depth, and a directory covers everything in it. Paths match case-insensitively on macOS and Windows.
- `Name(!path)` exempts the calls it matches from the other rules in its own list, from the same config layer or flag; it never reaches rules from anywhere else.
- A bare `Name` in `deny` removes the tool from the model.
- `Bash` takes no path: only a bare `Bash` or `*` rule applies to it, `Bash(path)` is rejected at start, and path rules such as `*(~/.ssh)` never apply to `Bash`. `"ask": ["Bash"]` makes every command ask.
- Plan mode refuses `Write`, `Edit`, unsandboxed `Bash` and MCP tools not marked read-only before any rule; otherwise rules apply to `Bash` the same in both modes.
- Deny beats ask, and ask beats allow. With no rule matching, `Read` and sandboxed `Bash` are allowed and any other call asks, including every `Bash` command without the sandbox.
- Built in: `Edit` and `Write` under any `.git` directory, and of any file named `.mcp.json` or the file the start directory's `.mcp.json` links to, always ask unless a deny rule refuses them.
- `--allowed-tools` and `--disallowed-tools` add allow and deny rules to the config's; each is repeatable and takes comma-separated rules (commas inside `()` or `{}` do not split).
- A rule's path counts as a match on either the path as written or its resolved target when it denies, asks or exempts from an allow, and only on both when it allows or exempts from a deny or ask.

The fullscreen interface asks before a call that needs approval, showing the change or the command (the status line's `sandbox` segment shows whether `Bash` runs sandboxed): `Yes`, `Yes, allow all edits this session` (for `Write` and `Edit` when no rule asked), or `No`, which takes an optional reason typed on its row. While it waits, the working row reads `Waiting for you…`. `No` with a reason refuses the call, gives the model the reason and lets the turn go on; a blank `No` stops the turn. The `menu-up` and `menu-down` keys choose and `submit` answers; a digit answers with that option, or moves to `No` to type the reason; `interrupt` answers `No` without a reason. In the reason, `newline` starts a line, `paste` and terminal paste insert text, and digits are text. Allowing all edits never overrides a deny or ask rule, never covers `Bash`, and lasts until the session ends. Print mode cannot ask, so it denies those calls, and the refusal says to add an allow rule, such as `--allowed-tools Bash`.

## MCP servers

`mcpServers` in `config.json` declares MCP servers; its value has the shape of the `mcpServers` object of a `.mcp.json`:

```json
{
  "version": 1,
  "mcpServers": {
    "context7": { "command": "npx", "args": ["-y", "@upstash/context7-mcp"], "env": { "CONTEXT7_API_KEY": "${CONTEXT7_API_KEY}" } },
    "github": { "type": "http", "url": "https://api.githubcopilot.com/mcp/", "headers": { "Authorization": "Bearer ${GITHUB_TOKEN}" } }
  }
}
```

- A stdio server has `command`, optional `args` and `env` (added to Kopi's environment), and `type` absent or `"stdio"`. A remote server has `"type": "http"` (streamable HTTP), `url` and optional `headers`.
- `${VAR}` and `${VAR:-default}` expand in `command`, `args`, `env` values, `url` and `headers` values. An unset variable with no default fails that server only.
- A `.mcp.json` in the start directory adds its `mcpServers`, only once you trust the folder; a server there replaces the `config.json` server of the same name whole. Its unknown keys are ignored, and a server of an unsupported type (`sse`, `ws`) fails alone.
- When the start directory's `.mcp.json` declares a server and you have not decided about the folder, the fullscreen interface lists its servers and asks before the session opens: `Yes, trust this folder`, `Yes, trust the parent folder` (not offered when the parent is the root or your home directory), or `No, don't trust`. The answer is saved in `trust.json` and covers the folders below; the closest saved folder decides. In your home directory or the root no parent is offered, and `Yes, trust this folder` holds for this run only and is not saved. Cancelling saves nothing and skips the servers for this run.
- Print mode never asks: in a folder with no decision it skips the `.mcp.json` servers and warns on stderr.
- To change a saved answer, remove the folder's entry from `trust.json`.
- Servers start at startup; Kopi waits up to 10 seconds for them before the session opens. A server that fails or is late is reported and its tools are absent for the run; the fullscreen interface shows the reasons in the transcript, print mode on stderr.
- A server's tools are named `mcp__<server>__<tool>`, with characters outside `A-Z`, `a-z`, `0-9`, `_` and `-` turned into `_`. The normalized server component must be nonempty, must not contain `__`, and must not end in `_`, so a whole-server permission prefix cannot match another server. Invalid server names fail before connecting. A name over 64 characters, or one that two tools share, is skipped and reported.
- A tool the server marks read-only (`readOnlyHint`) works in plan mode; any other works only in execute mode.
- Every MCP call asks unless a rule allows it, read-only or not, and print mode denies it without one. A rule names one tool, `mcp__context7__resolve-library-id`, or ends in `*` to cover a prefix: `mcp__context7__*` for a whole server, `mcp__*` for every server. A prefix must be `mcp__` or run past the server name's `__`, so one that ends inside a server name, such as `mcp__context*`, is rejected at start. MCP rules take no path, and are accepted for servers that are not configured here.
- Server instructions are added to the system prompt, at most 2 KB each. Results are text, capped like other tool output; images and other content show as placeholders.
- `kopi mcp list` starts the servers this directory uses, lists each with where it is declared, its state and its tools (`read-only` or `execute`), and exits. It never asks: undecided and untrusted folders' `.mcp.json` servers show as not trusted and are not started.

## Limitations

- Permission rules are not an OS boundary: they match the path a call names. Only `Bash` runs under the OS sandbox.
- The sandbox exists only on macOS; elsewhere it is unavailable.
- The sandbox is unavailable when Kopi itself runs inside a Seatbelt sandbox, because profiles do not nest; for the same reason tools that apply their own sandbox (`swift build`, `xcodebuild` packages) fail inside it.
- MCP servers run outside the sandbox, with your permissions and Kopi's environment, including provider API keys.
- Sandboxed commands inherit Kopi's environment, including provider API keys, and can read any file outside Kopi's home and `sandbox.denyRead`.
- With `sandbox.network` true, commands reach the network without asking, in plan mode too.
- Tools that write outside the start directory and temp directories, such as Go's build cache and package-manager caches, fail in the sandbox until those paths are in `sandbox.allowWrite`.
- In the sandbox, `git init` in the start directory, `git config` writes, `git worktree add`, commits in a linked worktree (its git directory is outside the start directory), commits inside a submodule and commit signing through an agent socket fail.
- The sandbox does not protect the config and hooks of repositories nested inside the start directory, or of the start directory itself when it is a bare repository or a git directory.
- Only the start directory's `.mcp.json` is resolved, once, at start: the file a nested folder's `.mcp.json` links to under another name stays writable. `Edit` and `Write` do not recognise that target when its name is spelled in a different Unicode normalization.
- When the start directory's `.mcp.json` links to a directory, sandboxed commands cannot write anything under that directory, so a link to the start directory itself or to `/` stops them writing there; when it links to a missing file, they cannot create the missing directories on the way to it, so `.mcp.json` linking to `build/mcp.json` stops `mkdir build`.
- A command can move into a write root a directory that already holds a `.mcp.json`, and a folder moved in takes the trust of its new parent.
- An MCP tool an allow rule covers can change `.mcp.json` and `.git` files without asking, as MCP rules take no path.
- The files a `.mcp.json` server runs, such as `./server.sh` or `node_modules/.bin/…`, stay writable.
- A `.mcp.json` changed outside Kopi, such as by `git pull` or an editor, starts its servers without asking again.
- In execute mode, sandboxed commands can write anything else in the start directory, so starting Kopi in a directory holding files other programs run, such as the home directory, lets commands change them.
- Plan mode keeps only the start directory read-only: commands can still write `/tmp`, the temp directory and `sandbox.allowWrite` paths outside it, and act over the network when `sandbox.network` is true. In plan mode `kopi -p` runs sandboxed `Bash` without asking. Commands started in execute mode, including background processes they leave running, keep their execute-mode access after a switch to plan mode.
- `ps`, `pgrep` and `top` do not work in the sandbox.
- Unix sockets (ssh-agent, Docker) are unreachable from the sandbox, even with `sandbox.network` true.
- A background process a command leaves running (`cmd &`) can outlive the call. A running command keeps running if Kopi is killed.
- `sandbox-exec` is deprecated by Apple.
- Kopi builds for macOS, Linux and Windows only.
- Symlinks in rule paths, sandbox paths, the start directory and the home directory are resolved once, at start.
- `Edit` detects identity or content changes at its pinned destination immediately before replacement and refuses detected conflicts. This is best-effort, not atomic compare-and-swap: reads can race and another writer can change the file between the check and rename. `Write` replaces unconditionally.
- `Write` and `Edit` pin and check parent-directory handles before making changes, so replacing a parent name with a symlink cannot redirect those changes. Another process can still move a checked directory, including into Kopi's home; later changes follow its handle. Permission checks and these protections are not an OS boundary.
- `Read` protection is by file identity, re-checked on every read: a hard link into Kopi's home or a credential copied elsewhere is readable, whether Kopi's home exists is observable, and if Kopi's home cannot be inspected all reads are refused.
- Kopi refuses no credential locations outside its home; deny them with rules, such as `*(~/.ssh)`, and to `Bash` with `sandbox.denyRead`.
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

Project-level config, loading project instruction files, a Linux sandbox, running commands from the input, network allowlists by domain, allowing chosen Unix sockets, command-pattern rules for `Bash`, a per-command way out of the sandbox, streaming command output, saved approvals, a `/permissions` command, approval classification, delegation and workflows, background jobs, a Kopi provider identity, release packaging and self-update, and login for providers that need an installation device ID (`openai`).

MCP: tool search and deferred tool exposure, OAuth for remote servers, the SSE transport, resources and prompts, `tools/list_changed`, image results, sandboxing servers or giving them a restricted environment, and a command or flag to set or change a folder's trust.

Recalled history and re-edited queued messages show their full text rather than paste blocks.

## Contributing

Kopi is opinionated and built around how I work, so I don't accept feature requests or feature PRs. Bug reports are welcome as issues, and bug fixes are the only contributions I consider.

## Credits

Built on [PiG](https://github.com/MichaelKinsy/PiG) by Michael Kinsy (MIT). Other dependencies are listed in `go.mod`.

The Seatbelt policy in `internal/sandbox/codex/` is adapted from [OpenAI Codex](https://github.com/openai/codex) (Apache-2.0); its `LICENSE` and `NOTICE` are in that directory.

## License

MIT, see `LICENSE`.
