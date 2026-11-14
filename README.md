# Kopi

Kopi is a coding-agent CLI built on [PiG](https://github.com/MichaelKinsy/PiG).
It is early: one read-only tool, a fullscreen terminal interface, and a line interface for piped use.

## Build and run

Needs Go (version in `go.mod`) and [Task](https://taskfile.dev/) to build; the built binary needs nothing else at runtime.

```sh
task build
bin/kopi login <provider>          # list providers with: bin/kopi login
bin/kopi --model provider/id
bin/kopi --model provider/id -c    # continue the latest session in this directory
bin/kopi --model provider/id -r <id>
```

| Command | Effect |
| --- | --- |
| `/plan` | Switch to plan mode |
| `/execute` | Switch to execute mode |
| `/status` | Show mode, model, session, running work and tools |
| `/new` | Start a new session |
| `/resume [id]` | Resume a session, or list this directory's sessions |
| `/help` | List commands |
| `/quit` | Exit |
| `//text` | Send a prompt that starts with `/` |

- When stdin and stdout are both terminals, Kopi opens a fullscreen interface; with piped or redirected stdin or stdout it uses the line interface.
- With piped stdin, lines run in order, each prompt's turn finishes before the next line is read, and Kopi exits at EOF.
- Resuming (`-c`, `-r` or `/resume`) redraws the earlier conversation in the fullscreen interface, with its tool activity folded.
- On exit from a session with messages, the fullscreen interface prints the command to resume it.

Keys in the fullscreen interface:

- Enter sends the prompt or command.
- Alt+Enter, Shift+Enter (in terminals with key disambiguation) or Ctrl+J inserts a newline.
- Esc interrupts the running turn.
- Ctrl+C interrupts the running turn; at idle it exits. In the line interface it does the same.
- Mouse wheel or PgUp/PgDn scrolls the transcript.
- Click a folded group to open it, and a call to show its output.
- Ctrl+O expands every group and call output until pressed again.

While the interface captures the mouse, selecting text needs the terminal's modifier-drag (Shift, or Option in some terminals).

## Configuration

Kopi keeps all its state in its home: `$KOPI_HOME`, default `~/.kopi`.

```json
{ "version": 1, "model": "openai-codex/gpt-5.6-luna" }
```

- `config.json` in the home; precedence is default < config file < flags.
- `kopi config` shows each value and its origin; unknown keys are rejected.
- The fullscreen interface logs to `kopi.log` in the home; the line interface logs to stderr.
- `agent/` is PiG's agent directory, created on first run: its credential file (`auth.json`), session logs (`sessions/`), other PiG state and an optional `models.json`.

## Modes and tools

- Sessions are in plan or execute mode; only the user switches, with `/plan` and `/execute`.
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

Manual live check: `kopi login openai-codex`, run with a model, ask it to read a file, then try `/execute`, `/plan`, `/status`, Ctrl+C and `--continue`.

## Not implemented

Project-level config and trust, loading project instruction files, sandboxed shell and file-modifying tools, approval classification, delegation and workflows, background jobs, MCP, a Kopi provider identity, release packaging and self-update, and login for providers that need an installation device ID (`openai`).

The fullscreen interface has no queued input while a turn runs, no in-app text selection and copy, and no slash-command completion menu.

## Contributing

Kopi is opinionated and built around how I work, so I don't accept feature requests or feature PRs. Bug reports are welcome as issues, and bug fixes are the only contributions I consider.

## Credits

Built on [PiG](https://github.com/MichaelKinsy/PiG) by Michael Kinsy (MIT). Other dependencies are listed in `go.mod`.

## License

MIT, see `LICENSE`.
