package pigruntime

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// limitsTransport answers every plan-limit request with status and body.
type limitsTransport struct {
	status int
	body   string
	mu     sync.Mutex
	reqs   []*http.Request
}

func (f *limitsTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	return &http.Response{StatusCode: f.status, Status: http.StatusText(f.status), Body: io.NopCloser(strings.NewReader(f.body)), Header: http.Header{}}, nil
}

func (f *limitsTransport) requests() []*http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs
}

// newLimitsRuntime is a runtime whose plan-limit requests go to the returned
// transport. provider is registered with a sign-in whose ToAuth is toAuth.
func newLimitsRuntime(t *testing.T, provider string, toAuth func(ai.Credential) (ai.ModelAuth, error), status int, body string) (*Runtime, *limitsTransport) {
	t.Helper()
	r, _ := newTestRuntime(t, newSandbox(t))
	f := &limitsTransport{status: status, body: body}
	r.limitsClient = &http.Client{Transport: f}
	require.NoError(t, r.RegisterProvider(loginTestProvider(provider, ai.ProviderAuth{OAuth: &ai.OAuthAuth{
		Name:    "Kopi test sign-in",
		Refresh: func(_ context.Context, c ai.Credential) (ai.Credential, error) { return c, nil },
		ToAuth:  toAuth,
	}})))
	return r, f
}

func storeCredential(t *testing.T, r *Runtime, provider string, cred ai.Credential) {
	t.Helper()
	_, err := r.svcs.Credentials().Modify(t.Context(), provider, func(*ai.Credential) (*ai.Credential, error) { return &cred, nil })
	require.NoError(t, err)
}

func oauthCredential(access, refresh string) ai.Credential {
	return ai.Credential{Type: ai.CredentialOAuth, Access: access, Refresh: refresh, Expires: time.Now().Add(time.Hour).UnixMilli()}
}

func accessAsKey(c ai.Credential) (ai.ModelAuth, error) { return ai.ModelAuth{APIKey: c.Access}, nil }

func TestLimitsCodex(t *testing.T) {
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"acct-1"}}`))
	token := "header." + claims + ".signature"
	r, f := newLimitsRuntime(t, "openai-codex", accessAsKey, http.StatusOK,
		`{"rate_limit":{"primary_window":{"used_percent":12,"limit_window_seconds":18000,"reset_at":1785269351}}}`)
	storeCredential(t, r, "openai-codex", oauthCredential(token, "r"))

	got, err := r.Limits(t.Context(), "openai-codex")

	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "5h", got[0].Label)
	require.Len(t, f.requests(), 1)
	req := f.requests()[0]
	assert.Equal(t, "https://chatgpt.com/backend-api/wham/usage", req.URL.String())
	assert.Equal(t, "Bearer "+token, req.Header.Get("Authorization"))
	assert.Equal(t, "acct-1", req.Header.Get("ChatGPT-Account-Id"))
}

func TestLimitsCopilotSendsTheGitHubToken(t *testing.T) {
	r, f := newLimitsRuntime(t, "github-copilot", accessAsKey, http.StatusOK,
		`{"quota_snapshots":{"premium_interactions":{"percent_remaining":66}}}`)
	cred := oauthCredential("copilot-short-lived", "github-token")
	cred.EnterpriseDomain = "octo.ghe.com"
	storeCredential(t, r, "github-copilot", cred)

	got, err := r.Limits(t.Context(), "github-copilot")

	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Len(t, f.requests(), 1)
	req := f.requests()[0]
	assert.Equal(t, "https://api.octo.ghe.com/copilot_internal/user", req.URL.String())
	assert.Equal(t, "Bearer github-token", req.Header.Get("Authorization"))
}

func TestLimitsKimiTokenComesFromTheHeader(t *testing.T) {
	r, f := newLimitsRuntime(t, "kimi-coding", func(c ai.Credential) (ai.ModelAuth, error) {
		return ai.ModelAuth{Headers: ai.ProviderHeaders{"Authorization": new("Bearer " + c.Access)}}, nil
	}, http.StatusOK, `{"usages":{"limit_5h":{"used_ratio":0.12}}}`)
	storeCredential(t, r, "kimi-coding", oauthCredential("kimi-token", "r"))

	got, err := r.Limits(t.Context(), "kimi-coding")

	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Len(t, f.requests(), 1)
	assert.Equal(t, "Bearer kimi-token", f.requests()[0].Header.Get("Authorization"))
}

func TestLimitsXAI(t *testing.T) {
	r, f := newLimitsRuntime(t, "xai", accessAsKey, http.StatusOK,
		`{"config":{"creditUsagePercent":5,"currentPeriod":{"start":"2026-11-17T00:00:00Z","end":"2026-11-24T00:00:00Z"}}}`)
	storeCredential(t, r, "xai", oauthCredential("xai-token", "r"))

	got, err := r.Limits(t.Context(), "xai")

	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Len(t, f.requests(), 1)
	assert.Equal(t, "Bearer xai-token", f.requests()[0].Header.Get("Authorization"))
}

func TestLimitsReadNothingWithoutAnAccount(t *testing.T) {
	r, f := newLimitsRuntime(t, "openai-codex", accessAsKey, http.StatusOK, `{}`)
	storeCredential(t, r, "anthropic", oauthCredential("tok", "r"))
	storeCredential(t, r, "xai", ai.Credential{Type: ai.CredentialAPIKey, Key: "key"})
	storeCredential(t, r, "github-copilot", oauthCredential("copilot-short-lived", ""))

	for _, provider := range []string{"anthropic", "openai-codex", "xai", "github-copilot", "unknown"} {
		got, err := r.Limits(t.Context(), provider)

		require.NoError(t, err, provider)
		assert.Empty(t, got, provider)
	}
	assert.Empty(t, f.requests(), "no reader, no credential and an API key send nothing")
}

func TestLimitsFailureIsWrapped(t *testing.T) {
	r, _ := newLimitsRuntime(t, "kimi-coding", accessAsKey, http.StatusInternalServerError, ``)
	storeCredential(t, r, "kimi-coding", oauthCredential("kimi-secret", "r"))

	_, err := r.Limits(t.Context(), "kimi-coding")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to read the plan limits of kimi-coding")
	assert.NotContains(t, err.Error(), "kimi-secret")
}
