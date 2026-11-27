# Permissions

## Rules

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

- A rule is `Name` or `Name(path)`, where the name is a tool name (`Read`, `Glob`, `Grep`, `LS`, `Write`, `Edit`, `Bash`, `Agent`, `SendMessage`, `Workflow`, `StructuredOutput`, `TaskStop`, `ToolSearch`, `Skill`, `WebFetch`, `WebSearch`, `AskUserQuestion`, `ExitPlanMode`), an [MCP tool name](mcp.md#plan-mode-and-rules) or `*` for every tool. An unknown name or malformed rule is rejected at start.
- Paths:
  - `/abs` is absolute, `~/x` is under the home directory, anything else is relative to the start directory.
  - A relative rule applies in each directory added with [`/add-dir`](usage.md#commands) too: `Edit(docs/**)` covers an added directory's `docs/`.
  - It reaches an added directory by the path it was added by and by its real path. A relative rule with `..` applies only from the start directory.
  - `*` and `**` glob. A name with no slash (`.env`) matches at any depth, and a directory covers everything in it.
  - Matching ignores case on macOS and Windows.
- `Name(!path)` exempts the calls it matches from the other rules in its own list (same config layer or flag), never from rules elsewhere.
- A bare `Name` in `deny` removes the tool from the model.
- A rule's path counts as a match:
  - on the path as written or its resolved target, when it denies, asks or exempts from an allow
  - only on both, when it allows or exempts from a deny or ask
- `--allowed-tools` and `--disallowed-tools` add allow and deny rules to the config's. Each is repeatable and takes comma-separated rules (commas inside `()` or `{}` do not split).

### Precedence

- The session's [plan file](tools.md#exitplanmode) is the one Kopi-home file `Read`, `Write` and `Edit` reach, in both modes, without asking by default. Deny and ask rules apply to it first; plan mode's refusal of `Write` and `Edit` does not.
- The session's [saved output](tools.md#saved-output) files are the other Kopi-home files `Read` and `Grep` reach, one at a time, in both modes, without asking by default. Rules apply as to any `Read`. `Write` and `Edit` refuse them.
- Plan mode refuses `Write`, `Edit`, unsandboxed `Bash` (including a call that sets `unsandboxed`) and MCP tools not marked read-only, before any rule. Rules apply to `Bash` the same in both modes.
- Deny beats ask, and ask beats allow.
- With no rule matching:
  - Allowed: `Read`, `Glob`, `Grep`, `LS`, `Skill`, `ToolSearch`, `Agent`, `SendMessage`, `Workflow`, `StructuredOutput`, `TaskStop`, `AskUserQuestion`, `ExitPlanMode` and sandboxed `Bash` without `unsandboxed`.
  - Anything else asks: every `WebFetch` and `WebSearch` call, every `Bash` command without the sandbox, and every `Bash` call that sets [`unsandboxed`](sandbox.md#outside-the-sandbox) where it is offered.
- Built in: `Edit` and `Write` under any `.git` directory, and of any `.mcp.json`, always ask unless a deny rule refuses them. This includes the file a symlinked `.mcp.json` in the start directory or an added directory resolves to.

### Rules per tool

- `Glob`, `Grep` and `LS` leave out entries that a `deny` or `ask` rule with a path for that tool or `*` matches.
  - Exception: a rule that also matches the searched path itself does not hide what is under it, as it already judged the call (unless a `!` exception in the same list admitted that path).
  - `Read(path)` rules do not apply to them; use `*(path)` to keep a path from every tool.
- `ToolSearch`, `Agent`, `SendMessage`, `Workflow`, `StructuredOutput`, `TaskStop`, `AskUserQuestion`, `ExitPlanMode`, `WebFetch` and `WebSearch` take no path: only a bare rule or `*` applies, and `Name(path)` is rejected at start.
- For `Skill` the path is the skill's directory, or the file when the call names one. A rule for a skill outside the start directory needs an absolute or `~/` path.
- `Bash` takes no path:
  - Only a bare `Bash` or `*` rule applies. `Bash(path)` is rejected at start; `*(~/.ssh)` never applies to it.
  - `"ask": ["Bash"]` makes every command ask.
  - An allow rule that applies also allows calls that set [`unsandboxed`](sandbox.md#outside-the-sandbox).

## Approval dialog

The fullscreen interface asks before a call that needs approval.

- It shows the whole change, command or arguments, scrolling when needed. An `unsandboxed` call's title reads `Bash (unsandboxed) …`.
- Options:
  - `Yes`
  - `Yes, allow all edits this session` (for `Write` and `Edit` when no rule asked)
  - `No`, with an optional reason typed on its row
- `No` with a reason refuses the call, gives the model the reason and lets the turn go on. A blank `No` stops the turn.
- A refused call's look is under [Transcript](usage.md#transcript).
- Keys:
  - `menu-up` and `menu-down` choose; `submit` answers.
  - A digit answers with that option, or moves to `No` to type the reason.
  - In the reason, `newline` starts a line, `paste` and terminal paste insert text, and digits are text.
  - `interrupt` answers `No` without a reason.
  - `page-up`, `page-down` and the mouse wheel scroll a preview that does not fit, else the transcript.
- Allowing all edits never overrides a deny or ask rule, never covers `Bash`, and lasts until the session ends.
- When the [classifier](#classifier) left a call to you, the dialog shows why (`the classifier would deny this: <reason>` or `the classifier could not decide`, with `: <reason>` when it gave one) and omits `Yes, allow all edits this session`.
- Interrupting during a review stops it without asking.
- Print mode cannot ask, so it denies those calls, including one the classifier leaves to you and one an ask rule matches. A call no rule matched, or the classifier could not decide, is refused with a hint to add an allow rule, such as `--allowed-tools Bash`, except for a `Bash` call that sets `unsandboxed`.
- Print mode runs the calls the classifier allows without an allow rule.

## Subagents

A [subagent](subagents.md#permissions)'s calls follow the main agent's rules, confinement and sandbox. A bare `Agent` in `deny` removes the subagent tools.

## Workflows

A [workflow](workflows.md#permissions-and-approvals) agent's calls follow the main agent's rules, confinement and sandbox. A bare `Workflow` in `deny` removes the tool. A bare `StructuredOutput` deny makes an `agent()` with a `schema` reject at once.

## Classifier

`classifier` configures the model that reviews calls that would otherwise ask:

```json
{
  "version": 1,
  "classifier": {
    "enabled": true,
    "model": "provider/id",
    "instructions": ["Deny pushes to the main branch."]
  }
}
```

- `classifier.enabled` (default `false`) turns the review on, in both modes.
- `classifier.model` (`provider/id`) is the reviewing model; unset, it is the session model and follows [`/model`](usage.md#commands). It is checked at start as [any model key](configuration.md#model) is, its model and credential only when the classifier is enabled.
- `classifier.instructions` lists your own rules, added after Kopi's. A blank entry is rejected at start. A later layer's list replaces an earlier one's.

With `classifier.enabled`:

- Plan mode's refusals and deny rules apply first.
- A call an ask rule matches (the [built-in asks](#precedence) too) still asks you; one an allow rule matches runs without review.
- With no rule matching, `Write` and `Edit` run without asking.
- With no rule matching, any other call that would ask is reviewed: `WebFetch`, `WebSearch`, MCP tools, `Bash` without the sandbox and `Bash` with [`unsandboxed`](sandbox.md#outside-the-sandbox) where offered.
  - The classifier allows it, denies it with a one-line reason the model gets while the turn goes on, or leaves it to you. A review that fails, times out or returns anything else is left to you.
  - Sandboxed `Bash` without `unsandboxed` never asks, so is never reviewed.
  - `sandbox.allowUnsandboxed` false keeps `unsandboxed` calls from being offered, reviewed or allowed.
- From the third denial in a row, every denial goes to you instead, until the classifier allows a call or you approve one. The twentieth denial in a session goes to you and restarts that count.
- The classifier is sent:
  - the session's prompts (the last 8,000 bytes)
  - its last 20 tool calls with their arguments (each cut to 2,000 bytes) and whether each failed
  - the call under review
- It is not sent the model's text, tool output or the text of skills a prompt loads.
- A review is up to two requests, each limited to 30 seconds: one at the lowest reasoning level the model supports and, unless that allows the call, one at medium whose answer stands.

## Limitations

- Rules are not an OS boundary: they match the path a call names. Only `Bash` runs under the OS [sandbox](sandbox.md).
- A command you type with `!` is your action: no rule or approval applies, and plan mode does not restrict it. See [your own commands](usage.md#your-own-commands).
- Kopi refuses no credential locations outside its home. Deny them with rules, such as `*(~/.ssh)`, and to `Bash` with `sandbox.denyRead`.
- Symlinks are resolved:
  - in rule paths, the start directory and the home directory, at start
  - in an added directory's rule paths, when it is added
  - in sandbox paths, at start on macOS and before each command on Linux
- A call waiting for your answer when a directory is added is judged by the earlier rules. A file call waiting when its directory is removed with [`/remove-dir`](usage.md#commands) is refused when it runs (`path is outside the workspace`), unless the path lies in the start directory or another added directory.
- A preview needs 3 or more rows to scroll; on a shorter screen the rest is unreachable.
- With `classifier.enabled`, the session's prompts, recent tool-call arguments and the call under review go to `classifier.model`'s provider. The classifier can be wrong either way, and a review can hold the turn for up to a minute.

## Not implemented

- Saved approvals.
- A `/permissions` command.
- Command-pattern rules for `Bash`.
- Approving a call the classifier denied after the fact.
- Review of sandboxed `Bash` commands.
- A separate figure for classifier requests (their cost is in `total`).
