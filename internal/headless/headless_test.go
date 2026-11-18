package headless

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/pigruntime/pigruntimetest"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/subagent"
	"github.com/keyute/kopi/internal/tool"
)

// waitLimit only guards against a hung test; no test relies on it elapsing.
const waitLimit = 30 * time.Second

type harness struct {
	c          *app.Controller
	faux       *ai.FauxProviderHandle
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
	return startWith(t, sb, cfg, mode, prompt, steps, nil)
}

// startWith is startRun with a second provider, "kopi-child/faux-1", serving
// childSteps, for subagents whose definitions name that model.
func startWith(t *testing.T, sb pigruntimetest.Sandbox, cfg app.Config, mode policy.Mode, prompt string, steps, childSteps []ai.FauxResponseStep) *harness {
	t.Helper()
	rt := pigruntimetest.NewRuntime(t, sb)
	faux, model := pigruntimetest.RegisterFaux(t, rt, "kopi-faux")
	faux.SetResponses(steps)
	if childSteps != nil {
		child, _ := pigruntimetest.RegisterFaux(t, rt, "kopi-child")
		child.SetResponses(childSteps)
	}
	cfg.Model, cfg.Workspace, cfg.JoinTimeout, cfg.Unattended, cfg.Mode = model, sb.Workspace, waitLimit, true, mode
	c := app.NewController(rt, cfg)
	t.Cleanup(func() { assert.NoError(t, c.Close(context.Background())) })
	require.NoError(t, c.Open(t.Context(), app.OpenRequest{}))
	ctx, cancel := context.WithCancel(t.Context())
	h := &harness{c: c, faux: faux, out: &bytes.Buffer{}, interrupts: make(chan os.Signal), cancel: cancel, result: make(chan error, 1)}
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
		Run: func(ctx context.Context, _ json.RawMessage, admit func(policy.Target) (policy.Mode, error)) (tool.Result, error) {
			if _, err := admit(policy.Target{}); err != nil {
				return tool.Result{}, err
			}
			close(started)
			<-ctx.Done()
			return tool.Result{}, ctx.Err()
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
	r, err := policy.NewRules(sb.Workspace, sb.Home, map[string]policy.ToolSpec{edit.Name: {Capability: edit.Capability, Target: edit.Target}}, rules...)
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

func agentConfig() app.Config {
	return app.Config{Agents: []subagent.Definition{{Name: "explorer", Description: "explores", Model: "kopi-child/faux-1", Instructions: true}}}
}

func launchCall(id, description string) pigruntimetest.Call {
	return pigruntimetest.Call{Name: "Agent", ID: id, Args: map[string]any{"description": description, "prompt": "go", "subagent_type": "explorer"}}
}

// gatedText answers text once gate is closed.
func gatedText(gate <-chan struct{}, text string) ai.FauxResponseStep {
	return ai.FauxFactoryStep(func(ai.TranscriptContext, ai.StreamOptions, *ai.FauxProviderState, *ai.Model) (ai.AssistantMessage, error) {
		<-gate
		return ai.FauxAssistantMessage(ai.FauxContentString(text), ai.FauxAssistantMessageOptions{}), nil
	})
}

// untilIdleWaiting waits until the run has only running subagents to wait for.
func untilIdleWaiting(t *testing.T, c *app.Controller) {
	t.Helper()
	q := c.FollowWork()
	defer q.Close()
	for {
		live, changed := settled(t, q)
		if live {
			return
		}
		select {
		case <-changed:
		case <-time.After(waitLimit):
			t.Fatal("timed out waiting for the run to wait on subagents")
		}
	}
}

// settled consumes q's turns once each has ended and reports whether any
// subagent is still running, with a channel that closes when that may change.
func settled(t *testing.T, q *app.WorkQueue) (bool, <-chan struct{}) {
	t.Helper()
	for {
		w, live, changed := q.Next()
		if w == nil {
			return live, changed
		}
		waitClosed(t, w.Done())
	}
}

func TestWaitsForChildAndPrintsTheWokenTurnsReply(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	gate := make(chan struct{})
	steps := []ai.FauxResponseStep{
		pigruntimetest.ToolCalls(launchCall("c1", "look")),
		pigruntimetest.Text("launched"),
		pigruntimetest.Text("the child found it"),
	}

	h := startWith(t, sb, agentConfig(), policy.Plan, "delegate", steps, []ai.FauxResponseStep{gatedText(gate, "child result")})
	untilIdleWaiting(t, h.c)
	close(gate)

	require.NoError(t, h.wait(t))
	assert.Equal(t, "the child found it\n", h.out.String())
}

func TestWaitsForEveryChild(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	gate := make(chan struct{})
	steps := []ai.FauxResponseStep{
		pigruntimetest.ToolCalls(launchCall("c1", "one"), launchCall("c2", "two")),
		pigruntimetest.Text("launched"),
		pigruntimetest.Text("woken"), pigruntimetest.Text("woken"),
	}

	h := startWith(t, sb, agentConfig(), policy.Plan, "delegate", steps, []ai.FauxResponseStep{gatedText(gate, "r1"), gatedText(gate, "r2")})
	untilIdleWaiting(t, h.c)
	close(gate)

	require.NoError(t, h.wait(t))
	assert.Equal(t, "woken\n", h.out.String())
	for _, info := range h.c.Tasks() {
		assert.Equal(t, tool.TaskCompleted, info.Status, info.ID)
	}
	assert.Len(t, h.c.Tasks(), 2)
}

func TestFailedFirstTurnDoesNotWaitForChildren(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	gate := make(chan struct{})
	steps := []ai.FauxResponseStep{pigruntimetest.ToolCalls(launchCall("c1", "look")), pigruntimetest.ProviderError("bad thing")}

	h := startWith(t, sb, agentConfig(), policy.Plan, "delegate", steps, []ai.FauxResponseStep{gatedText(gate, "late")})
	t.Cleanup(func() { close(gate) }) // runs before the controller's Close, which waits for the child

	err := h.wait(t)
	require.ErrorIs(t, err, ErrFailed)
	assert.Contains(t, err.Error(), "bad thing")
}

func TestInterruptWhileWaitingForChildrenEndsTheRun(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	gate := make(chan struct{})
	steps := []ai.FauxResponseStep{pigruntimetest.ToolCalls(launchCall("c1", "look")), pigruntimetest.Text("launched")}

	h := startWith(t, sb, agentConfig(), policy.Plan, "delegate", steps, []ai.FauxResponseStep{gatedText(gate, "late")})
	t.Cleanup(func() { close(gate) }) // runs before the controller's Close, which waits for the child
	untilIdleWaiting(t, h.c)
	h.interrupts <- os.Interrupt

	require.ErrorIs(t, h.wait(t), ErrInterrupted)
	assert.Empty(t, h.out.String())
}

func TestFailedChildStillLetsTheRunFinish(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	steps := []ai.FauxResponseStep{pigruntimetest.ToolCalls(launchCall("c1", "look")), pigruntimetest.Text("launched"), pigruntimetest.Text("it failed")}

	h := startWith(t, sb, agentConfig(), policy.Plan, "delegate", steps, []ai.FauxResponseStep{pigruntimetest.ProviderError("boom")})

	require.NoError(t, h.wait(t))
	assert.Equal(t, "it failed\n", h.out.String())
}

func TestStoppedChildStillLetsTheRunFinish(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	started := make(chan struct{})
	cfg := agentConfig()
	cfg.AgentTools = []tool.Tool{blockTool(started)}
	steps := []ai.FauxResponseStep{pigruntimetest.ToolCalls(launchCall("c1", "look")), pigruntimetest.Text("launched")}

	h := startWith(t, sb, cfg, policy.Plan, "delegate", steps, []ai.FauxResponseStep{pigruntimetest.ToolCalls(pigruntimetest.Call{Name: "Block", ID: "b1"})})
	waitClosed(t, started)
	untilIdleWaiting(t, h.c)
	require.NoError(t, h.c.StopTask("t1"))

	require.NoError(t, h.wait(t))
	assert.Equal(t, "launched\n", h.out.String(), "a stop wakes nobody, so the last turn's reply stands")
}

// spawnTool starts the shell command as a background task.
func spawnTool(tasks *tool.Tasks, sh *tool.Shell, command string) tool.Tool {
	return tool.Tool{
		Name: "Spawn", Label: "Spawn", Description: "Test-only tool.",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
		Capability: policy.Read,
		Run: func(ctx context.Context, _ json.RawMessage, admit func(policy.Target) (policy.Mode, error)) (tool.Result, error) {
			if _, err := admit(policy.Target{}); err != nil {
				return tool.Result{}, err
			}
			info, err := tasks.Start(ctx, sh, command)
			return tool.Result{Text: info.ID}, err
		},
	}
}

// pausedPoll holds the frontend's first wait while the controller completes wakes.
type pausedPoll struct {
	context.Context
	once             sync.Once
	entered, release chan struct{}
}

func (c *pausedPoll) Done() <-chan struct{} {
	c.once.Do(func() {
		close(c.entered)
		<-c.release
	})
	return c.Context.Done()
}

func TestCompletedWakesAreConsumedBeforeIdle(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprintf("failed=%t", failed), func(t *testing.T) {
			sb := pigruntimetest.NewSandbox(t)
			rt := pigruntimetest.NewRuntime(t, sb)
			faux, model := pigruntimetest.RegisterFaux(t, rt, "kopi-faux")
			wake := pigruntimetest.Text("wake reply")
			if failed {
				wake = pigruntimetest.ProviderError("wake failed")
			}
			faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("initial"), wake, pigruntimetest.Text("last")})
			tasks := tool.NewTasks()
			cfg := app.Config{Model: model, Workspace: sb.Workspace, JoinTimeout: waitLimit, Tasks: tasks, Wake: true}
			c := app.NewController(rt, cfg)
			t.Cleanup(func() { assert.NoError(t, c.Close(context.Background())) })
			require.NoError(t, c.Open(t.Context(), app.OpenRequest{}))
			ends := make(chan struct{}, 3)
			c.Subscribe(func(ev event.Event) {
				if _, ok := ev.(event.TurnEnd); ok {
					ends <- struct{}{}
				}
			})
			ctx := &pausedPoll{Context: t.Context(), entered: make(chan struct{}), release: make(chan struct{})}
			var out bytes.Buffer
			result := make(chan error, 1)
			q := c.FollowWork()
			defer q.Close()
			go func() { result <- New(c, "go", &out).Run(ctx, nil) }()
			waitClosed(t, ctx.entered)
			waitClosed(t, ends)
			// settled waits for the terminal queue transition after TurnEnd.
			settled(t, q)
			for range 2 {
				_, err := tasks.Start(t.Context(), tool.NewShell(sb.Workspace, nil), "true")
				require.NoError(t, err)
				waitClosed(t, ends)
				settled(t, q)
			}
			close(ctx.release)
			select {
			case err := <-result:
				if failed {
					require.ErrorIs(t, err, ErrFailed)
					assert.ErrorContains(t, err, "wake failed")
					assert.Empty(t, out.String())
				} else {
					require.NoError(t, err)
					assert.Equal(t, "last\n", out.String())
				}
			case <-time.After(waitLimit):
				t.Fatal("frontend did not consume completed turns")
			}
		})
	}
}
