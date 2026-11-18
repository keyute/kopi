package pigruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	oauthProvider  = "kopi-oauth"
	syntheticToken = "synthetic-token-5d1e"
	loginURL       = "https://login.example/authorize?state=kopi"
)

// oauthTestProvider is a native provider whose only auth method is a fake
// OAuth flow: it shows a URL, asks for a pasted code and a team, and returns a
// synthetic token.
func oauthTestProvider(t *testing.T) *ai.ModelsProvider {
	t.Helper()
	p := *ai.NewFauxProvider(ai.FauxConfig{ProviderID: oauthProvider, Model: "oauth-1"}).Provider()
	p.Auth = ai.ProviderAuth{OAuth: &ai.OAuthAuth{
		Name: "Kopi test OAuth",
		Login: func(ctx context.Context, in ai.AuthInteraction, _ ai.LoginOptions) (ai.Credential, error) {
			in.Notify(ai.AuthURLEvent{URL: loginURL, Instructions: "Paste the code shown after login."})
			code, err := in.Prompt(ctx, ai.AuthManualCodePrompt{Message: "Code:"})
			if err != nil {
				return ai.Credential{}, err
			}
			team, err := in.Prompt(ctx, ai.AuthSelectPrompt{Message: "Team:", Options: []ai.AuthSelectOption{{ID: "red", Label: "Red"}, {ID: "blue", Label: "Blue"}}})
			if err != nil {
				return ai.Credential{}, err
			}
			assert.Equal(t, "code-123", code)
			assert.Equal(t, "blue", team)
			return ai.Credential{Type: ai.CredentialOAuth, Access: syntheticToken, Refresh: "r", Expires: time.Now().Add(time.Hour).UnixMilli()}, nil
		},
		Refresh: func(_ context.Context, c ai.Credential) (ai.Credential, error) { return c, nil },
		ToAuth:  func(c ai.Credential) (ai.ModelAuth, error) { return ai.ModelAuth{APIKey: c.Access}, nil },
	}}
	return &p
}

// scriptedUI answers prompts from a script and records what it was asked and
// shown.
type scriptedUI struct {
	mu      sync.Mutex
	answers []string
	prompts []LoginPrompt
	shown   []string
}

func script(answers ...string) *scriptedUI { return &scriptedUI{answers: answers} }

func (u *scriptedUI) ui() LoginUI {
	return LoginUI{
		Ask: func(_ context.Context, p LoginPrompt) (string, error) {
			u.mu.Lock()
			defer u.mu.Unlock()
			u.prompts = append(u.prompts, p)
			if len(u.answers) == 0 {
				return "", fmt.Errorf("unscripted prompt %q", p.Message)
			}
			a := u.answers[0]
			u.answers = u.answers[1:]
			return a, nil
		},
		Show: func(text string) {
			u.mu.Lock()
			defer u.mu.Unlock()
			u.shown = append(u.shown, text)
		},
	}
}

func (u *scriptedUI) shownText() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return strings.Join(u.shown, "\n")
}

func TestLoginPersistsCredentialForFreshRuntime(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	require.NoError(t, r.RegisterProvider(oauthTestProvider(t)))
	ok, err := r.hasCredential(t.Context(), oauthProvider)
	require.NoError(t, err)
	assert.False(t, ok)

	ui := script("code-123", "blue")
	_, err = r.Login(t.Context(), oauthProvider, ui.ui())
	require.NoError(t, err)

	assert.Contains(t, ui.shownText(), loginURL)
	assert.Equal(t, []LoginPrompt{
		{Message: "Code:"},
		{Message: "Team:", Options: []LoginOption{{ID: "red", Label: "Red"}, {ID: "blue", Label: "Blue"}}},
	}, ui.prompts)
	assert.NotContains(t, ui.shownText(), syntheticToken)

	authFile := filepath.Join(sb.agentDir, "auth.json")
	info, err := os.Stat(authFile)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	content, err := os.ReadFile(authFile)
	require.NoError(t, err)
	assert.Contains(t, string(content), syntheticToken)

	fresh, _ := newTestRuntime(t, sb)
	require.NoError(t, fresh.RegisterProvider(oauthTestProvider(t)))
	ok, err = fresh.hasCredential(t.Context(), oauthProvider)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestLoginOpensSignInURLInBrowser(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	require.NoError(t, r.RegisterProvider(oauthTestProvider(t)))
	opened := make(chan string, 1)
	r.openURL = func(u string) error {
		opened <- u
		return errors.New("no browser")
	}

	ui := script("code-123", "blue")
	_, err := r.Login(t.Context(), oauthProvider, ui.ui())
	require.NoError(t, err)

	assert.Equal(t, loginURL, <-opened)
	assert.Contains(t, ui.shownText(), loginURL, "the shown URL is the fallback")
}

func TestLoginOpensDeviceVerificationURLInBrowser(t *testing.T) {
	opened := make(chan string, 1)
	ui := script()
	l := newLoginAdapter(ui.ui(), func(u string) error {
		opened <- u
		return nil
	})
	l.notify(ai.AuthDeviceCodeEvent{VerificationURI: loginURL, UserCode: "TEST-CODE"})

	assert.Contains(t, ui.shownText(), loginURL)
	assert.Contains(t, ui.shownText(), "TEST-CODE")
	select {
	case u := <-opened:
		assert.Equal(t, loginURL, u)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for browser opener")
	}
}

func TestLoginOpensOnlyWebURLsInBrowser(t *testing.T) {
	opened := make(chan string, 8)
	ui := script()
	l := newLoginAdapter(ui.ui(), func(u string) error {
		opened <- u
		return nil
	})
	for _, bad := range []string{"file:///etc/passwd", "javascript:alert(1)", "http://[::1"} {
		l.open(bad)
		assert.Empty(t, opened, "%s must not open in the browser", bad)
		l.notify(ai.AuthURLEvent{URL: bad})
		l.notify(ai.AuthDeviceCodeEvent{VerificationURI: bad, UserCode: "TEST-CODE"})
		assert.Equal(t, 2, strings.Count(ui.shownText(), bad), "%s is printed for both events", bad)
	}
	for _, web := range []string{loginURL, "http://login.example/authorize"} {
		l.open(web)
		require.Len(t, opened, 1)
		assert.Equal(t, web, <-opened)
	}
	assert.Empty(t, opened, "only web URLs were opened")
}

// loginCall is what a recording provider's sign-in received.
type loginCall struct {
	opts     ai.LoginOptions
	deviceID string
}

// recordingSignIn is a sign-in that sends its LoginOptions to calls, asking
// for the device ID first when askDeviceID, and fails as PiG's does when the
// ID is empty.
func recordingSignIn(calls chan<- loginCall, askDeviceID bool) *ai.OAuthAuth {
	return &ai.OAuthAuth{
		Name: "Kopi test sign-in",
		Login: func(_ context.Context, _ ai.AuthInteraction, opts ai.LoginOptions) (ai.Credential, error) {
			call := loginCall{opts: opts}
			if askDeviceID {
				call.deviceID = opts.GetDeviceID()
			}
			calls <- call
			if askDeviceID && call.deviceID == "" {
				return ai.Credential{}, errors.New("requires a device ID")
			}
			return ai.Credential{Type: ai.CredentialOAuth, Access: syntheticToken, Refresh: "r", Expires: time.Now().Add(time.Hour).UnixMilli()}, nil
		},
		Refresh: func(_ context.Context, c ai.Credential) (ai.Credential, error) { return c, nil },
		ToAuth:  func(c ai.Credential) (ai.ModelAuth, error) { return ai.ModelAuth{APIKey: c.Access}, nil },
	}
}

// storedKeyAuth takes an API key from the secret prompt, as PiG's default
// API-key login does.
func storedKeyAuth() *ai.APIKeyAuth {
	return &ai.APIKeyAuth{
		Name: "Kopi test key",
		Login: func(ctx context.Context, in ai.AuthInteraction) (ai.Credential, error) {
			key, err := in.Prompt(ctx, ai.AuthSecretPrompt{Message: "Enter API key"})
			return ai.Credential{Type: ai.CredentialAPIKey, Key: key}, err
		},
		Resolve: func(_ context.Context, in ai.APIKeyAuthInput) (*ai.AuthResult, error) {
			if in.Credential == nil {
				return nil, nil
			}
			return &ai.AuthResult{Auth: ai.ModelAuth{APIKey: in.Credential.Key}}, nil
		},
	}
}

func loginTestProvider(id string, auth ai.ProviderAuth) *ai.ModelsProvider {
	p := *ai.NewFauxProvider(ai.FauxConfig{ProviderID: id, Model: "login-1"}).Provider()
	p.Auth = auth
	return &p
}

func TestLoginIntroducesKopi(t *testing.T) {
	for provider, want := range map[string]string{oauthProvider: "kopi", "openai": "Kopi"} {
		t.Run(provider, func(t *testing.T) {
			sb := newSandbox(t)
			r, _ := newTestRuntime(t, sb)
			calls := make(chan loginCall, 1)
			require.NoError(t, r.RegisterProvider(loginTestProvider(provider, ai.ProviderAuth{OAuth: recordingSignIn(calls, false)})))

			_, err := r.Login(t.Context(), provider, script().ui())
			require.NoError(t, err)

			call := <-calls
			require.NotNil(t, call.opts.AgentName)
			assert.Equal(t, want, *call.opts.AgentName)
		})
	}
}

func TestLoginDeviceIDIsInstallationID(t *testing.T) {
	sb := newSandbox(t)
	file := filepath.Join(sb.kopiHome, "installation_id")
	login := func() string {
		r, _ := newTestRuntime(t, sb)
		calls := make(chan loginCall, 1)
		require.NoError(t, r.RegisterProvider(loginTestProvider(oauthProvider, ai.ProviderAuth{OAuth: recordingSignIn(calls, true)})))
		_, err := r.Login(t.Context(), oauthProvider, script().ui())
		require.NoError(t, err)
		return (<-calls).deviceID
	}

	id := login()

	content, err := os.ReadFile(file)
	require.NoError(t, err)
	assert.Equal(t, string(content), id)
	info, err := os.Stat(file)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	assert.Equal(t, id, login(), "a fresh runtime reads the same id")
}

func TestLoginNotAskingForDeviceIDCreatesNoInstallationID(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	calls := make(chan loginCall, 1)
	require.NoError(t, r.RegisterProvider(loginTestProvider(oauthProvider, ai.ProviderAuth{OAuth: recordingSignIn(calls, false)})))

	_, err := r.Login(t.Context(), oauthProvider, script().ui())
	require.NoError(t, err)

	<-calls
	assert.NoFileExists(t, filepath.Join(sb.kopiHome, "installation_id"))
}

func TestLoginReportsInvalidInstallationID(t *testing.T) {
	sb := newSandbox(t)
	file := filepath.Join(sb.kopiHome, "installation_id")
	require.NoError(t, os.WriteFile(file, []byte("garbage"), 0o600))
	r, _ := newTestRuntime(t, sb)
	calls := make(chan loginCall, 1)
	require.NoError(t, r.RegisterProvider(loginTestProvider(oauthProvider, ai.ProviderAuth{OAuth: recordingSignIn(calls, true)})))

	_, err := r.Login(t.Context(), oauthProvider, script().ui())

	require.Error(t, err)
	assert.Contains(t, err.Error(), file)
	assert.NotContains(t, err.Error(), "requires a device ID")
}

func TestLoginStoresAPIKey(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	require.NoError(t, r.RegisterProvider(loginTestProvider(oauthProvider, ai.ProviderAuth{APIKey: storedKeyAuth()})))

	ui := script(syntheticToken)
	_, err := r.Login(t.Context(), oauthProvider, ui.ui())
	require.NoError(t, err)

	assert.Equal(t, []LoginPrompt{{Message: "Enter API key", Secret: true}}, ui.prompts)
	assert.NotContains(t, ui.shownText(), syntheticToken)
	content, err := os.ReadFile(filepath.Join(sb.agentDir, "auth.json"))
	require.NoError(t, err)
	assert.Contains(t, string(content), syntheticToken)
	ok, err := r.hasCredential(t.Context(), oauthProvider)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestLoginTrimsAnswers(t *testing.T) {
	ui := script("  "+syntheticToken+"\r\n", "\t\n")
	l := newLoginAdapter(ui.ui(), nil)

	got, err := l.ask(t.Context(), LoginPrompt{Message: "Key:", Secret: true})
	require.NoError(t, err)
	assert.Equal(t, syntheticToken, got)

	_, err = l.ask(t.Context(), LoginPrompt{Message: "Key:", Secret: true})
	assert.ErrorContains(t, err, "empty answer")
}

func TestLoginRejectsEmptyAPIKey(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	require.NoError(t, r.RegisterProvider(loginTestProvider(oauthProvider, ai.ProviderAuth{APIKey: storedKeyAuth()})))

	_, err := r.Login(t.Context(), oauthProvider, script("").ui())

	require.ErrorContains(t, err, "empty answer")
	providers, err := r.Providers(t.Context())
	require.NoError(t, err)
	i := slices.IndexFunc(providers, func(p Provider) bool { return p.ID == oauthProvider })
	require.GreaterOrEqual(t, i, 0)
	assert.False(t, providers[i].LoggedIn)
}

func TestLoginRewordsSignInFailure(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	errPort := errors.New("address in use")
	signIn := recordingSignIn(make(chan loginCall, 1), false)
	signIn.Login = func(context.Context, ai.AuthInteraction, ai.LoginOptions) (ai.Credential, error) {
		return ai.Credential{}, fmt.Errorf("Port 1455 is in use, probably by an unfinished login in another pi session: %w", errPort)
	}
	require.NoError(t, r.RegisterProvider(loginTestProvider(oauthProvider, ai.ProviderAuth{OAuth: signIn})))

	_, err := r.Login(t.Context(), oauthProvider, script().ui())

	require.ErrorIs(t, err, errPort)
	assert.Contains(t, err.Error(), "another Kopi session")
	assert.NotContains(t, err.Error(), "another pi session")
}

func TestLoginAsksForMethodWhenProviderOffersBoth(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	calls := make(chan loginCall, 1)
	require.NoError(t, r.RegisterProvider(loginTestProvider(oauthProvider, ai.ProviderAuth{OAuth: recordingSignIn(calls, false), APIKey: storedKeyAuth()})))

	ui := script(string(ai.CredentialAPIKey), syntheticToken)
	_, err := r.Login(t.Context(), oauthProvider, ui.ui())
	require.NoError(t, err)

	assert.Equal(t, []LoginOption{
		{ID: string(ai.CredentialOAuth), Label: "Kopi test sign-in"},
		{ID: string(ai.CredentialAPIKey), Label: "API key"},
	}, ui.prompts[0].Options)
	assert.Empty(t, calls, "the sign-in never ran")
	content, err := os.ReadFile(filepath.Join(sb.agentDir, "auth.json"))
	require.NoError(t, err)
	assert.Contains(t, string(content), `"api_key"`)
}

func TestLoginReturnsAnthropicBillingNoticeAfterSignInOnly(t *testing.T) {
	for _, tc := range []struct {
		name    string
		answers []string
		notice  bool
	}{
		{name: "sign-in", answers: []string{string(ai.CredentialOAuth)}, notice: true},
		{name: "api key", answers: []string{string(ai.CredentialAPIKey), syntheticToken}, notice: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sb := newSandbox(t)
			r, _ := newTestRuntime(t, sb)
			calls := make(chan loginCall, 1)
			require.NoError(t, r.RegisterProvider(loginTestProvider("anthropic", ai.ProviderAuth{OAuth: recordingSignIn(calls, false), APIKey: storedKeyAuth()})))

			notice, err := r.Login(t.Context(), "anthropic", script(tc.answers...).ui())
			require.NoError(t, err)

			if tc.notice {
				assert.Equal(t, signInNotices["anthropic"], notice)
			} else {
				assert.Empty(t, notice)
			}
		})
	}
}

func TestProvidersReportLoginsAndStatus(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	require.NoError(t, r.RegisterProvider(oauthTestProvider(t)))
	find := func() Provider {
		providers, err := r.Providers(t.Context())
		require.NoError(t, err)
		assert.True(t, slices.IsSortedFunc(providers, func(a, b Provider) int { return strings.Compare(a.ID, b.ID) }))
		faux := slices.IndexFunc(providers, func(p Provider) bool { return p.ID == fauxProvider })
		require.GreaterOrEqual(t, faux, 0)
		assert.Equal(t, Provider{ID: fauxProvider}, providers[faux], "ambient-only auth is no login")
		i := slices.IndexFunc(providers, func(p Provider) bool { return p.ID == oauthProvider })
		require.GreaterOrEqual(t, i, 0)
		return providers[i]
	}

	assert.Equal(t, Provider{ID: oauthProvider, SignIn: "Kopi test OAuth"}, find())
	_, err := r.Login(t.Context(), oauthProvider, script("code-123", "blue").ui())
	require.NoError(t, err)
	assert.True(t, find().LoggedIn)
}

func TestLogout(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	require.NoError(t, r.RegisterProvider(oauthTestProvider(t)))
	_, err := r.Login(t.Context(), oauthProvider, script("code-123", "blue").ui())
	require.NoError(t, err)

	require.NoError(t, r.Logout(t.Context(), oauthProvider))

	ok, err := r.hasCredential(t.Context(), oauthProvider)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.ErrorIs(t, r.Logout(t.Context(), oauthProvider), ErrNotLoggedIn)
	assert.ErrorIs(t, r.Logout(t.Context(), "no-such-provider"), ErrUnknownProvider)
}

func TestLoginRejectsProvider(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	for _, tc := range []struct {
		name, provider string
		want           error
	}{
		{name: "unknown", provider: "no-such-provider", want: ErrUnknownProvider},
		{name: "without login", provider: fauxProvider, want: ErrNoLogin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := r.Login(t.Context(), tc.provider, script().ui())
			assert.ErrorIs(t, err, tc.want)
		})
	}
}

func TestLoginRejectsOptionThatWasNotOffered(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	require.NoError(t, r.RegisterProvider(oauthTestProvider(t)))

	_, err := r.Login(t.Context(), oauthProvider, script("code-123", "green").ui())

	require.ErrorContains(t, err, `invalid choice "green"`)
	ok, err := r.hasCredential(t.Context(), oauthProvider)
	require.NoError(t, err)
	assert.False(t, ok)
}

// promptingSignIn is a sign-in that returns at once while a goroutine of its
// own prompts under a ctx nothing cancels, as PiG's flows do after a callback
// server wins.
func promptingSignIn(prompt func(ai.AuthInteraction)) *ai.OAuthAuth {
	signIn := recordingSignIn(make(chan loginCall, 1), false)
	signIn.Login = func(_ context.Context, in ai.AuthInteraction, _ ai.LoginOptions) (ai.Credential, error) {
		prompt(in)
		return ai.Credential{Type: ai.CredentialOAuth, Access: syntheticToken, Refresh: "r", Expires: time.Now().Add(time.Hour).UnixMilli()}, nil
	}
	return signIn
}

func TestLoginCancelsAndJoinsAskInFlight(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	asking := make(chan struct{})
	require.NoError(t, r.RegisterProvider(loginTestProvider(oauthProvider, ai.ProviderAuth{OAuth: promptingSignIn(func(in ai.AuthInteraction) {
		go func() { _, _ = in.Prompt(context.Background(), ai.AuthTextPrompt{Message: "Code:"}) }()
		<-asking
	})})))
	returned := make(chan struct{})
	ui := LoginUI{
		Ask: func(ctx context.Context, _ LoginPrompt) (string, error) {
			close(asking)
			<-ctx.Done()
			close(returned)
			return "", context.Cause(ctx)
		},
		Show: func(string) {},
	}

	_, err := r.Login(t.Context(), oauthProvider, ui)

	require.NoError(t, err)
	select {
	case <-returned:
	default:
		t.Fatal("Login returned before the Ask in flight did")
	}
}

func TestLoginCancelledContextJoinsAskInFlight(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	signIn := recordingSignIn(make(chan loginCall, 1), false)
	signIn.Login = func(ctx context.Context, in ai.AuthInteraction, _ ai.LoginOptions) (ai.Credential, error) {
		go func() { _, _ = in.Prompt(context.Background(), ai.AuthTextPrompt{Message: "Code:"}) }()
		<-ctx.Done()
		return ai.Credential{}, ctx.Err()
	}
	require.NoError(t, r.RegisterProvider(loginTestProvider(oauthProvider, ai.ProviderAuth{OAuth: signIn})))
	ctx, cancel := context.WithCancel(t.Context())
	asking, returned := make(chan struct{}), make(chan struct{})
	ui := LoginUI{
		Ask: func(ctx context.Context, _ LoginPrompt) (string, error) {
			close(asking)
			<-ctx.Done()
			close(returned)
			return "", context.Cause(ctx)
		},
		Show: func(string) {},
	}
	go func() {
		<-asking
		cancel()
	}()

	_, err := r.Login(ctx, oauthProvider, ui)

	require.ErrorIs(t, err, context.Canceled)
	select {
	case <-returned:
	default:
		t.Fatal("Login returned before the Ask in flight did")
	}
}

func TestLoginRefusesAskAfterReturn(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	release := make(chan struct{})
	late := make(chan error, 1)
	require.NoError(t, r.RegisterProvider(loginTestProvider(oauthProvider, ai.ProviderAuth{OAuth: promptingSignIn(func(in ai.AuthInteraction) {
		go func() {
			<-release
			_, err := in.Prompt(context.Background(), ai.AuthTextPrompt{Message: "Code:"})
			late <- err
		}()
	})})))
	ui := script()

	_, err := r.Login(t.Context(), oauthProvider, ui.ui())
	require.NoError(t, err)
	close(release)

	require.ErrorIs(t, <-late, context.Canceled)
	assert.Empty(t, ui.prompts, "the late prompt never reached the UI")
}

func TestLoginDropsNotifyAfterReturn(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	opened := make(chan string, 1)
	r.openURL = func(u string) error {
		opened <- u
		return nil
	}
	release := make(chan struct{})
	notified := make(chan struct{})
	require.NoError(t, r.RegisterProvider(loginTestProvider(oauthProvider, ai.ProviderAuth{OAuth: promptingSignIn(func(in ai.AuthInteraction) {
		go func() {
			<-release
			in.Notify(ai.AuthURLEvent{URL: loginURL})
			in.Notify(ai.AuthProgressEvent{Message: "late progress"})
			close(notified)
		}()
	})})))
	ui := script()

	_, err := r.Login(t.Context(), oauthProvider, ui.ui())
	require.NoError(t, err)
	close(release)
	<-notified

	assert.Empty(t, ui.shownText(), "a late event is never shown")
	assert.Empty(t, opened, "a late URL is never opened")
}
