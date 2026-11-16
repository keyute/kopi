package pigruntime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding"
)

// Anthropic subscription tokens require the client identity PiG selects from
// the request's credential, even when that credential comes from the environment.
var keepClientIdentity = []string{"anthropic"}

// identify registers provider defaults so model headers retain higher precedence.
// Native providers must not be re-registered: that would replace them.
func (r *Runtime) identify(provider string) error {
	mr := r.svcs.ModelRuntime()
	if mr.GetRegisteredNativeProvider(provider) != nil {
		return nil
	}
	headers := map[string]string{}
	if !slices.Contains(keepClientIdentity, provider) {
		headers["User-Agent"] = fmt.Sprintf("kopi/%s (%s; %s)", r.version, runtime.GOOS, runtime.GOARCH)
	}
	if slices.ContainsFunc(mr.GetModels(provider), func(m *ai.Model) bool { return m.ProviderMeta.API == ai.APIOpenAICodexResponses }) {
		headers["originator"] = defaultSignInName
	}
	if len(headers) == 0 {
		return nil
	}
	// Read names only: resolving discarded header values could execute commands.
	var configured struct {
		Providers map[string]struct {
			Headers map[string]json.RawMessage `json:"headers"`
		} `json:"providers"`
	}
	data, err := os.ReadFile(filepath.Join(r.svcs.AgentDir(), "models.json"))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to read provider header names: %w", err)
	}
	if err == nil {
		if err := json.Unmarshal(data, &configured); err != nil {
			return fmt.Errorf("failed to decode provider header names: %w", err)
		}
	}
	registration := make(map[string]string, len(headers))
	for name, value := range headers {
		matched := false
		for existing := range configured.Providers[provider].Headers {
			if strings.EqualFold(existing, name) {
				registration[existing] = value
				matched = true
			}
		}
		if !matched {
			registration[name] = value
		}
	}
	if err := mr.RegisterProvider(provider, coding.ProviderConfigInput{Headers: registration}); err != nil {
		return fmt.Errorf("failed to set request identity for provider %q: %w", provider, err)
	}
	return nil
}
