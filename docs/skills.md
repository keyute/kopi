# Skills

## Format

- A skill is a directory with a `SKILL.md`.
- Its frontmatter has `name` (default: the directory name), `description` (required) and `disable-model-invocation`.

## Loading

- Skills load from these places; the first definition of a name wins:
  1. A trusted folder's `.agents/skills`, closest first, from the start directory up to the one holding `.git` (the start directory alone when none does).
  2. `$KOPI_HOME/skills`.
  3. `~/.agents/skills`.
- A project's skills load only in a [trusted folder](trust.md#folder-trust). Print mode in an undecided folder skips them and warns.
- Starting in the home directory does not count `~/.agents/skills` as a project's.
- Skills are read once at startup, and loading problems are reported then.
- `Skill` refuses, on every read, a file in Kopi's home outside `skills/` and a file of a skill whose directory is no longer the one found at start.

## Using a skill

- Type `/skill:<name>` (see [`skills.prefix`](configuration.md#skillsprefix)) anywhere in the prompt, as a whole word.
- The message is sent as typed; each skill named loads once with it. `//` at the start escapes it.

## Skill tool

- Returns a skill's instructions, or with `file` one of its files, read as `Read` does (images included).
- Its description lists the skills without `disable-model-invocation`.
- It only reads: it works in plan mode and is allowed by default. It is present only when a skill is loaded.
- [Rules](permissions.md#rules-per-tool) match the skill's directory or the file read; a bare `Skill` in `deny` removes it.

## Limitations

- `Read`, `Glob`, `Grep` and `LS` cannot reach a skill's files outside the start directory; `Skill`'s `file` can.
- `Skill` ties a skill to the directory found at start. A process that swaps that directory while Kopi starts can tie it to another, whose files `Skill` then reads (Kopi's home outside `skills/` stays refused).
- Its protection of Kopi's home is by file identity, as `Read`'s is, so a hard link into Kopi's home is readable.
- Sandboxed `Bash` cannot read `$KOPI_HOME/skills`, so scripts bundled there do not run; those in `~/.agents/skills` and project skills do.
- The model can write a trusted folder's `.agents/skills`, like its instruction files.
- A top-level `.md` file with a description in a skills directory loads as a skill, and nothing below a directory with a `SKILL.md` is searched.

## Not implemented

- Turning off one skill.
- A skill menu for a `/` typed mid-prompt.
- Reloading skills during a run.
- `allowed-tools` and other frontmatter fields.
- Argument placeholders.
