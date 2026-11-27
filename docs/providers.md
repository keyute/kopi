# Providers

## Login

- `kopi login` lists providers that offer a login, their methods and stored credentials; `kopi login <provider>` runs one.
- A provider offers an account sign-in, an API key or both (Kopi asks which). A key is read hidden on a terminal or as a line from piped stdin; a sign-in prints its URL and opens the browser.
- `kopi logout <provider>` removes the credential from `agent/auth.json`. An environment credential works without a login; `kopi login` marks only stored ones.

## In the fullscreen interface

- `/login <provider>` runs the same login in the session: URLs and progress show in the transcript, a key goes in a masked field, Esc cancels. `/logout <provider>` removes a credential.
- When the configured model's provider has no credential, Kopi opens its login, if it offers one, before the session opens.
- With no `model` and no credential, Kopi lists providers that offer a login, sorted by id, logs in your choice and opens the session on its default model. Cancelling leaves no session until `/login <provider>`.
- Headless use exits with the `kopi login` hint.

## Models

- A model is `provider/id`. Ids are built in, so a newer model is unknown until Kopi updates or its list is fetched.
- A provider's list refreshes in the background when a model of it is first used in a run: at start for the session model and other model keys with a credential, later after `/model`, a first login or a subagent. It has 15 seconds, never delays the start, and is skipped for a list under four hours old.
- An unknown configured model instead fetches its provider's list before the session opens, whatever its age, for at most 5 seconds, once per provider per process. If it is still unknown, the start stops, pointing to `agent/models.json`.
- The list comes from `pi-in-go.dev`. The request carries the provider id, `types=chat,image,classifier`, the saved ETag, and a user agent with PiG's version, the OS, its release and the architecture; no credential.
- `openrouter` (`/models/user`, narrowed to its key's models, replacing its built-in list) and `radius` (its own catalog) serve their own lists, fetched with their credential at every start that uses them, outside the four-hour window.
- The list is saved in `agent/models-store.json`, read first at every start; its entries replace that provider's built-in ones, base URL included, while newer than Kopi's built-in list. `PIG_OFFLINE` set to `1`, `true` or `yes`, or `PI_OFFLINE` set to any value, stops the refresh and the fetch, not the read.
- A refreshed list applies to the running session. A model known only from a saved list, or any `openrouter` model, becomes unknown once a refreshed list drops it, at the next start or fullscreen session open.
- Declare a model in `agent/models.json`. It takes the API, base URL and credential of its built-in provider; a provider with no built-in base URL, such as `azure`, also needs `baseUrl`.

```json
{"providers": {"mistral": {"models": [{"id": "brand-new"}]}}}
```

- Defaults: `contextWindow` (128000), `maxTokens` (16384), `cost` (`input`, `output`, `cacheRead`, `cacheWrite`, per million tokens; all 0), `reasoning` (false), `input` (`["text"]`; add `"image"`).
- `modelOverrides` under a provider, keyed by model id, changes a built-in model's fields, also over a refreshed list.

```json
{"providers": {"openai-codex": {"modelOverrides": {"gpt-6-astra": {"contextWindow": 800000}}}}}
```

- Kopi compacts past `contextWindow` less 16384 tokens, or after a reply whose input exceeded `contextWindow`; the value is not checked against what the provider accepts.
- With no `model` set, the default model for a provider still follows the Kopi build.
- Compaction changes only what the model reads: a summary, then the latest messages. The transcript shows two notices and keeps the earlier conversation, not the summary.

## Identity

- After an `anthropic` sign-in Kopi notes that Anthropic bills third-party tools as per-token extra usage, not from Claude plan limits.
- Sign-in names the app `Kopi` to OpenAI's Sign in with ChatGPT (`openai`, which also gets `installation_id`) and `kopi` to other sign-ins that take a name.
- Still PiG's: the browser sign-in's return page (pi's logo), the `radius` sign-in URL (`pi-gateway` on `radius.pi.dev`) and xAI's `referrer: pig`.

## Plan limits

- The `limit_<window>` and `reset_<window>` [status line](configuration.md#status-line) segments show one usage window of the main session's provider: share used (warning colour from 90%) and local reset time (`15:04`, `Mon 15:04` from a day away, `Jan 2` from a week). A segment is absent when the provider lacks that window or, for `reset_`, a reset, and in a subagent's or workflow agent's view on another provider.
- Only providers whose usage draws from the subscription's windows have plan limits; elsewhere use `cost` and `total`.

| Provider | Windows | Host called | Credential sent |
| --- | --- | --- | --- |
| `openai-codex` | `5h`, `wk`; some plans have one | `chatgpt.com` | the sign-in's access token and ChatGPT account id |
| `github-copilot` | `premium`, `chat`, unless unlimited | `api.github.com`, or `api.` plus the enterprise domain | the GitHub token, not the short-lived Copilot token |
| `kimi-coding` | `5h`, `wk`, `mo`, `mo_code` | `api.kimi.com` | the sign-in's access token |
| `xai` | `wk` or `mo`, by billing period length | `cli-chat-proxy.grok.com` | the sign-in's access token |

- The endpoints are undocumented. Copilot's zero-allowance placeholders and token credits are omitted.
- Kopi calls them only in the fullscreen interface with an account sign-in (an API key has no plan) and a `limit_` or `reset_` segment in the layout, or once [`/usage`](usage.md#usage-1) is opened. Headless use never calls.
- With a segment, a read follows activity such as a turn starting or ending, at most once a minute and never while idle, so a passed reset time stays until then. Opening `/usage` reads once, and activity reads again only while it stays open.
- Once anything has read, a model or sign-in change reads again; a failed read keeps the old values.
- Requests carry the credential and `Accept`, with Go's default user agent; Codex's add `ChatGPT-Account-Id`.

## Limitations

- PiG's built-in providers use ambient credentials: environment API keys, Google application-default credentials, and the AWS profile and credential chain.
- Importing PiG changes the TLS settings of Go's default HTTP transport, process-wide.
- `agent/models.json` is trusted configuration: its `apiKey` can run a shell command.
- Requests carry no PiG attribution headers but still identify as PiG: Codex requests send `originator: pig` and the user agent is `pig/…`.
- Codex and xAI name windows by length; one that is not five hours, a week or a month has no segment. Naming a window the provider lacks still makes Kopi call it.

## Not implemented

- Plan limits for `anthropic` (extra usage has no figure; read `cost` and `total`), `openai`, `meta`, and a subagent's or workflow agent's provider.
- Kimi's `limits[]` and `usage` counts: only `usages` ratios are read, so a plan whose ratios read 0 shows 0%.
- On-demand plan-limit reads, a model-list refresh command, and a key for the four-hour window.
- A compaction setting apart from the context window, `/compact`, a shown summary, mode- or plan-dependent compaction, and restored read files or loaded skills.
