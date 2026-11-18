# Skills

## Format

- A skill is a directory with a `SKILL.md`.
- Its frontmatter has:
  - `name` (default: the directory name)
  - `description` (required)
  - `disable-model-invocation`

## Loading

- Skills load from these places, the first definition of a name winning:
  1. A trusted folder's `.agents/skills`, from the start directory up to the directory holding `.git` (closest first; the start directory alone when none holds `.git`).
  2. `$KOPI_HOME/skills`.
  3. `~/.agents/skills`.
- A project's skills load only in a [trusted folder](trust.md#folder-trust): the trust question lists its skill folders, and print mode in an undecided folder skips them and warns.
- Starting in the home directory does not count `~/.agents/skills` as a project's.
- Skills are read once at startup. Problems loading a skill are reported at start.
- The `Skill` tool refuses, on every read, a file in Kopi's home outside `skills/`, and refuses a skill's files once its directory is no longer the one found at start.

## Using a skill

- Type `/skill:<name>` (see [`skills.prefix`](configuration.md#skillsprefix)) anywhere in the prompt, as a whole word.
- The message is sent as typed, and each skill named is loaded once with it. Text after the name is simply the rest of your prompt.
- `//` at the start escapes it.

## Skill tool

- Returns a skill's instructions, or with `file` one of its files, read as `Read` reads a file, images included.
- Its description lists the skills without `disable-model-invocation` to the model.
- It only reads, so it works in plan mode and is allowed by default.
- [Rules](permissions.md#rules-per-tool) match the skill's directory, or the file it reads, and a bare `Skill` in `deny` removes it.
- It is present only when a skill is loaded.

## Limitations

- `Read`, `Glob`, `Grep` and `LS` cannot reach a skill's files outside the start directory; the `Skill` tool's `file` can.
- The `Skill` tool ties a skill to the directory found at start.
  - A process that swaps that directory while Kopi starts can tie it to another directory, whose files `Skill` then reads (files in Kopi's home outside `skills/` stay refused on every read).
  - Like `Read`, its protection of Kopi's home is by file identity, so a hard link into Kopi's home is readable.
- Sandboxed `Bash` cannot read `$KOPI_HOME/skills`, so scripts bundled with skills there do not run; those in `~/.agents/skills` and in project skills do.
- A trusted folder's `.agents/skills` is writable by the model, like its instruction files.
- A top-level `.md` file with a description in a skills directory loads as a skill, and nothing below a directory that has a `SKILL.md` is searched.
- Loading skills lists PiG's agent directory and `~/.agents/skills` even when they hold nothing Kopi uses.

## Not implemented

- Turning off one skill.
- A skill menu for a `/` typed mid-prompt.
- Reloading skills during a run.
- `allowed-tools` and other frontmatter fields.
- Argument placeholders.
