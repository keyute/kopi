# Tools

## Modes

- Sessions are in plan or execute mode. Only the user switches: `/plan`, `/execute`, Shift+Tab, or approving a plan with [`ExitPlanMode`](#exitplanmode).
- Plan mode allows:
  - `Read`, `Glob`, `Grep`, `LS`, `Skill`, `ToolSearch`, `Agent`, `SendMessage`, `Workflow`, `TaskStop`, `WebFetch`, `WebSearch`, `AskUserQuestion`, `ExitPlanMode`, `StructuredOutput` (workflow agents with a `schema`)
  - read-only MCP tools
  - `Write` and `Edit` on the session's [plan file](#exitplanmode) only
  - `Bash` only in the [sandbox](sandbox.md), where it cannot write the workspace
- Other `Write` and `Edit` targets and other MCP tools need execute mode.
- Every session, `/new` and `/resume` included, starts in the configured [`mode`](configuration.md#mode); `--mode` overrides it.
- A hidden message tells the model the mode with a session's first prompt, the first prompt after a change and after a compaction; in plan mode it says to present a plan with [`ExitPlanMode`](#exitplanmode). Added [workspace](#file-tools) directories are announced the same way.
- [Rules](permissions.md#rules) decide which calls ask, run or are refused.

## File tools

- The workspace is the start directory plus directories added with [`/add-dir`](usage.md#commands) or `--add-dir`, until `/remove-dir` or exit. Relative paths and the default `path` of `Glob`, `Grep` and `LS` mean the start directory; a file in an added directory is named by its absolute path, and results there are absolute paths.
- `Read`, `Glob`, `Grep`, `LS`, `Write` and `Edit` refuse a path outside the workspace, except the plan file and saved output below.
- `/add-dir` and `--add-dir` refuse a path that does not exist or is not a directory, one already in the workspace or inside it, Kopi's home or anything in it, and, on Linux with the sandbox on, `/` (its policy would overlap `/proc`). A parent of a workspace directory can be added.
- `/remove-dir` refuses a path that is not an added directory, the start directory included, and tells the model with the next prompt. A call already running there finishes; one awaiting approval is refused when it runs unless the path is still inside another workspace directory.
- `Read` reads text files: up to 2000 lines, 2000 characters per line and 100 KiB per call. PNG, JPEG, GIF and WebP images come back whole, up to 20 MiB, ignoring `offset` and `limit`; one that cannot be processed is left out with a note. Other binary files and directories are refused.
- `Glob` finds files by glob (`*`, `?`, `**`, `{a,b}`; no `/` matches names at any depth): up to 100 paths, newest first.
- `Grep` searches a file or the files under a directory with an RE2 regex. `output_mode` is `files_with_matches` (default), `content` (up to 10 `context` lines, 100 KiB) or `count`; `limit` is 100 by default, at most 1000; `glob` filters files; `case_insensitive` ignores case.
- `LS` lists one directory, not recursively, up to 500 entries sorted by name, directories ending in `/`, hidden and gitignored ones and `.git/` included.
- `Glob` and `Grep` skip `.git` directories below the searched path, anything a `.gitignore` in the start directory or below ignores, and symlinks; `Grep` also skips binary files. A `path` inside `.git` or an ignored directory is searched.
- `Glob`, `Grep` and `LS` do not follow symlinks they find (`LS` lists them, except an unresolvable one). A `path` that is itself a symlink is resolved, as for `Read`.
- `Write` creates a file or replaces its whole content. `Edit` replaces a string that must match exactly once unless `replace_all` is set, in a text file up to 10 MiB. Both replace the file atomically and keep its mode.
- `Read`, `Write` and `Edit` refuse Kopi's home, paths there that do not exist yet included, except the session's [plan file](#exitplanmode); `Read` also reaches its [saved output](#saved-output). `Glob`, `Grep` and `LS` refuse a path there and leave it out of what they search or list; `Grep` also searches one saved output file named by `path`.

## Bash

- Runs `bash -c` (`/bin/sh` without `bash` on `PATH`) in the start directory with Kopi's environment. Every call is a fresh shell with no input; `cd` and variables do not persist.
- `timeout` is in milliseconds, default 120000, at most 600000. A [subagent's](subagents.md) command stops at its timeout; the main agent's moves to the background.
- A command still running at its timeout, started with `run_in_background` or moved with the `background` key runs on as a [background task](#background-tasks); the result names the task and its [output file](#saved-output).
- Interrupt kills a foreground command's whole process group (on Windows, only the shell).
- Returns stdout and stderr combined, keeping the first and last 15,000 bytes; the rest is [saved to a file](#saved-output). A non-zero exit is an error reporting the code.
- Runs in the [sandbox](sandbox.md) unless the call sets `unsandboxed` (offered only when sandboxed and `sandbox.allowUnsandboxed` is true), which asks like `Bash` without the sandbox, see [outside the sandbox](sandbox.md#outside-the-sandbox). With `sandbox.allowDomains` the output ends with the hosts the proxy refused, see [network](sandbox.md#network).

## Saved output

- `Bash`, background commands and MCP tool results keep the first and last 15,000 bytes; longer output is also written whole to `outputs/<session id>/<random name>.txt` in Kopi's home, and the cut notice names the file and the line to continue from.
- Output that fits is not saved, except a background command's, saved from when it backgrounds: the file is how the model reads it while it runs.
- The file is mode `0600` in a `0700` folder and holds at most 64 MiB; past that the notice says `first <n> bytes of output` and the end is only in the result.
- `Read` reaches the file with `offset` and `limit`, and `Grep` when `path` names it: the session's own files only, one at a time, read only. `Write`, `Edit` and the rest of Kopi's home, the `outputs/` folder included, stay refused. The root session, its [subagents](subagents.md) and [workflow agents](workflows.md#workflow-agents) share one folder.
- A resumed session keeps its folder, but files older than a week are removed at start. Long output of [your own commands](usage.md#your-own-commands) and any you moved to the background is saved there too.

## Background tasks

- Tasks are background commands (yours moved with the `background` key too, see [your own commands](usage.md#your-own-commands)), [subagents](subagents.md) and [workflows](workflows.md), numbered `t1`, `t2`, … per run.
- Tasks have no time limit. A command keeps the sandbox profile of the mode it started in; a subagent follows the session's mode unless its definition pins plan mode.
- `/tasks` lists running tasks, then the rest under `Finished`; Enter opens one, Esc goes back.
- An open task's output shows from its end and is read again only when a task starts or ends or the task is reopened, not as it prints.
- The model has no tool that reads a running task: a command's output is in its [saved output](#saved-output) file, which `Read` and `Grep` reach while it runs; a subagent's or workflow's result arrives when it ends.
- `TaskStop` kills a command task's process group (on Windows, only the shell), whatever mode it started in, and stops a subagent or workflow. On a task that was already over it says so and stops nothing.
- When a task completes or fails, the model is told at the next boundary of the running turn, or in a new turn when none runs: a command's status and last 20 non-empty output lines, a subagent's final message, a workflow's returned value or error. After an interrupt such notices wait for the next prompt.
- A task you stop from `/tasks` is reported with the next prompt or notice, starting no turn; one the model stops is not reported again.
- Completed, failed and `/tasks`-stopped tasks add a transcript row, restored on resume, see [Transcript](usage.md#transcript).
- Tasks are stopped on exit, on `/new` or `/resume`, and at the end of `kopi -p`. `kopi -p` starts no turn for a command's end, so a command still running at the final reply is killed; it waits for running [subagents](subagents.md#what-you-see).

## Agent and SendMessage

- `Agent` starts a [subagent](subagents.md) on a prompt and returns its task id at once; it runs in the background and its end is reported to the model. `SendMessage` sends a subagent a further message by task id, see [use](subagents.md#use). Both exist only when a subagent is defined.

## Workflow

- Runs a JavaScript script that starts agents and combines their results, returning a task id at once; it runs in the background as a [workflow](workflows.md) whose end is reported like `Agent`'s.
- Offered only in the fullscreen interface; callable once a prompt you type in the main session contains `ultracode`.
- It works in plan mode and does not ask by default; a bare `Workflow` rule in `deny` removes it.

## AskUserQuestion

- Asks one to four questions and waits; its transcript row keeps the answers.
- A question has a header (its tab, cut past 16 columns) and two to four options with descriptions; it may allow several.
- A single-select option may have a markdown preview, shown beside the options at 64 columns or wider and below otherwise; a long one scrolls with PgUp, PgDn or the mouse wheel.
- Every question ends with a `Type something.` row; an option labelled `Other` is refused. Tab adds a note to the focused option, sent with it when chosen and dropped otherwise.
- Several questions get a tab strip and a review page of answers and gaps before submitting.
- Esc or Ctrl+C dismisses the questions and stops the turn; the model sees that. There is no time limit.
- Fullscreen only; `kopi -p` has no `AskUserQuestion`.
- A response containing a question runs its tool calls one at a time. A subagent's approval request arriving meanwhile is shown first.

- Keys while a question is open; Enter, Tab, Esc, Up, Down, PgUp, PgDn, Ctrl+V and the newline keys follow [`keys`](configuration.md#keys):

| Where | Key | Action |
|---|---|---|
| Options | Up, Down | Move; Down from the last option focuses the `Type something.` row |
| Options | a number | Single-select: choose that option and go on. Multi-select: toggle it. The `Type something.` number focuses its field |
| Options | Space | Toggle the focused option (multi-select) |
| Options | Enter | Single-select: choose the focused option and go on. Multi-select: go on |
| Options | Tab | Open a note on the focused option |
| Options | Left, Right | Previous or next question; Right from the last opens the review |
| `Type something.` | Enter | Answer with the typed text and go on; blank text asks you to answer unless a multi-select question already has an option chosen, which goes on |
| `Type something.` | Up on the first row | Back to the options |
| `Type something.` | Left, Right | Switch questions while the field is empty, else move the cursor |
| Note | Enter or Tab | Save the note; a blank note removes it |
| Note | Esc or Ctrl+C | Discard the edit and close the note; the questions stay open |
| Review | Enter | Submit; with a gap, jump to the first unanswered question |
| Review | Left | Back to the last question |
| Any | PgUp, PgDn | Scroll a cut preview or review, in a note too (the mouse wheel over the dialog does too); they do not move the cursor in a text field |
| Any | Esc | Dismiss the questions (in a note, close the note) |

## ExitPlanMode

- Takes no arguments. In plan mode it shows the session's plan file and asks `Approve this plan?` with `Yes` and `No`.
- Each root session has a plan file, `plans/<session id>.md` in Kopi's home, kept on resume and never deleted. The model writes it with `Write` and `Edit`; `Read`, `Write` and `Edit` reach it in both modes without asking by default, and [rules](permissions.md#precedence) still apply.
- While the dialog is open the plan is drawn as Markdown in the main session's transcript, switching there from a subagent view. Once answered, the `Plan approval` row closes to your decision; a click or Ctrl+O reopens the plan, except after a resume, when it is only in the file.
- `Yes` switches to execute mode and the turn continues, telling the model to carry out the plan.
- `No` takes typed feedback; the model stays in plan mode and can revise and ask again. A blank `No` or Esc dismisses the plan and stops the turn.
- The call is an error outside plan mode or when the plan file is missing or empty. The model is told if you edited the file while the dialog was open.
- The [`edit-plan`](configuration.md#keys) key in the dialog, and `/plan open` any time, open the plan file in `$VISUAL`, else `$EDITOR`; a notice says so when neither is set or the file does not exist yet.
- Fullscreen only: `kopi -p` has no `ExitPlanMode` or plan file. A deny rule removes it; a bare ask rule naming it or `*` makes it ask for approval. [Subagents](subagents.md#what-a-subagent-gets) never get it or the plan file, and `Bash` in the [sandbox](sandbox.md) cannot reach it.
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

- Fetches one http or https page on a public host. The call's prompt and up to 100,000 characters of the page (from the optional `offset`, default 0) go to the provider of [`smallModel`](configuration.md#smallmodel), else of the session model, which answers. The main model gets the answer and the final URL, not the page.
- HTML becomes markdown from its main content; text, JSON and XML pass through; other content types are refused.
- Pages are up to 5 MiB, fetched within 30 seconds, without JavaScript or a cache. Only the `Content-Type` header decides the charset; without one, the type is sniffed and the body read as UTF-8 unless it starts with a UTF-16 byte-order mark.
- When more remains, the result says so and gives the `offset` to continue from.
- Up to 5 redirects on the same host (`www.` and http to https count as the same) are followed; any other is returned for the model to fetch anew.
- Refused: `localhost`, names without a dot, `.localhost`, `.local` and `.internal` names, and non-public addresses, including ones a name resolves to.
- Connects directly: ignores `HTTP_PROXY` and `HTTPS_PROXY`, and `sandbox.network` and `sandbox.allowDomains` do not govern it.
- Asks by default in both modes, see [precedence](permissions.md#precedence).

## WebSearch

- The query goes to the provider of `smallModel`, else of the session model, which searches with its built-in search and answers.
- The main model gets the answer and a `Sources:` list of up to 20 `- [title](url)` lines (`- url` without a title), or `Sources: none`: the pages the answer cites, or those the provider consulted when it cited none.
- Offered only while the model used has an API protocol with hosted search (OpenAI Responses, also Azure and Codex; Anthropic Messages; Google Generative AI), decided at session open and, for the main session, on `/model`; a running subagent or workflow agent keeps what it opened with.
- An endpoint speaking one of those protocols without the hosted tool fails at call time. Chat-completions providers, Bedrock, Vertex and Mistral get no `WebSearch`.
- Google returns redirect links, not the pages' own URLs. A search an Anthropic response pauses mid-turn returns what it has.
- Asks by default in both modes, see [precedence](permissions.md#precedence).

## Limitations

- `Edit` checks its pinned destination's identity and content just before replacing and refuses detected conflicts. This is best-effort, not an atomic compare-and-swap: another writer can change the file between check and rename. `Write` replaces unconditionally.
- `Write` and `Edit` check parent-directory handles first, so swapping a parent for a symlink cannot redirect them. Another process can still move a checked directory, into Kopi's home too; later changes follow its handle.
- Permission checks and these protections are not an OS boundary.
- `Read` and `Grep` protect Kopi's home by file identity, re-checked on every read and every directory searched:
  - a hard link into Kopi's home, or a credential copied elsewhere, is readable
  - whether Kopi's home exists is observable
  - if it cannot be inspected, all reads are refused
- The plan file is matched by its absolute, cleaned path; a symlink, hard link or different-case spelling of it counts as the rest of Kopi's home.
- `Read` and `Grep` tell binary files, and `Read` images, from the first 8 KiB, not the name.
- `Read` refuses absolute symlinks, even ones pointing inside the start directory.
- `Grep` cuts a matching line to its first 2000 characters.
- Outside the Linux sandbox, a process a command leaves running (`cmd &`) can outlive the call and its task, and a running command keeps running if Kopi is killed. In the Linux sandbox everything a command started ends with it or with Kopi.
- Background tasks live in memory and do not survive a restart; a command's [saved output](#saved-output) stays on disk for a week.
- A saved output that is one long line stays mostly unreachable, as `Read` cuts lines at 2000 characters. One with a NUL in its first 8 KiB is refused as binary. The file holds raw bytes; the inline text has invalid UTF-8 replaced.

## Not implemented

- Streaming command output.
- Domain rules for `WebFetch`; domain filters, citation markers and other backends for `WebSearch` (an MCP server can provide one); PDF and image pages.
- Typed questions for `AskUserQuestion` (yes/no, text only, number), questions in print mode, and MCP elicitation.
- Clearing the context when a plan is approved, `/plan <task>`, a tool that lets the model enter plan mode, and plan approval in print mode.
- `Glob` and `LS` on saved output, and searching the `outputs/` folder as a whole.
