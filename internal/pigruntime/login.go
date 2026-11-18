package pigruntime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"sync"

	"github.com/MichaelKinsy/PiG/ai"
)

// signInNames are the names Kopi introduces itself with during a provider's
// sign-in where the default does not fit: OpenAI shows the Sign in with
// ChatGPT name on its consent screen. The default is what Kopi sends as the
// Codex originator in the sign-in URL.
var signInNames = map[string]string{"openai": "Kopi"}

const defaultSignInName = "kopi"

// signInNotices are shown after a sign-in to the provider's account, not
// after storing an API key.
var signInNotices = map[string]string{
	"anthropic": "Anthropic bills third-party tools per token as extra usage, not from your Claude plan limits.",
}

type Provider struct {
	ID string
	// SignIn names the provider's account sign-in, such as "OpenAI (ChatGPT Plus/Pro)"; empty when it has none.
	SignIn string
	// APIKey reports that Kopi can store an API key for it.
	APIKey bool
	// LoggedIn reports a credential in Kopi's store; credentials in the environment are not counted.
	LoggedIn bool
}

// Providers lists every provider sorted by id, with the logins it offers.
func (r *Runtime) Providers(ctx context.Context) ([]Provider, error) {
	creds, err := r.svcs.ModelRuntime().ListCredentials(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list credentials: %w", err)
	}
	var out []Provider
	for _, p := range r.svcs.ModelRuntime().GetProviders() {
		info := Provider{ID: p.ID, APIKey: storesAPIKey(p)}
		if p.Auth.OAuth != nil {
			info.SignIn = p.Auth.OAuth.Name
		}
		info.LoggedIn = slices.ContainsFunc(creds, func(c ai.CredentialInfo) bool { return c.ProviderID == p.ID })
		out = append(out, info)
	}
	slices.SortFunc(out, func(a, b Provider) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// storesAPIKey is PiG's rule for an API-key login: an API-key method without
// a Login is ambient-only (environment, cloud credential chain).
func storesAPIKey(p *ai.ModelsProvider) bool {
	return p.Auth.APIKey != nil && p.Auth.APIKey.Login != nil
}

// storedCredential reports whether Kopi's store holds a credential for the
// provider.
func (r *Runtime) storedCredential(ctx context.Context, provider string) (bool, error) {
	creds, err := r.svcs.ModelRuntime().ListCredentials(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to list credentials: %w", err)
	}
	return slices.ContainsFunc(creds, func(c ai.CredentialInfo) bool { return c.ProviderID == provider }), nil
}

// LoginPrompt is a question a login flow puts to the user.
type LoginPrompt struct {
	Message     string
	Placeholder string
	// Secret asks for input that must not be echoed or kept.
	Secret bool
	// Options, when set, are the only valid answers; the answer is an option ID.
	Options []LoginOption
}

type LoginOption struct {
	ID, Label, Description string
}

// LoginUI is how a frontend takes part in a login.
type LoginUI struct {
	// Ask answers a prompt. Its ctx ends when the user cancels the login, the
	// flow no longer wants the answer, or Login returns; no Ask is running or
	// starts after Login returns.
	Ask func(ctx context.Context, p LoginPrompt) (string, error)
	// Show presents an instruction, such as a sign-in URL; it may span lines.
	Show func(text string)
}

// Login signs in to the provider's account or stores an API key for it,
// asking which when it offers both. A sign-in URL is also opened in the
// browser. PiG persists the credential in the agent dir's auth.json; Kopi
// never sees it written or printed. The notice is text to present after a
// successful sign-in to the provider's account.
func (r *Runtime) Login(ctx context.Context, provider string, ui LoginUI) (notice string, err error) {
	p := r.svcs.ModelRuntime().GetProvider(provider)
	if p == nil {
		return "", fmt.Errorf("failed to log in to %q: %w", provider, ErrUnknownProvider)
	}
	signIn, apiKey := p.Auth.OAuth != nil, storesAPIKey(p)
	if !signIn && !apiKey {
		return "", fmt.Errorf("failed to log in to %q: %w", provider, ErrNoLogin)
	}
	l := newLoginAdapter(ui, r.openURL)
	kind := ai.CredentialOAuth
	switch {
	case signIn && apiKey:
		choice, err := l.ask(ctx, LoginPrompt{Message: fmt.Sprintf("Log in to %s with:", provider), Options: []LoginOption{
			{ID: string(ai.CredentialOAuth), Label: p.Auth.OAuth.Name},
			{ID: string(ai.CredentialAPIKey), Label: "API key"},
		}})
		if err != nil {
			return "", fmt.Errorf("failed to log in to %q: %w", provider, err)
		}
		kind = ai.CredentialType(choice)
	case apiKey:
		kind = ai.CredentialAPIKey
	}
	name := defaultSignInName
	if n, ok := signInNames[provider]; ok {
		name = n
	}
	// PiG turns an empty device ID into its own generic error, so the cause is
	// kept to report instead.
	var idMu sync.Mutex
	var idErr error
	opts := ai.LoginOptions{AgentName: &name, GetDeviceID: func() string {
		id, err := installationID(r.installationFile)
		if err != nil {
			idMu.Lock()
			idErr = err
			idMu.Unlock()
		}
		return id
	}}
	_, err = r.svcs.ModelRuntime().Login(ctx, provider, kind, ai.AuthInteraction{Prompt: l.prompt, Notify: l.notify}, opts)
	// PiG returns on a cancelled ctx without joining the flow's goroutines; a
	// prompt still in flight must end before Login returns, and none may start
	// after.
	l.close()
	idMu.Lock()
	deviceErr := idErr
	idMu.Unlock()
	if deviceErr != nil {
		return "", fmt.Errorf("failed to log in to %q: %w", provider, deviceErr)
	}
	if err != nil {
		return "", fmt.Errorf("failed to log in to %q: %w", provider, reword(err))
	}
	if kind == ai.CredentialOAuth {
		notice = signInNotices[provider]
	}
	return notice, nil
}

// Logout deletes the provider's credential from Kopi's store. PiG's logout
// deletes without checking, so the credential is looked up first.
func (r *Runtime) Logout(ctx context.Context, provider string) error {
	ok, err := r.storedCredential(ctx, provider)
	if err != nil {
		return err
	}
	if !ok {
		if r.svcs.ModelRuntime().GetProvider(provider) == nil {
			return fmt.Errorf("failed to log out of %q: %w", provider, ErrUnknownProvider)
		}
		return fmt.Errorf("failed to log out of %q: %w", provider, ErrNotLoggedIn)
	}
	if err := r.svcs.ModelRuntime().Logout(ctx, provider); err != nil {
		return fmt.Errorf("failed to log out of %q: %w", provider, reword(err))
	}
	return nil
}

// loginAdapter gives PiG's prompts and events to a LoginUI.
type loginAdapter struct {
	ui      LoginUI
	openURL func(string) error
	// ctx ends when the flow is over; every ui.Ask runs under it.
	ctx    context.Context
	cancel context.CancelFunc
	// mu guards closed, which refuses prompts once the flow is over; asks joins
	// the ui.Ask calls in flight.
	mu     sync.Mutex
	closed bool
	asks   sync.WaitGroup
}

func newLoginAdapter(ui LoginUI, openURL func(string) error) *loginAdapter {
	ctx, cancel := context.WithCancel(context.Background())
	return &loginAdapter{ui: ui, openURL: openURL, ctx: ctx, cancel: cancel}
}

// close ends the flow's prompts and waits for the ones in flight.
func (l *loginAdapter) close() {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	l.cancel()
	l.asks.Wait()
}

func (l *loginAdapter) prompt(ctx context.Context, p ai.AuthPrompt) (string, error) {
	switch p := p.(type) {
	case ai.AuthTextPrompt:
		return l.ask(ctx, LoginPrompt{Message: p.Message, Placeholder: p.Placeholder})
	case ai.AuthManualCodePrompt:
		return l.ask(ctx, LoginPrompt{Message: p.Message, Placeholder: p.Placeholder})
	case ai.AuthSecretPrompt:
		return l.ask(ctx, LoginPrompt{Message: p.Message, Placeholder: p.Placeholder, Secret: true})
	case ai.AuthSelectPrompt:
		options := make([]LoginOption, len(p.Options))
		for i, o := range p.Options {
			options[i] = LoginOption{ID: o.ID, Label: o.Label, Description: o.Description}
		}
		return l.ask(ctx, LoginPrompt{Message: p.Message, Options: options})
	}
	return "", fmt.Errorf("unsupported login prompt %q", p.Type())
}

func (l *loginAdapter) ask(ctx context.Context, p LoginPrompt) (string, error) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return "", context.Canceled
	}
	l.asks.Add(1)
	l.mu.Unlock()
	defer l.asks.Done()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(l.ctx, cancel)()
	answer, err := l.ui.Ask(ctx, p)
	if err != nil {
		return "", err
	}
	answer = strings.TrimSpace(answer)
	if p.Secret && answer == "" {
		return "", errors.New("failed to read login input: empty answer")
	}
	if len(p.Options) > 0 && !slices.ContainsFunc(p.Options, func(o LoginOption) bool { return o.ID == answer }) {
		return "", fmt.Errorf("invalid choice %q", answer)
	}
	return answer, nil
}

// notify drops events once the flow is over: PiG may notify after Login
// returns.
func (l *loginAdapter) notify(e ai.AuthEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	switch e := e.(type) {
	case ai.AuthURLEvent:
		text := "Open this URL in a browser to log in:\n  " + e.URL
		if e.Instructions != "" {
			text += "\n" + e.Instructions
		}
		l.ui.Show(text)
		// The opener waits for the browser's launcher; do not block the login flow.
		go l.open(e.URL)
	case ai.AuthDeviceCodeEvent:
		l.ui.Show(fmt.Sprintf("Visit %s and enter the code %s", e.VerificationURI, e.UserCode))
		go l.open(e.VerificationURI)
	case ai.AuthProgressEvent:
		l.ui.Show(e.Message)
	case ai.AuthInfoEvent:
		text := e.Message
		for _, link := range e.Links {
			text += "\n  " + link.URL
		}
		l.ui.Show(text)
	}
}

func (l *loginAdapter) open(rawURL string) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return
	}
	if err := l.openURL(rawURL); err != nil {
		slog.Debug("failed to open browser", "error", err)
	}
}
