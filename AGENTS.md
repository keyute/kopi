# Kopi guidelines

## Workflow

- You must always use Taskfile to build, test, lint and format: `task build`, `task test`, `task lint`, `task fmt`. Tests run with `-race`; `task test:norace` runs them without it, for allocation checks the race detector distorts.
- Never commit, tag or push.

## Layout

- Root `main.go` only calls `cmd.Execute()`.
- `cmd/` holds one file per cobra subcommand, each as `func xCmd() *cobra.Command`; a nested subcommand lives in its parent's file. No `init()`.
- Everything else lives under `internal/<noun>`.
- Behaviour lives in `internal/app`, the frontend-neutral controller. Frontends (`internal/tui`, `internal/headless`) only pass input in and render events.

## PiG boundary

- Only `internal/pigruntime` (and tests) import `github.com/MichaelKinsy/PiG`; depguard enforces it.
- `internal/pigruntime` may use PiG's `mcp` client packages; never PiG's MCP extension `coding/mcpext`.
- Never import PiG `internal/` packages, never use `coding/cli`, never add a `replace` for PiG, and never copy upstream code.
- Create every PiG session with every input explicit: agent dir, in-memory settings, `coding.NoResources`, `SkipBuiltinTools`, and explicit tool names for both `AllowedTools` and `InitialActiveToolNames`. A nil there turns on ambient discovery or stock tools.
- Never call PiG's `Session.ListSessions` or `Session.Events`.
- `internal/pigruntime`'s exported API is stated in Kopi's terms; nothing exported may exist only because of how PiG works internally.

## Authority

- Every model-facing tool goes through `internal/policy`: `Gate.Before` in the pre-dispatch hook and `Gate.Admit` inside execution.
- Only human control input changes mode. Sessions start and resume in the configured `mode` (default plan); `--mode` overrides the config file.
- MCP servers start only from `mcpServers` in `config.json` and from a trusted folder's `.mcp.json`. Every MCP tool call asks unless a rule allows it; a tool is Read only when its server marks it read-only.
- File-modifying tools act only through `tool.Workspace` (os.Root confinement, Kopi's home refused before any side effect) and the permission rules. `Bash` runs commands only through `tool.Shell`: under the OS sandbox when it is enabled or available, otherwise only after approval or an allow rule. In plan mode `Bash` runs only under the sandbox's plan profile (workspace read-only), chosen from the mode `Gate.Admit` returns; without the sandbox, plan mode refuses it.

## State

- Kopi reads and writes only under its home (`$KOPI_HOME`, default `~/.kopi`). Never read or write `~/.pi`, `~/.pig`, `~/.claude` or any other tool's store, in code or in tests.
- `trust.json` under Kopi's home holds trust decisions; the binary never writes `config.json`.
- Tests use `t.TempDir()`, synthetic credentials and no network, and synchronise on channels, never sleeps.

## Built in or configured

- The binary owns how Kopi works: mechanics, tool contracts and their limits, model-facing text about Kopi's own tools, the default look, and protection of Kopi's home and of the gate itself. These are constants, not config keys.
- The binary assumes nothing that differs from one user's machine to the next: no built-in list of paths outside Kopi's home, other tools' stores, credential locations, providers, models or personal rules. The user states those in `config.json`.
- A behaviour users reasonably set differently is a `config.json` key with its default in `config.Defaults()`. A flag that overrides a key is a config layer over the file, never a separate code path.
- Add a key only for a setting someone needs to change, never to move mechanics out of the binary.
- When it is unclear which side something is on, check what peer agents build in and follow the majority.

## Style

- Keep implementations simple. Prefer an established, maintained library over hand-rolled code when one fits; do not add one for something trivial.
- Wrap errors with `%w` and word them `failed to …`. Put a package's sentinel errors in its `errors.go`.
- Log with `log/slog` only.
- Test with testify `assert`/`require` and hand-written fakes. Each test file pairs with its source file. Test helpers shared across packages live in a `<pkg>test` sub-package.
- Comments carry only what the code cannot say.
