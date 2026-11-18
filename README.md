# Kopi

Kopi is a coding-agent CLI built on [PiG](https://github.com/MichaelKinsy/PiG).
It is early: tools to read, search, write and edit files and to run commands, tools from MCP servers, a fullscreen terminal interface, and a print mode that answers one prompt.

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
| `/tasks` | List background tasks, show one's output and stop it |
| `/quit` | Exit |
| `//text` | Send a prompt that starts with `/` |

- When stdin and stdout are both terminals and `-p` is not given, Kopi opens a fullscreen interface; otherwise it prints the response to one prompt and exits.
- The prompt is the argument, piped stdin, or both (stdin, a blank line, then the argument). It is always sent as a prompt, never run as a command. Only the final message of the reply is printed.
- Ctrl+C interrupts the turn; a failed or interrupted turn exits with status 1. `-c` and `-r` work with `-p`.
- Resuming (`-c`, `-r` or `/resume`) redraws the earlier conversation in the fullscreen interface, with its tool activity folded.
- On exit from a session with messages, the fullscreen interface prints the command to resume it.

Keys in the fullscreen interface (defaults, see Configuration):

- Enter sends the prompt or command; while a turn runs it queues the prompt for that turn, and before the first session is open it queues the prompt for that session. Queued messages show in one shaded block above the working row, with `↳ queued · ctrl+enter to send now · up to edit` under it.
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
- Ctrl+B moves the running foreground `Bash` command to the background, where it keeps running as a task; while one runs, the working row shows `ctrl+b to background`. With none running, Ctrl+B keeps its meaning in the input.
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
- `AGENTS.md` in the home, when present, is added to the system prompt in every folder, whether or not you trust it.
- `trust.json` holds which folders you trust (see MCP servers); Kopi writes it and never writes `config.json`.
- `history.jsonl` holds input history per working directory, written by the fullscreen interface; the last 100 entries are recalled.
- `images/` holds pasted clipboard images, removed after a day.
- `installation_id` holds the installation's UUID, created the first time a sign-in asks for it.
- `agent/` is PiG's agent directory, created on first run: its credential file (`auth.json`), session logs (`sessions/`), other PiG state and an optional `models.json`.

`mode` is the mode sessions start in: `plan` (default) or `execute`.

`smallModel` (`provider/id`) is the model that answers `WebFetch` calls; unset, it is `model`, so set it to a cheaper model. A wrong value, an unknown model or one whose provider has no credential fails at start.

`instructionFallbacks` lists bare file names a directory's project instruction file is read from when it has no `AGENTS.md`, the first present winning (default `["CLAUDE.md"]`); `[]` turns the fallback off. Project instruction files load only in a trusted folder (see MCP servers).

`keys` maps each fullscreen action to a list of key strings, and `statusline` lists the status line's `left` and `right` segments from `model`, `context`, `branch`, `mode`, `notice`, which shows transient hints such as `Copied`, and `sandbox`, which shows `sandboxed` or `unsandboxed` when Bash is available, and `tasks`, which shows `1 shell` or `N shells` while background tasks run (default left `model, context, branch`, right `notice, sandbox, tasks, mode`):

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
| `background` | `ctrl+b` |
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

- Sessions are in plan or execute mode; only the user switches, with `/plan`, `/execute` or Shift+Tab. Plan mode allows `Read`, `Glob`, `Grep`, `LS`, `ToolSearch`, `TaskOutput`, `TaskStop`, `WebFetch`, read-only MCP tools, and `Bash` only in the sandbox, where it cannot write the start directory; `Write`, `Edit` and other MCP tools need execute mode.
- Sessions start and resume in the configured `mode` (default plan), including ones opened with `/new` and `/resume`; `--mode` overrides the config file. A mode switched to while the first session is opening is the mode it starts in.
- `Read` reads text files inside the start directory: up to 2000 lines, 2000 characters per line and 100 KiB per call. It returns PNG, JPEG, GIF and WebP images whole, up to 20 MiB (one that cannot be processed is left out with a note); `offset` and `limit` do not apply to them. Other binary files and directories are refused.
- `Glob` finds files under a directory inside the start directory by glob (`*`, `?`, `**`, `{a,b}`; a pattern without `/` matches names at any depth), up to 100 paths, most recently modified first.
- `Grep` searches the contents of a file or the files under a directory with an RE2 regular expression. `output_mode` is `files_with_matches` (default), `content` (with up to 10 `context` lines) or `count`; `limit` is 100 by default, at most 1000; `glob` filters the files searched and `case_insensitive` ignores case. Content output is capped at 100 KiB, like `Read`'s.
- `LS` lists one directory, not recursively, sorted by name with directories ending in `/`, up to 500 entries, including hidden and gitignored entries and `.git/`.
- `Glob` and `Grep` skip `.git` directories below the searched path, anything a `.gitignore` in the start directory or below ignores, symlinks and, for `Grep`, binary files; a `path` inside `.git` or inside an ignored directory is searched. None of the three follows a symlink it finds while searching or listing (`LS` lists them, except one whose destination cannot be resolved); a `path` that is itself a symlink is resolved, as for `Read`.
- `Write` creates a file or replaces its whole content; `Edit` replaces a string that must match exactly once unless `replace_all` is set, in a text file of up to 10 MiB. Both work inside the start directory, replace the file atomically and keep its mode.
- `Read`, `Write` and `Edit` refuse Kopi's home, including paths there that do not exist yet. `Glob`, `Grep` and `LS` refuse a path there and leave Kopi's home out of what they search or list.
- `Bash` runs a command with `bash -c` (`/bin/sh` when `bash` is not on `PATH`) in the start directory, with Kopi's environment. Every call is a fresh shell with no input; `cd` and variables do not carry over.
- `Bash`'s `timeout` is in milliseconds, default 120000, at most 600000. A command still running at its timeout keeps running as a background task, as does one started with `run_in_background` or moved with the `background` key; the result names the task. On interrupt a foreground command's whole process group is killed (on Windows, only the shell).
- `Bash` returns stdout and stderr combined, keeping the first and last 15,000 bytes. A non-zero exit is an error reporting the exit code.
- On macOS `Bash` runs inside an OS sandbox (`sandbox-exec`), configured with `sandbox` (see Configuration).
- In the sandbox a command can write only in the start directory, `/tmp`, the temp directory and the `sandbox.allowWrite` paths, and never the files of the start directory's `.git` that choose git's config and hooks (`hooks`, `config`, `config.worktree`, `commondir`, `worktrees`, `modules`); when the start directory has no `.git`, a command cannot write `HEAD` in it. In plan mode nothing in the start directory is writable, including `sandbox.allowWrite` paths inside it.
- In the sandbox a command cannot create, write, rename or remove a file named `.mcp.json`, in any letter case and in any directory, nor what the start directory's `.mcp.json` resolves to when it is a symlink; git operations that change a `.mcp.json` therefore fail in the sandbox.
- In the sandbox a command can read everything except Kopi's home and the `sandbox.denyRead` paths, which it also cannot write, rename or remove.
- In the sandbox a command has no network unless `sandbox.network` is true, which opens IP networking only; Unix sockets stay blocked.
- `WebFetch` fetches one http or https page on a public host and has `smallModel` answer the call's prompt from it; the main model gets the answer and the page's final URL, not the page. HTML is converted to markdown from its main content; text, JSON and XML pass through; other content types are refused. Pages are up to 5 MiB, fetched within 30 seconds and without JavaScript, and the small model reads their first 100,000 characters. Redirects on the same host (a leading `www.` and http to https count as the same) are followed, at most 5; any other redirect is returned for the model to fetch in a new call. `localhost`, names without a dot, `.localhost`, `.local` and `.internal` names, and non-public addresses are refused, including ones a name resolves to. Every call asks unless a rule allows it, in both modes.

Background tasks:

- Tasks are numbered `t1`, `t2`, … per run. They have no time limit and keep the sandbox profile of the mode they started in.
- `TaskOutput` returns a task's status and its output so far, kept like `Bash` output. `TaskStop` kills a task's process group (on Windows, only the shell), whichever mode it started in.
- When a task completes or fails, the model is told its status and its last 20 non-empty output lines at the next boundary of the running turn, or in a new turn when none runs. After an interrupt such notices wait for the next prompt; they never return to the input.
- A task stopped from `/tasks` is reported to the model along with the next prompt or the next task notice, without starting a turn; one the model stops is not reported again.
- A task that completes, fails or is stopped from `/tasks` adds a `• task t1 finished · <command> · 12s` row to the transcript (`failed` or `stopped` in place of `finished`), restored on resume.
- Tasks are stopped on exit, when `/new` or `/resume` opens another session, and at the end of `kopi -p`. `kopi -p` never starts a turn for a task's end, so a task still running at the final reply is killed.

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

- A rule is `Name` or `Name(path)`. The name is a tool name (`Read`, `Glob`, `Grep`, `LS`, `Write`, `Edit`, `Bash`, `TaskOutput`, `TaskStop`, `ToolSearch`, `WebFetch`), an MCP tool name (see MCP servers) or `*` for every tool; an unknown name or a malformed rule is rejected at start.
- `/abs` is absolute, `~/x` is under the home directory, and anything else is relative to the start directory. `*` and `**` glob. A name with no slash (`.env`) matches at any depth, and a directory covers everything in it. Paths match case-insensitively on macOS and Windows.
- `Name(!path)` exempts the calls it matches from the other rules in its own list, from the same config layer or flag; it never reaches rules from anywhere else.
- A bare `Name` in `deny` removes the tool from the model.
- `Glob`, `Grep` and `LS` leave out entries that a `deny` or `ask` rule with a path for that tool or `*` matches, except that a rule which also matches the searched path itself does not hide what is under it, as it already judged the whole call (unless a `!` exception in the same list is what admitted that path); `Read(path)` rules do not apply to them, so use `*(path)` to keep a path from every tool.
- `ToolSearch`, `TaskOutput`, `TaskStop` and `WebFetch` take no path: only a bare rule naming the tool or `*` applies to them; `ToolSearch(path)` and the like are rejected at start.
- `Bash` takes no path: only a bare `Bash` or `*` rule applies to it, `Bash(path)` is rejected at start, and path rules such as `*(~/.ssh)` never apply to `Bash`. `"ask": ["Bash"]` makes every command ask.
- Plan mode refuses `Write`, `Edit`, unsandboxed `Bash` and MCP tools not marked read-only before any rule; otherwise rules apply to `Bash` the same in both modes.
- Deny beats ask, and ask beats allow. With no rule matching, `Read`, `Glob`, `Grep`, `LS`, `ToolSearch`, `TaskOutput`, `TaskStop` and sandboxed `Bash` are allowed and any other call asks, including every `WebFetch` call and every `Bash` command without the sandbox.
- Built in: `Edit` and `Write` under any `.git` directory, and of any file named `.mcp.json` or the file the start directory's `.mcp.json` resolves to when it is a symlink, always ask unless a deny rule refuses them.
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
- A trusted folder's project instruction files are added to the system prompt after the home's `AGENTS.md`: one file per directory from the directory holding `.git` (a directory or a file) down to the start directory (its symlinks resolved first), outermost first, or the start directory alone when none holds `.git`. Each directory's file is `AGENTS.md`, else the first `instructionFallbacks` name present; a symlinked file is followed. Instruction files are read once at startup and have no size limit.
- When the start directory's `.mcp.json` declares a server or the folder has project instruction files, and you have not decided about the folder, the fullscreen interface lists its instruction files and servers and asks before the session opens: `Yes, trust this folder`, `Yes, trust the parent folder` (not offered when the parent is the root or your home directory), or `No, don't trust`. The answer is saved in `trust.json` and covers the folders below; the closest saved folder decides. In your home directory or the root no parent is offered, and the answer, yes or no, holds for this run only and is not saved. Cancelling saves nothing and skips the servers and instruction files for this run.
- Print mode never asks: in a folder with no decision it skips the `.mcp.json` servers and project instruction files and warns on stderr.
- To change a saved answer, remove the folder's entry from `trust.json`.
- Servers start at startup. The fullscreen interface shows at once with "Connecting to MCP servers…", and the session opens when the servers have settled or after 10 seconds; a prompt sent before then is queued and sent when the session opens. Print mode waits for the servers before it runs. A server that fails or is late is reported and its tools are absent for the run; the fullscreen interface shows the reasons in the transcript, print mode on stderr.
- A server's tools are named `mcp__<server>__<tool>`, with characters outside `A-Z`, `a-z`, `0-9`, `_` and `-` turned into `_`. The normalized server component must be nonempty, must not contain `__`, and must not end in `_`, so a whole-server permission prefix cannot match another server. Invalid server names fail before connecting. A name over 64 characters, or one that two tools share, is skipped and reported.
- A tool the server marks read-only (`readOnlyHint`) works in plan mode; any other works only in execute mode.
- Every MCP call asks unless a rule allows it, read-only or not, and print mode denies it without one. A rule names one tool, `mcp__context7__resolve-library-id`, or ends in `*` to cover a prefix: `mcp__context7__*` for a whole server, `mcp__*` for every server. A prefix must be `mcp__` or run past the server name's `__`, so one that ends inside a server name, such as `mcp__context*`, is rejected at start. MCP rules take no path, and are accepted for servers that are not configured here.
- The system prompt lists each server's tool names; a tool's schema reaches the model only after the model loads it with `ToolSearch`, by keywords or by exact names, at most 20 per keyword search. `ToolSearch` works in both modes and, with no rule matching, never asks. Each new or resumed session starts with no MCP tool loaded.
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
- When the start directory's `.mcp.json` is a symlink, what it resolves to when Kopi starts is protected like `.mcp.json` itself; when that is a directory, sandboxed commands cannot write anywhere in it. Another symlink on the way to it, a link that is broken at startup, a hard link that already exists, and the link targets of `.mcp.json` files in other folders are not protected.
- A command can move into a write root a directory that already holds a `.mcp.json`, and a folder moved in takes the trust of its new parent.
- An MCP tool an allow rule covers can change `.mcp.json` and `.git` files without asking, as MCP rules take no path.
- The files a `.mcp.json` server runs, such as `./server.sh` or `node_modules/.bin/…`, stay writable.
- A `.mcp.json` changed outside Kopi, such as by `git pull` or an editor, starts its servers without asking again.
- In execute mode, sandboxed commands can write anything else in the start directory, so starting Kopi in a directory holding files other programs run, such as the home directory, lets commands change them.
- Plan mode keeps only the start directory read-only: commands can still write `/tmp`, the temp directory and `sandbox.allowWrite` paths outside it, and act over the network when `sandbox.network` is true. In plan mode `kopi -p` runs sandboxed `Bash` without asking. Commands started in execute mode, including background tasks and processes they leave running, keep their execute-mode access after a switch to plan mode.
- `ps`, `pgrep` and `top` do not work in the sandbox.
- Unix sockets (ssh-agent, Docker) are unreachable from the sandbox, even with `sandbox.network` true.
- A background process a command leaves running (`cmd &`) can outlive the call, and its background task. A running command or background task keeps running if Kopi is killed.
- Background tasks and their output live only in memory and do not survive a restart.
- `sandbox-exec` is deprecated by Apple.
- Kopi builds for macOS, Linux and Windows only.
- Symlinks in rule paths, sandbox paths, the start directory and the home directory are resolved once, at start.
- `Edit` detects identity or content changes at its pinned destination immediately before replacement and refuses detected conflicts. This is best-effort, not atomic compare-and-swap: reads can race and another writer can change the file between the check and rename. `Write` replaces unconditionally.
- `Write` and `Edit` pin and check parent-directory handles before making changes, so replacing a parent name with a symlink cannot redirect those changes. Another process can still move a checked directory, including into Kopi's home; later changes follow its handle. Permission checks and these protections are not an OS boundary.
- `Read` and `Grep` protection is by file identity, re-checked on every read and, for the searches, on every directory searched: a hard link into Kopi's home or a credential copied elsewhere is readable, whether Kopi's home exists is observable, and if Kopi's home cannot be inspected all reads are refused.
- Kopi refuses no credential locations outside its home; deny them with rules, such as `*(~/.ssh)`, and to `Bash` with `sandbox.denyRead`.
- `Read` and `Grep` tell binary files, and `Read` images, from their first 8 KiB, not their names. `Read` refuses absolute symlinks, even ones pointing inside the start directory.
- `Grep` shows only the first 2000 characters of a line; a match past that shows the line cut to that prefix.
- PiG's built-in providers still use ambient credentials: API keys in the environment, Google application-default credentials and the AWS profile and credential chain.
- Importing PiG changes the TLS settings of Go's process-wide default HTTP transport.
- `agent/models.json` is trusted configuration: PiG reads it, and its `apiKey` can run a shell command.
- Provider requests identify as PiG, not Kopi.
- `WebFetch` connects directly and ignores `HTTP_PROXY` and `HTTPS_PROXY`; `sandbox.network` does not govern it.
- An approved `WebFetch` URL is requested from its host, and the call's prompt and up to 100,000 characters of the page go to `smallModel`'s provider.
- `WebFetch` runs no JavaScript, and only the `Content-Type` header decides a page's charset; without one, the type is sniffed from the body and it is read as UTF-8 unless it starts with a UTF-16 byte-order mark.

## Development

```sh
task test   # with -race
task lint   # needs golangci-lint
task fmt
```

Tests are mock-backed (PiG's faux provider and a local HTTP mock server); none use a live provider.

Manual live check: `kopi login openai-codex`, run with a model, ask it to read a file, then try `/execute`, `/plan`, Ctrl+C and `--continue`; then `kopi -p` with an argument and with piped input. For sign-in, with a scratch `KOPI_HOME`: `kopi login` lists `openai` and `openai-codex`; `kopi login openai-codex` opens the browser with `originator=kopi` in the URL; `kopi login openai` has `agent_name_hint=Kopi` and `ext_agent_host_id=urn:uuid:<installation_id>` in the URL and OpenAI's consent screen names Kopi; `kopi logout openai-codex`, then a run suggests `kopi login`. For background tasks: ask for `sleep 30; echo done` with Bash, press Ctrl+B while it runs, and see `1 shell` in the status line, the `• task t1 finished` row when it ends and the model's reply without a prompt; `/tasks` lists and stops a task; after quitting, `pgrep sleep` finds nothing.

## Not implemented

Project-level config, a Linux sandbox, running commands from the input, network allowlists by domain, allowing chosen Unix sockets, command-pattern rules for `Bash`, a per-command way out of the sandbox, streaming command output, saved approvals, a `/permissions` command, approval classification, delegation and workflows, a Kopi provider identity, domain rules for `WebFetch`, PDF and image pages, and release packaging and self-update.

MCP: OAuth for remote servers, the SSE transport, resources and prompts, `tools/list_changed`, image results, sandboxing servers or giving them a restricted environment, and a command or flag to set or change a folder's trust.

Recalled history and re-edited queued messages show their full text rather than paste blocks.

## Contributing

Kopi is opinionated and built around how I work, so I don't accept feature requests or feature PRs. Bug reports are welcome as issues, and bug fixes are the only contributions I consider.

## Credits

Built on [PiG](https://github.com/MichaelKinsy/PiG) by Michael Kinsy (MIT). Other dependencies are listed in `go.mod`.

The Seatbelt policy in `internal/sandbox/codex/` is adapted from [OpenAI Codex](https://github.com/openai/codex) (Apache-2.0); its `LICENSE` and `NOTICE` are in that directory.

## License

MIT, see `LICENSE`.
