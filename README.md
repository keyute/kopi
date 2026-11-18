# Kopi

Kopi is a coding-agent CLI built on [PiG](https://github.com/MichaelKinsy/PiG).
It is early: tools to read, search, write and edit files and to run commands, tools from MCP servers, a fullscreen terminal interface, and a print mode that answers one prompt.

## Install

- Needs Go (version in `go.mod`) and [Task](https://taskfile.dev/) to build; the built binary needs nothing else at runtime.
- Builds for macOS, Linux and Windows only.

```sh
task build    # writes bin/kopi
```

## Quick start

```sh
bin/kopi login                     # list providers
bin/kopi login <provider>
```

Set the model in `~/.kopi/config.json` (`$KOPI_HOME` overrides the home):

```json
{ "version": 1, "model": "openai-codex/gpt-5.6-luna" }
```

```sh
bin/kopi                           # fullscreen interface, starts in plan mode
bin/kopi -p "explain main.go"      # print mode
git diff | bin/kopi -p "review this"
```

Shift+Tab or `/execute` switches from plan to execute mode.

## Documentation

- [Usage](docs/usage.md): fullscreen and print mode, commands, keys, mouse, paste
- [Configuration](docs/configuration.md): home, `config.json` keys, key bindings, status line, themes
- [Providers](docs/providers.md): login and credentials
- [Tools](docs/tools.md): modes, built-in tools, background tasks, web tools
- [Permissions](docs/permissions.md): rules, approval dialog, classifier
- [Sandbox](docs/sandbox.md): the OS sandbox for `Bash`
- [MCP servers](docs/mcp.md)
- [Trust and instructions](docs/trust.md): folder trust, `AGENTS.md`
- [Skills](docs/skills.md)

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## Credits

Built on [PiG](https://github.com/MichaelKinsy/PiG) by Michael Kinsy (MIT). Other dependencies are listed in `go.mod`.

The Seatbelt policy in `internal/sandbox/codex/` is adapted from [OpenAI Codex](https://github.com/openai/codex) (Apache-2.0); its `LICENSE` and `NOTICE` are in that directory. The Linux sandbox's mount plan in `internal/sandbox/bwrap.go` is adapted from Anthropic's [sandbox-runtime](https://github.com/anthropic-experimental/sandbox-runtime) (Apache-2.0); its `LICENSE` is in `internal/sandbox/srt/`.

## License

MIT, see `LICENSE`.
