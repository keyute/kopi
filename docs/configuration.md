# Configuration

Kopi keeps all its state in its home: `$KOPI_HOME`, default `~/.kopi`.

- Precedence is default < config file < flags.
- `kopi config` shows each value and its origin; unknown keys are rejected.
- Print mode logs to stderr, the fullscreen interface to `kopi.log`.

## Home

| Entry | Holds |
| --- | --- |
| `config.json` | Your configuration; Kopi never writes it |
| `AGENTS.md` | Instruction file, see [trust](trust.md#home-agentsmd) |
| `trust.json` | Folder [trust](trust.md#folder-trust) decisions; Kopi writes it |
| `history.jsonl` | Fullscreen input history per working directory; the last 100 are recalled |
| `skills/` | Your [skills](skills.md) |
| `plans/` | One [plan file](tools.md#exitplanmode) per session; never deleted |
| `outputs/` | [Saved tool output](tools.md#saved-output), one folder per session; files over a week old are removed at startup |
| `agents/` | Your [subagents](subagents.md), one Markdown file each |
| `sandbox/` | Placeholder lock and record (Linux), see [sandbox](sandbox.md#linux-placeholders) |
| `themes/` | Your [themes](#theme), `<name>.json` |
| `kopi.log` | Fullscreen log |
| `images/` | Pasted images, removed after a day |
| `installation_id` | Installation UUID, created when a sign-in first asks |
| `runtime/` | PiG's agent directory: `auth.json`, `sessions/`, `models-store.json` (see [models](providers.md#models)), an optional `models.json` and other PiG state |

## Top-level keys

| Key | Meaning |
| --- | --- |
| `version` | `1` |
| `model` | `provider/id` of the model, optional, see [`model`](#model) |
| `mode` | Mode sessions start in, see [`mode`](#mode) |
| `effort` | Reasoning effort a new main session opens at, see [`effort`](#effort) |
| `smallModel` | Model that answers `WebFetch` and `WebSearch` and writes session titles, see [`smallModel`](#smallmodel) |
| `instructionFallbacks` | Fallback names for project instruction files, see [`instructionFallbacks`](#instructionfallbacks) |
| `skills` | `skills.prefix`, see [`skills.prefix`](#skillsprefix) |
| `workflows` | `workflows.model`, `workflows.maxWorkflows` and `workflows.maxAgents`, see [workflows](workflows.md#configuration) |
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

- `provider/id` of the session model. `--model` overrides it; [`/model`](usage.md#commands) switches it until Kopi exits. An unknown id is fetched or declared as in [models](providers.md#models).
- Unset, each start uses the default model of a provider with a credential, environment credentials included. Nothing is saved; a notice names the model.
- With several providers' credentials, the runtime's order decides; set `model` to pin it.
- With no credential, the fullscreen interface asks which provider to log in to; print mode fails with a hint to run `kopi login`.
- Every model key (`smallModel`, `classifier.model`, `workflows.model`, a [subagent](subagents.md) definition's `model`) is checked at start. A value that is not `provider/id` always fails. An unknown model, or one whose provider has no credential, fails naming the provider, but only where the key's feature can run:
  - `smallModel`: always.
  - `classifier.model`: when `classifier.enabled`.
  - `workflows.model`: in the fullscreen interface with no `Workflow` deny rule.
  - A definition's `model`: with no `Agent` deny rule, or in the fullscreen interface with no `Workflow` deny rule.
- In the fullscreen interface, a model key that shares the provider of a configured `model` without a credential gets the first-run login instead, when that provider offers a sign-in or an API key and Kopi knows the id; an unknown id fails at start with the login hint.

## `mode`

`plan` (default) or `execute`: the starting mode, see [modes](tools.md#modes).

## `effort`

- `off`, `minimal`, `low`, `medium` (default), `high`, `xhigh` or `max`: the reasoning effort every main session opens at, new or resumed. [`/effort`](usage.md#commands) replaces it until Kopi exits. A level the model lacks is clamped to the nearest it has.
- On a model that does not reason, `/effort` leaves the process-wide choice unchanged; switch to a reasoning model first to change it.
- [Subagents](subagents.md) use their definition's `effort`.
- A wrong value fails at start.

## `smallModel`

- `provider/id` of the model that answers `WebFetch` and `WebSearch` and writes the terminal tab's session title, see [terminal](usage.md#terminal).
- Unset, all three use the session model and follow [`/model`](usage.md#commands).
- `WebSearch` is offered while that model has hosted search, decided at session open and on `/model`; a running subagent or workflow agent keeps what it opened with.
- Checked at start like every [model key](#model).

## `instructionFallbacks`

- File names tried in a directory with no `AGENTS.md`; the first present wins.
- Default `["CLAUDE.md"]`; `[]` turns the fallback off. Project instruction files load only in a trusted folder, see [trust](trust.md#project-instruction-files).

## `skills.prefix`

- Default `skill`; lowercase letters, digits, hyphens. Skills are typed as `/skill:name`.
- `""` types them as `/name`; a skill named like a built-in command then cannot be typed.

## Keys

- `keys` maps a fullscreen action to key strings; setting an action replaces its list.
- An unknown action, empty list or blank string is rejected at start; a string naming no real key never matches.

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
| `stop-agent` | `x` (stops the selected subagent or workflow in the [agent rows](subagents.md#agent-rows), `/tasks` or a progress dialog) |
| `edit-plan` | `ctrl+g` |
| `paste` | `ctrl+v` |
| `history-prev`, `history-next` | `up`, `down` |
| `queue-edit` | `up` |
| `complete` | `tab` |
| `menu-up`, `menu-down` | `up`, `down` |
| `menu-close` | `esc` |
| `expand-all` | `ctrl+o` |
| `page-up`, `page-down` | `pgup`, `pgdown` |

What each key does is in [usage](usage.md#keys); `interrupt`, `cancel` and `background` also act on [your own commands](usage.md#your-own-commands).

## Status line

`statusline` lists the `left` and `right` entries. Each is a [format](#formats) naming built-in segments or your own `segments`.

- Defaults: left `$model`, `$context`, `$branch( $changes)`; right `$notice`, `$shells`, `$ultracode`, `$sandbox`, `$mode`.
- `separator` goes between entries (default ` · `); an entry that draws nothing is dropped with its separator.
- On a narrow terminal the left side drops whole entries from its right end. If it still does not fit, the left side is cleared and the right side cut, keeping the `mode` entry whole when it fits.
- A dialog hides the status line and the rows under it; a notice then joins the dialog's key hints.

### Formats

`statusline` and [`subagentline`](#subagent-rows) entries use a subset of starship's format syntax:

| Written | Meaning |
| --- | --- |
| `$name` | A segment (letters, digits, `_`); draws nothing when it has nothing to show |
| `( … )` | Drawn only when a segment inside has something to show; groups nest |
| `\x` | The character `x`: `\$`, `\(`, `\)`, `\\` (in JSON, `"\\("`) |
| anything else | Drawn as written |

- A format must name a segment in every entry and `( … )` group; a bare `"mode"` is an error. An unknown segment, unbalanced parenthesis, nameless `$`, trailing `\` or control character is one too. Errors stop Kopi at start and name the config layer.

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
| `effort` | Reasoning level in use; absent when the model does not reason |
| `tools` | Tool calls made (`12 tools`); absent until the first |
| `tokens` | Context size (`22.1k tokens`); absent until known |
| `cost` | The session's cost (`$0.42`); absent while zero |
| `total` | Estimated cost of the whole session (`$1.87`): main, subagents, ended workflow agents, classifier, `WebFetch`, `WebSearch` and title requests; absent while zero |
| `limit_<window>` | Share of a subscription usage window used (`12%`), see [Plan limits](providers.md#plan-limits) |
| `reset_<window>` | When that window resets (`14:30`, `Wed 09:00`) |
| `cwd` | The workspace, home as `~` |
| `notice` | Transient hints such as `Copied` |
| `sandbox` | `sandboxed` or `unsandboxed` when `Bash` is available; the default, not a call that sets `unsandboxed` |
| `agents`, `shells`, `workflows` | Running subagents, background shells and workflows (`2 agents`); absent at none |
| `ultracode` | `ultracode` while `Workflow` is callable; absent otherwise |
| `changes` | Lines added and removed against `HEAD` in tracked files (`+12 -3`; a zero side is omitted); absent with none, outside a repository and with no commit |

- `model`, `effort`, `context`, `tokens`, `tools` and `cost` follow the open view.
- `cost` and `total` are list-price estimates, not what a subscription sign-in bills.
- `total` covers the whole session in any view, omits a running workflow agent, and after a resume holds only the main session's earlier cost.
- `<window>` is `5h`, `wk`, `mo`, `mo_code`, `premium` or `chat`; [Plan limits](providers.md#plan-limits) lists which providers have which. These segments draw no label or separator, are not in the default layout, and naming one makes Kopi call the provider's usage endpoint.
- `changes`:
  - Untracked files are not counted.
  - Kopi runs a hardened `git diff` in the workspace, unsandboxed, in any folder trusted or not, only when an entry names `changes` and the workspace is in a repository at start. External diff drivers, textconv and fsmonitor hooks are off; git does not write the index.
  - A clean or process filter from the repository's or your git config still runs on files `.gitattributes` assigns to it.
  - A bare repository found without a `.git` is not used (git 2.38 or later).
  - A missing object in a partial clone is not fetched (git 2.45 or later).
  - It refreshes at start, when a turn starts and on every status refresh.

A `segments` entry:

- Has a name (letters, digits, `_`), either `text` or `command`, and an optional `color`: a theme role such as `accent`, `#rgb`, `#rrggbb` or 0 to 255.
- A `command` runs with `sh -c` in the workspace, unsandboxed, with a 5 second timeout: at start, when a turn starts, on every status refresh and on an optional `interval` (`"30s"`, at least 1s).
- It shows the first line of stdout with ANSI colours, and is hidden while the command fails or prints nothing.

```json
{ "statusline": { "right": ["$load", "$mode"],
    "segments": { "load": { "command": "uptime | awk '{print $NF}'", "interval": "30s", "color": "muted" } } } }
```

## Subagent rows

`subagentline` sets the `left` and `right` entries of a subagent's row in the [agent rows](subagents.md#agent-rows), in the status line's [format syntax](#formats). The `main` row and a [workflow](workflows.md#footer-row)'s row are fixed.

- Defaults: left `$agent`, `$model`, `$tokens`; right none. `separator` works as in the status line.
- At most five rows show, `main` and workflow rows included; fewer on a short terminal.
- There are no user-defined segments; an unknown name stops Kopi at start.

| Segment | Shows |
| --- | --- |
| `agent` | Name and description cut to 36 columns (`explorer › Find the config loader`) |
| `tokens` | Context size (`22.1k tokens`); absent until known |
| `model` | Model |
| `effort` | Reasoning level in use; absent when the model does not reason |
| `context` | Share of the context window in use (`18.0%`); absent until known |
| `tools` | Tool calls made; absent until the first |
| `cost` | Estimated cost; absent while zero |
| `elapsed` | Current turn's run time; absent while idle |
| `state` | `running`, `completed`, `failed` or `stopped` |
| `activity` | The tool call running now; absent when none |
| `id` | The task id (`t3`) |

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

- `theme.dark` and `theme.light` name the theme for a dark or light terminal (defaults `catppuccin-mocha`, `catppuccin-latte`).
- Built-in names: `catppuccin-mocha`, `catppuccin-latte`, `catppuccin-frappe`, `catppuccin-macchiato`, `gruvbox-dark`, `gruvbox-light`, `tokyonight-night`, `tokyonight-day`, `rose-pine`, `rose-pine-dawn`, `solarized-dark`, `solarized-light`, `dracula`, `ansi-dark` and `ansi-light`; the last two use the terminal's own 16 colours.
- Any other name reads `themes/<name>.json` in Kopi's home:
  - `base`: required, a built-in theme.
  - `colors`: optional overrides by role: `accent`, `success`, `error`, `warning`, `muted`, `dim`, `text`, `userMessageBg`, `userCommandBg`, `heading`, `link`, `code`, `frame`. `userMessageBg` shades your messages; `userCommandBg` a `!` command and the input while you type one; `code` also colours plain code-block text.
  - `codeTheme`: optional Chroma style for code blocks.
- A colour is `#rgb`, `#rrggbb` or 0 to 255.
- Kopi follows dark/light changes where the terminal reports them (DEC mode 2031); elsewhere the start theme stays.
- An unknown theme, role, colour or style fails at start.

```json
{ "base": "dracula", "colors": { "accent": "#ff79c6", "dim": "244" }, "codeTheme": "monokai" }
```

## Terminal

- `terminal.title` (default `true`): sets the terminal's title, see [usage](usage.md#terminal). `false` leaves the title alone, skips the OSC 7501 probe and asks no model for a session name.
- `terminal.progress` (default `false`): shows the terminal's progress bar while working.
- `terminal.notifications` (default `auto`): how Kopi notifies you while the terminal is unfocused: `osc9` (desktop notification), `bell` or `off`.
  - `auto` sends OSC 9 when `TERM_PROGRAM` is `ghostty`, `iTerm.app`, `WarpTerminal` or `WezTerm`, or `TERM` is `xterm-kitty` or `xterm-ghostty`; the bell otherwise, including under tmux.
- Any other value stops Kopi at start.

## Not implemented

- A project `config.json`.
