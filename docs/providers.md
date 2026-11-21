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
- Kopi fetches a provider's list only when a configured model of that provider is unknown, the provider has a credential, and the provider has not been fetched in this process. The fetch happens at start, once per provider per process, and waits at most 5 seconds per provider, so unknown ids under several providers add up.
- The list comes from `pi-in-go.dev`. That request carries the provider id and a user agent with PiG's name and version, the OS, its release and the architecture; it carries no credential.
- A provider that serves its own list is also asked for it, with its own credential:
  - `openrouter`: `/models/user`. The result replaces openrouter's whole built-in list, narrowed to the models its key may use, and does so on later starts too.
  - `radius`: its own catalog, in place of `pi-in-go.dev`.
- An expired sign-in of the provider is renewed before the fetch; for `github-copilot` the renewal also reads the account's available models.
- The fetched list is saved in `agent/models-store.json`. A later start reads it without a request, and its entries replace the built-in ones of that provider, base URL included, while it is newer than the list built into Kopi. `PIG_OFFLINE=1` stops the fetch; the saved list is still read.
- A model still unknown after the fetch stops the start, naming `agent/models.json`; if the fetch failed, the message says the list could not be refreshed. The outcome of a fetch is kept for the process, so a later check in the same process does not repeat it; a fetch cut short by cancelling the start is not kept.
- Declare a model yourself in `agent/models.json`. Under a built-in provider whose models carry an API and base URL, the entry takes both from them and uses the provider's credential:

```json
{"providers": {"mistral": {"models": [{"id": "brand-new"}]}}}
```

- A provider whose built-in models ship without a base URL, such as `azure`, also needs `baseUrl`.
- Fields worth setting, with the value used when omitted: `contextWindow` (128000), `maxTokens` (16384), `cost` (`input`, `output`, `cacheRead`, `cacheWrite`, per million tokens; all 0), `reasoning` (false), `input` (`["text"]`; add `"image"`).
- With no `model` set, the default model for a provider still follows the Kopi build.

## Identity

- After an `anthropic` account sign-in Kopi notes that Anthropic bills third-party tools per token as extra usage, not from Claude plan limits.
- Sign-in introduces the app as `Kopi` to OpenAI's Sign in with ChatGPT (`openai`), which also receives `installation_id`, and as `kopi` to other sign-ins that take a name.
- Requests carry no PiG attribution headers.
- Still PiG's:
  - The page a browser sign-in returns to shows pi's logo.
  - The `radius` sign-in URL names `pi-gateway` on `radius.pi.dev`.
  - xAI's sign-in sends `referrer: pig`.

## Limitations

- PiG's built-in providers still use ambient credentials: API keys in the environment, Google application-default credentials and the AWS profile and credential chain.
- Importing PiG changes the TLS settings of Go's process-wide default HTTP transport.
- `agent/models.json` is trusted configuration: PiG reads it, and its `apiKey` can run a shell command.
- Provider requests identify as PiG, not Kopi.

## Not implemented

- A Kopi provider identity.
