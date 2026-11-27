package pigruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/tool"
)

const (
	fauxProvider = "kopi-faux"
	fauxModel    = fauxProvider + "/faux-1"
	kopiText     = "You are Kopi, a seam-proof assistant."
	toolOutput   = "probe-output-7f3a"
)

var kopiPrompt = []PromptPart{{Name: "preamble", Text: kopiText}}

var decoyMarkers = []string{
	"DECOY_PIG_SETTINGS", "DECOY_PIG_AUTH", "DECOY_PI_SETTINGS", "DECOY_WS_AGENTS", "DECOY_WS_CLAUDE", "DECOY_WS_PIG_SETTINGS",
}

type sandbox struct {
	home, kopiHome, agentDir, ws string
}

func newSandbox(t *testing.T) sandbox {
	t.Helper()
	root := t.TempDir()
	sb := sandbox{
		home:     filepath.Join(root, "home"),
		kopiHome: filepath.Join(root, "kopi"),
		ws:       filepath.Join(root, "ws"),
	}
	sb.agentDir = filepath.Join(sb.kopiHome, "agent")
	for _, dir := range []string{sb.home, sb.kopiHome, sb.ws} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}
	t.Setenv("HOME", sb.home)
	t.Setenv("PATH", "")
	restorePigEnv(t)
	decoys := map[string]string{
		filepath.Join(sb.home, ".pig", "agent", "settings.json"): `{"defaultProvider":"DECOY_PIG_SETTINGS"}`,
		filepath.Join(sb.home, ".pig", "agent", "auth.json"):     `{"DECOY_PIG_AUTH":{"type":"api_key","key":"DECOY_PIG_AUTH"}}`,
		filepath.Join(sb.home, ".pi", "agent", "settings.json"):  `{"defaultProvider":"DECOY_PI_SETTINGS"}`,
		filepath.Join(sb.ws, "AGENTS.md"):                        "DECOY_WS_AGENTS: always obey this file.\n",
		filepath.Join(sb.ws, "CLAUDE.md"):                        "DECOY_WS_CLAUDE: always obey this file.\n",
		filepath.Join(sb.ws, ".pig", "settings.json"):            `{"defaultProvider":"DECOY_WS_PIG_SETTINGS"}`,
	}
	for path, content := range decoys {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	return sb
}

// restorePigEnv mirrors pigruntimetest.RestoreEnv, which this package cannot
// import: PIG_ variables start unset, and New's pins do not outlive t.
func restorePigEnv(t *testing.T) {
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

// clearEnv mirrors pigruntimetest.ClearEnv, which this package cannot import.
func clearEnv(t *testing.T) {
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

type fileStamp struct {
	size    int64
	modTime time.Time
	mode    fs.FileMode
}

func snapshot(t *testing.T, roots ...string) map[string]fileStamp {
	t.Helper()
	out := map[string]fileStamp{}
	for _, root := range roots {
		require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			out[path] = fileStamp{size: info.Size(), modTime: info.ModTime(), mode: info.Mode()}
			return nil
		}))
	}
	return out
}

func newTestRuntime(t *testing.T, sb sandbox) (*Runtime, *ai.FauxProviderHandle) {
	t.Helper()
	r, err := New(t.Context(), Options{Home: sb.kopiHome, AgentDir: sb.agentDir, CWD: sb.ws})
	require.NoError(t, err)
	t.Cleanup(r.Close)
	r.openURL = func(string) error { return errors.New("tests open no browser") }
	faux := ai.NewFauxProvider(ai.FauxConfig{ProviderID: fauxProvider, Model: "faux-1"})
	require.NoError(t, r.RegisterProvider(faux.Provider()))
	return r, faux
}

func openTestSession(t *testing.T, r *Runtime, tools ...tool.Tool) *Session {
	t.Helper()
	sess, err := r.Open(t.Context(), OpenOptions{Model: fauxModel, SystemPrompt: kopiPrompt, Tools: tools, Authority: policy.NewAuthority()})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, sess.Close(context.Background())) })
	return sess
}

func probeTool(run func(ctx context.Context, args json.RawMessage) (string, error)) tool.Tool {
	return tool.Tool{
		Name:        "Probe",
		Label:       "Probe",
		Description: "Returns a fixed probe string.",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
		Capability:  policy.Read,
		Run: func(ctx context.Context, args json.RawMessage, admit func(policy.Target) (policy.Mode, error)) (tool.Result, error) {
			if _, err := admit(policy.Target{}); err != nil {
				return tool.Result{}, err
			}
			out, err := run(ctx, args)
			return tool.Result{Text: out}, err
		},
	}
}

func callProbe() ai.FauxResponseStep {
	return ai.FauxAssistantMessage(
		ai.FauxContentBlocks{ai.FauxToolCall("Probe", map[string]any{}, &ai.FauxToolCallOptions{ID: "call-1"})},
		ai.FauxAssistantMessageOptions{StopReason: string(ai.StopReasonToolUse)},
	)
}

func toolSchemaNames(tools []ai.ToolSchema) []string {
	names := make([]string, 0, len(tools))
	for _, ts := range tools {
		names = append(names, ts.Name)
	}
	return names
}

func toolResultTexts(msgs []ai.Message) []string {
	var texts []string
	for _, m := range msgs {
		var content []ai.ToolResultMessageContent
		switch m := m.(type) {
		case ai.ToolResultMessage:
			content = m.Content
		case *ai.ToolResultMessage:
			content = m.Content
		}
		if len(content) > 0 {
			texts = append(texts, toolResultContentText(content))
		}
	}
	return texts
}

// captureRequest answers with text and hands the request's transcript to the test.
func captureRequest(requests chan<- []ai.Message, text string) ai.FauxResponseStep {
	return ai.FauxFactoryStep(func(tc ai.TranscriptContext, _ ai.StreamOptions, _ *ai.FauxProviderState, _ *ai.Model) (ai.AssistantMessage, error) {
		requests <- tc.Messages()
		return ai.FauxAssistantMessage(ai.FauxContentString(text), ai.FauxAssistantMessageOptions{}), nil
	})
}

func reply(text string) ai.FauxResponseStep {
	return ai.FauxAssistantMessage(ai.FauxContentString(text), ai.FauxAssistantMessageOptions{})
}

func TestSeamOpenStreamsFauxTurnWithZeroAmbient(t *testing.T) {
	sb := newSandbox(t)
	before := snapshot(t, sb.home, sb.ws)

	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{callProbe(), captureRequest(requests, "all done")})

	sess := openTestSession(t, r, probeTool(func(context.Context, json.RawMessage) (string, error) {
		return toolOutput, nil
	}))

	// Listeners run synchronously on PiG's side and have all run when Send
	// returns, so no lock or end-of-turn wait is needed.
	var (
		deltas     strings.Builder
		toolStarts []event.ToolStart
		toolEnds   []event.ToolEnd
	)
	unsubscribe := sess.Subscribe(func(ev event.Event) {
		switch ev := ev.(type) {
		case event.TextDelta:
			deltas.WriteString(ev.Text)
		case event.ToolStart:
			toolStarts = append(toolStarts, ev)
		case event.ToolEnd:
			toolEnds = append(toolEnds, ev)
		}
	})
	defer unsubscribe()

	res, err := sess.Send(t.Context(), Prompt{Text: "probe please"}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, event.Outcome{Status: event.Completed}, res.Outcome)

	assert.Equal(t, "all done", deltas.String())
	assert.Equal(t, []event.ToolStart{{CallID: "call-1", Name: "Probe", Args: "{}"}}, toolStarts)
	assert.Equal(t, []event.ToolEnd{{CallID: "call-1", Name: "Probe", Args: "{}", Output: toolOutput}}, toolEnds)

	assert.Equal(t, []string{"Probe"}, sess.inner.CallableToolNames())
	assert.Equal(t, []string{"Probe"}, sess.inner.ActiveToolNames())
	assert.Equal(t, 4, sess.MessageCount())

	// PiG v0.8.0 carries the system prompt and tool set as system messages in
	// the transcript, not in ai.Context.SystemPrompt/Tools.
	req := <-requests
	assert.Equal(t, []string{"Probe"}, toolSchemaNames(ai.GetCurrentTools(req)))
	assert.Equal(t, kopiText, ai.GetCurrentSystemPrompt(req))
	raw, err := json.Marshal(req)
	require.NoError(t, err)
	for _, marker := range decoyMarkers {
		assert.NotContains(t, string(raw), marker)
	}
	assert.Contains(t, toolResultTexts(req), toolOutput)

	assert.Len(t, sessionFiles(t, filepath.Join(sb.agentDir, "sessions")), 1)

	assert.Equal(t, before, snapshot(t, sb.home, sb.ws))
}

func writeAgentFile(t *testing.T, sb sandbox, name, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(sb.agentDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(sb.agentDir, name), []byte(content), 0o600))
}

func TestRuntimeOmitsPiGAttribution(t *testing.T) {
	sb := newSandbox(t)
	t.Setenv("PIG_TELEMETRY", "1")
	t.Setenv("PI_TELEMETRY", "1")
	requests := make(chan http.Header, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	writeAgentFile(t, sb, "models.json", fmt.Sprintf(`{"providers":{"openrouter":{"baseUrl":%q,"api":"openai-completions","apiKey":"synthetic-key","models":[{"id":"attribution-test"}]}}}`, server.URL))
	r, err := New(t.Context(), Options{Home: sb.kopiHome, AgentDir: sb.agentDir, CWD: sb.ws})
	require.NoError(t, err)
	t.Cleanup(r.Close)
	sess, err := r.Open(t.Context(), OpenOptions{
		Model: "openrouter/attribution-test", SystemPrompt: kopiPrompt,
		Tools: []tool.Tool{}, Authority: policy.NewAuthority(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, sess.Close(context.Background())) })

	res, err := sess.Send(t.Context(), Prompt{Text: "hello"}, nil, nil)
	require.NoError(t, err)
	assert.True(t, res.Sent)
	assert.Equal(t, event.Outcome{Status: event.Completed}, res.Outcome)

	headers := <-requests
	for _, name := range []string{"HTTP-Referer", "X-OpenRouter-Title", "X-OpenRouter-Categories"} {
		assert.NotContains(t, headers, http.CanonicalHeaderKey(name))
	}
}

func sessionFiles(t *testing.T, root string) []string {
	t.Helper()
	var jsonl []string
	require.NoError(t, filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(path, ".jsonl") {
			jsonl = append(jsonl, path)
		}
		return err
	}))
	return jsonl
}

// PiG v0.8.0 behaviour: an abort during a running tool cancels the tool's
// ctx, and PiG's Send returns a nil error whose last assistant message has
// StopReason "aborted" with ErrorMessage "context canceled".
// Kopi reports Interrupted because it requested the abort.
func TestSeamCancelDuringToolReportsInterrupted(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	faux.SetResponses([]ai.FauxResponseStep{callProbe(), reply("should not be requested")})

	started := make(chan struct{})
	sess, err := r.Open(t.Context(), OpenOptions{
		Model: fauxModel, SystemPrompt: kopiPrompt, Authority: policy.NewAuthority(),
		Tools: []tool.Tool{probeTool(func(ctx context.Context, _ json.RawMessage) (string, error) {
			close(started)
			<-ctx.Done()
			return "", ctx.Err()
		})},
	})
	require.NoError(t, err)

	type result struct {
		outcome event.Outcome
		err     error
	}
	turnCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan result, 1)
	go func() {
		res, err := sess.Send(turnCtx, Prompt{Text: "block"}, nil, nil)
		done <- result{res.Outcome, err}
	}()
	<-started
	cancel()
	res := <-done

	require.NoError(t, res.err)
	assert.Equal(t, event.Outcome{Status: event.Interrupted}, res.outcome)
	// The aborted assistant message is synthesized by PiG; no second request is made.
	assert.Equal(t, 1, faux.CallCount())
	msgs := sess.inner.Messages()
	last := msgs[len(msgs)-1]
	require.NotNil(t, last.Assistant)
	assert.Equal(t, ai.StopReasonAborted, last.Assistant.StopReason)
	assert.Equal(t, context.Canceled.Error(), last.Assistant.ErrorMessage)
	// user, tool call, tool result and PiG's synthesized aborted message
	assert.Equal(t, 4, sess.MessageCount())

	closeCtx, closeCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer closeCancel()
	assert.NoError(t, sess.Close(closeCtx))

	// The partial turn is persisted.
	resumed, err := r.Open(t.Context(), OpenOptions{Model: fauxModel, ResumeID: sess.ID(), Authority: policy.NewAuthority()})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, resumed.Close(context.Background())) })
	assert.Equal(t, 4, resumed.MessageCount())
}

// Hold asynchronous cancellation forwarding until the loop has ended, so only
// Send's synchronous event listener can prevent the second provider request.
func TestCancelDuringToolAbortsBeforeCancellationForwarding(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	faux.SetResponses([]ai.FauxResponseStep{callProbe(), reply("should not be requested")})

	started, release := make(chan struct{}), make(chan struct{})
	releaseProbe := sync.OnceFunc(func() { close(release) })
	sess := openTestSession(t, r, probeTool(func(context.Context, json.RawMessage) (string, error) {
		close(started)
		<-release
		return toolOutput, nil
	}))
	runEnded := make(chan struct{}, 1)
	t.Cleanup(sess.inner.Subscribe(func(ev agent.AgentEvent) {
		if _, ok := ev.(agent.AgentEndEvent); ok {
			runEnded <- struct{}{}
		}
	}))
	forward, cancel := context.WithCancel(t.Context())
	ctx := &gatedAbortContext{Context: context.WithoutCancel(t.Context()), forward: forward, started: make(chan struct{}), release: make(chan struct{})}
	releaseForwarding := sync.OnceFunc(func() { close(ctx.release) })
	type result struct {
		res SendResult
		err error
	}
	done := make(chan result, 1)
	joined := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		releaseProbe()
		releaseForwarding()
		<-joined
	})
	go func() {
		defer close(joined)
		res, err := sess.Send(ctx, Prompt{Text: "block"}, nil, nil)
		done <- result{res, err}
	}()
	<-started
	cancel()
	<-ctx.started
	releaseProbe()
	<-runEnded
	require.Equal(t, 1, faux.CallCount())

	releaseForwarding()
	res := <-done
	<-joined
	require.NoError(t, res.err)
	assert.Equal(t, event.Outcome{Status: event.Interrupted}, res.res.Outcome)
}

func TestCancelledBeforeSendInterruptsWithoutProviderCall(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	faux.SetResponses([]ai.FauxResponseStep{reply("after the cancelled turn")})
	sess := openTestSession(t, r)

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	res, err := sess.Send(cancelled, Prompt{Text: "cancelled before start"}, nil, nil)
	require.NoError(t, err)
	assert.False(t, res.Sent)
	assert.Empty(t, res.Undelivered)
	assert.Equal(t, event.Outcome{Status: event.Interrupted}, res.Outcome)
	assert.Zero(t, faux.CallCount())
	assert.Zero(t, sess.MessageCount())

	res, err = sess.Send(t.Context(), Prompt{Text: "next turn"}, nil, nil)
	require.NoError(t, err)
	assert.True(t, res.Sent)
	assert.Equal(t, event.Outcome{Status: event.Completed}, res.Outcome)
	assert.Equal(t, 1, faux.CallCount())
	assert.Equal(t, 2, sess.MessageCount())
}

// A cancel that lands after Send's entry check but before PiG's run is busy
// is dropped by PiG; cancelling from a listener that runs ahead of Send's
// run-start listener reproduces that window deterministically.
func TestCancelBeforeRunStartIsReissued(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	faux.SetResponses([]ai.FauxResponseStep{reply("should not be requested")})
	sess := openTestSession(t, r)
	turnCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	unsubscribe := sess.inner.Subscribe(func(ev agent.AgentEvent) {
		if _, ok := ev.(agent.AgentStartEvent); ok {
			cancel()
		}
	})
	defer unsubscribe()

	res, err := sess.Send(turnCtx, Prompt{Text: "abort me"}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, event.Outcome{Status: event.Interrupted}, res.Outcome)
	assert.Zero(t, faux.CallCount())
}

// The cancel lands at agent_end, while PiG is still finishing the turn, so its
// abort runs concurrently with Send's return; Send waits for it, and the next
// turn on a fresh ctx is unaffected.
func TestCancelRacingSendReturnSparesNextTurn(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	faux.SetResponses([]ai.FauxResponseStep{reply("first"), reply("second")})
	sess := openTestSession(t, r)
	turnCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	unsubscribe := sess.inner.Subscribe(func(ev agent.AgentEvent) {
		if _, ok := ev.(agent.AgentEndEvent); ok {
			cancel()
		}
	})
	_, err := sess.Send(turnCtx, Prompt{Text: "first turn"}, nil, nil)
	require.NoError(t, err)
	unsubscribe()

	res, err := sess.Send(t.Context(), Prompt{Text: "second turn"}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, event.Outcome{Status: event.Completed}, res.Outcome)
	assert.Equal(t, 2, faux.CallCount())
}

// A cancel at agent_end lands after PiG has finished the turn, so the abort
// hook fires for a turn that already completed.
func TestCancelAfterTurnFinishedReportsCompleted(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	faux.SetResponses([]ai.FauxResponseStep{reply("done")})
	sess := openTestSession(t, r)
	turnCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	unsubscribe := sess.inner.Subscribe(func(ev agent.AgentEvent) {
		if _, ok := ev.(agent.AgentEndEvent); ok {
			cancel()
		}
	})
	defer unsubscribe()

	res, err := sess.Send(turnCtx, Prompt{Text: "finish me"}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, event.Outcome{Status: event.Completed}, res.Outcome)
	assert.Equal(t, 1, faux.CallCount())
	assert.Equal(t, 2, sess.MessageCount())
}

func TestAbortOnEvent(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cancelled bool
		evs       []agent.AgentEvent
		want      int
	}{
		{name: "agent start with ctx cancelled", cancelled: true, evs: []agent.AgentEvent{agent.AgentStartEvent{}}, want: 1},
		{name: "agent start with ctx live", evs: []agent.AgentEvent{agent.AgentStartEvent{}}},
		{name: "tool end with ctx live", evs: []agent.AgentEvent{agent.ToolExecutionEndEvent{}}},
		{name: "two other events with ctx cancelled", cancelled: true, evs: []agent.AgentEvent{agent.ToolExecutionEndEvent{}, agent.TurnEndEvent{}}, want: 2},
		{name: "other event then agent start with ctx cancelled", cancelled: true, evs: []agent.AgentEvent{agent.TurnStartEvent{}, agent.AgentStartEvent{}}, want: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.cancelled {
				cancel()
			}
			calls := 0
			listen := abortOnEvent(ctx, func() { calls++ })
			for _, ev := range tc.evs {
				listen(ev)
			}
			assert.Equal(t, tc.want, calls)
		})
	}
}

// PiG v0.8.0 behaviour, recorded for the login/diagnostic step: a native
// provider with API-key auth reports a check with an empty Source, and a
// built-in OAuth provider without a credential reports (nil, nil).
func TestSeamCheckAuthOnFauxProvider(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)

	check, err := r.svcs.ModelRuntime().CheckAuth(t.Context(), fauxProvider)
	t.Logf("CheckAuth(%s): check=%+v err=%v", fauxProvider, check, err)
	require.NoError(t, err)
	require.NotNil(t, check)

	check, err = r.svcs.ModelRuntime().CheckAuth(t.Context(), "openai-codex")
	t.Logf("CheckAuth(openai-codex): check=%+v err=%v", check, err)
	require.NoError(t, err)
	assert.Nil(t, check)
}

func TestOpenUnknownModelFails(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	_, err := r.Open(t.Context(), OpenOptions{Model: fauxProvider + "/missing", Authority: policy.NewAuthority()})
	assert.ErrorIs(t, err, ErrUnknownModel)
}

func TestOpenWithoutCredentialFails(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	models := r.svcs.ModelRuntime().GetAllModels("openai-codex")
	require.NotEmpty(t, models)
	model, ok := models[0].(*ai.Model)
	require.True(t, ok)

	_, err := r.Open(t.Context(), OpenOptions{Model: "openai-codex/" + model.ID, Authority: policy.NewAuthority()})
	require.ErrorIs(t, err, ErrNoCredential)
	assert.Contains(t, err.Error(), "openai-codex")
}

func TestDefaultModelChoosesProviderWithCredential(t *testing.T) {
	clearEnv(t)
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	before := snapshot(t, sb.home, sb.kopiHome, sb.ws)

	model, err := r.DefaultModel(t.Context())

	require.NoError(t, err)
	assert.Equal(t, fauxModel, model)
	assert.Equal(t, before, snapshot(t, sb.home, sb.kopiHome, sb.ws))
}

func TestDefaultModelWithoutCredentialFails(t *testing.T) {
	clearEnv(t)
	sb := newSandbox(t)
	r, err := New(t.Context(), Options{Home: sb.kopiHome, AgentDir: sb.agentDir, CWD: sb.ws})
	require.NoError(t, err)
	t.Cleanup(r.Close)

	_, err = r.DefaultModel(t.Context())

	require.ErrorIs(t, err, ErrNoCredential)
}

func TestOpenWorksAfterDefaultModel(t *testing.T) {
	clearEnv(t)
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)

	model, err := r.DefaultModel(t.Context())
	require.NoError(t, err)

	s, err := r.Open(t.Context(), OpenOptions{Model: model, Authority: policy.NewAuthority()})
	require.NoError(t, err)
	require.NoError(t, s.Close(t.Context()))
}

func TestOpenRejectsResumeWithContinue(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	_, err := r.Open(t.Context(), OpenOptions{Model: fauxModel, ResumeID: "x", Continue: true, Authority: policy.NewAuthority()})
	assert.Error(t, err)
}

func TestOpenRunsSandboxedToolWithoutApproval(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{callTool("Exec", "call-1"), captureRequest(requests, "ran")})

	var ran atomic.Int32
	exec := execTool(&ran)
	exec.Sandboxed = true
	authority := policy.NewAuthority()
	authority.EnterExecute()
	sess, err := r.Open(t.Context(), OpenOptions{Model: fauxModel, Tools: []tool.Tool{exec}, Authority: authority})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, sess.Close(context.Background())) })

	_, err = sess.Send(t.Context(), Prompt{Text: "run it"}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, execOutput, lastToolResult(t, <-requests))
	assert.Equal(t, int32(1), ran.Load())
}

func TestSessionsListsOnlyKopiAgentDir(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	faux.SetResponses([]ai.FauxResponseStep{reply("hello back")})
	sess := openTestSession(t, r)
	_, err := sess.Send(t.Context(), Prompt{Text: "hello kopi"}, nil, nil)
	require.NoError(t, err)

	// A decoy session in PiG's default tree, in the same per-workspace
	// directory layout, must never be listed.
	kopiFile := sess.inner.Path()
	content, err := os.ReadFile(kopiFile)
	require.NoError(t, err)
	decoy := filepath.Join(sb.home, ".pig", "agent", "sessions", filepath.Base(filepath.Dir(kopiFile)), "decoy.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(decoy), 0o755))
	require.NoError(t, os.WriteFile(decoy, []byte(strings.ReplaceAll(string(content), sess.ID(), "decoy-session")), 0o600))

	infos, err := r.Sessions()
	require.NoError(t, err)
	require.Len(t, infos, 1)
	assert.Equal(t, sess.ID(), infos[0].ID)
	assert.Equal(t, "hello kopi", infos[0].FirstMessage)
	assert.False(t, infos[0].Modified.IsZero())
}

// sendOnce opens a session on r, runs one turn and closes it.
func sendOnce(t *testing.T, r *Runtime, faux *ai.FauxProviderHandle, prompt, answer string) *Session {
	t.Helper()
	faux.SetResponses([]ai.FauxResponseStep{reply(answer)})
	sess, err := r.Open(t.Context(), OpenOptions{Model: fauxModel, SystemPrompt: kopiPrompt, Authority: policy.NewAuthority()})
	require.NoError(t, err)
	_, err = sess.Send(t.Context(), Prompt{Text: prompt}, nil, nil)
	require.NoError(t, err)
	require.NoError(t, sess.Close(t.Context()))
	return sess
}

func TestResumeFromFreshRuntimeRestoresConversation(t *testing.T) {
	sb := newSandbox(t)
	first, faux := newTestRuntime(t, sb)
	saved := sendOnce(t, first, faux, "first question", "first answer")

	// The resumed session must use the model passed to Open, not the saved one.
	fresh, savedFaux := newTestRuntime(t, sb)
	other := ai.NewFauxProvider(ai.FauxConfig{ProviderID: "kopi-faux-b", Model: "faux-1"})
	require.NoError(t, fresh.RegisterProvider(other.Provider()))
	requests := make(chan []ai.Message, 1)
	other.SetResponses([]ai.FauxResponseStep{captureRequest(requests, "second answer")})

	sess, err := fresh.Open(t.Context(), OpenOptions{Model: "kopi-faux-b/faux-1", ResumeID: saved.ID(), SystemPrompt: kopiPrompt, Authority: policy.NewAuthority()})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, sess.Close(context.Background())) })
	assert.Equal(t, saved.ID(), sess.ID())
	assert.Equal(t, 2, sess.MessageCount())

	res, err := sess.Send(t.Context(), Prompt{Text: "second question"}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, event.Completed, res.Outcome.Status)
	raw, err := json.Marshal(<-requests)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "first question")
	assert.Contains(t, string(raw), "first answer")
	assert.Zero(t, savedFaux.CallCount())
	assert.Equal(t, 4, sess.MessageCount())
}

func TestContinueResumesMostRecentSession(t *testing.T) {
	sb := newSandbox(t)
	first, faux := newTestRuntime(t, sb)
	older := sendOnce(t, first, faux, "older question", "older answer")
	newer := sendOnce(t, first, faux, "newer question", "newer answer")
	// PiG picks the most recent by mtime; pin the order instead of relying on clock resolution.
	past := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(older.inner.Path(), past, past))

	fresh, _ := newTestRuntime(t, sb)
	sess, err := fresh.Open(t.Context(), OpenOptions{Model: fauxModel, Continue: true, Authority: policy.NewAuthority()})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, sess.Close(context.Background())) })
	assert.Equal(t, newer.ID(), sess.ID())
	assert.Equal(t, 2, sess.MessageCount())
}

func TestOpenMissingSessionFails(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	for _, tc := range []struct {
		name string
		opts OpenOptions
	}{
		{name: "continue without sessions", opts: OpenOptions{Continue: true}},
		{name: "unknown resume id", opts: OpenOptions{ResumeID: "no-such-session"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.Model = fauxModel
			tc.opts.Authority = policy.NewAuthority()
			_, err := r.Open(t.Context(), tc.opts)
			assert.ErrorIs(t, err, ErrSessionNotFound)
		})
	}
}

func TestCheckReportsWhatOpenWould(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	models := r.svcs.ModelRuntime().GetAllModels("openai-codex")
	require.NotEmpty(t, models)
	codex, ok := models[0].(*ai.Model)
	require.True(t, ok)

	for _, tc := range []struct {
		name     string
		opts     OpenOptions
		sentinel error
	}{
		{name: "unknown model", opts: OpenOptions{Model: fauxProvider + "/missing"}, sentinel: ErrUnknownModel},
		{name: "missing credential", opts: OpenOptions{Model: "openai-codex/" + codex.ID}, sentinel: ErrNoCredential},
		{name: "missing session to resume", opts: OpenOptions{Model: fauxModel, ResumeID: "no-such-session"}, sentinel: ErrSessionNotFound},
		{name: "nothing to continue", opts: OpenOptions{Model: fauxModel, Continue: true}, sentinel: ErrSessionNotFound},
		{name: "resume with continue", opts: OpenOptions{Model: fauxModel, ResumeID: "x", Continue: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.Authority = policy.NewAuthority()
			checkErr := r.Check(t.Context(), tc.opts)
			_, openErr := r.Open(t.Context(), tc.opts)
			require.Error(t, checkErr)
			require.Error(t, openErr)
			if tc.sentinel != nil {
				assert.ErrorIs(t, checkErr, tc.sentinel)
				assert.ErrorIs(t, openErr, tc.sentinel)
			}
			assert.Equal(t, openErr.Error(), checkErr.Error())
		})
	}

	t.Run("valid request", func(t *testing.T) {
		require.NoError(t, r.Check(t.Context(), OpenOptions{Model: fauxModel, Authority: policy.NewAuthority()}))
		infos, err := r.Sessions()
		require.NoError(t, err)
		assert.Empty(t, infos)
	})
}

func TestCloseReportsJoinTimeout(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	faux.SetResponses([]ai.FauxResponseStep{callProbe(), reply("after release")})

	started := make(chan struct{})
	release := make(chan struct{})
	sess, err := r.Open(t.Context(), OpenOptions{
		Model: fauxModel, Authority: policy.NewAuthority(),
		Tools: []tool.Tool{probeTool(func(context.Context, json.RawMessage) (string, error) {
			close(started)
			<-release
			return toolOutput, nil
		})},
	})
	require.NoError(t, err)
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		_, _ = sess.Send(context.Background(), Prompt{Text: "block"}, nil, nil)
	}()
	// Runs before the runtime's Close cleanup.
	t.Cleanup(func() {
		close(release)
		<-sent
	})
	<-started

	expired, cancel := context.WithCancel(t.Context())
	cancel()
	assert.ErrorIs(t, sess.Close(expired), ErrJoinTimeout)
}

func TestSendOutcome(t *testing.T) {
	for _, tc := range []struct {
		name string
		step ai.FauxResponseStep
		want event.Outcome
	}{
		{name: "completed", step: reply("fine"), want: event.Outcome{Status: event.Completed}},
		{
			name: "failed",
			step: ai.FauxAssistantMessage(ai.FauxContentString(""), ai.FauxAssistantMessageOptions{StopReason: "error", ErrorMessage: "invalid request: bad thing"}),
			want: event.Outcome{Status: event.Failed, Message: "invalid request: bad thing"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sb := newSandbox(t)
			r, faux := newTestRuntime(t, sb)
			faux.SetResponses([]ai.FauxResponseStep{tc.step})
			sess := openTestSession(t, r)
			res, err := sess.Send(t.Context(), Prompt{Text: "go"}, nil, nil)
			require.NoError(t, err)
			assert.Equal(t, tc.want, res.Outcome)
		})
	}
}

func TestOpenSendsPromptPartsInOrder(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{captureRequest(requests, "done")})
	parts := []PromptPart{
		{Name: "preamble", Text: kopiText},
		{Name: "instructions /some/dir", Text: "Instructions from /some/dir/AGENTS.md:\nBe brief."},
		{Name: "mcp-servers", Text: "MCP server s tools: mcp__s__t."},
	}
	sess, err := r.Open(t.Context(), OpenOptions{Model: fauxModel, SystemPrompt: parts, Authority: policy.NewAuthority()})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, sess.Close(context.Background())) })

	_, err = sess.Send(t.Context(), Prompt{Text: "hello"}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, kopiText+"\n\nInstructions from /some/dir/AGENTS.md:\nBe brief.\n\nMCP server s tools: mcp__s__t.", ai.GetCurrentSystemPrompt(<-requests))
}

func TestResumeRecordsOnlyChangedPromptPart(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	parts := []PromptPart{
		{Name: "preamble", Text: kopiText},
		{Name: "instructions /some/dir", Text: "Be brief."},
	}
	faux.SetResponses([]ai.FauxResponseStep{reply("first answer")})
	saved, err := r.Open(t.Context(), OpenOptions{Model: fauxModel, SystemPrompt: parts, Authority: policy.NewAuthority()})
	require.NoError(t, err)
	_, err = saved.Send(t.Context(), Prompt{Text: "first question"}, nil, nil)
	require.NoError(t, err)
	require.NoError(t, saved.Close(t.Context()))

	changed := []PromptPart{parts[0], {Name: "instructions /some/dir", Text: "Be thorough."}}
	requests := make(chan []ai.Message, 1)
	faux.SetResponses([]ai.FauxResponseStep{captureRequest(requests, "second answer")})
	sess, err := r.Open(t.Context(), OpenOptions{Model: fauxModel, ResumeID: saved.ID(), SystemPrompt: changed, Authority: policy.NewAuthority()})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, sess.Close(context.Background())) })
	_, err = sess.Send(t.Context(), Prompt{Text: "second question"}, nil, nil)
	require.NoError(t, err)

	req := <-requests
	var sections [][]string
	for _, m := range req {
		if sys, ok := m.(ai.SystemMessage); ok && len(sys.Sections) > 0 {
			var texts []string
			for _, s := range sys.Sections {
				texts = append(texts, s.Name+"="+*s.Value)
			}
			sections = append(sections, texts)
		}
	}
	assert.Equal(t, [][]string{
		{"preamble=" + kopiText, "instructions /some/dir=Be brief."},
		{"instructions /some/dir=Be thorough."},
	}, sections, "the resume records only the changed part")
	assert.Equal(t, kopiText+"\n\nBe thorough.", ai.GetCurrentSystemPrompt(req))
}

func TestResumePromptSectionsPreserveOrderAndHistory(t *testing.T) {
	preamble := PromptPart{Name: "preamble", Text: kopiText}
	global := PromptPart{Name: "instructions /home", Text: "Global instructions."}
	project := PromptPart{Name: "instructions /project", Text: "Project instructions."}
	mcp := PromptPart{Name: "mcp-servers", Text: "MCP server tools."}
	for _, tc := range []struct {
		name    string
		saved   []PromptPart
		desired []PromptPart
		patches ai.OrderedSections
		repair  ai.OrderedSections
	}{
		{
			name:  "add project before MCP",
			saved: []PromptPart{preamble, mcp}, desired: []PromptPart{preamble, project, mcp},
			patches: ai.OrderedSections{{Name: project.Name, Value: &project.Text}},
			repair:  ai.OrderedSections{{Name: mcp.Name}, {Name: project.Name}},
		},
		{
			name:  "add global before project",
			saved: []PromptPart{preamble, project, mcp}, desired: []PromptPart{preamble, global, project, mcp},
			patches: ai.OrderedSections{{Name: global.Name, Value: &global.Text}},
			repair:  ai.OrderedSections{{Name: project.Name}, {Name: mcp.Name}, {Name: global.Name}},
		},
		{
			name:  "omit instruction",
			saved: []PromptPart{preamble, project, mcp}, desired: []PromptPart{preamble, mcp},
			patches: ai.OrderedSections{{Name: project.Name}},
		},
		{
			name:  "unchanged",
			saved: []PromptPart{preamble, project, mcp}, desired: []PromptPart{preamble, project, mcp},
		},
		{
			name:  "remove all",
			saved: []PromptPart{preamble, project}, desired: []PromptPart{},
			patches: ai.OrderedSections{{Name: preamble.Name}, {Name: project.Name}},
		},
		{name: "explicit empty", saved: []PromptPart{}, desired: []PromptPart{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sb := newSandbox(t)
			r, faux := newTestRuntime(t, sb)
			faux.SetResponses([]ai.FauxResponseStep{reply("first answer")})
			saved, err := r.Open(t.Context(), OpenOptions{Model: fauxModel, SystemPrompt: tc.saved, Authority: policy.NewAuthority()})
			require.NoError(t, err)
			_, err = saved.Send(t.Context(), Prompt{Text: "first question"}, nil, nil)
			require.NoError(t, err)
			history := saved.inner.Agent().MessagesSnapshot()
			normal := reflect.ValueOf(saved.inner.Agent().PreparePromptHook()).Pointer()
			require.NoError(t, saved.Close(t.Context()))

			var desired ai.OrderedSections
			var texts []string
			for _, part := range tc.desired {
				desired = append(desired, ai.PromptSection{Name: part.Name, Value: &part.Text})
				texts = append(texts, part.Text)
			}
			// The second reopen must replay the persisted repair, not emit it again.
			for round := range 2 {
				requests := make(chan []ai.Message, 1)
				faux.SetResponses([]ai.FauxResponseStep{captureRequest(requests, "resumed answer")})
				sess, err := r.Open(t.Context(), OpenOptions{Model: fauxModel, ResumeID: saved.ID(), SystemPrompt: tc.desired, Authority: policy.NewAuthority()})
				require.NoError(t, err)
				_, err = sess.Send(t.Context(), Prompt{Text: "next question"}, nil, nil)
				require.NoError(t, err)
				req := <-requests
				assert.Equal(t, strings.Join(texts, "\n\n"), ai.GetCurrentSystemPrompt(req))
				if current := ai.GetCurrentSystemMessage(req); current != nil {
					assert.Equal(t, desired, current.Sections)
				} else {
					assert.Empty(t, desired)
				}
				after := sess.inner.Agent().MessagesSnapshot()
				require.GreaterOrEqual(t, len(after), len(history))
				assert.Equal(t, history, after[:len(history)], "historical prefix must not change")
				var updates []ai.OrderedSections
				for _, message := range after[len(history):] {
					if message.System != nil && len(message.System.Sections) > 0 {
						updates = append(updates, message.System.Sections)
					}
				}
				var want []ai.OrderedSections
				if round == 0 && len(tc.patches) > 0 {
					want = append(want, tc.patches)
					if len(tc.repair) > 0 {
						want = append(want, tc.repair, desired[1:])
					}
				}
				assert.Equal(t, want, updates)
				if round == 1 || len(tc.patches) == 0 {
					assert.Equal(t, normal, reflect.ValueOf(sess.inner.Agent().PreparePromptHook()).Pointer(), "reconciled transcripts retire the repair hook")
				}
				history = after
				require.NoError(t, sess.Close(t.Context()))
			}
		})
	}
}

func TestResumePromptRepairRetiresOnlyAfterCommit(t *testing.T) {
	for _, retry := range []string{"pre-cancelled", "preparation error", "unapplied preparation"} {
		for _, continuing := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/continue=%t", retry, continuing), func(t *testing.T) {
				sb := newSandbox(t)
				r, faux := newTestRuntime(t, sb)
				parts := []PromptPart{{Name: "preamble", Text: kopiText}, {Name: "mcp-servers", Text: "MCP tools."}}
				saved, err := r.Open(t.Context(), OpenOptions{Model: fauxModel, SystemPrompt: parts, Authority: policy.NewAuthority()})
				require.NoError(t, err)
				normal := reflect.ValueOf(saved.inner.Agent().PreparePromptHook()).Pointer()
				faux.SetResponses([]ai.FauxResponseStep{reply("saved")})
				_, err = saved.Send(t.Context(), Prompt{Text: "save"}, nil, nil)
				require.NoError(t, err)
				history := saved.inner.Agent().MessagesSnapshot()
				require.NoError(t, saved.Close(t.Context()))

				desired := []PromptPart{parts[0], {Name: "instructions /project", Text: "Project instructions."}, parts[1]}
				opts := OpenOptions{Model: fauxModel, SystemPrompt: desired, Authority: policy.NewAuthority(), Continue: continuing}
				if !continuing {
					opts.ResumeID = saved.ID()
				}
				sess, err := r.Open(t.Context(), opts)
				require.NoError(t, err)
				t.Cleanup(func() { assert.NoError(t, sess.Close(context.Background())) })
				a := sess.inner.Agent()
				wrapper := reflect.ValueOf(a.PreparePromptHook()).Pointer()
				require.NotEqual(t, normal, wrapper, "fresh sessions use PiG's hook, resumes install the repair")
				switch retry {
				case "pre-cancelled":
					ctx, cancel := context.WithCancel(t.Context())
					cancel()
					res, err := sess.Send(ctx, Prompt{Text: "cancelled"}, nil, nil)
					require.NoError(t, err)
					assert.False(t, res.Sent)
				case "preparation error":
					model := a.Model()
					a.SetModel(&ai.Model{})
					_, err := sess.Send(t.Context(), Prompt{Text: "invalid model"}, nil, nil)
					require.ErrorIs(t, err, agent.ErrNoModelSelected)
					a.SetModel(model)
				case "unapplied preparation":
					prepared, err := a.PreparePromptHook()(t.Context(), nil)
					require.NoError(t, err)
					require.NotEmpty(t, prepared)
				}
				assert.Equal(t, history, a.MessagesSnapshot())
				assert.Equal(t, wrapper, reflect.ValueOf(a.PreparePromptHook()).Pointer())

				requests := make(chan []ai.Message, 1)
				faux.SetResponses([]ai.FauxResponseStep{captureRequest(requests, "repaired")})
				_, err = sess.Send(t.Context(), Prompt{Text: "retry"}, nil, nil)
				require.NoError(t, err)
				assert.Equal(t, kopiText+"\n\nProject instructions.\n\nMCP tools.", ai.GetCurrentSystemPrompt(<-requests))
				assert.Equal(t, history, a.MessagesSnapshot()[:len(history)])
				assert.Equal(t, wrapper, reflect.ValueOf(a.PreparePromptHook()).Pointer(), "emitting a repair is not confirmation")

				faux.SetResponses([]ai.FauxResponseStep{captureRequest(requests, "confirmed")})
				_, err = sess.Send(t.Context(), Prompt{Text: "confirm"}, nil, nil)
				require.NoError(t, err)
				<-requests
				assert.Equal(t, normal, reflect.ValueOf(a.PreparePromptHook()).Pointer())

				updated := "Updated instructions."
				sess.inner.SetSystemPromptSections(ai.OrderedSections{{Name: "preamble", Value: &updated}})
				faux.SetResponses([]ai.FauxResponseStep{captureRequest(requests, "updated")})
				_, err = sess.Send(t.Context(), Prompt{Text: "update"}, nil, nil)
				require.NoError(t, err)
				assert.Equal(t, updated, ai.GetCurrentSystemPrompt(<-requests), "PiG's per-run updates survive retirement")
			})
		}
	}
}

func TestOpenEffortReachesProviderRequest(t *testing.T) {
	r, _ := newTestRuntime(t, newSandbox(t))
	thinker := ai.NewFauxProvider(ai.FauxConfig{ProviderID: "faux-thinker", Models: []ai.FauxModelDefinition{{ID: "thinker", Reasoning: true}}})
	require.NoError(t, r.RegisterProvider(thinker.Provider()))
	levels := make(chan ai.ThinkingLevel, 1)
	thinker.SetResponses([]ai.FauxResponseStep{
		ai.FauxFactoryStep(func(_ ai.TranscriptContext, o ai.StreamOptions, _ *ai.FauxProviderState, _ *ai.Model) (ai.AssistantMessage, error) {
			levels <- o.Thinking
			return ai.FauxAssistantMessage(ai.FauxContentString("ok"), ai.FauxAssistantMessageOptions{}), nil
		}),
	})
	sess, err := r.Open(t.Context(), OpenOptions{Model: "faux-thinker/thinker", Effort: LowEffort, SystemPrompt: kopiPrompt, Authority: policy.NewAuthority()})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, sess.Close(context.Background())) })
	_, err = sess.Send(t.Context(), Prompt{Text: "go"}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, ai.ThinkingLevelLow, <-levels)
}

func TestResumedEffortComesFromOpenNotTheSavedLevel(t *testing.T) {
	r, _ := newTestRuntime(t, newSandbox(t))
	provider, thinker := registerThinker(t, r)
	saved := openOn(t, r, thinker, HighEffort, "")
	provider.SetResponses([]ai.FauxResponseStep{reply("noted")})
	_, err := saved.Send(t.Context(), Prompt{Text: "hello"}, nil, nil)
	require.NoError(t, err)

	levels := make(chan ai.ThinkingLevel, 1)
	provider.SetResponses([]ai.FauxResponseStep{
		ai.FauxFactoryStep(func(_ ai.TranscriptContext, o ai.StreamOptions, _ *ai.FauxProviderState, _ *ai.Model) (ai.AssistantMessage, error) {
			levels <- o.Thinking
			return ai.FauxAssistantMessage(ai.FauxContentString("ok"), ai.FauxAssistantMessageOptions{}), nil
		}),
	})
	resumed := openOn(t, r, thinker, LowEffort, saved.ID())
	assert.Equal(t, "low", resumed.Effort())
	_, err = resumed.Send(t.Context(), Prompt{Text: "go"}, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, ai.ThinkingLevelLow, <-levels)

	assert.Equal(t, "medium", openOn(t, r, thinker, DefaultEffort, saved.ID()).Effort(), "DefaultEffort is medium, not the saved level")
}

func TestOpenWithParentSavesBesideParentUnlisted(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	parent := sendOnce(t, r, faux, "parent question", "parent answer")

	faux.SetResponses([]ai.FauxResponseStep{reply("child answer")})
	child, err := r.Open(t.Context(), OpenOptions{Model: fauxModel, Parent: parent, SystemPrompt: kopiPrompt, Authority: policy.NewAuthority()})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, child.Close(context.Background())) })
	_, err = child.Send(t.Context(), Prompt{Text: "child question"}, nil, nil)
	require.NoError(t, err)

	parentFile := parent.inner.Path()
	files, err := filepath.Glob(filepath.Join(filepath.Dir(parentFile), parent.ID(), "*.jsonl"))
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Equal(t, child.inner.Path(), files[0])

	infos, err := r.Sessions()
	require.NoError(t, err)
	require.Len(t, infos, 1)
	assert.Equal(t, parent.ID(), infos[0].ID)

	fresh, _ := newTestRuntime(t, sb)
	continued, err := fresh.Open(t.Context(), OpenOptions{Model: fauxModel, Continue: true, Authority: policy.NewAuthority()})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, continued.Close(context.Background())) })
	assert.Equal(t, parent.ID(), continued.ID())
}

func TestOpenRejectsParentWithResumeOrContinue(t *testing.T) {
	r, faux := newTestRuntime(t, newSandbox(t))
	parent := sendOnce(t, r, faux, "q", "a")
	for _, o := range []OpenOptions{{ResumeID: parent.ID()}, {Continue: true}} {
		o.Model, o.Parent, o.Authority = fauxModel, parent, policy.NewAuthority()
		assert.ErrorIs(t, r.Check(t.Context(), o), ErrParentResume)
		_, err := r.Open(t.Context(), o)
		assert.ErrorIs(t, err, ErrParentResume)
	}
	assert.NoError(t, r.Check(t.Context(), OpenOptions{Model: fauxModel, Parent: parent, Effort: MaxEffort, Authority: policy.NewAuthority()}))
}

func TestUnknownModelWithoutCredentialMakesNoRequest(t *testing.T) {
	clearEnv(t)
	sb := newSandbox(t)
	probe := newCatalogRuntime(t, sb, "http://127.0.0.1:1")
	s := newCatalogServer(t, bundledMistral(t, probe), http.StatusOK, "catalog-new-1")
	r := newCatalogRuntime(t, sb, s.URL)

	err := r.Check(t.Context(), OpenOptions{Model: catalogModel})

	require.ErrorIs(t, err, ErrNoCredential)
	assert.ErrorIs(t, err, ErrUnknownModel)
	assert.Empty(t, s.requests)
}

func TestUnknownProviderMakesNoRequest(t *testing.T) {
	_, s, r := catalogSetup(t, http.StatusOK, "catalog-new-1")

	err := r.Check(t.Context(), OpenOptions{Model: "nosuch/catalog-new-1"})

	require.ErrorIs(t, err, ErrUnknownModel)
	assert.Empty(t, s.requests)
}

func TestModelsFileEntryInheritsProviderAndCredential(t *testing.T) {
	sb := newSandbox(t)
	withMistralKey(t, sb)
	writeAgentFile(t, sb, "models.json", `{"providers":{"`+catalogProvider+`":{"models":[{"id":"brand-new"}]}}}`)
	s := newCatalogServer(t, &ai.Model{}, http.StatusOK)
	r := newCatalogRuntime(t, sb, s.URL)

	require.NoError(t, r.Check(t.Context(), OpenOptions{Model: catalogProvider + "/brand-new"}))
	assert.Empty(t, s.requests)
	declared := r.svcs.ModelRuntime().GetModel(catalogProvider, "brand-new")
	require.NotNil(t, declared)
	bundled := bundledMistral(t, r)
	assert.Equal(t, bundled.ProviderMeta.API, declared.ProviderMeta.API)
	assert.Equal(t, bundled.ProviderMeta.BaseURL, declared.ProviderMeta.BaseURL)
	assert.NotEmpty(t, declared.ProviderMeta.BaseURL)
}

func TestSetModelSwitchesSessionAndRejectsUnknown(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	otherModel := registerSecondFaux(t, r, "kopi-other")
	s := openTestSession(t, r)

	require.NoError(t, r.SetModel(t.Context(), s, otherModel))
	assert.Equal(t, otherModel, s.inner.Model().ProviderMeta.ProviderID+"/"+s.inner.Model().ID)

	err := r.SetModel(t.Context(), s, fauxProvider+"/missing")
	require.ErrorIs(t, err, ErrUnknownModel)
	assert.Equal(t, otherModel, s.inner.Model().ProviderMeta.ProviderID+"/"+s.inner.Model().ID)
}

// openOn opens a session on model, resuming resumeID when set.
func openOn(t *testing.T, r *Runtime, model string, effort Effort, resumeID string) *Session {
	t.Helper()
	s, err := r.Open(t.Context(), OpenOptions{Model: model, Effort: effort, ResumeID: resumeID, SystemPrompt: kopiPrompt, Authority: policy.NewAuthority()})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, s.Close(context.Background())) })
	return s
}

func registerThinker(t *testing.T, r *Runtime) (*ai.FauxProviderHandle, string) {
	t.Helper()
	thinker := ai.NewFauxProvider(ai.FauxConfig{ProviderID: "faux-thinker", Models: []ai.FauxModelDefinition{{ID: "thinker", Reasoning: true}}})
	require.NoError(t, r.RegisterProvider(thinker.Provider()))
	return thinker, "faux-thinker/thinker"
}

func TestSetModelKeepsTheEffortAskedFor(t *testing.T) {
	r, _ := newTestRuntime(t, newSandbox(t))
	_, thinker := registerThinker(t, r)
	for name, s := range map[string]*Session{
		"at open":   openOn(t, r, thinker, HighEffort, ""),
		"SetEffort": openOn(t, r, thinker, LowEffort, ""),
	} {
		t.Run(name, func(t *testing.T) {
			if name == "SetEffort" {
				require.NoError(t, s.SetEffort(HighEffort))
			}
			require.NoError(t, r.SetModel(t.Context(), s, fauxModel))
			assert.Empty(t, s.Effort())

			require.NoError(t, r.SetModel(t.Context(), s, thinker))
			assert.Equal(t, "high", s.Effort())
		})
	}
}

func TestSetModelReportsAFailedSaveAfterSwitching(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	thinker, thinkerRef := registerThinker(t, r)
	thinker.SetResponses([]ai.FauxResponseStep{reply("answer")})
	s := openOn(t, r, thinkerRef, HighEffort, "")
	_, err := s.Send(t.Context(), Prompt{Text: "question"}, nil, nil)
	require.NoError(t, err)
	require.NoError(t, r.SetModel(t.Context(), s, fauxModel))
	require.Empty(t, s.Effort())
	files := sessionFiles(t, sb.kopiHome)
	require.Len(t, files, 1)
	// The write is refused as the file is opened for appending.
	require.NoError(t, os.Chmod(files[0], 0o400))
	t.Cleanup(func() { _ = os.Chmod(files[0], 0o600) })
	if f, err := os.OpenFile(files[0], os.O_WRONLY|os.O_APPEND, 0); err == nil {
		_ = f.Close()
		t.Skip("file permissions do not stop this user from appending")
	}

	err = r.SetModel(t.Context(), s, thinkerRef)

	require.ErrorIs(t, err, ErrModelNotSaved)
	assert.Contains(t, err.Error(), thinkerRef)
	assert.Equal(t, thinkerRef, s.inner.Model().ProviderMeta.ProviderID+"/"+s.inner.Model().ID)
	assert.Equal(t, "high", s.Effort())
}

func TestSetModelToTheCurrentModelReportsAFailureAsIs(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	_, thinkerRef := registerThinker(t, r)
	s := openOn(t, r, thinkerRef, HighEffort, "")
	_, err := s.Send(t.Context(), Prompt{Text: "question"}, nil, nil)
	require.NoError(t, err)
	files := sessionFiles(t, sb.kopiHome)
	require.Len(t, files, 1)
	// The write is refused as the file is opened for appending.
	require.NoError(t, os.Chmod(files[0], 0o400))
	t.Cleanup(func() { _ = os.Chmod(files[0], 0o600) })
	if f, err := os.OpenFile(files[0], os.O_WRONLY|os.O_APPEND, 0); err == nil {
		_ = f.Close()
		t.Skip("file permissions do not stop this user from appending")
	}

	err = r.SetModel(t.Context(), s, thinkerRef)

	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrModelNotSaved)
}

func TestSetModelClampsAnUnaskedLevelAfterAFailedSave(t *testing.T) {
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	_, thinkerRef := registerThinker(t, r)
	s := openOn(t, r, fauxModel, DefaultEffort, "")
	_, err := s.Send(t.Context(), Prompt{Text: "question"}, nil, nil)
	require.NoError(t, err)
	files := sessionFiles(t, sb.kopiHome)
	require.Len(t, files, 1)
	// The write is refused as the file is opened for appending.
	require.NoError(t, os.Chmod(files[0], 0o400))
	t.Cleanup(func() { _ = os.Chmod(files[0], 0o600) })
	if f, err := os.OpenFile(files[0], os.O_WRONLY|os.O_APPEND, 0); err == nil {
		_ = f.Close()
		t.Skip("file permissions do not stop this user from appending")
	}

	err = r.SetModel(t.Context(), s, thinkerRef)

	require.ErrorIs(t, err, ErrModelNotSaved)
	assert.Equal(t, thinkerRef, s.inner.Model().ProviderMeta.ProviderID+"/"+s.inner.Model().ID)
	assert.Equal(t, "medium", s.Effort())
}

func TestSetModelGivesAnUnaskedSessionTheNewModelsDefault(t *testing.T) {
	r, _ := newTestRuntime(t, newSandbox(t))
	_, thinker := registerThinker(t, r)
	s := openOn(t, r, fauxModel, DefaultEffort, "")

	require.NoError(t, r.SetModel(t.Context(), s, thinker))

	assert.Equal(t, "medium", s.Effort())
	assert.Equal(t, openOn(t, r, thinker, DefaultEffort, "").Effort(), s.Effort())
}

func TestSetModelKeepsAResumedSessionsOpenedEffort(t *testing.T) {
	r, _ := newTestRuntime(t, newSandbox(t))
	provider, thinker := registerThinker(t, r)
	saved := openOn(t, r, thinker, LowEffort, "")
	require.NoError(t, saved.SetEffort(HighEffort))
	provider.SetResponses([]ai.FauxResponseStep{reply("noted")})
	_, err := saved.Send(t.Context(), Prompt{Text: "hello"}, nil, nil)
	require.NoError(t, err)

	resumed := openOn(t, r, thinker, LowEffort, saved.ID())
	require.NoError(t, r.SetModel(t.Context(), resumed, fauxModel))
	require.NoError(t, r.SetModel(t.Context(), resumed, thinker))
	assert.Equal(t, "low", resumed.Effort())

	continued, err := r.Open(t.Context(), OpenOptions{Model: thinker, Effort: LowEffort, Continue: true, SystemPrompt: kopiPrompt, Authority: policy.NewAuthority()})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, continued.Close(context.Background())) })
	require.NoError(t, r.SetModel(t.Context(), continued, fauxModel))
	require.NoError(t, r.SetModel(t.Context(), continued, thinker))
	assert.Equal(t, "low", continued.Effort())

	onPlain := openOn(t, r, fauxModel, DefaultEffort, saved.ID())
	require.NoError(t, r.SetModel(t.Context(), onPlain, thinker))
	assert.Equal(t, "medium", onPlain.Effort())

	onPlainLow := openOn(t, r, fauxModel, LowEffort, saved.ID())
	require.NoError(t, r.SetModel(t.Context(), onPlainLow, thinker))
	assert.Equal(t, "low", onPlainLow.Effort())
}

func TestModelsListsCredentialedProvidersSorted(t *testing.T) {
	clearEnv(t)
	sb := newSandbox(t)
	r, _ := newTestRuntime(t, sb)
	registerSecondFaux(t, r, "aaa-faux")

	models, err := r.Models(t.Context())

	require.NoError(t, err)
	assert.Equal(t, []Model{{Ref: "aaa-faux/faux-1", Name: "Faux Model"}, {Ref: fauxModel, Name: "Faux Model"}}, models)
}

func registerSecondFaux(t *testing.T, r *Runtime, id string) string {
	t.Helper()
	faux := ai.NewFauxProvider(ai.FauxConfig{ProviderID: id, Model: "faux-1"})
	require.NoError(t, r.RegisterProvider(faux.Provider()))
	return id + "/faux-1"
}
