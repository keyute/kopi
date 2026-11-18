# Contributing

Kopi is opinionated and built around how I work, so I don't accept feature requests or feature PRs. Bug reports are welcome as issues, and bug fixes are the only contributions I consider.

## Development

```sh
task test   # with -race
task lint   # needs golangci-lint
task fmt
```

Tests are mock-backed (PiG's faux provider and a local HTTP mock server); none use a live provider.

### Manual live check

Basics:

- [ ] `kopi login openai-codex`
- [ ] Run with a model and ask it to read a file
- [ ] Try `/execute`, `/plan`, Ctrl+C and `--continue`
- [ ] `kopi -p` with an argument
- [ ] `kopi -p` with piped input

Sign-in, with a scratch `KOPI_HOME`:

- [ ] `kopi login` lists `openai` and `openai-codex`
- [ ] `kopi login openai-codex` opens the browser with `originator=kopi` in the URL
- [ ] `kopi login openai` has `agent_name_hint=Kopi` and `ext_agent_host_id=urn:uuid:<installation_id>` in the URL, and OpenAI's consent screen names Kopi
- [ ] `kopi logout openai-codex`, then a run suggests `kopi login`

Background tasks:

- [ ] Ask for `sleep 30; echo done` with Bash
- [ ] Press Ctrl+B while it runs
- [ ] The status line shows `1 shell`
- [ ] When it ends, the `• task t1 finished` row appears and the model replies without a prompt
- [ ] `/tasks` lists and stops a task
- [ ] After quitting, `pgrep sleep` finds nothing
