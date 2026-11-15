package pigruntime

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewPinsIsolationEnv(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	kopiHome := filepath.Join(root, "kopi")
	agentDir := filepath.Join(kopiHome, "agent")
	ws := filepath.Join(root, "ws")
	for _, dir := range []string{home, ws} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", "")
	// Ambient values New must override; t.Setenv also restores the originals.
	t.Setenv("PIG_CODING_AGENT_DIR", filepath.Join(home, ".pig", "agent"))
	t.Setenv("PIG_HOME", filepath.Join(home, ".pig"))
	t.Setenv("PIG_USE_PI_DIRS", "1")
	t.Setenv("PIG_TEST_FAUX", "1")

	r, err := New(t.Context(), Options{Home: kopiHome, AgentDir: agentDir, CWD: ws})
	require.NoError(t, err)
	t.Cleanup(r.Close)

	assert.Equal(t, agentDir, os.Getenv("PIG_CODING_AGENT_DIR"))
	assert.Equal(t, filepath.Join(kopiHome, "pig"), os.Getenv("PIG_HOME"))
	for _, k := range []string{"PIG_USE_PI_DIRS", "PIG_TEST_FAUX"} {
		_, ok := os.LookupEnv(k)
		assert.False(t, ok, k)
	}
	info, err := os.Stat(agentDir)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}
