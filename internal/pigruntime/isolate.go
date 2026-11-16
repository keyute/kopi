package pigruntime

import (
	"fmt"
	"os"
	"path/filepath"
)

// isolate pins PiG's environment process-wide and creates the agent dir. PiG
// resolves its default agent dir even when one is passed explicitly (session
// lookup) and panics when no home is resolvable, so both roots are pinned
// under Kopi home rather than left to fall back to ~/.pig or ~/.pi.
func isolate(kopiHome, agentDir string) error {
	if !filepath.IsAbs(kopiHome) || !filepath.IsAbs(agentDir) {
		return fmt.Errorf("failed to isolate pig: home %q and agent dir %q must be absolute", kopiHome, agentDir)
	}
	set := map[string]string{
		"PIG_CODING_AGENT_DIR": agentDir,
		"PIG_HOME":             filepath.Join(kopiHome, "pig"),
	}
	for k, v := range set {
		if err := os.Setenv(k, v); err != nil {
			return fmt.Errorf("failed to set %s: %w", k, err)
		}
	}
	// PI_TELEMETRY would override the install-telemetry setting that keeps
	// PiG's attribution headers off Kopi's requests.
	for _, k := range []string{"PIG_USE_PI_DIRS", "PIG_TEST_FAUX", "PI_TELEMETRY"} {
		if err := os.Unsetenv(k); err != nil {
			return fmt.Errorf("failed to unset %s: %w", k, err)
		}
	}
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		return fmt.Errorf("failed to create agent dir: %w", err)
	}
	return nil
}
