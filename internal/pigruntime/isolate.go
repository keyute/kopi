package pigruntime

import (
	"fmt"
	"os"
	"path/filepath"
)

// IsolationEnv is the process environment PiG must see. PiG resolves its
// default agent dir even when one is passed explicitly (session lookup) and
// panics when no home is resolvable, so both roots are pinned under Kopi home
// rather than left to fall back to ~/.pig or ~/.pi. New applies it; it is
// exported only so test helpers can restore these variables.
func IsolationEnv(kopiHome, agentDir string) (set map[string]string, unset []string) {
	set = map[string]string{
		"PIG_CODING_AGENT_DIR": agentDir,
		"PIG_HOME":             filepath.Join(kopiHome, "pig"),
	}
	unset = []string{"PIG_USE_PI_DIRS", "PIG_TEST_FAUX"}
	return set, unset
}

// isolate applies IsolationEnv process-wide and creates the agent dir.
func isolate(kopiHome, agentDir string) error {
	set, unset := IsolationEnv(kopiHome, agentDir)
	for k, v := range set {
		if err := os.Setenv(k, v); err != nil {
			return fmt.Errorf("failed to set %s: %w", k, err)
		}
	}
	for _, k := range unset {
		if err := os.Unsetenv(k); err != nil {
			return fmt.Errorf("failed to unset %s: %w", k, err)
		}
	}
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		return fmt.Errorf("failed to create agent dir: %w", err)
	}
	return nil
}
