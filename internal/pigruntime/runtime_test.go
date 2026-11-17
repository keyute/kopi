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
	kopiPrompt   = "You are Kopi, a seam-proof assistant."
	toolOutput   = "probe-output-7f3a"
)

var decoyMarkers = []string{
	"DECOY_PIG_SETTINGS", "DECOY_PIG_AUTH", "DECOY_PI_SETTINGS", "DECOY_WS_AGENTS", "DECOY_WS_PIG_SETTINGS",
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
		Run: func(ctx context.Context, args json.RawMessage, admit func(policy.Target) (policy.Mode, error)) (string, error) {
			if _, err := admit(policy.Target{}); err != nil {
				return "", err
			}
			return run(ctx, args)
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

	res, err := sess.Send(t.Context(), Prompt{Text: "probe please"}, nil)
	require.NoError(t, err)
	assert.Equal(t, event.Outcome{Status: event.Completed}, res.Outcome)

	assert.Equal(t, "all done", deltas.String())
	assert.Equal(t, []event.ToolStart{{CallID: "call-1", Name: "Probe", Args: "{}"}}, toolStarts)
	assert.Equal(t, []event.ToolEnd{{CallID: "call-1", Name: "Probe", Output: toolOutput}}, toolEnds)

	assert.Equal(t, []string{"Probe"}, sess.inner.CallableToolNames())
	assert.Equal(t, []string{"Probe"}, sess.inner.ActiveToolNames())
	assert.Equal(t, 4, sess.MessageCount())

	// PiG v0.7.0 carries the system prompt and tool set as system messages in
	// the transcript, not in ai.Context.SystemPrompt/Tools.
	req := <-requests
	assert.Equal(t, []string{"Probe"}, toolSchemaNames(ai.GetCurrentTools(req)))
	assert.Equal(t, kopiPrompt, ai.GetCurrentSystemPrompt(req))
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

	res, err := sess.Send(t.Context(), Prompt{Text: "hello"}, nil)
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

// PiG v0.7.0 behaviour: an abort during a running tool cancels the tool's
// ctx, and PiG's Send returns a nil error whose last assistant message has
// StopReason "error" (not "aborted") with ErrorMessage "context canceled".
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
		res, err := sess.Send(turnCtx, Prompt{Text: "block"}, nil)
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
	assert.Equal(t, ai.StopReasonError, last.Assistant.StopReason)
	assert.Equal(t, context.Canceled.Error(), last.Assistant.ErrorMessage)
	// user, tool call, tool result and PiG's synthesized error message
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
		res, err := sess.Send(ctx, Prompt{Text: "block"}, nil)
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
	res, err := sess.Send(cancelled, Prompt{Text: "cancelled before start"}, nil)
	require.NoError(t, err)
	assert.False(t, res.Sent)
	assert.Empty(t, res.Undelivered)
	assert.Equal(t, event.Outcome{Status: event.Interrupted}, res.Outcome)
	assert.Zero(t, faux.CallCount())
	assert.Zero(t, sess.MessageCount())

	res, err = sess.Send(t.Context(), Prompt{Text: "next turn"}, nil)
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

	res, err := sess.Send(turnCtx, Prompt{Text: "abort me"}, nil)
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
	_, err := sess.Send(turnCtx, Prompt{Text: "first turn"}, nil)
	require.NoError(t, err)
	unsubscribe()

	res, err := sess.Send(t.Context(), Prompt{Text: "second turn"}, nil)
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

	res, err := sess.Send(turnCtx, Prompt{Text: "finish me"}, nil)
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

// PiG v0.7.0 behaviour, recorded for the login/diagnostic step: a native
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

	_, err = sess.Send(t.Context(), Prompt{Text: "run it"}, nil)
	require.NoError(t, err)
	assert.Equal(t, execOutput, lastToolResult(t, <-requests))
	assert.Equal(t, int32(1), ran.Load())
}

func TestSessionsListsOnlyKopiAgentDir(t *testing.T) {
	sb := newSandbox(t)
	r, faux := newTestRuntime(t, sb)
	faux.SetResponses([]ai.FauxResponseStep{reply("hello back")})
	sess := openTestSession(t, r)
	_, err := sess.Send(t.Context(), Prompt{Text: "hello kopi"}, nil)
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
	_, err = sess.Send(t.Context(), Prompt{Text: prompt}, nil)
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

	res, err := sess.Send(t.Context(), Prompt{Text: "second question"}, nil)
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
		_, _ = sess.Send(context.Background(), Prompt{Text: "block"}, nil)
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
			res, err := sess.Send(t.Context(), Prompt{Text: "go"}, nil)
			require.NoError(t, err)
			assert.Equal(t, tc.want, res.Outcome)
		})
	}
}
