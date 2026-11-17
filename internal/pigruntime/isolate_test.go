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
	t.Setenv("PI_TELEMETRY", "1")

	r, err := New(t.Context(), Options{Home: kopiHome, AgentDir: agentDir, CWD: ws})
	require.NoError(t, err)
	t.Cleanup(r.Close)

	assert.Equal(t, agentDir, os.Getenv("PIG_CODING_AGENT_DIR"))
	assert.Equal(t, filepath.Join(kopiHome, "pig"), os.Getenv("PIG_HOME"))
	for _, k := range []string{"PIG_USE_PI_DIRS", "PIG_TEST_FAUX", "PI_TELEMETRY"} {
		_, ok := os.LookupEnv(k)
		assert.False(t, ok, k)
	}
	info, err := os.Stat(agentDir)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

func TestNewRejectsNonAbsoluteRoots(t *testing.T) {
	tests := map[string]func(sb sandbox) Options{
		"relative home":      func(sb sandbox) Options { return Options{Home: "kopi", AgentDir: sb.agentDir, CWD: sb.ws} },
		"empty home":         func(sb sandbox) Options { return Options{AgentDir: sb.agentDir, CWD: sb.ws} },
		"relative agent dir": func(sb sandbox) Options { return Options{Home: sb.kopiHome, AgentDir: "agent", CWD: sb.ws} },
		"empty agent dir":    func(sb sandbox) Options { return Options{Home: sb.kopiHome, CWD: sb.ws} },
	}
	for name, opts := range tests {
		t.Run(name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			sb := newSandbox(t)
			t.Setenv("PIG_HOME", filepath.Join(sb.home, ".pig"))
			t.Setenv("PIG_USE_PI_DIRS", "1")
			before := os.Environ()

			_, err := New(t.Context(), opts(sb))
			require.Error(t, err)
			assert.ElementsMatch(t, before, os.Environ())
		})
	}
}
