package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/pigruntime"
)

func TestLogoutRemovesStoredCredential(t *testing.T) {
	e := newTestEnv(t)
	authFile := filepath.Join(e.agentDir, "auth.json")
	e.writeFile(t, authFile, `{"mistral":{"type":"api_key","key":"`+syntheticKey+`"}}`)

	stdout, _, err := execute(t, "", "logout", "mistral")

	require.NoError(t, err)
	assert.Equal(t, "Logged out of mistral.\n", stdout)
	content, err := os.ReadFile(authFile)
	require.NoError(t, err)
	assert.NotContains(t, string(content), syntheticKey)

	_, _, err = execute(t, "", "logout", "mistral")
	require.ErrorIs(t, err, pigruntime.ErrNotLoggedIn)
}

func TestLogoutUnknownProviderFails(t *testing.T) {
	newTestEnv(t)

	_, _, err := execute(t, "", "logout", "nosuchprovider")

	require.ErrorIs(t, err, pigruntime.ErrUnknownProvider)
}

func TestLogoutNeedsProvider(t *testing.T) {
	newTestEnv(t)

	_, _, err := execute(t, "", "logout")

	require.Error(t, err)
}
