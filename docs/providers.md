# Providers

## Login

- `kopi login` lists the providers that offer a login, with their methods and the ones Kopi holds a credential for.
- `kopi login <provider>` runs one.
- A provider offers an account sign-in, a stored API key or both; with both, Kopi asks which.
- An API key is read hidden on a terminal, or as a line from piped stdin.
- A sign-in prints its URL and opens it in the browser; when the browser does not open, use the printed URL.
- `kopi logout <provider>` removes the provider's credential from `agent/auth.json`.
- A credential in the environment works without a login; `kopi login` marks only stored credentials as logged in.

## In the fullscreen interface

- `/login <provider>` runs the same login without leaving the session:
  - URLs, device codes and progress appear in the transcript.
  - The browser opens.
  - An API key is entered in a masked field.
  - Esc cancels.
- `/logout <provider>` removes a credential.
- `kopi login` and `kopi logout` remain for headless and scripted use.
- When the configured model has no credential and its provider offers a sign-in or an API key, Kopi opens a login for that provider before the session opens. Headless use exits with the `kopi login` hint.
- With no `model` and no credential for any provider, the fullscreen interface lists the providers that offer a login, sorted by id; the one you choose is logged in, then the session opens with that provider's default model. Cancelling leaves no session, and `/login <provider>` opens it. Headless use exits with the `kopi login` hint.

## Models

- A model is `provider/id`. The ids a provider offers are built into Kopi, so a model released after your Kopi build is unknown until Kopi is updated or its list is fetched.
- At every start, in the background, Kopi refreshes the lists of the providers of the models this run uses (the session model, `smallModel`, `workflows.model`, `classifier.model` and the models of subagent definitions) that have a credential. A list checked under four hours ago is not requested. The refresh has 15 seconds, does not delay the start, and quitting stops it.
- Separately, a configured model that is unknown fetches its provider's list before the session opens, whatever the list's age, once per provider per process and for at most 5 seconds per provider, so unknown ids under several providers add up.
- The list comes from `pi-in-go.dev`. That request carries the provider id, `types=chat,image,classifier`, `If-None-Match` with the ETag when a list is saved (not for `openrouter`), and a user agent with PiG's name and version, the OS, its release and the architecture; it carries no credential.
- A provider that serves its own list is also asked for it, with its own credential:
  - `openrouter`: `/models/user`. The result replaces openrouter's whole built-in list, narrowed to the models its key may use, and does so on later starts too. It has no four-hour window, so it is asked on every start that uses it.
  - `radius`: its own catalog, in place of `pi-in-go.dev`. It has no four-hour window, so it is asked on every start that uses it.
- The request to `pi-in-go.dev` does not renew an expired sign-in. A provider asked for its own list (`openrouter`, `radius`) has its credential resolved first, which renews an expired `radius` sign-in.
- The list is saved in `agent/models-store.json`. Every start reads the saved list first, and its entries replace the built-in ones of that provider, base URL included, while it is newer than the list built into Kopi. `PIG_OFFLINE` set to `1`, `true` or `yes`, or `PI_OFFLINE` set to any value, stops both the background refresh and the fetch; the saved list is still read.
- A refreshed list applies to the running session (context window, prices, base URL); the status line shows a new context window after the next tool call or turn.
- A model known only from a saved list, and any `openrouter` model, is unknown once a refreshed list leaves it out: at the next start, or in the fullscreen interface when the session opens after the refresh.
- A provider first used later in a run (after `/model`, a first login or a subagent) has its list refreshed then, under the same rules as at start.
- A model still unknown after the fetch stops the start, naming `agent/models.json`; if the fetch failed, the message says the list could not be refreshed. The outcome of a fetch is kept for the process, so a later check in the same process does not repeat it; a fetch cut short by cancelling the start is not kept.
- Declare a model yourself in `agent/models.json`. Under a built-in provider whose models carry an API and base URL, the entry takes both from them and uses the provider's credential:

```json
{"providers": {"mistral": {"models": [{"id": "brand-new"}]}}}
```

- A provider whose built-in models ship without a base URL, such as `azure`, also needs `baseUrl`.
- Fields worth setting, with the value used when omitted: `contextWindow` (128000), `maxTokens` (16384), `cost` (`input`, `output`, `cacheRead`, `cacheWrite`, per million tokens; all 0), `reasoning` (false), `input` (`["text"]`; add `"image"`).
- `modelOverrides` under a provider, keyed by model id, changes a built-in model's values and takes the same fields; it also applies over a refreshed list:

```json
{"providers": {"openai-codex": {"modelOverrides": {"gpt-6-astra": {"contextWindow": 800000}}}}}
```

- The context window is where Kopi compacts: the conversation is summarised once it passes `contextWindow` less 16384 tokens, and a reply whose input exceeded `contextWindow` is compacted too. The value is not checked against what the provider accepts.
- A compaction changes only what the model reads: a summary written by the session model, then the most recent messages. The transcript shows it as two notices and keeps the earlier conversation, also in a resumed session. The summary is not shown.
- After a compaction the main session's model is [told the mode again](tools.md#modes).
- With no `model` set, the default model for a provider still follows the Kopi build.

## Identity

- After an `anthropic` account sign-in Kopi notes that Anthropic bills third-party tools per token as extra usage, not from Claude plan limits.
- Sign-in introduces the app as `Kopi` to OpenAI's Sign in with ChatGPT (`openai`), which also receives `installation_id`, and as `kopi` to other sign-ins that take a name.
- Requests carry no PiG attribution headers.
- Still PiG's:
  - The page a browser sign-in returns to shows pi's logo.
  - The `radius` sign-in URL names `pi-gateway` on `radius.pi.dev`.
  - xAI's sign-in sends `referrer: pig`.

## Plan limits

- The `limit_<window>` and `reset_<window>` segments of the [status line](configuration.md#status-line) show one usage window of the main session's model's provider: `limit_` the share used, `reset_` when it resets, as a local time (`15:04` under a day away, `Mon 15:04` from a day, `Jan 2` from a week). A `limit_` segment at 90% or more shows in the warning colour. Each is absent when the provider has no such window, and in a subagent's or workflow agent's view on another provider; `reset_` is also absent when the window has no reset.
- A provider has plan limits only where Kopi's usage draws from the subscription's own windows. Elsewhere `cost` and `total` are the figure to read.

| Provider | Windows | Host called | Credential sent |
| --- | --- | --- | --- |
| `openai-codex` | `5h`, `wk`; some plans have only one | `chatgpt.com` | the sign-in's access token and its ChatGPT account id |
| `github-copilot` | `premium`, `chat`, unless unlimited | `api.github.com`, or `api.` plus the enterprise domain of the sign-in | the GitHub token of the sign-in, not the short-lived Copilot token |
| `kimi-coding` | `5h`, `wk`, `mo`, `mo_code` | `api.kimi.com` | the sign-in's access token |
| `xai` | `wk` or `mo`, by the billing period's length | `cli-chat-proxy.grok.com` | the sign-in's access token |

- Copilot's zero-allowance placeholders (explicit zero entitlement and zero remaining) are omitted. Token-billing credits are not displayed.
- The endpoints are undocumented, so their answers can change without notice.
- Kopi calls them only in the fullscreen interface, with a `limit_` or `reset_` segment in the layout or `/usage` opened, and an account sign-in for the provider; an API key has no plan to read. Headless use never calls.
- A read follows activity, such as a turn starting or ending, at most once a minute, and never while idle, so a reset time that has passed stays until the next activity.
- Opening [`/usage`](usage.md#usage-1) also makes Kopi call the provider, and shows the windows it reads. While it is open, activity reads again at most once a minute, and never while idle. From then on a model or sign-in change reads again, as with a `limit_` or `reset_` segment.
- A failed read keeps the last values.
- Requests carry only the credential and an `Accept` header, with Go's default user agent.

## Limitations

- PiG's built-in providers still use ambient credentials: API keys in the environment, Google application-default credentials and the AWS profile and credential chain.
- Importing PiG changes the TLS settings of Go's process-wide default HTTP transport.
- `agent/models.json` is trusted configuration: PiG reads it, and its `apiKey` can run a shell command.
- Provider requests identify as PiG, not Kopi.
- Codex and xAI name a window by its length, so one that is not five hours, a week or a month has no segment and is not shown.
- Naming a window the model's provider does not have still makes Kopi call that provider.

## Not implemented

- A way to force a fresh read of the plan limits.
- Plan limits for `anthropic`: its third-party usage is billed as extra usage, not from the plan's windows, so read `cost` and `total`; there is no extra-usage figure either.
- Plan limits for `openai` and `meta`, and for a subagent's or workflow agent's provider.
- Kimi's `limits[]` and `usage` counts: only the `usages` ratios are read, so on a plan whose ratios read 0 its `limit_` segments show 0%.
- A Kopi provider identity.
- A command to refresh the model lists on demand.
- A key for the four-hour window.
- A setting for where compaction starts apart from the context window.
- Compacting on demand (`/compact`).
- Showing the summary a compaction wrote.
- A compaction that depends on the mode or the plan, such as a handoff to the approved plan.
- Putting read files or loaded skills back after a compaction.
