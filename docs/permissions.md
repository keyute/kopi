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

- A rule is `Name` or `Name(path)`.
- The name is a tool name (`Read`, `Glob`, `Grep`, `LS`, `Write`, `Edit`, `Bash`, `TaskOutput`, `TaskStop`, `ToolSearch`, `Skill`, `WebFetch`, `WebSearch`), an [MCP tool name](mcp.md#plan-mode-and-rules) or `*` for every tool.
- An unknown name or a malformed rule is rejected at start.
- Paths:
  - `/abs` is absolute, `~/x` is under the home directory, and anything else is relative to the start directory.
  - `*` and `**` glob.
  - A name with no slash (`.env`) matches at any depth, and a directory covers everything in it.
  - Paths match case-insensitively on macOS and Windows.
- `Name(!path)` exempts the calls it matches from the other rules in its own list, from the same config layer or flag; it never reaches rules from anywhere else.
- A bare `Name` in `deny` removes the tool from the model.
- A rule's path counts as a match:
  - on either the path as written or its resolved target when it denies, asks or exempts from an allow
  - only on both when it allows or exempts from a deny or ask
- `--allowed-tools` and `--disallowed-tools` add allow and deny rules to the config's. Each is repeatable and takes comma-separated rules (commas inside `()` or `{}` do not split).

### Precedence

- Plan mode refuses `Write`, `Edit`, unsandboxed `Bash` and MCP tools not marked read-only before any rule; otherwise rules apply to `Bash` the same in both modes.
- Deny beats ask, and ask beats allow.
- With no rule matching:
  - `Read`, `Glob`, `Grep`, `LS`, `Skill`, `ToolSearch`, `TaskOutput`, `TaskStop` and sandboxed `Bash` are allowed.
  - Any other call asks, including every `WebFetch` and `WebSearch` call and every `Bash` command without the sandbox.
- Built in: `Edit` and `Write` under any `.git` directory, and of any file named `.mcp.json` or the file the start directory's `.mcp.json` resolves to when it is a symlink, always ask unless a deny rule refuses them.

### Rules per tool

- `Glob`, `Grep` and `LS` leave out entries that a `deny` or `ask` rule with a path for that tool or `*` matches.
  - Exception: a rule which also matches the searched path itself does not hide what is under it, as it already judged the whole call (unless a `!` exception in the same list is what admitted that path).
  - `Read(path)` rules do not apply to them; use `*(path)` to keep a path from every tool.
- `ToolSearch`, `TaskOutput`, `TaskStop`, `WebFetch` and `WebSearch` take no path: only a bare rule naming the tool or `*` applies to them. `ToolSearch(path)` and the like are rejected at start.
- For `Skill` the path is the skill's directory, or the file when the call names one, so path rules apply to it. A rule for a skill outside the start directory needs an absolute or `~/` path.
- `Bash` takes no path:
  - Only a bare `Bash` or `*` rule applies to it.
  - `Bash(path)` is rejected at start.
  - Path rules such as `*(~/.ssh)` never apply to `Bash`.
  - `"ask": ["Bash"]` makes every command ask.

## Approval dialog

The fullscreen interface asks before a call that needs approval.

- It shows the change or the command; the status line's `sandbox` segment shows whether `Bash` runs sandboxed.
- Options:
  - `Yes`
  - `Yes, allow all edits this session` (for `Write` and `Edit` when no rule asked)
  - `No`, which takes an optional reason typed on its row
- While it waits, the working row reads `Waiting for you…`.
- `No` with a reason refuses the call, gives the model the reason and lets the turn go on. A blank `No` stops the turn.
- Keys:
  - The `menu-up` and `menu-down` keys choose and `submit` answers.
  - A digit answers with that option, or moves to `No` to type the reason.
  - `interrupt` answers `No` without a reason.
  - In the reason, `newline` starts a line, `paste` and terminal paste insert text, and digits are text.
- Allowing all edits never overrides a deny or ask rule, never covers `Bash`, and lasts until the session ends.
- When the [classifier](#classifier) left a call to the user, the dialog shows why and does not offer `Yes, allow all edits this session`:
  - `the classifier would deny this: <reason>`
  - `the classifier could not decide`
  - `: <reason>` is added when the classifier gave one.
- Interrupting during a review stops it without asking.
- Print mode cannot ask, so it denies those calls; the refusal says to add an allow rule, such as `--allowed-tools Bash`. It also:
  - denies a call the classifier leaves to the user and one an ask rule matches
  - runs the calls the classifier allows without an allow rule

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

- `classifier.enabled` (default `false`) has the classifier review calls that would otherwise ask, in both modes.
- `classifier.model` (`provider/id`) is the model that reviews; unset, it is `model`.
  - A value that is not `provider/id` fails at start.
  - When the classifier is enabled, an unknown model or one whose provider has no credential also fails at start.
  - In the fullscreen interface, a `classifier.model` that shares `model`'s provider without a credential gets the first-run login instead when that provider offers a sign-in or an API key.
- `classifier.instructions` lists your own rules for the classifier, added after Kopi's. A blank entry is rejected at start. A later layer's list replaces an earlier one's whole.

Behaviour with `classifier.enabled`:

- Plan mode's refusals and deny rules apply first.
- A call an ask rule matches, including the [built-in asks](#precedence), still asks you.
- A call an allow rule matches runs without review.
- With no rule matching, `Write` and `Edit` run without asking.
- With no rule matching, any other call that would ask is reviewed instead: every `WebFetch` and `WebSearch` call, every MCP tool call, every `Bash` command without the sandbox.
  - The classifier allows it, denies it with a one-line reason the model gets while the turn goes on, or leaves it to you.
  - A review that fails, times out or returns anything else is left to you too.
  - Sandboxed `Bash` never asks, so it is never reviewed.
- The third denial in a row, and the twentieth in a session, goes to you instead of being denied. A call the classifier allows or you approve ends the run of denials.
- The classifier is sent:
  - the session's prompts (the last 8,000 bytes)
  - its last 20 tool calls with their arguments (each cut to 2,000 bytes) and whether each failed
  - the call under review
- It is not sent the model's text, any tool output or the text of skills a prompt loads.
- A review is up to two requests to the classifier model, each limited to 30 seconds: one at the lowest reasoning level the model supports and, unless that one allows the call, one at medium whose answer stands.

## Limitations

- Permission rules are not an OS boundary: they match the path a call names. Only `Bash` runs under the OS [sandbox](sandbox.md).
- Kopi refuses no credential locations outside its home; deny them with rules, such as `*(~/.ssh)`, and to `Bash` with `sandbox.denyRead`.
- Symlinks in rule paths, the start directory and the home directory are resolved once, at start; in sandbox paths too on macOS, and before each command on Linux.
- With `classifier.enabled`, the session's prompts, recent tool-call arguments and the call under review go to `classifier.model`'s provider. The classifier can be wrong in both directions, and a review can hold the turn for up to a minute.

## Not implemented

- Saved approvals.
- A `/permissions` command.
- Command-pattern rules for `Bash`.
- Approving a call the classifier denied after the fact.
- Review of sandboxed `Bash` commands.
- Usage accounting for classifier requests.
