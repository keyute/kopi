package pigruntime

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/muesli/cancelreader"
	"golang.org/x/term"
)

// signInNames are the names Kopi introduces itself with during a provider's
// sign-in where the default does not fit: OpenAI shows the Sign in with
// ChatGPT name on its consent screen. The default matches the originator Kopi
// sends on requests.
var signInNames = map[string]string{"openai": "Kopi"}

const defaultSignInName = "kopi"

// signInNotices are printed after a sign-in to the provider's account, not
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

// Login signs in to the provider's account or stores an API key for it,
// asking which when it offers both, answering the flow's prompts from in and
// printing its instructions to out. A sign-in URL is also opened in the
// browser. PiG persists the credential in the agent dir's auth.json; Kopi
// never sees it written or printed.
func (r *Runtime) Login(ctx context.Context, provider string, in io.Reader, out io.Writer) error {
	p := r.svcs.ModelRuntime().GetProvider(provider)
	if p == nil {
		return fmt.Errorf("failed to log in to %q: %w", provider, ErrUnknownProvider)
	}
	signIn, apiKey := p.Auth.OAuth != nil, storesAPIKey(p)
	if !signIn && !apiKey {
		return fmt.Errorf("failed to log in to %q: %w", provider, ErrNoLogin)
	}
	l := &loginIO{in: in, lines: bufio.NewReader(in), out: out, openURL: r.openURL}
	kind := ai.CredentialOAuth
	switch {
	case signIn && apiKey:
		choice, err := l.selectOption(ctx, ai.AuthSelectPrompt{Message: fmt.Sprintf("Log in to %s with:", provider), Options: []ai.AuthSelectOption{
			{ID: string(ai.CredentialOAuth), Label: p.Auth.OAuth.Name},
			{ID: string(ai.CredentialAPIKey), Label: "API key"},
		}})
		if err != nil {
			return fmt.Errorf("failed to log in to %q: %w", provider, err)
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
	_, err := r.svcs.ModelRuntime().Login(ctx, provider, kind, ai.AuthInteraction{Prompt: l.prompt, Notify: l.notify}, opts)
	idMu.Lock()
	deviceErr := idErr
	idMu.Unlock()
	if deviceErr != nil {
		return fmt.Errorf("failed to log in to %q: %w", provider, deviceErr)
	}
	if err != nil {
		return fmt.Errorf("failed to log in to %q: %w", provider, reword(err))
	}
	if notice, ok := signInNotices[provider]; ok && kind == ai.CredentialOAuth {
		if _, err := fmt.Fprintln(out, notice); err != nil {
			return fmt.Errorf("failed to print login notice: %w", err)
		}
	}
	return nil
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

type loginIO struct {
	in      io.Reader
	lines   *bufio.Reader
	out     io.Writer
	openURL func(string) error
	// pending is the read still in flight after its prompt's ctx ended; the
	// next prompt takes its answer instead of starting a concurrent read.
	// PiG joins a cancelled prompt before issuing the next, so prompts never
	// overlap.
	pending chan readResult
}

type readResult struct {
	text string
	err  error
}

func (l *loginIO) prompt(ctx context.Context, p ai.AuthPrompt) (string, error) {
	switch p := p.(type) {
	case ai.AuthTextPrompt:
		l.ask(p.Message, p.Placeholder)
		return l.read(ctx, l.readLine)
	case ai.AuthManualCodePrompt:
		l.ask(p.Message, p.Placeholder)
		return l.read(ctx, l.readLine)
	case ai.AuthSecretPrompt:
		secret, err := l.readSecret(ctx, p)
		if err == nil && secret == "" {
			return "", errors.New("failed to read login input: empty answer")
		}
		return secret, err
	case ai.AuthSelectPrompt:
		return l.selectOption(ctx, p)
	}
	return "", fmt.Errorf("unsupported login prompt %q", p.Type())
}

func (l *loginIO) notify(e ai.AuthEvent) {
	switch e := e.(type) {
	case ai.AuthURLEvent:
		fmt.Fprintf(l.out, "Open this URL in a browser to log in:\n  %s\n", e.URL)
		if e.Instructions != "" {
			fmt.Fprintln(l.out, e.Instructions)
		}
		l.open(e.URL)
	case ai.AuthDeviceCodeEvent:
		fmt.Fprintf(l.out, "Visit %s and enter the code %s\n", e.VerificationURI, e.UserCode)
		l.open(e.VerificationURI)
	case ai.AuthProgressEvent:
		fmt.Fprintln(l.out, e.Message)
	case ai.AuthInfoEvent:
		fmt.Fprintln(l.out, e.Message)
		for _, link := range e.Links {
			fmt.Fprintf(l.out, "  %s\n", link.URL)
		}
	}
}

// open runs off the flow's goroutine because the opener waits for the
// browser's launcher; the printed URL is the fallback when it fails.
func (l *loginIO) open(rawURL string) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return
	}
	go func() {
		if err := l.openURL(rawURL); err != nil {
			slog.Debug("failed to open browser", "error", err)
		}
	}()
}

func (l *loginIO) ask(message, placeholder string) {
	if placeholder != "" {
		fmt.Fprintf(l.out, "%s [%s] ", message, placeholder)
		return
	}
	fmt.Fprintf(l.out, "%s ", message)
}

func (l *loginIO) selectOption(ctx context.Context, p ai.AuthSelectPrompt) (string, error) {
	fmt.Fprintln(l.out, p.Message)
	for i, o := range p.Options {
		if o.Description != "" {
			fmt.Fprintf(l.out, "  %d) %s - %s\n", i+1, o.Label, o.Description)
			continue
		}
		fmt.Fprintf(l.out, "  %d) %s\n", i+1, o.Label)
	}
	fmt.Fprint(l.out, "Choice: ")
	answer, err := l.read(ctx, l.readLine)
	if err != nil {
		return "", err
	}
	if n, err := strconv.Atoi(answer); err == nil && n >= 1 && n <= len(p.Options) {
		return p.Options[n-1].ID, nil
	}
	if slices.ContainsFunc(p.Options, func(o ai.AuthSelectOption) bool { return o.ID == answer }) {
		return answer, nil
	}
	return "", fmt.Errorf("invalid choice %q", answer)
}

// read runs fn off the prompt's goroutine so a cancelled ctx (the OAuth
// callback server winning against a pasted code) returns at once.
func (l *loginIO) read(ctx context.Context, fn func() (string, error)) (string, error) {
	if l.pending == nil {
		ch := make(chan readResult, 1)
		l.pending = ch
		go func() {
			text, err := fn()
			ch <- readResult{text: text, err: err}
		}()
	}
	select {
	case res := <-l.pending:
		l.pending = nil
		return res.text, res.err
	case <-ctx.Done():
		return "", context.Cause(ctx)
	}
}

func (l *loginIO) readLine() (string, error) {
	line, err := l.lines.ReadString('\n')
	if err != nil && (!errors.Is(err, io.EOF) || line == "") {
		return "", fmt.Errorf("failed to read login input: %w", err)
	}
	return strings.TrimSpace(line), nil
}

func (l *loginIO) readSecret(ctx context.Context, p ai.AuthSecretPrompt) (string, error) {
	f, ok := l.in.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		l.ask(p.Message, p.Placeholder)
		return l.read(ctx, l.readLine)
	}
	if err := context.Cause(ctx); err != nil {
		return "", err
	}
	reader, err := cancelreader.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("failed to create login input reader: %w", err)
	}
	defer func() { _ = reader.Close() }()
	state, err := term.MakeRaw(int(f.Fd()))
	if err != nil {
		return "", fmt.Errorf("failed to set terminal state: %w", err)
	}
	defer func() { _ = term.Restore(int(f.Fd()), state) }()
	terminal := term.NewTerminal(struct {
		io.Reader
		io.Writer
	}{reader, l.out}, "")
	prompt := p.Message
	if p.Placeholder != "" {
		prompt += " [" + p.Placeholder + "]"
	}
	done := make(chan readResult, 1)
	go func() {
		text, err := terminal.ReadPassword(prompt + " ")
		done <- readResult{text: text, err: err}
	}()
	select {
	case res := <-done:
		// In raw mode the editor reports Ctrl-C, and Ctrl-D on an empty line, as EOF.
		if errors.Is(res.err, io.EOF) {
			return "", context.Canceled
		}
		if res.err != nil {
			return "", fmt.Errorf("failed to read login input: %w", res.err)
		}
		return res.text, nil
	case <-ctx.Done():
		// The editor must stop writing before the caller regains the terminal.
		reader.Cancel()
		<-done
		return "", context.Cause(ctx)
	}
}
