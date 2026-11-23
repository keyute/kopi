# Configuration

Kopi keeps all its state in its home: `$KOPI_HOME`, default `~/.kopi`.

- Precedence is default < config file < flags.
- `kopi config` shows each value and its origin; unknown keys are rejected.
- The fullscreen interface logs to `kopi.log` in the home; print mode logs to stderr.

## Home

| Entry | Holds |
| --- | --- |
| `config.json` | Your configuration. Kopi never writes it. |
| `AGENTS.md` | Instruction file, see [trust](trust.md#home-agentsmd) |
| `trust.json` | Which folders you trust, see [trust](trust.md#folder-trust); Kopi writes it |
| `history.jsonl` | Input history per working directory, written by the fullscreen interface; the last 100 entries are recalled |
| `skills/` | Your [skills](skills.md) |
| `plans/` | One [plan file](tools.md#exitplanmode) per session, `<session id>.md`; Kopi never deletes them |
| `outputs/` | One folder per session of [saved tool output](tools.md#saved-output) too long to return whole; Kopi removes files older than a week at startup |
| `agents/` | Your [subagents](subagents.md), one Markdown file each |
| `sandbox/` | The lock and the record of placeholders that Kopi processes sharing the home use to remove them safely (Linux), see [sandbox](sandbox.md#linux-placeholders) |
| `images/` | Pasted clipboard images, removed after a day |
| `installation_id` | The installation's UUID, created the first time a sign-in asks for it |
| `agent/` | PiG's agent directory, created on first run: its credential file (`auth.json`), session logs (`sessions/`), other PiG state, the saved model lists (`models-store.json`, see [models](providers.md#models)) and an optional `models.json` |

## Top-level keys

| Key | Meaning |
| --- | --- |
| `version` | `1` |
| `model` | `provider/id` of the model, optional, see [`model`](#model) |
| `mode` | Mode sessions start in, see [`mode`](#mode) |
| `smallModel` | Model that answers `WebFetch` and `WebSearch` and writes session titles, see [`smallModel`](#smallmodel) |
| `instructionFallbacks` | Fallback names for project instruction files, see [`instructionFallbacks`](#instructionfallbacks) |
| `skills` | `skills.prefix`, see [`skills.prefix`](#skillsprefix) |
| `workflows` | `workflows.model`, `workflows.maxWorkflows` and `workflows.maxAgents`, see [`workflows`](#workflows) |
| `keys` | Key bindings for fullscreen actions, see [Keys](#keys) |
| `statusline` | Status line entries, see [Status line](#status-line) |
| `subagentline` | Subagent row entries, see [Subagent rows](#subagent-rows) |
| `theme` | Colour themes, see [Theme](#theme) |
| `terminal` | What Kopi tells the terminal, see [Terminal](#terminal) |
| `permissions` | Allow, ask and deny rules, see [permissions](permissions.md#rules) |
| `classifier` | Reviewer of calls that would ask, see [permissions](permissions.md#classifier) |
| `sandbox` | The OS sandbox `Bash` runs in, see [sandbox](sandbox.md#configuration) |
| `mcpServers` | MCP servers, see [MCP](mcp.md) |

## `model`

- `provider/id` of the session model; `--model` overrides it, and [`/model`](usage.md#commands) switches it until Kopi exits. An id Kopi does not know is fetched or declared as described in [models](providers.md#models).
- Unset, each start uses the default model of a provider with a credential, environment credentials included. Nothing is saved, and a notice names the model and the config file to set it in.
- With credentials for several providers, the default depends on the order the runtime prefers; set `model` to pin it.
- With no credential at all, the fullscreen interface asks which provider to log in to, then opens the session; print mode fails with a hint to run `kopi login`. Start fails first, naming the provider, when `smallModel`, an enabled `classifier.model`, `workflows.model` (checked only in the fullscreen interface and when no rule denies `Workflow`) or a [subagent](subagents.md) definition's `model` (checked only where something can use it: when no rule denies `Agent`, or in the fullscreen interface when no rule denies `Workflow`) names a provider with no credential.

## `mode`

`plan` (default) or `execute`: the mode sessions start in. See [modes](tools.md#modes).

## `smallModel`

- `provider/id` of the model that answers `WebFetch` and `WebSearch` calls and writes the session title shown in the terminal tab, see [terminal](usage.md#terminal).
- Unset, `WebFetch`, `WebSearch` and session titles all use the session model and follow [`/model`](usage.md#commands), so set it to a cheaper model. `WebSearch` is offered while that model has hosted search, decided when a session opens and on `/model`; a running subagent or workflow agent keeps what it opened with.
- A wrong value, an unknown model or one whose provider has no credential fails at start.
- In the fullscreen interface, a `smallModel` that shares the provider of a configured `model` without a credential gets the first-run login instead when that provider offers a sign-in or an API key and the id is one Kopi knows; an unknown id fails at start with the login hint.

## `workflows`

- `workflows.model` is the `provider/id` of the model workflow agents use when their definition sets none; unset, it is the session model. A wrong value, an unknown model or one whose provider has no credential fails at start, checked only where workflows can run: in the fullscreen interface, with no rule denying `Workflow`.
- `workflows.maxWorkflows` (default 4, at least 1) is how many [workflows](workflows.md) run at once, and `workflows.maxAgents` (default 16, at least 1) how many agents of one workflow run at once.
- In the fullscreen interface, a `workflows.model` that shares the provider of a configured `model` without a credential gets the first-run login instead when that provider offers a sign-in or an API key and the id is one Kopi knows; an unknown id fails at start with the login hint.

## `instructionFallbacks`

- Bare file names a directory's project instruction file is read from when it has no `AGENTS.md`, the first present winning.
- Default `["CLAUDE.md"]`; `[]` turns the fallback off.
- Project instruction files load only in a trusted folder, see [trust](trust.md#project-instruction-files).

## `skills.prefix`

- Default `skill`; lowercase letters, digits and hyphens.
- Typed before a skill's name and a colon: `/skill:name`.
- `""` types skills as `/name`, and then a skill named like a built-in command cannot be typed.

## Keys

- `keys` maps each fullscreen action to a list of key strings.
- Setting one action replaces that action's list and leaves the others at their defaults.
- An unknown action or segment, an empty list or a blank key string is rejected at start; a key string that names no real key never matches.

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
| `stop-agent` | `x` (stops the running subagent or workflow selected in the [agent rows](subagents.md#agent-rows) or the `/tasks` list, or the agent selected in a workflow's progress dialog) |
| `edit-plan` | `ctrl+g` |
| `paste` | `ctrl+v` |
| `history-prev`, `history-next` | `up`, `down` |
| `queue-edit` | `up` |
| `complete` | `tab` |
| `menu-up`, `menu-down` | `up`, `down` |
| `menu-close` | `esc` |
| `expand-all` | `ctrl+o` |
| `page-up`, `page-down` | `pgup`, `pgdown` |

What each key does is in [usage](usage.md#keys).

## Status line

`statusline` lists the status line's `left` and `right` entries. Each entry is a [format](#formats) naming built-in segments or your own `segments`.

- Defaults: left `$model`, `$context`, `$branch( $changes)`; right `$notice`, `$shells`, `$ultracode`, `$sandbox`, `$mode`.
- `separator` goes between entries (default ` · `, `""` for none). An entry that draws nothing is left out with its separator. Nothing is trimmed.
- On a narrow terminal the left side drops whole entries from its right end. When the line still does not fit, the left side is cleared and the right side is cut, keeping the entry that names `mode` whole at the right end when it fits.

### Formats

The entries of `statusline` and of [`subagentline`](#subagent-rows) use a subset of starship's format syntax:

| Written | Meaning |
| --- | --- |
| `$name` | A segment. A name is letters, digits and `_`. Draws nothing when the segment has nothing to show |
| `( … )` | Drawn only when a segment inside it has something to show. Groups nest |
| `\x` | The character `x` itself: `\$`, `\(`, `\)`, `\\` (in JSON a backslash is written `\\`, so `"\\("`) |
| anything else | Drawn as written |

- Every entry, and every `( … )` group in it, must name at least one segment. A bare `"mode"` stops Kopi at start with an error that says to write `"$mode"`.
- An unknown segment, an unclosed `(`, a `)` without `(`, a `$` without a name, a trailing `\` and a control character also stop Kopi at start with an error naming the config layer.

With `branch` `main` and `changes` `+12 -3`:

| Entry | In a repository with changes | Without changes | Outside a repository |
| --- | --- | --- | --- |
| `$branch( $changes)` | `main +12 -3` | `main` | nothing |
| `$branch $changes` | `main +12 -3` | `main ` | ` ` |

With `context` `18.0%`, and with it not yet known:

| Entry | Known | Not known |
| --- | --- | --- |
| `(ctx $context)` | `ctx 18.0%` | nothing |
| `ctx $context` | `ctx 18.0%` | `ctx ` |

With the entries `(5h $limit_5h( $reset_5h))` and `(wk $limit_wk( $reset_wk))`:

| Reading | Draws |
| --- | --- |
| Both windows | `5h 12% 14:30 · wk 40% Wed 09:00` |
| Only the weekly window | `wk 40% Wed 09:00` |
| API key, or before the first read | nothing |

### Segments

Built-in segments: `model`, `context`, `branch`, `mode`, and:

| Segment | Shows |
| --- | --- |
| `effort` | The reasoning level in use (`high`); absent when the model does not reason |
| `tools` | The tool calls the session has made (`12 tools`); absent until the first |
| `tokens` | The session's context size (`22.1k tokens`); absent until known |
| `cost` | The session's cost (`$0.42`); absent while zero |
| `total` | The whole session's estimated cost (`$1.87`): the main session, its subagents, workflow agents that have ended, and the classifier, `WebFetch`, `WebSearch` and session-title requests; absent while zero |
| `limit_<window>` | The share used of one usage window of the provider's subscription plan (`12%`); absent when the provider has no such window, with an API key, until read and in a subagent's or workflow agent's view on another provider |
| `reset_<window>` | When that window resets (`14:30`, `Wed 09:00`, `Dec 1`); absent in the same cases and when the window has no reset |
| `cwd` | The workspace, with your home directory as `~` |
| `notice` | Transient hints such as `Copied` |
| `sandbox` | `sandboxed` or `unsandboxed` when `Bash` is available; shows the default, not a call that sets `unsandboxed` |
| `agents` | Running subagents (`2 agents`); absent at none |
| `shells` | Running background shells (`1 shell`); absent at none |
| `workflows` | Running workflows (`1 workflow`); absent at none |
| `ultracode` | `ultracode` while the `Workflow` tool is callable in this session; absent otherwise |
| `changes` | Lines added and removed against `HEAD` in tracked files, staged or not (`+12 -3`; only the non-zero side shows, `+12` or `-3`); absent when there are none, outside a git repository and in a repository with no commit |

- `model`, `effort`, `context`, `tokens`, `tools` and `cost` show the session whose view is open.
- `cost` is an estimate computed from the model's listed price; on a subscription sign-in it is not what you are billed. `total` is the same kind of estimate.
- `total` always shows the whole session, whichever view is open. It leaves out a workflow agent that is still running. After a resume (`-c`, `-r` or `/resume`) it holds the main session's cost from before the resume, but not what subagents, workflow agents and those requests cost then.
- `<window>` is `5h`, `wk`, `mo`, `mo_code`, `premium` or `chat`; [Plan limits](providers.md#plan-limits) lists which providers have which.
- `limit_<window>` and `reset_<window>` draw no label and no separator; write them in the entry. None is in the default layout, and naming any makes Kopi call the provider's usage endpoint; opening [`/usage`](usage.md#usage-1) does too. See [Plan limits](providers.md#plan-limits) for what is sent and when.
- `limit_<window>` shows in the warning colour from 90%; `reset_<window>` does not.
- `changes`:
  - Untracked files are not counted.
  - Kopi runs `git diff` in the workspace for it, unsandboxed, in any folder whether trusted or not, and only when an entry names `changes` and the workspace is in a repository when Kopi starts.
  - External diff drivers, textconv and fsmonitor hooks are turned off.
  - Git does not refresh or write the index.
  - A bare repository found without a `.git` is not used (git 2.38 or later).
  - A missing object in a partial clone is not fetched (git 2.45 or later).
  - A clean or process filter defined in the repository's or your own git config still runs on files that `.gitattributes` assigns to it.
  - It refreshes when Kopi starts, when a turn starts and on every status refresh, so a change made outside Kopi shows at the next of those. A refresh that arrives during a run reruns it once when it finishes.

A segment in `segments`:

- Is named with letters, digits and `_`, and has either `text` or `command`, and an optional `color`: a theme colour role such as `accent`, `#rgb`, `#rrggbb` or a number from 0 to 255.
- A `command` runs with `sh -c` in the workspace, unsandboxed, with a 5 second timeout.
  - It runs at start, when a turn starts, on every status refresh and, with an `interval` such as `"30s"` (at least 1s), on that schedule.
  - A refresh that arrives while it is still running is skipped.
  - The segment shows the first line of its stdout, keeping ANSI colours, and is hidden while the command fails or prints nothing.

```json
{
  "version": 1,
  "keys": { "send-now": ["ctrl+s"] },
  "statusline": {
    "left": ["$model", "$branch( $changes)", "$cwd"],
    "right": ["$notice", "$load", "(ctx $context)", "$mode"],
    "separator": " | ",
    "segments": { "load": { "command": "uptime | awk '{print $NF}'", "interval": "30s", "color": "muted" } }
  }
}
```

## Subagent rows

`subagentline` lists the `left` and `right` entries of a subagent's row in the [agent rows](subagents.md#agent-rows) shown under the status line. Entries use the status line's [format syntax](#formats). The `main` row reads only `main` and a [workflow](workflows.md#footer-row)'s row has a fixed format; `subagentline` configures neither.

- Defaults: left `$agent`, `$model`, `$tokens`; right none.
- `separator` goes between entries (default ` · `, `""` for none; no control characters).
- At most five agent rows show, the `main` row and workflow rows among them; the limit is fixed. On a short terminal fewer rows show.

Segments:

| Segment | Shows |
| --- | --- |
| `agent` | The subagent's name and what it was started for, the description cut to 36 columns (`explorer › Find the config loader`) |
| `tokens` | The subagent's context size (`22.1k tokens`); absent until known |
| `model` | The subagent's model, in the form of the status line's `model` |
| `effort` | The reasoning level in use (`high`); absent when the model does not reason |
| `context` | The share of the context window in use (`18.0%`), as in the status line; absent until known |
| `tools` | The tool calls the subagent has made (`12 tools`); absent until the first |
| `cost` | The subagent's estimated cost (`$0.42`), as in the status line; absent while zero |
| `elapsed` | How long the current turn has been running; absent while idle |
| `state` | `running`, `completed`, `failed` or `stopped` |
| `activity` | The tool call running now; absent when none |
| `id` | The task id (`t3`) |

- Only these segments exist; there are no user-defined segments. An unknown name stops Kopi at start with an error naming the config layer.

```json
{
  "version": 1,
  "subagentline": {
    "left": ["$agent", "$activity"],
    "right": ["$state( $elapsed)", "$id"],
    "separator": " | "
  }
}
```

## Theme

- `theme.dark` and `theme.light` name the theme used on a dark or light terminal (defaults `catppuccin-mocha`, `catppuccin-latte`).
- Built-in names: `catppuccin-mocha`, `catppuccin-latte`, `catppuccin-frappe`, `catppuccin-macchiato`, `gruvbox-dark`, `gruvbox-light`, `tokyonight-night`, `tokyonight-day`, `rose-pine`, `rose-pine-dawn`, `solarized-dark`, `solarized-light`, `dracula`, `ansi-dark` and `ansi-light`; the last two use the terminal's own 16 colours.
- Any other name reads `themes/<name>.json` in Kopi's home:
  - `base`: required, a built-in theme.
  - `colors`: optional overrides by role: `accent`, `success`, `error`, `warning`, `muted`, `dim`, `text`, `userMessageBg`, `heading`, `link`, `code`, `frame`.
  - `codeTheme`: optional Chroma style for code blocks.
- A colour is `#rgb`, `#rrggbb` or a number from 0 to 255.
- An unknown theme, role, colour or style fails at start.

```json
{ "base": "dracula", "colors": { "accent": "#ff79c6", "dim": "244" }, "codeTheme": "monokai" }
```

## Terminal

- `terminal.title` (default `true`): sets the terminal's title to `κ`, or a state glyph where the terminal does not read OSC 7501, then the session's name and the workspace folder, see [usage](usage.md#terminal). It is the only switch for the generated title, which comes from `smallModel`, else the session model. With `false`, Kopi leaves the title alone, does not ask the terminal whether it reads OSC 7501, and asks no model for a session name.
- `terminal.progress` (default `false`): shows the terminal's indeterminate progress bar while the session works.
- `terminal.notifications` (default `auto`): how Kopi notifies you while the terminal is not focused.
  - `osc9`: a desktop notification (OSC 9).
  - `bell`: the terminal bell.
  - `off`: nothing.
  - `auto`: OSC 9 when `TERM_PROGRAM` is `ghostty`, `iTerm.app`, `WarpTerminal` or `WezTerm`, or `TERM` is `xterm-kitty` or `xterm-ghostty`; the bell otherwise, including under tmux.
- Any other value stops Kopi at start with an error naming the config layer.

```json
{ "version": 1, "terminal": { "progress": true, "notifications": "bell" } }
```

## Not implemented

- Project-level config.
