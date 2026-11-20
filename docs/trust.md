# Trust and instructions

## Home AGENTS.md

`AGENTS.md` in Kopi's home, when present, is added to the system prompt in every folder, whether or not you trust it.

## Project instruction files

- A trusted folder's project instruction files are added to the system prompt after the home's `AGENTS.md`.
- One file per directory, from the directory holding `.git` (a directory or a file) down to the start directory (its symlinks resolved first), outermost first. The start directory alone when none holds `.git`.
- Each directory's file is `AGENTS.md`, else the first [`instructionFallbacks`](configuration.md#instructionfallbacks) name present. A symlinked file is followed.
- Instruction files are read once at startup and have no size limit.

## Folder trust

Trust gates what a folder can add:

- `.mcp.json` servers ([MCP](mcp.md#mcpjson))
- project instruction files ([above](#project-instruction-files))
- project skills ([skills](skills.md))

The fullscreen interface asks before the session opens when you have not decided about the folder and any of these holds:

- The start directory's `.mcp.json` declares a server.
- The folder has project instruction files.
- The folder has project skills.

It lists the folder's instruction files, skill folders and servers, with these answers. When the lists do not fit the terminal's height, the page-up and page-down keys scroll them and the answers stay in view. When the terminal leaves the lists one row, only a count of the hidden lines shows, and with none they are left out:

- `Yes, trust this folder`
- `Yes, trust the parent folder` (not offered when the parent is the root or your home directory)
- `No, don't trust`

Answers:

- The answer is saved in `trust.json` and covers the folders below; the closest saved folder decides.
- In your home directory or the root no parent is offered, and the answer, yes or no, holds for this run only and is not saved.
- Cancelling saves nothing and skips the servers, instruction files and skills for this run.
- Print mode never asks: in a folder with no decision it skips the `.mcp.json` servers, project instruction files and project skills and warns on stderr.
- To change a saved answer, remove the folder's entry from `trust.json`.

## Limitations

- A `.mcp.json` changed outside Kopi, such as by `git pull` or an editor, starts its servers without asking again.

## Not implemented

- A command or flag to set or change a folder's trust.
