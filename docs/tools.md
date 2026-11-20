# Tools

## Modes

- Sessions are in plan or execute mode. Only the user switches, with `/plan`, `/execute`, Shift+Tab or by approving a plan with [`ExitPlanMode`](#exitplanmode).
- Plan mode allows:
  - `Read`, `Glob`, `Grep`, `LS`, `Skill`, `ToolSearch`, `Agent`, `SendMessage`, `TaskOutput`, `TaskStop`, `WebFetch`, `WebSearch`, `AskUserQuestion`, `ExitPlanMode`
  - read-only MCP tools
  - `Write` and `Edit` on the session's [plan file](#exitplanmode) only
  - `Bash` only in the [sandbox](sandbox.md), where it cannot write the start directory
- `Write`, `Edit` on any other file and other MCP tools need execute mode.
- Sessions start and resume in the configured [`mode`](configuration.md#mode), including ones opened with `/new` and `/resume`.
- `--mode` overrides the config file.
- A mode switched to while the first session is opening is the mode it starts in.
- The model is told the session's mode, and in plan mode in the fullscreen interface the plan file's path, in one hidden message sent with the first prompt of a session and with the first prompt after the mode changes. It is not part of the system prompt. No message follows an approved plan: the tool's result already tells the model the session is in execute mode.
- [Rules](permissions.md#rules) decide which calls ask, run or are refused.

## File tools

- `Read` reads text files inside the start directory: up to 2000 lines, 2000 characters per line and 100 KiB per call.
  - It returns PNG, JPEG, GIF and WebP images whole, up to 20 MiB; one that cannot be processed is left out with a note. `offset` and `limit` do not apply to them.
  - Other binary files and directories are refused.
- `Glob` finds files under a directory inside the start directory by glob (`*`, `?`, `**`, `{a,b}`); a pattern without `/` matches names at any depth. Up to 100 paths, most recently modified first.
- `Grep` searches the contents of a file or the files under a directory with an RE2 regular expression.
  - `output_mode`: `files_with_matches` (default), `content` (with up to 10 `context` lines) or `count`.
  - `limit` is 100 by default, at most 1000.
  - `glob` filters the files searched; `case_insensitive` ignores case.
  - Content output is capped at 100 KiB, like `Read`'s.
- `LS` lists one directory, not recursively, sorted by name with directories ending in `/`, up to 500 entries, including hidden and gitignored entries and `.git/`.
- `Glob` and `Grep` skip:
  - `.git` directories below the searched path
  - anything a `.gitignore` in the start directory or below ignores
  - symlinks
  - for `Grep`, binary files
- A `path` inside `.git` or inside an ignored directory is searched.
- None of `Glob`, `Grep` and `LS` follows a symlink it finds while searching or listing (`LS` lists them, except one whose destination cannot be resolved). A `path` that is itself a symlink is resolved, as for `Read`.
- `Write` creates a file or replaces its whole content.
- `Edit` replaces a string that must match exactly once unless `replace_all` is set, in a text file of up to 10 MiB.
- `Write` and `Edit` work inside the start directory, replace the file atomically and keep its mode.
- `Read`, `Write` and `Edit` refuse Kopi's home, including paths there that do not exist yet, except the session's [plan file](#exitplanmode).
- `Glob`, `Grep` and `LS` refuse a path there and leave Kopi's home out of what they search or list.

## Bash

- Runs a command with `bash -c` (`/bin/sh` when `bash` is not on `PATH`) in the start directory, with Kopi's environment.
- Every call is a fresh shell with no input; `cd` and variables do not carry over.
- `timeout` is in milliseconds, default 120000, at most 600000.
- A command still running at its timeout keeps running as a [background task](#background-tasks), as does one started with `run_in_background` or moved with the `background` key. The result names the task.
- On interrupt a foreground command's whole process group is killed (on Windows, only the shell).
- Returns stdout and stderr combined, keeping the first and last 15,000 bytes.
- A non-zero exit is an error reporting the exit code.
- Runs in the [sandbox](sandbox.md), unless the call sets `unsandboxed` (offered only when sandboxed and `sandbox.allowUnsandboxed` is true), which asks like `Bash` without the sandbox, see [outside the sandbox](sandbox.md#outside-the-sandbox). With `sandbox.allowDomains` the output ends with the hosts the proxy refused, see [network](sandbox.md#network).

## Background tasks

- Tasks are background commands and [subagents](subagents.md), numbered `t1`, `t2`, … per run in one sequence.
- They have no time limit. A command keeps the sandbox profile of the mode it started in; a subagent follows the session's mode unless its definition pins plan mode.
- `TaskOutput` returns a task's status and its output so far, kept like `Bash` output; for a subagent, its activity so far.
- `TaskStop` kills a command task's process group (on Windows, only the shell), whichever mode it started in, and stops a subagent.
- When a task completes or fails, the model is told, for a command, its status and its last 20 non-empty output lines, and for a subagent its final message, at the next boundary of the running turn, or in a new turn when none runs. After an interrupt such notices wait for the next prompt; they never return to the input.
- A task stopped from `/tasks` is reported to the model along with the next prompt or the next task notice, without starting a turn. One the model stops is not reported again.
- A task that completes, fails or is stopped from `/tasks` adds a `• task t1 finished · <command> · 12s` row to the transcript (`failed` or `stopped` in place of `finished`), restored on resume.
- Tasks are stopped on exit, when `/new` or `/resume` opens another session, and at the end of `kopi -p`.
- `kopi -p` never starts a turn for a command's end, so a command still running at the final reply is killed. It waits for running [subagents](subagents.md#what-you-see).

## Agent and SendMessage

- `Agent` starts a [subagent](subagents.md) on a prompt and returns its task id at once; the subagent runs in the background.
- `SendMessage` sends a further message to a subagent by task id. A running subagent reads it when its current step ends; one that has ended, or whose run is being stopped, starts again with its earlier context, and the call does not wait for the stopping run to end.
- The notice the agent receives when a subagent's run completes or fails, or when you stop it, lists the messages queued for it that it never read as undelivered. When the agent stops a subagent with `TaskStop` it receives no notice, and those messages are dropped.
- Both exist only when a subagent is defined.

## AskUserQuestion

- Puts one to four questions to you and waits for the answers.
- Each question has a header, shown as its tab and cut with `…` past 16 columns (the model is asked for at most 12 characters), and two to four options, each with a description. A question may allow several options (multi-select).
- A single-select option may have a markdown preview, shown in a box while the option is focused: beside the options at 64 columns or wider, below them otherwise. Beside the options the box is as wide as the widest preview of that question needs, up to the room there; below them it spans the width. A preview that does not fit shows `… N earlier lines` and `… N more lines` inside the box and scrolls with PgUp and PgDn, or with the mouse wheel over the dialog.
- Every question ends with a `Type something.` row for a typed answer, and an option labelled `Other` is refused. A single-select answer is one option or typed text, a multi-select answer is any options and/or typed text.
- Tab adds a note to the focused option; the note goes to the model with the option when it is chosen.
- With several questions a tab strip shows which are answered, and a review page after the last one lists the answers and the gaps before you submit. A single question answers at once.
- The result lists each question with its answer, any note and any typed text, or says you dismissed the questions.
- Esc dismisses the questions and stops the turn; the model sees that you dismissed them. Ctrl+C interrupts the turn. There is no time limit.
- Only in the fullscreen interface; `kopi -p` has no `AskUserQuestion`.
- A response's tool calls run one at a time when it contains a question.
- A subagent's approval request that arrives while a question is open is shown first; the question returns after it.
- Keys while a question is open; Enter, Tab, Esc, Up, Down, PgUp, PgDn, Ctrl+V and the newline keys follow [`keys`](configuration.md#keys):

| Where | Key | Action |
|---|---|---|
| Options | Up, Down | Move; Down from the last option focuses the `Type something.` row |
| Options | a number | Single-select: choose that option and go on. Multi-select: toggle it. The `Type something.` number focuses its field |
| Options | Space | Toggle the focused option (multi-select) |
| Options | Enter | Single-select: choose the focused option and go on. Multi-select: go on |
| Options | Tab | Open a note on the focused option |
| Options | Left, Right | Previous or next question; Right from the last opens the review |
| Options | PgUp, PgDn | Scroll a cut preview (the mouse wheel over the dialog does too) |
| `Type something.` | Enter | Answer with the typed text and go on; blank text asks you to answer unless a multi-select question already has an option chosen, which goes on |
| `Type something.` | Up on the first row | Back to the options |
| `Type something.` | Left, Right | Switch questions while the field is empty, else move the cursor |
| Note | Enter or Tab | Save the note; a blank note removes it |
| Note | Esc | Discard the edit and close the note |
| Review | Enter | Submit; with a gap, jump to the first unanswered question |
| Review | Left | Back to the last question |
| Review | PgUp, PgDn | Scroll a cut review (the mouse wheel over the dialog does too) |
| Any | Esc | Dismiss the questions (in a note, close the note) |

## ExitPlanMode

- Takes no arguments. In plan mode it shows you the plan from the session's plan file and asks `Approve this plan?` with `Yes` and `No`.
- Each root session in the fullscreen interface has a plan file, `plans/<session id>.md` in Kopi's home. The model writes it with `Write` and `Edit`; `Read`, `Write` and `Edit` reach that one file in both modes without asking by default. [Rules](permissions.md#precedence) still apply to it.
- A resumed session keeps its plan file; a new session gets a new one. Kopi never deletes plan files.
- The plan is drawn in the main session's transcript as Markdown when it is presented, and that view opens if you were viewing a subagent. On resume only the tool's row is restored; the plan is in the file.
- `Yes` switches the session to execute mode and the turn continues.
- `No` takes feedback typed on its row. The feedback goes to the model, which stays in plan mode and can revise the plan and ask again. A blank `No` or Esc dismisses the plan and stops the turn; the model sees that you dismissed it.
- The call is an error when the session is not in plan mode or the plan file is missing or empty.
- If you edited the plan file while the dialog was open, the model is told.
- The [`edit-plan`](configuration.md#keys) key in the dialog, and `/plan open` at any time, open the plan file in `$VISUAL`, else `$EDITOR`; with neither set a notice says so. Before the plan file exists `/plan open` shows a notice instead.
- Only in the fullscreen interface; a deny rule removes it, a bare ask rule naming it or `*` makes it ask for approval, and [subagents](subagents.md#what-a-subagent-gets) never get it or the plan file. `Bash` in the [sandbox](sandbox.md) cannot reach it, as it cannot read or write Kopi's home. `kopi -p` has no `ExitPlanMode` and no plan file.
- Keys while the plan is open; Up, Down, Enter, Esc, PgUp, PgDn, Ctrl+V and the newline keys follow [`keys`](configuration.md#keys):

| Where | Key | Action |
| --- | --- | --- |
| Options | Up, Down | Move between `Yes` and `No` |
| Options | Enter | Choose the focused option |
| Options | 1 | Approve at once |
| Options | 2 | Move to `No` to type the feedback |
| `No` | Enter | Send the typed feedback; blank dismisses the plan and stops the turn |
| `No` | Up on the first row | Back to `Yes` |
| `No` | `newline`, `paste` and terminal paste | Start a line, insert clipboard or pasted text; digits are text |
| Any | `edit-plan` (default Ctrl+G) | Open the plan file in your editor |
| Any | Esc | Dismiss the plan and stop the turn |
| Any | PgUp, PgDn | Scroll the transcript |

## WebFetch

- Fetches one http or https page on a public host and has [`smallModel`](configuration.md#smallmodel) answer the call's prompt from it.
- The main model gets the answer and the page's final URL, not the page.
- HTML is converted to markdown from its main content; text, JSON and XML pass through; other content types are refused.
- Pages are up to 5 MiB, fetched within 30 seconds and without JavaScript. The small model reads their first 100,000 characters.
- Redirects on the same host (a leading `www.` and http to https count as the same) are followed, at most 5. Any other redirect is returned for the model to fetch in a new call.
- Refused: `localhost`, names without a dot, `.localhost`, `.local` and `.internal` names, and non-public addresses, including ones a name resolves to.
- Asks by default, in both modes: see [precedence](permissions.md#precedence).

## WebSearch

- Has `smallModel` search the web with its provider's built-in search and answer the call's query.
- The main model gets the answer followed by a `Sources:` list of up to 20 `- [title](url)` lines (`- url` when the page has no title), or `Sources: none`.
- The list is the pages the answer cites, or the pages the provider consulted when it cited none, not the pages themselves.
- The tool exists only when `smallModel`'s API protocol has hosted search: OpenAI Responses (also Azure and Codex), Anthropic Messages and Google Generative AI.
- Asks by default, in both modes: see [precedence](permissions.md#precedence).

## Limitations

- `Edit` detects identity or content changes at its pinned destination immediately before replacement and refuses detected conflicts. This is best-effort, not atomic compare-and-swap: reads can race and another writer can change the file between the check and rename. `Write` replaces unconditionally.
- `Write` and `Edit` pin and check parent-directory handles before making changes, so replacing a parent name with a symlink cannot redirect those changes.
  - Another process can still move a checked directory, including into Kopi's home; later changes follow its handle.
  - Permission checks and these protections are not an OS boundary.
- `Read` and `Grep` protection is by file identity, re-checked on every read and, for the searches, on every directory searched:
  - a hard link into Kopi's home or a credential copied elsewhere is readable
  - whether Kopi's home exists is observable
  - if Kopi's home cannot be inspected, all reads are refused
- `Read` and `Grep` tell binary files, and `Read` images, from their first 8 KiB, not their names.
- `Read` refuses absolute symlinks, even ones pointing inside the start directory.
- `Grep` shows only the first 2000 characters of a line; a match past that shows the line cut to that prefix.
- Outside the Linux sandbox, a background process a command leaves running (`cmd &`) can outlive the call and its background task, and a running command or background task keeps running if Kopi is killed. In the Linux sandbox everything a command started ends when it does, or when Kopi does.
- Background tasks and their output live only in memory and do not survive a restart.
- `WebFetch` connects directly and ignores `HTTP_PROXY` and `HTTPS_PROXY`; `sandbox.network` and `sandbox.allowDomains` do not govern it.
- An approved `WebFetch` URL is requested from its host, and the call's prompt and up to 100,000 characters of the page go to `smallModel`'s provider.
- `WebFetch` runs no JavaScript, and only the `Content-Type` header decides a page's charset; without one, the type is sniffed from the body and it is read as UTF-8 unless it starts with a UTF-16 byte-order mark.
- `WebSearch`'s query goes to `smallModel`'s provider, which runs the search.
  - Hosted search is offered by protocol, so an endpoint that speaks one of those protocols but lacks the hosted tool fails at call time with the provider's error.
  - Chat-completions providers, Bedrock, Vertex and Mistral get no `WebSearch`.
  - Google returns redirect links, not the pages' own URLs.
  - A search an Anthropic response pauses mid-turn returns what it has.
- In an `AskUserQuestion` dialog Space, Left, Right and the number keys are fixed keys, not [`keys`](configuration.md#keys) entries.
- A note on an option that is not chosen is dropped.
- The plan file is matched by its absolute, cleaned path; a symlink, hard link or different-case spelling of it is treated like the rest of Kopi's home.
- On resume the plan is not drawn again.
- In an `ExitPlanMode` dialog the number keys are fixed keys, not [`keys`](configuration.md#keys) entries.
- While a question is open PgUp and PgDn, and the mouse wheel over the dialog, scroll a cut preview or review, not the transcript, and do nothing when nothing is cut.

## Not implemented

- Streaming command output.
- Workflows.
- Domain rules for `WebFetch`.
- Domain filters and citation markers for `WebSearch`.
- Other search backends (an MCP server can provide one).
- PDF and image pages.
- Typed questions for `AskUserQuestion` (yes/no, text only, number), questions in print mode, and MCP elicitation.
- Clearing the context when a plan is approved.
- `/plan <task>`.
- A tool that lets the model enter plan mode.
- Plan approval in print mode.
- Deleting old plan files.
- A configurable plans directory.
