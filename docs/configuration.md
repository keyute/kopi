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
| `agents/` | Your [subagents](subagents.md), one Markdown file each |
| `sandbox/` | The lock and the record of placeholders that Kopi processes sharing the home use to remove them safely (Linux), see [sandbox](sandbox.md#linux-placeholders) |
| `images/` | Pasted clipboard images, removed after a day |
| `installation_id` | The installation's UUID, created the first time a sign-in asks for it |
| `agent/` | PiG's agent directory, created on first run: its credential file (`auth.json`), session logs (`sessions/`), other PiG state and an optional `models.json` |

## Top-level keys

| Key | Meaning |
| --- | --- |
| `version` | `1` |
| `model` | `provider/id` of the model, optional, see [`model`](#model) |
| `mode` | Mode sessions start in, see [`mode`](#mode) |
| `smallModel` | Model that answers `WebFetch` and `WebSearch`, see [`smallModel`](#smallmodel) |
| `instructionFallbacks` | Fallback names for project instruction files, see [`instructionFallbacks`](#instructionfallbacks) |
| `skills` | `skills.prefix`, see [`skills.prefix`](#skillsprefix) |
| `keys` | Key bindings for fullscreen actions, see [Keys](#keys) |
| `statusline` | Status line segments, see [Status line](#status-line) |
| `theme` | Colour themes, see [Theme](#theme) |
| `permissions` | Allow, ask and deny rules, see [permissions](permissions.md#rules) |
| `classifier` | Reviewer of calls that would ask, see [permissions](permissions.md#classifier) |
| `sandbox` | The OS sandbox `Bash` runs in, see [sandbox](sandbox.md#configuration) |
| `mcpServers` | MCP servers, see [MCP](mcp.md) |

## `model`

- `provider/id` of the session model; `--model` overrides it.
- Unset, each start uses the default model of a provider with a credential, environment credentials included. Nothing is saved, and a notice names the model and the config file to set it in.
- With credentials for several providers, the default depends on the order the runtime prefers; set `model` to pin it.
- With no credential at all, the fullscreen interface asks which provider to log in to, then opens the session; print mode fails with a hint to run `kopi login`. Start fails first, naming the provider, when `smallModel`, an enabled `classifier.model` or a [subagent](subagents.md) definition's `model` names a provider with no credential.

## `mode`

`plan` (default) or `execute`: the mode sessions start in. See [modes](tools.md#modes).

## `smallModel`

- `provider/id` of the model that answers `WebFetch` and `WebSearch` calls.
- Unset, it is the session model, so set it to a cheaper model.
- A wrong value, an unknown model or one whose provider has no credential fails at start.
- In the fullscreen interface, a `smallModel` that shares the provider of a configured `model` without a credential gets the first-run login instead when that provider offers a sign-in or an API key.

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

`statusline` lists the status line's `left` and `right` segments, from the built-in ones or your own `segments`.

- Defaults: left `model, context, branch`; right `notice, sandbox, tasks, mode`.
- `separator` goes between segments (default ` · `, `""` for none).

Built-in segments: `model`, `context`, `branch`, `mode`, and:

| Segment | Shows |
| --- | --- |
| `cwd` | The workspace, with your home directory as `~` |
| `notice` | Transient hints such as `Copied` |
| `sandbox` | `sandboxed` or `unsandboxed` when `Bash` is available |
| `tasks` | Running background work, such as `2 agents, 1 shell` |

A segment in `segments`:

- Has either `text` or `command`, and an optional `color`: a theme colour role such as `accent`, `#rgb`, `#rrggbb` or a number from 0 to 255.
- A `command` runs with `sh -c` in the workspace, unsandboxed, with a 5 second timeout.
  - It runs at start, on every status refresh and, with an `interval` such as `"30s"` (at least 1s), on that schedule.
  - A refresh that arrives while it is still running is skipped.
  - The segment shows the first line of its stdout, keeping ANSI colours, and is hidden while the command fails or prints nothing.

```json
{
  "version": 1,
  "keys": { "send-now": ["ctrl+s"] },
  "statusline": {
    "left": ["model", "branch", "cwd"],
    "right": ["notice", "load", "context", "mode"],
    "separator": " | ",
    "segments": { "load": { "command": "uptime | awk '{print $NF}'", "interval": "30s", "color": "muted" } }
  }
}
```

## Theme

- `theme.dark` and `theme.light` name the theme used on a dark or light terminal (defaults `catppuccin-mocha`, `catppuccin-latte`).
- Built-in names: `catppuccin-mocha`, `catppuccin-latte`, `gruvbox-dark`, `gruvbox-light`, `tokyonight-night`, `tokyonight-day`, `rose-pine`, `rose-pine-dawn`, `solarized-dark`, `solarized-light`, `dracula`, `ansi-dark` and `ansi-light`; the last two use the terminal's own 16 colours.
- Any other name reads `themes/<name>.json` in Kopi's home:
  - `base`: required, a built-in theme.
  - `colors`: optional overrides by role: `accent`, `success`, `error`, `warning`, `muted`, `dim`, `text`, `userMessageBg`, `heading`, `link`, `code`, `frame`.
  - `codeTheme`: optional Chroma style for code blocks.
- A colour is `#rgb`, `#rrggbb` or a number from 0 to 255.
- An unknown theme, role, colour or style fails at start.

```json
{ "base": "dracula", "colors": { "accent": "#ff79c6", "dim": "244" }, "codeTheme": "monokai" }
```

## Not implemented

- Project-level config.
