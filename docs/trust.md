# Trust and instructions

## Home AGENTS.md

`AGENTS.md` in Kopi's home, when present, is added to the system prompt in every folder, whether or not you trust it.

## Project instruction files

- A trusted folder's project instruction files are added to the system prompt after the home's `AGENTS.md`.
- One file per directory, from the directory holding `.git` (a directory or a file) down to the start directory (its symlinks resolved first), outermost first. The start directory alone when none holds `.git`.
- Each directory's file is `AGENTS.md`, else the first [`instructionFallbacks`](configuration.md#instructionfallbacks) name present. A symlinked file is followed.
- Instruction files are read once at startup, except an [added directory's](#added-directories), and have no size limit.

## Folder trust

Trust gates what a folder can add:

- `.mcp.json` servers ([MCP](mcp.md#mcpjson))
- project instruction files ([above](#project-instruction-files))
- project skills ([skills](skills.md))

The fullscreen interface asks before the session opens when you have not decided about the folder and any of these holds:

- The start directory's `.mcp.json` declares a server.
- The folder has project instruction files.
- The folder has project skills.

It lists the folder's servers, instruction files and skill folders, with these answers. When the lists do not fit the terminal's height, the page-up and page-down keys scroll them and the answers stay in view. When the terminal leaves the lists one row, only a count of the hidden lines shows, and with none they are left out:

- `Yes, trust this folder`
- `Yes, trust the parent folder` (not offered when the parent is the root or your home directory)
- `No, don't trust`

Answers:

- The answer is saved in `trust.json` and covers the folders below; the closest saved folder decides.
- In your home directory or the root no parent is offered, and the answer, yes or no, holds for this run only and is not saved.
- Cancelling saves nothing and skips the servers, instruction files and skills for this run.
- Print mode never asks: in a folder with no decision it skips the `.mcp.json` servers, project instruction files and project skills and warns on stderr.
- To change a saved answer, remove the folder's entry from `trust.json`.

## Added directories

A folder added with [`/add-dir` or `--add-dir`](usage.md#commands) is trusted or not by the same saved decisions.

- A folder a saved decision covers is added without a question. Trusted, its instruction files go to the model. Untrusted, it has file access only.
- Any other folder, in the fullscreen interface, asks first. The dialog lists the folder's instruction files and, when the answer is saved, the `.mcp.json` servers and skill folders that the answer would also cover whenever Kopi starts in that folder:
  - `Yes, and trust this folder` adds it with its instruction files and saves it as trusted in `trust.json`.
  - `Yes, but don't trust it` adds it with file access only and saves it as not trusted.
  - `No, don't add` and Esc add nothing and save nothing.
- For your home directory or the root the dialog does not say the answer is saved, and it holds for this run only.
- Print mode never asks: it adds a folder no decision covers with file access only and saves nothing.
- The instruction files are found as for the start directory, one per directory from the one holding `.git` down to the folder. They reach the model in a message, not the system prompt, and only when the folder is trusted. A file that a directory already added holds is not sent again in the same session; a session started or resumed is told again.
- [`/remove-dir`](usage.md#commands) keeps the saved decision, so adding the folder again asks nothing, unless it is your home directory or the root; change the decision in `trust.json`. It does not take back instruction files a session has already been sent; a session started afterwards is not sent them. A file only the removed directory held is sent again with the next directory added that holds it, the same one included. The dialog lists the instruction files that no directory already added holds when it opens, so after a `/remove-dir` before you answer, a file it did not list can be sent with the folder.

## Limitations

- A `.mcp.json` changed outside Kopi, such as by `git pull` or an editor, starts its servers without asking again.

## Not implemented

- A command or flag to set or change a folder's trust.
