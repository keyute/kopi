package pigruntime

import (
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/MichaelKinsy/PiG/ai"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var kopiUserAgent = "kopi/v0.0.0-test (" + runtime.GOOS + "; " + runtime.GOARCH + ")"

func writeAgentFile(t *testing.T, sb sandbox, name, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(sb.agentDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(sb.agentDir, name), []byte(content), 0o600))
}

func TestIdentifyRegistersKopiHeadersForCodexProvider(t *testing.T) {
	sb := newSandbox(t)
	writeAgentFile(t, sb, "models.json", `{"providers":{"kopi-codex":{"baseUrl":"http://127.0.0.1:1/v1","api":"openai-codex-responses","apiKey":"synthetic-key","models":[{"id":"codex-1"}]}}}`)
	r, _ := newTestRuntime(t, sb)

	require.NoError(t, r.identify("kopi-codex"))

	cfg := r.svcs.ModelRuntime().GetRegisteredProviderConfig("kopi-codex")
	require.NotNil(t, cfg)
	assert.Equal(t, map[string]string{"User-Agent": kopiUserAgent, "originator": "kopi"}, cfg.Headers)
	assert.Empty(t, cfg.Models)
	assert.NotNil(t, r.svcs.ModelRuntime().GetModel("kopi-codex", "codex-1"), "the models.json models are kept")
}

func TestIdentifyAnthropicKeepsPiGUserAgent(t *testing.T) {
	for name, auth := range map[string]string{
		"sign-in":        `{"anthropic":{"type":"oauth","access":"synthetic-access","refresh":"r","expires":4102444800000}}`,
		"api key":        `{"anthropic":{"type":"api_key","key":"synthetic-key"}}`,
		"nothing stored": `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			sb := newSandbox(t)
			writeAgentFile(t, sb, "auth.json", auth)
			r, _ := newTestRuntime(t, sb)

			require.NoError(t, r.identify("anthropic"))

			assert.Nil(t, r.svcs.ModelRuntime().GetRegisteredProviderConfig("anthropic"))
		})
	}
}

func TestIdentifyLeavesNativeProviderAlone(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)

	require.NoError(t, r.identify(fauxProvider))

	assert.Nil(t, r.svcs.ModelRuntime().GetRegisteredProviderConfig(fauxProvider))
	assert.NotNil(t, r.svcs.ModelRuntime().GetRegisteredNativeProvider(fauxProvider))
}

func TestIdentifyLeavesConfiguredProviderIdentityCaseInsensitively(t *testing.T) {
	t.Setenv("KOPI_UNSET_IDENTITY_TEST", "")
	require.NoError(t, os.Unsetenv("KOPI_UNSET_IDENTITY_TEST"))
	for _, tc := range []struct {
		name, headers string
		// unresolved marks values that fail at request resolution, which
		// identify must never reach.
		unresolved bool
	}{
		{"lowercase", `"user-agent":"provider-agent","originator":"provider-origin"`, false},
		{"mixed case", `"uSeR-aGeNt":"provider-agent","Originator":"provider-origin"`, false},
		{"duplicate case", `"User-Agent":"earlier-agent","user-agent":"provider-agent","originator":"earlier-origin","Originator":"provider-origin"`, false},
		{"reversed duplicate case", `"user-agent":"earlier-agent","User-Agent":"provider-agent","Originator":"earlier-origin","originator":"provider-origin"`, false},
		{"unset environment", `"user-agent":"$KOPI_UNSET_IDENTITY_TEST","Originator":"$KOPI_UNSET_IDENTITY_TEST"`, true},
		{"failing command", `"user-agent":"!exit 73","Originator":"!exit 74"`, true},
		{"duplicate unresolved values", `"User-Agent":"$KOPI_UNSET_IDENTITY_TEST","user-agent":"!exit 73","originator":"!exit 74","Originator":"$KOPI_UNSET_IDENTITY_TEST"`, true},
		{"reversed duplicate unresolved values", `"user-agent":"!exit 73","User-Agent":"$KOPI_UNSET_IDENTITY_TEST","Originator":"$KOPI_UNSET_IDENTITY_TEST","originator":"!exit 74"`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sb := newSandbox(t)
			writeAgentFile(t, sb, "models.json", fmt.Sprintf(`{"providers":{"kopi-codex":{"baseUrl":"http://127.0.0.1:1/v1","api":"openai-codex-responses","apiKey":"synthetic-key","headers":{%s,"X-Provider":"kept"},"models":[{"id":"codex-1","name":"Catalog name","contextWindow":12345,"maxTokens":4321,"cost":{"input":2,"output":3,"cacheRead":0,"cacheWrite":0},"headers":{"X-Model":"kept"}},{"id":"explicit","headers":{"USER-agent":"model-agent","ORIGINATOR":"model-origin"}},{"id":"chat","api":"openai-completions"}]}}}`, tc.headers))
			r, _ := newTestRuntime(t, sb)
			mr := r.svcs.ModelRuntime()
			before := mr.GetAllModels("kopi-codex")
			require.Len(t, before, 3)
			for range 2 {
				require.NoError(t, r.identify("kopi-codex"))
				assert.Nil(t, mr.GetRegisteredProviderConfig("kopi-codex"), "the configured identity is not overridden")
				after := mr.GetAllModels("kopi-codex")
				require.Len(t, after, len(before))
				for i, model := range after {
					original, current := *before[i].(*ai.Model), *model.(*ai.Model)
					assert.Equal(t, original, current, "catalog fields remain unchanged")
					configured, err := mr.GetCompatibilityRequestConfig(model.(*ai.Model))
					if tc.unresolved {
						require.Error(t, err, "configured values are deferred until request resolution")
						continue
					}
					require.NoError(t, err)
					headers := http.Header{}
					for key, value := range configured.Headers {
						headers.Add(key, *value)
					}
					wantAgent, wantOrigin := "provider-agent", "provider-origin"
					if model.ModelID() == "explicit" {
						wantAgent, wantOrigin = "model-agent", "model-origin"
						auth, err := mr.GetModelAuth(t.Context(), model)
						require.NoError(t, err)
						require.NotNil(t, auth)
						require.Contains(t, auth.Auth.Headers, "USER-agent")
						require.Contains(t, auth.Auth.Headers, "ORIGINATOR")
						assert.Equal(t, wantAgent, *auth.Auth.Headers["USER-agent"])
						assert.Equal(t, wantOrigin, *auth.Auth.Headers["ORIGINATOR"])
					}
					assert.Equal(t, []string{wantAgent}, headers.Values("User-Agent"))
					assert.Equal(t, []string{wantOrigin}, headers.Values("Originator"))
					assert.Equal(t, "kept", headers.Get("X-Provider"))
					if model.ModelID() == "codex-1" {
						assert.Equal(t, "kept", headers.Get("X-Model"))
					}
				}
			}
		})
	}
}

func TestIdentifyRegistersOnlyUnconfiguredIdentity(t *testing.T) {
	sb := newSandbox(t)
	// PiG accepts a BOM, comments and trailing commas in models.json.
	writeAgentFile(t, sb, "models.json", "\uFEFF"+`{
	// A user's own identity for this provider.
	"providers": {"kopi-codex": {"baseUrl": "http://127.0.0.1:1/v1", "api": "openai-codex-responses", "apiKey": "synthetic-key",
		"headers": {"user-agent": "!exit 73",},
		"models": [{"id": "codex-1"},],
	},},
}`)
	r, _ := newTestRuntime(t, sb)
	mr := r.svcs.ModelRuntime()
	require.NotNil(t, mr.GetModel("kopi-codex", "codex-1"), "PiG loaded the file")

	require.NoError(t, r.identify("kopi-codex"))

	cfg := mr.GetRegisteredProviderConfig("kopi-codex")
	require.NotNil(t, cfg)
	assert.Equal(t, map[string]string{"originator": "kopi"}, cfg.Headers)
}

func TestIdentifyIgnoresUnparsableModelsFile(t *testing.T) {
	sb := newSandbox(t)
	writeAgentFile(t, sb, "models.json", `{"providers":{"openai-codex":{"headers":{"User-Agent":"user-agent"`)
	r, _ := newTestRuntime(t, sb)

	require.NoError(t, r.identify("openai-codex"))

	cfg := r.svcs.ModelRuntime().GetRegisteredProviderConfig("openai-codex")
	require.NotNil(t, cfg)
	assert.Equal(t, map[string]string{"User-Agent": kopiUserAgent, "originator": "kopi"}, cfg.Headers)
}

func TestIdentifyDoesNotResolveUnrelatedProviderHeaders(t *testing.T) {
	t.Setenv("KOPI_UNSET_IDENTITY_TEST", "")
	require.NoError(t, os.Unsetenv("KOPI_UNSET_IDENTITY_TEST"))
	for _, value := range []string{"$KOPI_UNSET_IDENTITY_TEST", "!exit 73"} {
		t.Run(value, func(t *testing.T) {
			sb := newSandbox(t)
			writeAgentFile(t, sb, "models.json", fmt.Sprintf(`{"providers":{"kopi-codex":{"baseUrl":"http://127.0.0.1:1/v1","api":"openai-codex-responses","apiKey":"synthetic-key","headers":{"X-Unrelated":%q},"models":[{"id":"codex-1"}]}}}`, value))
			r, _ := newTestRuntime(t, sb)

			require.NoError(t, r.identify("kopi-codex"))

			mr := r.svcs.ModelRuntime()
			_, err := mr.GetCompatibilityRequestConfig(mr.GetModel("kopi-codex", "codex-1"))
			require.Error(t, err, "unrelated values are deferred until request resolution")
		})
	}
}

func TestIdentifyPreservesCopilotModelIdentity(t *testing.T) {
	sb := newSandbox(t)
	writeAgentFile(t, sb, "auth.json", `{"github-copilot":{"type":"api_key","key":"synthetic-key"}}`)
	r, _ := newTestRuntime(t, sb)
	mr := r.svcs.ModelRuntime()
	before := mr.GetModels("github-copilot")
	require.NotEmpty(t, before)
	want := maps.Clone(before[0].ProviderMeta.Headers)
	require.NotEmpty(t, want)
	for range 2 {
		require.NoError(t, r.identify("github-copilot"))
		auth, err := mr.GetModelAuth(t.Context(), mr.GetModel("github-copilot", before[0].ID))
		require.NoError(t, err)
		require.NotNil(t, auth)
		for name, value := range want {
			require.Contains(t, auth.Auth.Headers, name)
			assert.Equal(t, value, *auth.Auth.Headers[name])
		}
	}
}
