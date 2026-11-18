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
