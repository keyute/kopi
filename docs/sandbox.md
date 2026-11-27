# Sandbox

`Bash` runs inside an OS sandbox when available (unless `sandbox.enabled` is `false`): `sandbox-exec` on macOS, `bwrap` on Linux. See [tools](tools.md#bash) for `Bash` itself.

## Requirements

- The sandbox exists only on macOS and on Linux on amd64 and arm64.
- Linux needs `bwrap` on `PATH` and unprivileged user namespaces; containers that forbid them or forbid mounting `/proc` have no sandbox.
  - Ubuntu 24.04 restricts them through AppArmor until `bwrap` has an AppArmor profile or `kernel.apparmor_restrict_unprivileged_userns` is `0`. 25.04 and later work as shipped.
- It is unavailable when Kopi itself runs inside a Seatbelt sandbox, as profiles do not nest; tools that apply their own sandbox (`swift build`, `xcodebuild` packages) fail inside it for the same reason.

## Configuration

`sandbox` configures the OS sandbox `Bash` runs in:

```json
{
  "version": 1,
  "sandbox": {
    "allowWrite": ["~/Library/Caches/go-build"],
    "denyRead": ["~/.ssh"],
    "network": false,
    "allowDomains": ["github.com", "*.github.com", "registry.npmjs.org:443"],
    "denyDomains": ["gist.github.com"],
    "allowUnsandboxed": true
  }
}
```

- `sandbox.enabled`: `true` refuses to start when the sandbox is unavailable, `false` runs `Bash` without it, and unset uses it when available, else warns and runs `Bash` without it.
- Without the sandbox, `Bash` runs only after approval or when an allow rule or the [classifier](permissions.md#classifier) allows it, and plan mode refuses it. `Read`, `Glob`, `Grep` and `LS` work the same either way.
- `sandbox.allowWrite` and `sandbox.denyRead` take plain paths, no globs: `/abs` is absolute, `~/x` is under the home directory, anything else is relative to the start directory. So `.env` means only `<start directory>/.env`, unlike in a [rule](permissions.md#rules).
- `sandbox.network` (default `false`) allows IP networking.
- `sandbox.allowDomains` and `sandbox.denyDomains` (default empty) list the hosts commands may reach while `sandbox.network` is false, see [Network](#network); with `sandbox.network` true, either list refuses to start.
- A later layer's list replaces an earlier one's.
- `sandbox.allowUnsandboxed` (default `true`) offers `Bash` the [`unsandboxed`](#outside-the-sandbox) parameter.

## Write

- A command can write only in the start directory, `/tmp`, the temp directory and the `sandbox.allowWrite` paths.
- Not writable in the start directory's `.git`: on macOS the files that choose git's config and hooks (`hooks`, `config`, `config.worktree`, `commondir`, `worktrees`, `modules`); on Linux anything.
- `HEAD` in the start directory is not writable on Linux, or on macOS when there is no `.git`.
- In plan mode nothing in the start directory is writable, including `sandbox.allowWrite` paths inside it.
- A directory added with [`/add-dir`](usage.md#commands) is a write root like the start directory, with the same `.git`, `HEAD` and `.mcp.json` protection, read-only in plan mode. [`/remove-dir`](usage.md#commands) removes it for later commands; a running command keeps its profile.
- A command cannot create, write, rename or remove the start directory's `.mcp.json`, nor its symlink target. On macOS this holds for `.mcp.json` in any letter case and any directory. Git operations that change a protected `.mcp.json` fail.

## Read

- A command can read everything except Kopi's home (so not [saved output](tools.md#saved-output), which the model reads with `Read`) and the `sandbox.denyRead` paths. It cannot write, rename or remove those paths either.

## Network

- A command has no network unless `sandbox.network` is true (IP networking only) or `sandbox.allowDomains` lists hosts.
- With `sandbox.allowDomains`, commands reach the network only through an HTTP proxy inside Kopi, which connects to a host only when an `allowDomains` entry matches and no `denyDomains` entry does.
- An entry is a host name, or `*.example.com` (every subdomain, not `example.com`), case-insensitive; a `:port` suffix limits it to that port.
- Not accepted: IP addresses, `localhost`, a scheme or path, `*`, a wildcard over one label such as `*.com`, and a name ending in a numeric label.
- A listed host that resolves to a loopback, private or link-local address is refused.
- The command gets `HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY` and `GRPC_PROXY` pointing at the proxy and `NO_PROXY` set to `localhost,127.0.0.1,::1`, each in upper and lower case. Commands run `unsandboxed` or without the sandbox get neither.
- A refused request gets 403, and the output ends with a line naming up to 20 refused hosts, `[sandbox refused network access to: example.org:443]` (a background command reports it when it ends).
- Without the sandbox the lists have no effect.
- Unix sockets (ssh-agent, Docker) stay blocked. On Linux so do VM sockets, Unix datagram socket pairs (stream and seqpacket pairs work) and io_uring.

## Outside the sandbox

- A sandboxed `Bash` takes an optional boolean `unsandboxed`; a call that sets it runs that one command outside the sandbox.
- It is treated as `Bash` without the sandbox ([permissions](permissions.md#precedence)): plan mode refuses it before any rule; in execute mode rules apply, else it asks or the [classifier](permissions.md#classifier) reviews it.
- A [subagent](subagents.md) can use it; one whose definition pins plan mode is always refused.
- Without the sandbox, or with `sandbox.allowUnsandboxed` false, the parameter is not offered.
- A command you type with `!` is not a tool call and runs outside the sandbox; see [permissions](permissions.md#limitations).

## Limitations

### macOS

- `sandbox-exec` is deprecated by Apple.
- `ps`, `pgrep` and `top` do not work, and programs that open a pseudo-terminal, such as `script`, fail.
- With `sandbox.allowDomains`, a command can connect only to the proxy: it cannot listen on a port, reach a server it started, or resolve names. Any local process can use a running command's proxy port, and a process a command leaves running keeps access to that port number and reaches whatever listens there later.

### Linux

- Only the `.mcp.json` of the start directory and of added directories is protected by name; a command can create or change one in any other directory.
- A denied directory reads as empty, and opening a denied file fails with "Permission denied".
- Sandboxed commands refuse policies whose deny-write self-bind would overlap `/proc` or `/dev` (an ancestor or descendant too), to keep the private process and device mounts.
  - This includes plan mode when the start directory is `/`. `/add-dir /` fails with `failed to add /` and adds nothing.
  - Read-deny masks over those mounts remain supported.
  - A refused command does not run unsandboxed.
- While sandboxed commands run, an outside program that removes a protected path or renames over it, such as an editor's save, lifts the protection on that path for those commands.
- Every command that writes the start directory's `.git` fails (`git add`, `commit`, `checkout`, `stash`, `fetch`).
- Sandboxed commands refuse to run while a denied path passes through a symlink whose entry is in a write root, such as a symlinked `.mcp.json` or `.git`, valid or broken.
  - They also refuse an alias outside write roots when, after following a symlink, the first missing component lies inside a write root.
- 32-bit programs cannot run: their system calls end the process.
- Commands cannot create user namespaces: rootless containers, browsers' own sandboxes and nested bubblewrap fail.
- `ps`, `pgrep` and `top` see only the command's own processes.
- With `sandbox.allowDomains`, port 3128 on the command's loopback is the proxy, programs can listen and connect on that loopback, and every command starts through the `kopi` binary, which must not be under Kopi's home or a `sandbox.denyRead` path (Kopi refuses to start otherwise).

### Linux placeholders

- While a sandboxed command runs, each denied path that does not exist shows outside the sandbox as an empty file or directory: in a typical repository `.mcp.json` and `HEAD`, and `.git` when the start directory has none.
- Kopi removes only its recorded creations that are still empty and match their creation-time device, inode and kind, once no sandboxed command (background tasks included) runs in any Kopi with the same home. The check is best-effort: outside changes can race removal and inodes can be reused.
- A killed Kopi's creations are cleaned after the next sandboxed command; interruption before recording can leave orphans.
- Kopis with different `KOPI_HOME`s do not coordinate: one can remove the other's placeholders in the same directory, such as for a missing `.mcp.json`, which unmounts the deny inside the other's running command.

### macOS and Linux

- Sandboxed commands inherit Kopi's environment, including provider API keys, and can read any file outside Kopi's home and `sandbox.denyRead`.
- With `sandbox.network` true, or for `sandbox.allowDomains` hosts, commands reach the network without asking, in plan mode too.
- Programs that ignore the proxy variables have no network under `sandbox.allowDomains` (Node's built-in `fetch` needs `NODE_USE_ENV_PROXY=1`).
- The proxy does not inspect TLS, so a listed host that fronts other sites relays to them. Listed hosts receive whatever a command sends, including inherited provider API keys.
- Kopi's own `HTTP_PROXY` and `HTTPS_PROXY` are replaced, not chained: behind a mandatory outbound proxy listed hosts are unreachable.
- A bare `Bash` or `*` allow rule, or `--allowed-tools Bash`, lets `unsandboxed` calls run without asking; `sandbox.allowUnsandboxed` false stops them. The status line's `sandbox` segment shows the default, not whether a call set `unsandboxed`.
- Tools that write outside the start and temp directories, such as Go's build cache and package-manager caches, fail until those paths are in `sandbox.allowWrite`.
- These fail: `git init` in the start directory, `git config` writes, `git worktree add`, commits in a linked worktree (its git directory is outside the start directory) or a submodule, and commit signing through an agent socket.
- The config and hooks of repositories nested in the start directory, or of the start directory itself when it is a bare repository or a git directory, are unprotected.
- When the start directory's `.mcp.json` is a symlink, what it resolves to when Kopi starts is protected like `.mcp.json` itself; when that is a directory, sandboxed commands cannot write anywhere in it.
  - Not protected: another symlink on the way to it, a link broken at startup, an existing hard link, and the link targets of `.mcp.json` files in other folders.
- A command can move a directory holding a `.mcp.json` into a write root; it takes the [trust](trust.md#folder-trust) of its new parent.
- The files a `.mcp.json` server runs, such as `./server.sh`, stay writable.
- In execute mode, commands can write anything else in the start directory or an added one, so starting Kopi in a directory holding files other programs run, such as the home directory, lets commands change them.
- Plan mode makes only the workspace read-only:
  - Commands can still write `/tmp`, the temp directory and `sandbox.allowWrite` paths outside it, and use the network where allowed.
  - `kopi -p` runs sandboxed `Bash` without asking in plan mode.
  - Commands started in execute mode, background tasks and processes they leave running included, keep their access after a switch to plan mode.

## Not implemented

- Approving an unlisted host while a command runs.
- SOCKS5 (so `ssh`, git over ssh and other non-HTTP protocols cannot pass the proxy).
- TLS inspection and rules by URL path.
- Proxy authentication.
- Sending the proxy's traffic through another proxy.
- Allowing chosen Unix sockets.
