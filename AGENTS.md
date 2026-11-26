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

- PiG is Kopi's runtime: the agent loop, sessions and their files, providers and sign-in, compaction and retry, the MCP client and skill parsing are PiG's. Use PiG's public API for anything in that territory. Hand-write only what PiG keeps internal, what it does only through ambient discovery, or what would bypass `internal/policy`, `tool.Workspace` or `tool.Shell`.
- Only `internal/pigruntime` (and tests) import `github.com/MichaelKinsy/PiG`; depguard enforces it.
- `internal/pigruntime` may use PiG's `mcp` client packages and, from `coding/mcpext`, only what works without the extension: a server connection, OAuth credentials and sign-in, and result conversion. Never register the extension, load its config, or use its tool definitions, commands or prompt sections: they read PiG's `mcp.json` and run tools outside `Gate.Admit`.
- Model-written scripts run only through `pigruntime.RunScript`, over PiG's `codemode`.
- Never import PiG `internal/` packages, never use `coding/cli`, never add a `replace` for PiG, and never copy upstream code.
- Create every PiG session with every input explicit: agent dir, in-memory settings, `coding.NoResources`, the system prompt as sections, `SkipBuiltinTools`, and explicit tool names for both `AllowedTools` and `InitialActiveToolNames`. A nil there turns on ambient discovery, PiG's stock prompt or stock tools. A root session's `AllowedTools` also takes one `mcp__<server>__*` pattern for each MCP server whose tools can join it later (none is added when nothing can load them). The one exception is the session `Runtime.DefaultModel` creates only to read PiG's default model: in memory, no model, `coding.NoResources`, `NoTools: "all"`, never prompted, closed at once. Never name a prompt part `tools` or `rules`; PiG rewrites those.
- The runtime has one PiG extension of Kopi's own, which holds tool registrations only: no handlers, commands, flags or resources. A root session's tools that are not always callable (MCP tools, `WebSearch`, `Workflow`) are registered there with deferred exposure, so a tool can join an open session and only Kopi makes a tool callable. Their calls go through `Gate.Before` and `Gate.Admit` like any other. A subagent's or workflow agent's session takes every tool at open and none later.
- Skills are loaded by a PiG resource loader that is never attached to a session and is created with every input explicit and the project untrusted; sessions keep `coding.NoResources`.
- List sessions with `Runtime.Sessions` and observe a session with `Subscribe`. Never call `Session.ListSessions` (it resolves PiG's default agent dir, not the runtime's) or `Session.Events` (an undrained channel stalls the agent loop).
- A workaround for PiG behaviour lives in `internal/pigruntime` with a comment naming that behaviour. Recheck them when PiG is bumped.
- `internal/pigruntime`'s exported API is stated in Kopi's terms; nothing exported may exist only because of how PiG works internally.

## Authority

- Every model-facing tool goes through `internal/policy`: `Gate.Before` in the pre-dispatch hook and `Gate.Admit` inside execution.
- Skills load only from `$KOPI_HOME/skills`, `~/.agents/skills` and a trusted folder's `.agents/skills`; the `Skill` tool reads only a loaded skill's text and files inside the directory that skill was loaded from, and on every read refuses a file in Kopi's home outside `skills/`.
- Subagent definitions load only from `$KOPI_HOME/agents`, once at startup. A subagent's session has its own gate over the same rules and shares the mode holder unless its definition pins plan mode. A subagent never gets `Agent`, `SendMessage`, `Workflow`, `AskUserQuestion`, `ExitPlanMode` or `TaskStop`, never reaches the plan file, and its `Bash` never backgrounds.
- `Workflow` is offered only in the fullscreen interface and becomes callable only when a human prompt in the main session contains `ultracode`. A workflow agent's session is opened as a subagent's is, from the same definitions, and never gets `Workflow`; the script reaches Kopi only through `agent()`.
- A command the human types with `!` is control input, not a tool call: it runs through `tool.Shell` outside the sandbox and the gate, in both modes, only from the fullscreen composer and only for the main session. Model output, `Controller.Prompt`, print mode, skills and subagents never run one.
- Only human control input changes mode: `/plan`, `/execute`, Shift+Tab and Yes in the `ExitPlanMode` dialog. Sessions start and resume in the configured `mode` (default plan); `--mode` overrides the config file.
- A session never waits for MCP servers. A server's tools join the root session when it settles, and what the servers say about themselves joins once the last has settled; a session opened later has them from the start, and a subagent or workflow agent keeps what it opened with. Only a prompt and print mode wait: a prompt sent while servers connect waits for them at most 2 seconds after start, print mode at most 10 seconds before its one turn. No state exists in which there is no session yet, except the first-run login.
- MCP servers start only from `mcpServers` in `config.json` and from a trusted folder's `.mcp.json`. Every MCP tool call asks unless a rule allows it or the opt-in classifier reviews an unmatched call; deny rules take precedence, and explicit and built-in ask rules always ask the human. A tool is Read only when its server marks it read-only.
- `$KOPI_HOME/AGENTS.md` always loads. Project instruction files (`AGENTS.md`, else the first name in `instructionFallbacks`, one per directory from the git root down to the start directory) load only from a trusted folder. An added directory's, found by the same walk, reach the model as a message, only when the folder is trusted.
- The workspace is the start directory plus the directories the human adds with `/add-dir` or `--add-dir`, until `/remove-dir` removes it or Kopi exits; each is confined, ruled and sandboxed as the start directory is. Only human control input adds or removes one. An added directory is resolved once, into the spelling it was given in and its real path; the rules, the sandbox, trust and the model text take them from there.
- File-modifying tools act only through `tool.Workspace` (os.Root confinement, Kopi's home refused before any side effect, except the root session's plan file `$KOPI_HOME/plans/<session id>.md`, which the root's `Read`, `Write` and `Edit` reach in both modes without a default ask, and the session's saved output files under `$KOPI_HOME/outputs/<session id>/`, which only `Read` and `Grep` reach, one file at a time, for the root, its subagents and workflow agents) and the permission rules. `Bash` runs commands only through `tool.Shell`: under the OS sandbox when it is enabled or available, unless the call sets `unsandboxed` and `sandbox.allowUnsandboxed` allows it; such a call, and every call without the sandbox, runs only after approval or an allow rule. In plan mode `Bash` runs only under the sandbox's plan profile (workspace read-only), chosen from the mode `Gate.Admit` returns; without the sandbox, or for an `unsandboxed` call, plan mode refuses it.
- The sandbox is what lets `Bash` run unasked. Without it Kopi adds nothing in its place: no command analysis, no notice, and no tool, tool description or preamble text offered only then. `Bash` then runs only as the bullet above says; `Read`, `Grep`, `Glob` and `LS` are enough alone to explore the workspace and are the same with and without the sandbox.

## State

- Kopi reads and writes only under its home (`$KOPI_HOME`, default `~/.kopi`). Never read or write `~/.pi`, `~/.pig`, `~/.claude` or any other tool's store, in code or in tests. A shared place outside the project (see Built in or configured), such as `~/.agents/skills`, is not another tool's store; Kopi only reads it.
- `trust.json` under Kopi's home holds trust decisions; the binary never writes `config.json`.
- `plans/` under Kopi's home holds one plan file per root session; Kopi never deletes them.
- `outputs/` under Kopi's home holds tool output too long to return whole, every background command's output and the output of the human's `!` commands, one folder per root session; Kopi removes files older than a week at startup.
- Tests use `t.TempDir()`, synthetic credentials and no network, and synchronise on channels, never sleeps.

## Built in or configured

- The binary owns how Kopi works: mechanics, tool contracts and their limits, model-facing text about Kopi's own tools, the default look, and protection of Kopi's home and of the gate itself. These are constants, not config keys.
- The binary assumes nothing that differs from one user's machine to the next: no built-in list of paths outside Kopi's home, other tools' stores, credential locations, providers, models or personal rules. The user states those in `config.json`. A shared place outside the project, such as `~/.agents/skills`, is the exception, and Kopi only reads it.
- What agents share in one format and place is read there, in that format: a project's `AGENTS.md`, `.agents/skills` and `~/.agents/skills`, `.mcp.json` and the `mcpServers` entry shape. Kopi's home holds only what has no shared place, such as the global `AGENTS.md`, or what only Kopi reads. A place is shared when most peers read it. Kopi adds no fields of its own to a shared format; its extras live in `config.json`.
- A behaviour users reasonably set differently is a `config.json` key with its default in `config.Defaults()`. A flag that overrides a key is a config layer over the file, never a separate code path.
- Add a key only for a setting someone needs to change, never to move mechanics out of the binary.
- A number in the binary bounds what one call does or returns, or stops a runaway, and is a constant. The exception is a number sized to something outside Kopi (a model's context, a slow server): it becomes a key when someone hits it, not before.
- When it is unclear which side something is on, check what peer agents build in and follow the majority.

## Models

- The session model is `model`, else PiG's default model for a provider with a credential, chosen at each start and never saved; `/model` replaces it until Kopi exits. Code reads it at the moment of use and never keeps the value it had at start.
- A model key (`smallModel`, `classifier.model`, `workflows.model`, a definition's `model`) only picks a model; unset, it falls back at the moment of use, ending at the session model, so it follows `/model`. It never switches a feature on or off: a feature key does.
- A feature that calls a model takes it from a model key or the session model and resolves it per call, or once for a session it opens.
- What a model can do (reason and at which levels, search the web, read images) is read from the model resolved for that use, never from the one Kopi started with. A level the model lacks is clamped and an image it cannot read is left out; a call it cannot serve at all fails naming the model. A tool that exists only for a capable model is offered or withdrawn where the model is resolved: when a session opens and when `/model` switches it.
- Only `/model` changes the model of an open session, and only the main one's. What was asked of the session (its effort) is kept and applied to the new model, clamped to what it has; within a run a limit of the old model is never carried over. A subagent or workflow agent keeps the model and tools it opened with; one started later resolves again.
- A set model key is checked at start: its format always, its model and credential when its feature can run, and `smallModel`'s always. `/model` checks its argument the same way.
- What Kopi reads per provider is not fixed at start: plan limits are read for the session model's provider and follow `/model`, and a provider's model list is refreshed once a model of it is first used.

## Model-facing text

- The text Kopi ships, which users can add to but not change, is minimal: it says what exists and how it works, enough for the agent to use it. It steers only where one of Kopi's own mechanisms has a single intended use that a model gets wrong untold: plan mode ends in a plan presented for approval, an approved plan is carried out, a task whose end is reported is not polled, delegated work is not redone, a timeout is not a yield. Such steering is one sentence in the narrowest place. Preferences are the user's to state in their instruction files. For a subagent tool, say what subagents are and how a call works; do not say to delegate as much as possible.
- A shipped sentence is the same for every user, is one an agent with an empty Kopi home needs to operate, and sits in the narrowest place: the result it applies to, else the tool's description, else the preamble. A hint naming the tool's own way forward (`use offset=N to continue`) says how it works and stays.
- A fact is stated once, in the narrowest place: the result it applies to, else the parameter it describes, else the tool's description, else the preamble. The preamble never lists tools; the tools a session is given are the list.
- The prompt of a model Kopi itself calls (the approval classifier, `WebFetch`, `WebSearch`) is part of that feature's implementation and the binary's; users add to the classifier's through `classifier.instructions`.
- The system prompt is built once per process from named parts, ordered from what changes least to most: Kopi's text, `$KOPI_HOME/AGENTS.md`, a trusted folder's instruction files, what MCP servers report. Nothing in it depends on the time, git state, the mode or the turn. The last part is the only one replaced while a session is open, once, when the last server has settled; a session resumed before then keeps the part its transcript has; Kopi's own text never depends on what servers report, only on whether any is configured.
- Never rewrite text the model has been sent: a mid-session change replaces or removes one named part through `internal/pigruntime`, or arrives as a message. Mode is enforced by the gate, not by prompt text.
- Whatever is listed for the model (tools, servers, files) is in a fixed order: sorted, or the order discovery defines; never map order.
- Instruction files are read once at startup, an added directory's when it is added. `internal/instruction` finds them; PiG places them.
- Skills are read once at startup; `internal/skill` finds the project folders and PiG loads them. The `Skill` tool's description lists them, and a skill a user names arrives as a message with that prompt.
- Always-loaded text is what every session needs. Guidance for one tool goes in its description; what is needed only sometimes loads on demand, as deferred MCP tools do, and `Workflow` on its keyword.
- Text is injected on an event, once; no per-turn reminders. Hidden text counts as told when the session reports that the model read it, never when it is handed over.
- Human input reaches the model in the order typed; the transcript shows what is in the session in session order, and input not in yet is shown above the composer until the event for its being read.
- Kopi sets no size budget on instruction files; text from an MCP server is capped.

## TUI

- A session screen ends with a footer: the status line, with the agent and workflow rows under it while any show. A dialog takes the footer's place along with the input's, so its bottom rule is the last row of whatever screen it is on, a session's or one of its own. A dialog always draws both rules; do not add a dialog variant for what surrounds it. A notice shown while a dialog is open joins its hints.
- Which prompt is in front is decided only by `front()` in `internal/tui/prompt.go`: the open prompt of the earliest kind, the first to arrive among them. A covered prompt stays open. No other code tests a prompt's kind to decide what shows or takes input.
- A prompt kind is one type implementing `prompt`, a `promptKind` and one `open` call. It fills a `dialog`'s slots and never cuts, windows or offsets rows: only `dialog.rows` decides height, scrolling and field areas.
- A prompt keeps what it renders for a width and theme (a body, a preview); `dialog.rows` only slices it, so a frame's cost does not grow with the content.
- A dialog's key hints are one row, the last above its bottom rule. A prompt lists its own as `hints`, the way out last, each `<key> to <verb>`, from `binding` where a binding names the key (the way out is `binding(km.dismiss, …)`). Only `dialog.rows` orders and joins them: the scroll hint first, while the body or pane is cut, then the hints, then the notice last; only the dialog's host sets the scroll hint and the notice. A `note` is text, never keys; only `hintRows` colours a hint. Every dialog names its way out.
- A menu's argument candidates are a list the menu filters or, for a command marked `Typed`, asked again from the controller on every change and shown as given. A candidate ending in `/` is a step, filled in and not run, for `@` and arguments alike. While a `Typed` reply is pending, the keys that pick a row wait for it and apply to its rows.
- Dialog text with no colour of its own is drawn in the theme's `text`.
- In a transcript row, a line's text starts `step` columns past the text of the line it sits under: a `↳` line under its title, a body under the last of the two. Only `outline` in `internal/tui/render.go` places a line and no caller names a column; a tool's `Display` hands over text flush left and never a column, and a preview has a sign column only when it has added or removed lines.
- A row's colour is a neutral or a signal: `text` for what the row is, `muted` for content shown on request and for running, `dim` for detail to skip, green for success, yellow for a person or a rule saying no or cutting it short, red for a failure that ran. The `↳` line takes the bullet's colour only when the row did not succeed; no accent colour appears on a row.
- A prompt for a request closes only on that request's own `…Ended` event, which `internal/app` sends once for every request. It never closes on `TurnEnd` or `SessionChanged`.

## Style

- Keep implementations simple. Prefer an established, maintained library over hand-rolled code when one fits; do not add one for something trivial.
- Wrap errors with `%w` and word them `failed to …`. Put a package's sentinel errors in its `errors.go`.
- Log with `log/slog` only.
- Test with testify `assert`/`require` and hand-written fakes. Each test file pairs with its source file. Test helpers shared across packages live in a `<pkg>test` sub-package.
- Comments carry only what the code cannot say.
