# Trust and instructions

## Home AGENTS.md

`AGENTS.md` in Kopi's home joins the system prompt in every folder, trusted or not.

## Project instruction files

- A trusted folder's instruction files join the system prompt after the home's `AGENTS.md`.
- One file per directory, outermost first, from the one holding `.git` (directory or file) down to the start directory (symlinks resolved); the start directory alone when none holds `.git`.
- A directory's file is `AGENTS.md`, else the first [`instructionFallbacks`](configuration.md#instructionfallbacks) name present. Symlinks are followed.
- Files are read once at startup (an [added directory's](#added-directories) when added), with no size limit.

## Folder trust

Trust gates three things a folder can add:

- `.mcp.json` servers ([MCP](mcp.md#mcpjson))
- project instruction files ([above](#project-instruction-files))
- project skills ([skills](skills.md))

The fullscreen interface asks before the session opens when you have not decided about the folder and it has any of them (for servers, the start directory's `.mcp.json`). The dialog lists them, with these answers:

- `Yes, trust this folder`
- `Yes, trust the parent folder` (not offered when the parent is the root or your home directory)
- `No, don't trust`

When the lists do not fit, `page-up` and `page-down` scroll them with the answers in view. With one row left for them only a count of hidden lines shows; with none they are left out.

- An answer is saved in `trust.json` and covers the folders below it; the closest saved folder decides.
- In your home directory or the root the answer holds for this run only.
- Cancelling saves nothing and skips all three for this run. Print mode never asks: in an undecided folder it skips them and warns on stderr.
- To change a saved answer, remove the folder's entry from `trust.json`.

## Added directories

A folder added with [`/add-dir` or `--add-dir`](usage.md#commands) follows the same saved decisions.

- A folder a saved decision covers is added without a question: trusted, its instruction files go to the model; untrusted, it has file access only.
- Any other folder asks first in the fullscreen interface (for your home directory or the root, the answer holds for this run only). The dialog lists its instruction files and, when the answer is saved, the servers and skill folders it would cover whenever Kopi starts there:
  - `Yes, and trust this folder` adds it with its instruction files and saves it as trusted.
  - `Yes, but don't trust it` adds it with file access only and saves it as not trusted.
  - `No, don't add` and Esc add nothing and save nothing.
- Print mode never asks: it adds an undecided folder with file access only and saves nothing.
- Instruction files are found as for the start directory. They reach the model in a message, not the system prompt, only when the folder is trusted. A file an added directory already holds is not sent again in the session; a started or resumed session is told again.
- [`/remove-dir`](usage.md#commands) keeps the saved decision, so adding the folder again asks nothing, unless it is your home directory or the root. It does not take back instruction files already sent.
- The dialog lists the instruction files no added directory already holds. After a `/remove-dir` before you answer, a file it did not list can be sent with the folder.

## Limitations

- A `.mcp.json` changed outside Kopi, such as by `git pull` or an editor, starts its servers without asking again.

## Not implemented

- A command or flag to set or change a folder's trust.
