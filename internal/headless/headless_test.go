package headless

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/pigruntime/pigruntimetest"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/tool"
)

// waitLimit only guards against a hung test; no test relies on it elapsing.
const waitLimit = 30 * time.Second

type harness struct {
	c          *app.Controller
	out        *bytes.Buffer
	interrupts chan os.Signal
	cancel     context.CancelFunc
	result     chan error
}

// start opens a controller over a faux provider and runs a frontend for
// prompt. out is read only after Run has returned.
func start(t *testing.T, sb pigruntimetest.Sandbox, prompt string, steps []ai.FauxResponseStep, tools ...tool.Tool) *harness {
	t.Helper()
	return startRun(t, sb, app.Config{Tools: tools}, policy.Plan, prompt, steps)
}

// startRun is start with cfg's tools and rules, unattended as a print run is,
// in mode.
func startRun(t *testing.T, sb pigruntimetest.Sandbox, cfg app.Config, mode policy.Mode, prompt string, steps []ai.FauxResponseStep) *harness {
	t.Helper()
	rt := pigruntimetest.NewRuntime(t, sb)
	faux, model := pigruntimetest.RegisterFaux(t, rt, "kopi-faux")
	faux.SetResponses(steps)
	cfg.Model, cfg.Workspace, cfg.JoinTimeout, cfg.Unattended = model, sb.Workspace, waitLimit, true
	c := app.NewController(rt, cfg)
	t.Cleanup(func() { assert.NoError(t, c.Close(context.Background())) })
	require.NoError(t, c.Open(t.Context(), app.OpenRequest{Mode: mode}))
	ctx, cancel := context.WithCancel(t.Context())
	h := &harness{c: c, out: &bytes.Buffer{}, interrupts: make(chan os.Signal), cancel: cancel, result: make(chan error, 1)}
	f := New(c, prompt, h.out)
	go func() { h.result <- f.Run(ctx, h.interrupts) }()
	return h
}

func (h *harness) wait(t *testing.T) error {
	t.Helper()
	select {
	case err := <-h.result:
		return err
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for Run to return")
		return nil
	}
}

func waitClosed(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting")
	}
}

// blockTool blocks until its ctx is cancelled.
func blockTool(started chan<- struct{}) tool.Tool {
	return tool.Tool{
		Name: "Block", Label: "Block", Description: "Test-only tool.",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
		Capability: policy.Read,
		Run: func(ctx context.Context, _ json.RawMessage) (string, error) {
			close(started)
			<-ctx.Done()
			return "", ctx.Err()
		},
	}
}

func blockSteps() []ai.FauxResponseStep {
	return []ai.FauxResponseStep{
		pigruntimetest.ToolCalls(pigruntimetest.Call{Name: "Block", ID: "call-1"}),
		pigruntimetest.Text("unreached"),
	}
}

func TestPrintsOnlyFinalMessage(t *testing.T) {
	for _, tt := range []struct {
		name  string
		final ai.FauxResponseStep
		want  string
	}{
		{name: "text", final: pigruntimetest.Text("The notes are short."), want: "The notes are short.\n"},
		{name: "thinking only", final: ai.FauxAssistantMessage(ai.FauxContentBlocks{ai.FauxThinking("Considering.")}, ai.FauxAssistantMessageOptions{})},
		{name: "empty content", final: ai.FauxAssistantMessage(ai.FauxContentBlocks{}, ai.FauxAssistantMessageOptions{})},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sb := pigruntimetest.NewSandbox(t)
			require.NoError(t, os.WriteFile(filepath.Join(sb.Workspace, "notes.txt"), []byte("fixture-body-77\n"), 0o600))
			ws, err := tool.NewWorkspace(sb.Workspace, nil)
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, ws.Close()) })
			steps := []ai.FauxResponseStep{
				ai.FauxAssistantMessage(ai.FauxContentBlocks{
					ai.FauxText("Let me read the notes."),
					ai.FauxToolCall("Read", map[string]any{"file_path": "notes.txt"}, &ai.FauxToolCallOptions{ID: "call-1"}),
				}, ai.FauxAssistantMessageOptions{StopReason: string(ai.StopReasonToolUse)}),
				tt.final,
			}

			h := start(t, sb, "read the notes", steps, tool.NewRead(ws))

			require.NoError(t, h.wait(t))
			assert.Equal(t, tt.want, h.out.String())
		})
	}
}

func TestPrintsLengthTruncatedReply(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	truncated := ai.FauxAssistantMessage(ai.FauxContentString("partial"), ai.FauxAssistantMessageOptions{StopReason: string(ai.StopReasonLength)})
	h := start(t, sb, "go", []ai.FauxResponseStep{truncated})

	require.NoError(t, h.wait(t))
	assert.Equal(t, "partial\n", h.out.String())
}

func TestFailureReturnsProviderError(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	h := start(t, sb, "go", []ai.FauxResponseStep{pigruntimetest.ProviderError("invalid request: bad thing")})

	err := h.wait(t)

	require.ErrorIs(t, err, ErrFailed)
	assert.Contains(t, err.Error(), "invalid request: bad thing")
	assert.Empty(t, h.out.String())
}

func TestInterruptReturnsInterrupted(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	started := make(chan struct{})
	h := start(t, sb, "block", blockSteps(), blockTool(started))

	waitClosed(t, started)
	h.interrupts <- os.Interrupt

	require.ErrorIs(t, h.wait(t), ErrInterrupted)
	assert.Empty(t, h.out.String())
}

func TestCancelReturns(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	started := make(chan struct{})
	h := start(t, sb, "block", blockSteps(), blockTool(started))

	waitClosed(t, started)
	h.cancel()

	require.ErrorIs(t, h.wait(t), context.Canceled)
	assert.Empty(t, h.out.String())
}

func TestCommandTextIsSentAsPrompt(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	requests := make(chan pigruntimetest.Request, 1)
	h := start(t, sb, "/execute", []ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "not a command")})

	require.NoError(t, h.wait(t))
	select {
	case req := <-requests:
		assert.Equal(t, []string{"/execute"}, req.UserTexts)
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for a provider request")
	}
	assert.Equal(t, policy.Plan, h.c.Status().Mode)
	assert.Equal(t, "not a command\n", h.out.String())
}

// editRun runs a print run in mode that edits docs/a.md with rules, and
// returns the tool result the model saw and the file's content afterwards.
func editRun(t *testing.T, mode policy.Mode, rules ...policy.Source) (result, content string) {
	t.Helper()
	sb := pigruntimetest.NewSandbox(t)
	path := filepath.Join(sb.Workspace, "docs", "a.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte("alpha\n"), 0o600))
	ws, err := tool.NewWorkspace(sb.Workspace, nil)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, ws.Close()) })
	edit := tool.NewEdit(ws)
	r, err := policy.NewRules(sb.Workspace, sb.Home, []string{edit.Name}, rules...)
	require.NoError(t, err)
	requests := make(chan pigruntimetest.Request, 1)
	steps := []ai.FauxResponseStep{
		pigruntimetest.ToolCalls(pigruntimetest.Call{Name: "Edit", ID: "call-1", Args: map[string]any{"file_path": "docs/a.md", "old_string": "alpha", "new_string": "beta"}}),
		pigruntimetest.Capture(t, requests, "done"),
	}

	h := startRun(t, sb, app.Config{Tools: []tool.Tool{edit}, Rules: r}, mode, "edit docs/a.md", steps)

	require.NoError(t, h.wait(t))
	assert.Equal(t, "done\n", h.out.String())
	var req pigruntimetest.Request
	select {
	case req = <-requests:
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for a provider request")
	}
	require.Len(t, req.ToolResults, 1)
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return req.ToolResults[0], string(b)
}

func TestPlanRunRefusesEdit(t *testing.T) {
	result, content := editRun(t, policy.Plan)

	assert.Contains(t, result, `tool "Edit" needs execute mode`)
	assert.Equal(t, "alpha\n", content)
}

func TestExecuteRunWithoutAllowRuleDeniesWithHint(t *testing.T) {
	result, content := editRun(t, policy.Execute)

	assert.Contains(t, result, "needs approval")
	assert.Contains(t, result, "--allowed-tools")
	assert.Contains(t, result, policy.ErrDenied.Error())
	assert.Equal(t, "alpha\n", content)
}

func TestExecuteRunWithAllowRuleEdits(t *testing.T) {
	result, content := editRun(t, policy.Execute, policy.Source{Name: "--allowed-tools", Allow: []string{"Edit(docs/**)"}})

	assert.NotContains(t, result, policy.ErrDenied.Error())
	assert.Equal(t, "beta\n", content)
}
