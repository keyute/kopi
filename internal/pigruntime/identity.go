package pigruntime

import (
	"fmt"
	"runtime"
	"slices"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding"
)

// keepClientIdentity are providers whose subscription tokens work only under
// the client identity PiG sends for them (Claude Code's User-Agent for an
// anthropic OAuth token), which a provider header would replace. PiG picks
// that identity from the token it resolves per request, which may come from
// the environment or a pasted key, so Kopi cannot tell in advance and never
// sets its User-Agent on them. github-copilot needs no
// entry: every model PiG ships for it sets its own User-Agent, and model
// headers outrank provider headers.
var keepClientIdentity = []string{"anthropic"}

// identify makes every request to provider name Kopi. PiG sets its own
// User-Agent per request, ignores the agent's, and re-reads the session's
// model from its catalog; provider headers reach every path (turns,
// compaction, Codex SSE and WebSocket) and rank above PiG's own. A provider
// registered natively is left alone: re-registering it with headers would
// replace it.
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
	if err := mr.RegisterProvider(provider, coding.ProviderConfigInput{Headers: headers}); err != nil {
		return fmt.Errorf("failed to set request identity for provider %q: %w", provider, err)
	}
	return nil
}
