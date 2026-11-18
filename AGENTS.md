# Kopi guidelines

## Workflow

- You must always use Taskfile to build, test, lint and format: `task build`, `task test`, `task lint`, `task fmt`. Tests run with `-race`; `task test:norace` runs them without it, for allocation checks the race detector distorts.
- Never commit, tag or push.
- README.md stays a landing page (install, quick start, doc links); user-facing behaviour, its limitations and what is not implemented go in the matching `docs/<topic>.md`.

## Layout

- Root `main.go` only calls `cmd.Execute()`.
- `cmd/` holds one file per cobra subcommand, each as `func xCmd() *cobra.Command`; a nested subcommand lives in its parent's file. No `init()`.
- Everything else lives under `internal/<noun>`.
- Behaviour lives in `internal/app`, the frontend-neutral controller. Frontends (`internal/tui`, `internal/headless`) only pass input in and render events.

## PiG boundary

- Only `internal/pigruntime` (and tests) import `github.com/MichaelKinsy/PiG`; depguard enforces it.
- `internal/pigruntime` may use PiG's `mcp` client packages; never PiG's MCP extension `coding/mcpext`.
- Never import PiG `internal/` packages, never use `coding/cli`, never add a `replace` for PiG, and never copy upstream code.
- Create every PiG session with every input explicit: agent dir, in-memory settings, `coding.NoResources`, the system prompt as sections, `SkipBuiltinTools`, and explicit tool names for both `AllowedTools` and `InitialActiveToolNames`. A nil there turns on ambient discovery, PiG's stock prompt or stock tools. Never name a prompt part `tools` or `rules`; PiG rewrites those.
- Skills are loaded by a PiG resource loader that is never attached to a session and is created with every input explicit and the project untrusted; sessions keep `coding.NoResources`.
- Never call PiG's `Session.ListSessions` or `Session.Events`.
- `internal/pigruntime`'s exported API is stated in Kopi's terms; nothing exported may exist only because of how PiG works internally.

## Authority

- Every model-facing tool goes through `internal/policy`: `Gate.Before` in the pre-dispatch hook and `Gate.Admit` inside execution.
- Skills load only from `$KOPI_HOME/skills`, `~/.agents/skills` and a trusted folder's `.agents/skills`; the `Skill` tool reads only a loaded skill's text and files inside the directory that skill was loaded from, and on every read refuses a file in Kopi's home outside `skills/`.
- Only human control input changes mode. Sessions start and resume in the configured `mode` (default plan); `--mode` overrides the config file.
- MCP servers start only from `mcpServers` in `config.json` and from a trusted folder's `.mcp.json`. Every MCP tool call asks unless a rule allows it or the opt-in classifier reviews an unmatched call; deny rules take precedence, and explicit and built-in ask rules always ask the human. A tool is Read only when its server marks it read-only.
- `$KOPI_HOME/AGENTS.md` always loads. Project instruction files (`AGENTS.md`, else the first name in `instructionFallbacks`, one per directory from the git root down to the start directory) load only from a trusted folder.
- File-modifying tools act only through `tool.Workspace` (os.Root confinement, Kopi's home refused before any side effect) and the permission rules. `Bash` runs commands only through `tool.Shell`: under the OS sandbox when it is enabled or available, otherwise only after approval or an allow rule. In plan mode `Bash` runs only under the sandbox's plan profile (workspace read-only), chosen from the mode `Gate.Admit` returns; without the sandbox, plan mode refuses it.

## State

- Kopi reads and writes only under its home (`$KOPI_HOME`, default `~/.kopi`). Never read or write `~/.pi`, `~/.pig`, `~/.claude` or any other tool's store, in code or in tests. A shared place outside the project (see Built in or configured), such as `~/.agents/skills`, is not another tool's store; Kopi only reads it.
- `trust.json` under Kopi's home holds trust decisions; the binary never writes `config.json`.
- Tests use `t.TempDir()`, synthetic credentials and no network, and synchronise on channels, never sleeps.

## Built in or configured

- The binary owns how Kopi works: mechanics, tool contracts and their limits, model-facing text about Kopi's own tools, the default look, and protection of Kopi's home and of the gate itself. These are constants, not config keys.
- The binary assumes nothing that differs from one user's machine to the next: no built-in list of paths outside Kopi's home, other tools' stores, credential locations, providers, models or personal rules. The user states those in `config.json`. A shared place outside the project, such as `~/.agents/skills`, is the exception, and Kopi only reads it.
- What agents share in one format and place is read there, in that format: a project's `AGENTS.md`, `.agents/skills` and `~/.agents/skills`, `.mcp.json` and the `mcpServers` entry shape. Kopi's home holds only what has no shared place, such as the global `AGENTS.md`, or what only Kopi reads. A place is shared when most peers read it. Kopi adds no fields of its own to a shared format; its extras live in `config.json`.
- A behaviour users reasonably set differently is a `config.json` key with its default in `config.Defaults()`. A flag that overrides a key is a config layer over the file, never a separate code path.
- Add a key only for a setting someone needs to change, never to move mechanics out of the binary.
- When it is unclear which side something is on, check what peer agents build in and follow the majority.

## Model-facing text

- The text Kopi ships, which users can add to but not change, is minimal: it says what exists and how it works, enough for the agent to use it, and never how to behave. Preferences are the user's to state in their instruction files. For a subagent tool, say what subagents are and how a call works; do not say to delegate as much as possible.
- The system prompt is built once per process from named parts, ordered from what changes least to most: Kopi's text, `$KOPI_HOME/AGENTS.md`, a trusted folder's instruction files, what MCP servers report. Nothing in it depends on the time, git state, the mode or the turn.
- Never rewrite text the model has been sent: a mid-session change replaces or removes one named part through `internal/pigruntime`, or arrives as a message. Mode is enforced by the gate, not by prompt text.
- Whatever is listed for the model (tools, servers, files) is in a fixed order: sorted, or the order discovery defines; never map order.
- Instruction files are read once at startup. `internal/instruction` finds them; PiG places them.
- Skills are read once at startup; `internal/skill` finds the project folders and PiG loads them. The `Skill` tool's description lists them, and a skill a user names arrives as a message with that prompt.
- Always-loaded text is what every session needs. Guidance for one tool goes in its description; what is needed only sometimes loads on demand, as deferred MCP tools do.
- Text is injected on an event, once; no per-turn reminders.
- Kopi sets no size budget on instruction files; text from an MCP server is capped.

## TUI

- Every screen ends with a footer row: the status line in a session, key hints on a screen of its own. A dialog sits directly above it and always draws both rules; do not add a dialog variant for what surrounds it.

## Style

- Keep implementations simple. Prefer an established, maintained library over hand-rolled code when one fits; do not add one for something trivial.
- Wrap errors with `%w` and word them `failed to …`. Put a package's sentinel errors in its `errors.go`.
- Log with `log/slog` only.
- Test with testify `assert`/`require` and hand-written fakes. Each test file pairs with its source file. Test helpers shared across packages live in a `<pkg>test` sub-package.
- Comments carry only what the code cannot say.
