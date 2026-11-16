package pigruntime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding"
	"github.com/tailscale/hujson"
)

// Anthropic subscription tokens require the client identity PiG selects from
// the request's credential, even when that credential comes from the environment.
var keepClientIdentity = []string{"anthropic"}

// identify registers provider defaults so model headers retain higher
// precedence. A header the provider's models.json headers already set is left
// out, since registered headers outrank them and the user's value must win.
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
	// Read names only: resolving header values could execute commands.
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
		// PiG accepts a BOM, comments and trailing commas, and ignores a
		// models.json it cannot parse, so such a file configures no headers.
		std, err := hujson.Standardize(bytes.TrimPrefix(data, []byte("\uFEFF")))
		if err == nil {
			err = json.Unmarshal(std, &configured)
		}
		if err != nil {
			configured.Providers = nil
		}
	}
	for existing := range configured.Providers[provider].Headers {
		for name := range headers {
			if strings.EqualFold(existing, name) {
				delete(headers, name)
			}
		}
	}
	if len(headers) == 0 {
		return nil
	}
	if err := mr.RegisterProvider(provider, coding.ProviderConfigInput{Headers: headers}); err != nil {
		return fmt.Errorf("failed to set request identity for provider %q: %w", provider, err)
	}
	return nil
}
