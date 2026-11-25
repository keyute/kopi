# Sandbox

On macOS and Linux, `Bash` runs inside an OS sandbox when it is available (unless `sandbox.enabled` is `false`): `sandbox-exec` on macOS, bubblewrap's `bwrap` on Linux. See [tools](tools.md#bash) for `Bash` itself.

## Requirements

- The sandbox exists only on macOS and on Linux on amd64 and arm64; elsewhere it is unavailable.
- Linux needs `bwrap` on `PATH` and unprivileged user namespaces, and is unavailable without them, as in containers that forbid them or forbid mounting `/proc`.
  - Ubuntu 24.04 restricts unprivileged user namespaces through AppArmor until `bwrap` has an AppArmor profile or `kernel.apparmor_restrict_unprivileged_userns` is `0`.
  - Ubuntu 25.04 and later work as shipped.
- The sandbox is unavailable when Kopi itself runs inside a Seatbelt sandbox, because profiles do not nest. For the same reason tools that apply their own sandbox (`swift build`, `xcodebuild` packages) fail inside it.

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

- `sandbox.enabled`:
  - `true` refuses to start when the sandbox is unavailable.
  - `false` runs `Bash` without it.
  - Unset uses it when available and otherwise logs a warning and runs `Bash` without it.
- Without the sandbox, a `Bash` command runs only after approval, or when an allow rule or the [classifier](permissions.md#classifier) allows it, and plan mode refuses `Bash`. `Read`, `Glob`, `Grep` and `LS` work the same with and without it.
- `sandbox.allowWrite` and `sandbox.denyRead` take plain paths, no globs: `/abs` is absolute, `~/x` is under the home directory, and anything else is relative to the start directory. So `.env` means only `<start directory>/.env`, unlike in a [rule](permissions.md#rules).
- A later layer's `sandbox.allowWrite` or `sandbox.denyRead` replaces an earlier one's whole.
- `sandbox.network` (default `false`) allows IP networking in the sandbox.
- `sandbox.allowDomains` and `sandbox.denyDomains` (default empty) list the hosts commands may reach while `sandbox.network` is false, see [Network](#network). `sandbox.network` true with either list set refuses to start.
- A later layer's `sandbox.allowDomains` or `sandbox.denyDomains` replaces an earlier one's whole.
- `sandbox.allowUnsandboxed` (default `true`) offers `Bash` the [`unsandboxed`](#outside-the-sandbox) parameter.

## Write

- A command can write only in:
  - the start directory
  - `/tmp`
  - the temp directory
  - the `sandbox.allowWrite` paths
- Never writable in the start directory's `.git`:
  - macOS: the files that choose git's config and hooks (`hooks`, `config`, `config.worktree`, `commondir`, `worktrees`, `modules`).
  - Linux: anything.
- A command cannot write `HEAD` in the start directory on Linux, or on macOS when it has no `.git`.
- In plan mode nothing in the start directory is writable, including `sandbox.allowWrite` paths inside it.
- A directory added with [`/add-dir`](usage.md#commands) is a write root like the start directory, with the same `.git`, `HEAD` and `.mcp.json` protection, and is read-only in plan mode. A command already running keeps the profile it started with.
- A command cannot create, write, rename or remove the start directory's `.mcp.json`, nor what it resolves to when it is a symlink.
  - On macOS this holds for a file named `.mcp.json` in any letter case and in any directory.
  - Git operations that change a protected `.mcp.json` therefore fail in the sandbox.

## Read

- A command can read everything except Kopi's home and the `sandbox.denyRead` paths; it cannot read [saved output](tools.md#saved-output), which the model reads with `Read`.
- It also cannot write, rename or remove the `sandbox.denyRead` paths.

## Network

- A command has no network unless `sandbox.network` is true, which opens IP networking only, or `sandbox.allowDomains` lists hosts.
- With `sandbox.allowDomains`, a command reaches the network only through an HTTP proxy inside Kopi, which connects to a host only when an `allowDomains` entry matches it and no `denyDomains` entry does.
- An entry is a host name, or `*.example.com`, which matches every subdomain but not `example.com` itself; letter case is ignored; a `:port` suffix limits the entry to that port.
- Not accepted as entries: IP addresses, `localhost`, a scheme or path, `*`, a wildcard over a single label such as `*.com`, and a name ending in a numeric label.
- A listed host that resolves to a loopback, private or link-local address is refused.
- The command gets `HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY` and `GRPC_PROXY` pointing at the proxy and `NO_PROXY` set to `localhost,127.0.0.1,::1`, each in upper and lower case. `unsandboxed` commands and commands run without the sandbox get neither the proxy nor the variables.
- A refused request is answered with 403, and the command's output ends with a line naming the refused hosts, at most 20: `[sandbox refused network access to: example.org:443]`. A command moved to the background reports them when it ends.
- Without the sandbox the lists have no effect; `sandbox.enabled` true refuses to start instead of running without it.
- Unix sockets (ssh-agent, Docker) stay blocked; on Linux so do VM sockets, Unix datagram socket pairs (stream and seqpacket pairs work) and io_uring.

## Outside the sandbox

- When `Bash` runs sandboxed it takes an optional boolean `unsandboxed`; a call that sets it runs that one command outside the sandbox.
- Such a call is treated as `Bash` without the sandbox, see [permissions](permissions.md#precedence): plan mode refuses it before any rule; in execute mode rules apply and otherwise it asks, or the [classifier](permissions.md#classifier) reviews it.
- Its title reads `Bash (unsandboxed) …`, in the approval dialog too. Once it succeeded its row reads `Ran <command> · unsandboxed`, and a group of rows counts it: `ran 9 commands (1 unsandboxed)`.
- A [subagent](subagents.md) can use it like the main agent; one whose definition pins plan mode is always refused.
- Without the sandbox, or with `sandbox.allowUnsandboxed` false, the parameter is not offered.
- A command you type with `!` is your action, not the agent's: it runs outside the sandbox, and plan mode does not restrict it. See [your own commands](usage.md#your-own-commands).

## Limitations

- A command the sandbox blocks can be run with `unsandboxed` in execute mode, unless `sandbox.allowUnsandboxed` is false.

### macOS

- `sandbox-exec` is deprecated by Apple.
- `ps`, `pgrep` and `top` do not work.
- Programs that open a pseudo-terminal, such as `script`, fail.
- With `sandbox.allowDomains`, a command can connect only to the proxy: it cannot listen on a port, reach a server it started itself, or resolve names; any local process can use a running command's proxy port, and a process a command leaves running keeps access to that port number after the command ends and reaches whatever listens there later.

### Linux

- Only the `.mcp.json` of the start directory and of added directories is protected by name: a command can create or change a `.mcp.json` in any other directory.
- A denied directory reads as empty in the sandbox, and opening a denied file fails with "Permission denied".
- Sandboxed commands refuse policies whose deny-write self-bind would overlap `/proc` or `/dev`, including an ancestor or descendant, to preserve the private process and device mounts.
  - This includes plan mode when the start directory is `/`, and adding `/` with `/add-dir`.
  - Read-deny masks over those mounts remain supported.
  - A refused command does not run unsandboxed.
- While sandboxed commands run, a program outside the sandbox that removes a protected path or replaces it by renaming over it, such as an editor's save, lifts the protection on that path for those commands.
- Every command that writes the start directory's `.git` fails (`git add`, `commit`, `checkout`, `stash`, `fetch`).
- Symlinks: while a denied path passes through a symlink whose entry is in a write root, such as a symlinked `.mcp.json` or `.git`, sandboxed commands refuse to run, whether the link is valid or broken.
  - They also refuse unresolved aliases outside write roots when, after following a symlink, the first missing component lies inside a write root.
- 32-bit programs cannot run: their system calls end the process.
- Commands cannot create user namespaces, so tools that need them fail, such as rootless containers, browsers' own sandboxes and nested bubblewrap.
- `ps`, `pgrep` and `top` see only the command's own processes.
- With `sandbox.allowDomains`, port 3128 on the command's own loopback is the proxy; every command starts through the `kopi` binary, which must not be under Kopi's home or a `sandbox.denyRead` path (Kopi refuses to start otherwise); programs can listen and connect on the command's own loopback.

### Linux placeholders

- While a sandboxed command runs, each denied path that does not exist shows outside the sandbox as an empty file or directory: in a typical repository `.mcp.json` and `HEAD`, and `.git` when the start directory has no `.git`.
- Kopi removes only its recorded creations that are still empty and match their creation-time device, inode and kind, once no sandboxed command, background tasks included, runs in any Kopi using the same home.
- The identity check is best-effort: outside changes can race removal and inodes can be reused.
- A killed Kopi's committed creations are cleaned after the next sandboxed command; interruption before recording can leave orphans.
- Kopi processes with different `KOPI_HOME`s do not coordinate: one running sandboxed commands in the same directory can remove the other's placeholders, such as for a missing `.mcp.json`, which unmounts the deny inside the other's running command.

### macOS and Linux

- Sandboxed commands inherit Kopi's environment, including provider API keys, and can read any file outside Kopi's home and `sandbox.denyRead`.
- With `sandbox.network` true, or for hosts in `sandbox.allowDomains`, commands reach the network without asking, in plan mode too.
- Programs that ignore the proxy variables have no network under `sandbox.allowDomains` (Node's built-in `fetch` needs `NODE_USE_ENV_PROXY=1` in the environment).
- The proxy does not inspect TLS, so a listed host that fronts other sites relays to them.
- Listed hosts receive whatever a command sends, including inherited provider API keys.
- Kopi's own `HTTP_PROXY` and `HTTPS_PROXY` are replaced for the command and not chained, so behind a mandatory outbound proxy listed hosts are unreachable.
- A bare `Bash` or `*` allow rule, or `--allowed-tools Bash`, lets `unsandboxed` calls run without asking; `sandbox.allowUnsandboxed` false stops them.
- The status line's `sandbox` segment shows the default, not whether a call set `unsandboxed`.
- Tools that write outside the start directory and temp directories, such as Go's build cache and package-manager caches, fail until those paths are in `sandbox.allowWrite`.
- `git init` in the start directory, `git config` writes, `git worktree add`, commits in a linked worktree (its git directory is outside the start directory), commits inside a submodule and commit signing through an agent socket fail.
- The sandbox does not protect the config and hooks of repositories nested inside the start directory, or of the start directory itself when it is a bare repository or a git directory.
- When the start directory's `.mcp.json` is a symlink, what it resolves to when Kopi starts is protected like `.mcp.json` itself; when that is a directory, sandboxed commands cannot write anywhere in it.
  - Not protected: another symlink on the way to it, a link that is broken at startup, a hard link that already exists, and the link targets of `.mcp.json` files in other folders.
- A command can move into a write root a directory that already holds a `.mcp.json`, and a folder moved in takes the [trust](trust.md#folder-trust) of its new parent.
- The files a `.mcp.json` server runs, such as `./server.sh` or `node_modules/.bin/…`, stay writable.
- In execute mode, sandboxed commands can write anything else in the start directory or an added one, so starting Kopi in a directory holding files other programs run, such as the home directory, lets commands change them.
- Plan mode keeps only the workspace read-only:
  - Commands can still write `/tmp`, the temp directory and `sandbox.allowWrite` paths outside it, and act over the network when `sandbox.network` is true or for hosts in `sandbox.allowDomains`.
  - `kopi -p` runs sandboxed `Bash` without asking in plan mode.
  - Commands started in execute mode, including background tasks and processes they leave running, keep their execute-mode access after a switch to plan mode.

## Not implemented

- Approving an unlisted host while a command runs.
- SOCKS5: `ssh`, git over ssh and other non-HTTP protocols cannot pass the proxy.
- TLS inspection and rules by URL path.
- Proxy authentication.
- Sending the proxy's traffic through another proxy.
- IP addresses and `localhost` as `sandbox.allowDomains` entries.
- Listening on a port or reaching local servers under `sandbox.allowDomains` on macOS.
- Allowing chosen Unix sockets.
