package cmd

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/pigruntime"
)

func TestLoginWithoutProviderListsProviders(t *testing.T) {
	e := newTestEnv(t)
	e.writeFile(t, filepath.Join(e.agentDir, "auth.json"), `{"mistral":{"type":"api_key","key":"`+syntheticKey+`"}}`)

	stdout, _, err := execute(t, "", "login")

	require.NoError(t, err)
	assert.Regexp(t, regexp.MustCompile(`(?m)^  openai-codex +OpenAI \(ChatGPT Plus/Pro\) *$`), stdout)
	assert.Regexp(t, regexp.MustCompile(`(?m)^  openai +OpenAI \(ChatGPT subscription\), API key *$`), stdout)
	assert.Regexp(t, regexp.MustCompile(`(?m)^  mistral +API key +logged in$`), stdout)
	assert.Contains(t, stdout, "kopi login <provider>")
	assert.NotContains(t, stdout, syntheticKey)
}

func TestLoginStoresAPIKey(t *testing.T) {
	e := newTestEnv(t)

	stdout, _, err := execute(t, syntheticKey+"\n", "login", "mistral")

	require.NoError(t, err)
	assert.Contains(t, stdout, "Logged in to mistral.")
	assert.NotContains(t, stdout, syntheticKey)
	content, err := os.ReadFile(filepath.Join(e.agentDir, "auth.json"))
	require.NoError(t, err)
	assert.Contains(t, string(content), syntheticKey)
}

func TestLoginUnknownProviderFails(t *testing.T) {
	newTestEnv(t)

	_, _, err := execute(t, "", "login", "nosuchprovider")

	require.ErrorIs(t, err, pigruntime.ErrUnknownProvider)
}
