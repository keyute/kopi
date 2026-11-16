package pigruntime

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

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
