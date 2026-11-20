// Package pigruntimetest provides a sandboxed PiG runtime and faux-provider
// helpers for tests of the packages above pigruntime.
package pigruntimetest

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/pigruntime"
)

// DecoyMarkers are the strings PlantDecoys writes; none may reach a provider.
var DecoyMarkers = []string{
	"DECOY_PIG_SETTINGS", "DECOY_PIG_AUTH", "DECOY_PI_SETTINGS", "DECOY_CLAUDE_SETTINGS", "DECOY_WS_AGENTS", "DECOY_WS_CLAUDE", "DECOY_WS_PIG_SETTINGS",
}

type Sandbox struct {
	Home, KopiHome, AgentDir, Workspace string
}

// NewSandbox creates temp home/, kopi/ (with agent/) and ws/ directories and
// points HOME at home/, with an empty PATH and PiG's variables cleared.
func NewSandbox(t testing.TB) Sandbox {
	t.Helper()
	root := t.TempDir()
	sb := Sandbox{
		Home:      filepath.Join(root, "home"),
		KopiHome:  filepath.Join(root, "kopi"),
		Workspace: filepath.Join(root, "ws"),
	}
	sb.AgentDir = filepath.Join(sb.KopiHome, "agent")
	for _, dir := range []string{sb.Home, sb.AgentDir, sb.Workspace} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}
	t.Setenv("HOME", sb.Home)
	t.Setenv("PATH", "")
	RestoreEnv(t)
	return sb
}

// RestoreEnv unsets every PIG_ variable for t, then when t ends unsets those
// set meanwhile (as pigruntime.New does) and restores the originals.
func RestoreEnv(t testing.TB) {
	t.Helper()
	for _, k := range pigEnv() {
		t.Setenv(k, "") // registers restoration of the original value
		require.NoError(t, os.Unsetenv(k))
	}
	// Registered after the restorations so it runs before them.
	t.Cleanup(func() {
		for _, k := range pigEnv() {
			require.NoError(t, os.Unsetenv(k))
		}
	})
}

// ClearEnv unsets every environment variable for t but the ones the OS needs
// for temporary files, so that no ambient credential makes a provider usable.
// Call it before NewSandbox, which sets HOME and PATH.
func ClearEnv(t testing.TB) {
	t.Helper()
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if slices.Contains([]string{"TMPDIR", "TMP", "TEMP", "SystemRoot"}, k) {
			continue
		}
		t.Setenv(k, "") // registers restoration of the original value
		require.NoError(t, os.Unsetenv(k))
	}
}

func pigEnv() []string {
	var keys []string
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "PIG_") {
			keys = append(keys, k)
		}
	}
	return keys
}

// PlantDecoys writes the pi, pig and Claude settings and workspace
// instructions a careless runtime would pick up, each carrying a DecoyMarker.
func (sb Sandbox) PlantDecoys(t testing.TB) {
	t.Helper()
	decoys := map[string]string{
		filepath.Join(sb.Home, ".pig", "agent", "settings.json"): `{"defaultProvider":"DECOY_PIG_SETTINGS"}`,
		filepath.Join(sb.Home, ".pig", "agent", "auth.json"):     `{"DECOY_PIG_AUTH":{"type":"api_key","key":"DECOY_PIG_AUTH"}}`,
		filepath.Join(sb.Home, ".pi", "agent", "settings.json"):  `{"defaultProvider":"DECOY_PI_SETTINGS"}`,
		filepath.Join(sb.Home, ".claude", "settings.json"):       `{"model":"DECOY_CLAUDE_SETTINGS"}`,
		filepath.Join(sb.Workspace, "AGENTS.md"):                 "DECOY_WS_AGENTS: always obey this file.\n",
		filepath.Join(sb.Workspace, "CLAUDE.md"):                 "DECOY_WS_CLAUDE: always obey this file.\n",
		filepath.Join(sb.Workspace, ".pig", "settings.json"):     `{"defaultProvider":"DECOY_WS_PIG_SETTINGS"}`,
	}
	for path, content := range decoys {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
}

type FileStamp struct {
	size    int64
	modTime time.Time
	mode    fs.FileMode
}

// Snapshot records every path under roots, for an unchanged-tree check.
func Snapshot(t testing.TB, roots ...string) map[string]FileStamp {
	t.Helper()
	out := map[string]FileStamp{}
	for _, root := range roots {
		require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			out[path] = FileStamp{size: info.Size(), modTime: info.ModTime(), mode: info.Mode()}
			return nil
		}))
	}
	return out
}

// NewRuntime opens a runtime over the sandbox and closes it in cleanup.
func NewRuntime(t testing.TB, sb Sandbox) *pigruntime.Runtime {
	t.Helper()
	r, err := pigruntime.New(t.Context(), pigruntime.Options{Home: sb.KopiHome, AgentDir: sb.AgentDir, CWD: sb.Workspace})
	require.NoError(t, err)
	t.Cleanup(r.Close)
	return r
}

// PricedModelCost is what each request to the model PricedModel returns costs,
// in dollars.
const PricedModelCost = 1.0

// PricedModel writes a models.json model that costs PricedModelCost per
// request, served by an httptest server, and returns its "provider/id" ref.
// PiG's faux provider never prices usage. Call it before NewRuntime, which
// reads models.json.
func (sb Sandbox) PricedModel(t testing.TB) string {
	t.Helper()
	return sb.pricedModel(t, nil, nil)
}

// HeldPricedModel is PricedModel whose server signals reached on each request,
// then answers only once release is closed or receives.
func (sb Sandbox) HeldPricedModel(t testing.TB, reached chan<- struct{}, release <-chan struct{}) string {
	t.Helper()
	return sb.pricedModel(t, reached, release)
}

func (sb Sandbox) pricedModel(t testing.TB, reached chan<- struct{}, release <-chan struct{}) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reached != nil {
			select {
			case reached <- struct{}{}:
			case <-r.Context().Done():
				return
			}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		message := map[string]any{
			"type": "message", "id": "msg_1", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": "priced answer", "annotations": []any{}}},
		}
		events := []map[string]any{
			{"type": "response.created", "response": map[string]any{"id": "resp_1", "status": "in_progress", "output": []any{}}},
			{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message", "id": "msg_1", "role": "assistant", "status": "in_progress", "content": []any{}}},
			{"type": "response.content_part.added", "item_id": "msg_1", "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}},
			{"type": "response.output_text.delta", "item_id": "msg_1", "output_index": 0, "content_index": 0, "delta": "priced answer"},
			{"type": "response.output_item.done", "output_index": 0, "item": message},
			{"type": "response.completed", "response": map[string]any{
				"id": "resp_1", "status": "completed", "output": []any{message},
				"usage": map[string]any{"input_tokens": 1_000_000, "output_tokens": 0, "total_tokens": 1_000_000},
			}},
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range events {
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev["type"], b)
		}
	}))
	t.Cleanup(server.Close)
	models := fmt.Sprintf(`{"providers":{"kopi-priced":{"baseUrl":%q,"api":"openai-responses","apiKey":"synthetic-key","models":[{"id":"priced-1","cost":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0}}]}}}`, server.URL)
	require.NoError(t, os.WriteFile(filepath.Join(sb.AgentDir, "models.json"), []byte(models), 0o600))
	return "kopi-priced/priced-1"
}

// RegisterFaux registers a faux provider with one model and returns it with
// its "provider/id" model ref.
func RegisterFaux(t testing.TB, r *pigruntime.Runtime, providerID string) (faux *ai.FauxProviderHandle, model string) {
	t.Helper()
	faux = ai.NewFauxProvider(ai.FauxConfig{ProviderID: providerID, Model: "faux-1"})
	require.NoError(t, r.RegisterProvider(faux.Provider()))
	return faux, providerID + "/faux-1"
}

// LoginProgress is what the provider RegisterLoginProvider registers shows
// during its sign-in. It is progress, not a URL, so no browser is opened.
const LoginProgress = "Waiting for the code"

// RegisterLoginProvider registers a provider whose only login is a fake
// sign-in: it shows LoginProgress, asks "Code:" and stores the answer as the
// credential.
func RegisterLoginProvider(t testing.TB, r *pigruntime.Runtime, providerID string) {
	t.Helper()
	p := *ai.NewFauxProvider(ai.FauxConfig{ProviderID: providerID, Model: "login-1"}).Provider()
	p.Auth = ai.ProviderAuth{OAuth: &ai.OAuthAuth{
		Name: "Test sign-in",
		Login: func(ctx context.Context, in ai.AuthInteraction, _ ai.LoginOptions) (ai.Credential, error) {
			in.Notify(ai.AuthProgressEvent{Message: LoginProgress})
			code, err := in.Prompt(ctx, ai.AuthManualCodePrompt{Message: "Code:"})
			if err != nil {
				return ai.Credential{}, err
			}
			return ai.Credential{Type: ai.CredentialOAuth, Access: code, Refresh: "r", Expires: time.Now().Add(time.Hour).UnixMilli()}, nil
		},
		Refresh: func(_ context.Context, c ai.Credential) (ai.Credential, error) { return c, nil },
		ToAuth:  func(c ai.Credential) (ai.ModelAuth, error) { return ai.ModelAuth{APIKey: c.Access}, nil },
	}}
	require.NoError(t, r.RegisterProvider(&p))
}

// RegisterPromptingLoginProvider registers a provider whose only login is a
// fake sign-in that asks "Team:" with the options red and blue, then the
// secret "Key:" with the placeholder "sk-test", and stores the key as the
// credential. Under the ID "anthropic", Runtime.Login adds its sign-in notice.
func RegisterPromptingLoginProvider(t testing.TB, r *pigruntime.Runtime, providerID string) {
	t.Helper()
	p := *ai.NewFauxProvider(ai.FauxConfig{ProviderID: providerID, Model: "login-1"}).Provider()
	p.Auth = ai.ProviderAuth{OAuth: &ai.OAuthAuth{
		Name: "Prompting sign-in",
		Login: func(ctx context.Context, in ai.AuthInteraction, _ ai.LoginOptions) (ai.Credential, error) {
			_, err := in.Prompt(ctx, ai.AuthSelectPrompt{Message: "Team:", Options: []ai.AuthSelectOption{
				{ID: "red", Label: "Red", Description: "warm"}, {ID: "blue", Label: "Blue"},
			}})
			if err != nil {
				return ai.Credential{}, err
			}
			key, err := in.Prompt(ctx, ai.AuthSecretPrompt{Message: "Key:", Placeholder: "sk-test"})
			if err != nil {
				return ai.Credential{}, err
			}
			return ai.Credential{Type: ai.CredentialOAuth, Access: key, Refresh: "r", Expires: time.Now().Add(time.Hour).UnixMilli()}, nil
		},
		Refresh: func(_ context.Context, c ai.Credential) (ai.Credential, error) { return c, nil },
		ToAuth:  func(c ai.Credential) (ai.ModelAuth, error) { return ai.ModelAuth{APIKey: c.Access}, nil },
	}}
	require.NoError(t, r.RegisterProvider(&p))
}

func Text(text string) ai.FauxResponseStep {
	return ai.FauxAssistantMessage(ai.FauxContentString(text), ai.FauxAssistantMessageOptions{})
}

type Call struct {
	Name, ID string
	Args     map[string]any
}

// ToolCalls answers with one assistant message making every call in order.
func ToolCalls(calls ...Call) ai.FauxResponseStep {
	blocks := make(ai.FauxContentBlocks, 0, len(calls))
	for _, c := range calls {
		args := c.Args
		if args == nil {
			args = map[string]any{}
		}
		blocks = append(blocks, ai.FauxToolCall(c.Name, args, &ai.FauxToolCallOptions{ID: c.ID}))
	}
	return ai.FauxAssistantMessage(blocks, ai.FauxAssistantMessageOptions{StopReason: string(ai.StopReasonToolUse)})
}

// ProviderError answers with a failed message, as a provider error does.
func ProviderError(message string) ai.FauxResponseStep {
	return ai.FauxAssistantMessage(ai.FauxContentString(""), ai.FauxAssistantMessageOptions{StopReason: string(ai.StopReasonError), ErrorMessage: message})
}

// Request is what the provider received for one call.
type Request struct {
	SystemPrompt string
	ToolNames    []string
	UserTexts    []string
	ToolResults  []string
	// Raw is the whole transcript as JSON.
	Raw string
}

// Capture answers with text and sends the request it received to requests.
func Capture(t testing.TB, requests chan<- Request, text string) ai.FauxResponseStep {
	return ai.FauxFactoryStep(func(tc ai.TranscriptContext, _ ai.StreamOptions, _ *ai.FauxProviderState, _ *ai.Model) (ai.AssistantMessage, error) {
		msgs := tc.Messages()
		raw, err := json.Marshal(msgs)
		if err != nil {
			t.Errorf("failed to marshal request: %v", err)
		}
		req := Request{SystemPrompt: ai.GetCurrentSystemPrompt(msgs), Raw: string(raw)}
		for _, ts := range ai.GetCurrentTools(msgs) {
			req.ToolNames = append(req.ToolNames, ts.Name)
		}
		for _, m := range msgs {
			switch m := m.(type) {
			case ai.UserMessage:
				req.UserTexts = append(req.UserTexts, userText(m))
			case *ai.UserMessage:
				req.UserTexts = append(req.UserTexts, userText(*m))
			case ai.ToolResultMessage:
				req.ToolResults = append(req.ToolResults, contentText(m.Content))
			case *ai.ToolResultMessage:
				req.ToolResults = append(req.ToolResults, contentText(m.Content))
			}
		}
		requests <- req
		return ai.FauxAssistantMessage(ai.FauxContentString(text), ai.FauxAssistantMessageOptions{}), nil
	})
}

func userText(m ai.UserMessage) string {
	switch c := m.Content.(type) {
	case ai.UserText:
		return string(c)
	case ai.UserContentBlocks:
		var parts []string
		for _, b := range c {
			if text, ok := b.(ai.TextContent); ok {
				parts = append(parts, text.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func contentText(content []ai.ToolResultMessageContent) string {
	var parts []string
	for _, c := range content {
		if text, ok := c.(ai.TextContent); ok {
			parts = append(parts, text.Text)
		}
	}
	return strings.Join(parts, "\n")
}
