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

## Releasing

Pushing a tag `vX.Y.Z` runs `.github/workflows/release.yml`: it builds the four archives (macOS and Linux, arm64 and amd64), tests the formula, creates the GitHub release and pushes the formula to `keyute/homebrew-tap`. Push one tag at a time.

- The formula source is `.github/homebrew/kopi.rb`. The copy in the tap is overwritten on every release, so edit the template, not the tap.
- A run uses the workflow and formula of its tag. Re-running an old tag's run republishes that version to the tap.
- `gh attestation verify <file> -R keyute/kopi` checks a downloaded archive.

One-time setup:

1. Create the public repository `keyute/homebrew-tap` with an initial commit. If its ruleset requires the `ci` check, list deploy keys as a bypass actor: the release pushes straight to its `main`, and the required check would reject that push.
2. Generate a key with `ssh-keygen -t ed25519 -N "" -C kopi-release -f tap-deploy-key`. Add `tap-deploy-key.pub` to the tap as a deploy key with write access, add the private half as the Actions secret `HOMEBREW_TAP_DEPLOY_KEY` in `keyute/kopi`, then delete both files.
3. Optionally, enable immutable releases in the settings of `keyute/kopi`.
