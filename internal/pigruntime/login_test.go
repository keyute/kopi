package pigruntime

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

func TestLoginPersistsCredentialForFreshRuntime(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	require.NoError(t, r.RegisterProvider(oauthTestProvider(t)))
	ok, err := r.hasCredential(t.Context(), oauthProvider)
	require.NoError(t, err)
	assert.False(t, ok)

	var out strings.Builder
	require.NoError(t, r.Login(t.Context(), oauthProvider, strings.NewReader("code-123\n2\n"), &out))

	assert.Contains(t, out.String(), loginURL)
	assert.Contains(t, out.String(), "2) Blue")
	assert.NotContains(t, out.String(), syntheticToken)

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

	var out strings.Builder
	require.NoError(t, r.Login(t.Context(), oauthProvider, strings.NewReader("code-123\n2\n"), &out))

	assert.Equal(t, loginURL, <-opened)
	assert.Contains(t, out.String(), loginURL, "the printed URL is the fallback")
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

			require.NoError(t, r.Login(t.Context(), provider, strings.NewReader(""), io.Discard))

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
		require.NoError(t, r.Login(t.Context(), oauthProvider, strings.NewReader(""), io.Discard))
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

	require.NoError(t, r.Login(t.Context(), oauthProvider, strings.NewReader(""), io.Discard))

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

	err := r.Login(t.Context(), oauthProvider, strings.NewReader(""), io.Discard)

	require.Error(t, err)
	assert.Contains(t, err.Error(), file)
	assert.NotContains(t, err.Error(), "requires a device ID")
}

func TestLoginStoresAPIKey(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	require.NoError(t, r.RegisterProvider(loginTestProvider(oauthProvider, ai.ProviderAuth{APIKey: storedKeyAuth()})))

	var out strings.Builder
	require.NoError(t, r.Login(t.Context(), oauthProvider, strings.NewReader(syntheticToken+"\n"), &out))

	assert.Contains(t, out.String(), "Enter API key")
	assert.NotContains(t, out.String(), syntheticToken)
	content, err := os.ReadFile(filepath.Join(sb.agentDir, "auth.json"))
	require.NoError(t, err)
	assert.Contains(t, string(content), syntheticToken)
	ok, err := r.hasCredential(t.Context(), oauthProvider)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestLoginRejectsEmptyAPIKey(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	require.NoError(t, r.RegisterProvider(loginTestProvider(oauthProvider, ai.ProviderAuth{APIKey: storedKeyAuth()})))

	err := r.Login(t.Context(), oauthProvider, strings.NewReader("\n"), io.Discard)

	require.Error(t, err)
	providers, err := r.Providers(t.Context())
	require.NoError(t, err)
	i := slices.IndexFunc(providers, func(p Provider) bool { return p.ID == oauthProvider })
	require.GreaterOrEqual(t, i, 0)
	assert.False(t, providers[i].LoggedIn)
}

func TestLoginAsksForMethodWhenProviderOffersBoth(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	calls := make(chan loginCall, 1)
	require.NoError(t, r.RegisterProvider(loginTestProvider(oauthProvider, ai.ProviderAuth{OAuth: recordingSignIn(calls, false), APIKey: storedKeyAuth()})))

	var out strings.Builder
	require.NoError(t, r.Login(t.Context(), oauthProvider, strings.NewReader("2\n"+syntheticToken+"\n"), &out))

	assert.Contains(t, out.String(), "1) Kopi test sign-in")
	assert.Contains(t, out.String(), "2) API key")
	assert.Empty(t, calls, "the sign-in never ran")
	content, err := os.ReadFile(filepath.Join(sb.agentDir, "auth.json"))
	require.NoError(t, err)
	assert.Contains(t, string(content), `"api_key"`)
}

func TestLoginPrintsAnthropicBillingNoticeAfterSignInOnly(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		notice      bool
	}{
		{name: "sign-in", input: "1\n", notice: true},
		{name: "api key", input: "2\n" + syntheticToken + "\n", notice: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sb := newSandbox(t)
			r, _ := newTestRuntime(t, sb)
			calls := make(chan loginCall, 1)
			require.NoError(t, r.RegisterProvider(loginTestProvider("anthropic", ai.ProviderAuth{OAuth: recordingSignIn(calls, false), APIKey: storedKeyAuth()})))

			var out strings.Builder
			require.NoError(t, r.Login(t.Context(), "anthropic", strings.NewReader(tc.input), &out))

			if tc.notice {
				assert.Contains(t, out.String(), signInNotices["anthropic"])
			} else {
				assert.NotContains(t, out.String(), signInNotices["anthropic"])
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
	require.NoError(t, r.Login(t.Context(), oauthProvider, strings.NewReader("code-123\n2\n"), io.Discard))
	assert.True(t, find().LoggedIn)
}

func TestLogout(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	require.NoError(t, r.RegisterProvider(oauthTestProvider(t)))
	require.NoError(t, r.Login(t.Context(), oauthProvider, strings.NewReader("code-123\n2\n"), io.Discard))

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
			err := r.Login(t.Context(), tc.provider, strings.NewReader(""), io.Discard)
			assert.ErrorIs(t, err, tc.want)
		})
	}
}

func TestLoginPromptReturnsWhenContextEnds(t *testing.T) {
	in, w := io.Pipe()
	t.Cleanup(func() { _ = w.Close() })
	l := &loginIO{in: in, lines: bufio.NewReader(in), out: io.Discard}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := l.prompt(ctx, ai.AuthManualCodePrompt{Message: "Code:"})
	assert.ErrorIs(t, err, context.Canceled)
}

func TestLoginPromptCancelledHandsPendingReadToNextPrompt(t *testing.T) {
	in, w := io.Pipe()
	t.Cleanup(func() { _ = w.Close() })
	l := &loginIO{in: in, lines: bufio.NewReader(in), out: io.Discard}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := l.prompt(ctx, ai.AuthManualCodePrompt{Message: "Code:"})
	require.ErrorIs(t, err, context.Canceled)
	require.NotNil(t, l.pending, "the cancelled prompt leaves its read in flight")

	type result struct {
		text string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		text, err := l.prompt(t.Context(), ai.AuthManualCodePrompt{Message: "Code:"})
		done <- result{text, err}
	}()
	_, err = io.WriteString(w, "code-123\n")
	require.NoError(t, err)
	res := <-done
	require.NoError(t, res.err)
	assert.Equal(t, "code-123", res.text)
	assert.Nil(t, l.pending)
}
