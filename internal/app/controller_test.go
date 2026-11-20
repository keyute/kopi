package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/history"
	"github.com/keyute/kopi/internal/instruction"
	"github.com/keyute/kopi/internal/pigruntime"
	"github.com/keyute/kopi/internal/pigruntime/pigruntimetest"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/skill"
	"github.com/keyute/kopi/internal/tool"
)

const (
	fauxProvider = "kopi-faux"
	execOutput   = "exec-output-91c2"
	// waitLimit only guards against a hung test; no test relies on it elapsing.
	waitLimit = 30 * time.Second
)

type recorder struct {
	mu  sync.Mutex
	evs []event.Event
}

func (r *recorder) add(ev event.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evs = append(r.evs, ev)
}

func (r *recorder) all() []event.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.evs)
}

func ofType[T event.Event](evs []event.Event) []T {
	var out []T
	for _, ev := range evs {
		if e, ok := ev.(T); ok {
			out = append(out, e)
		}
	}
	return out
}

// untimed zeroes each TurnEnd's Elapsed, which is wall time, so expectations
// can compare whole events.
func untimed(evs []event.Event) []event.Event {
	for i, ev := range evs {
		if end, ok := ev.(event.TurnEnd); ok {
			end.Elapsed = 0
			evs[i] = end
		}
	}
	return evs
}

type harness struct {
	c     *Controller
	faux  *ai.FauxProviderHandle
	rec   *recorder
	tasks *fakeTasks
	// showModeLine stops buildHarness marking each new session's mode line told.
	showModeLine func()
	// read counts the user messages userTexts has already returned.
	read int
}

// newHarness builds a controller over a fresh runtime on sb. Its cleanup
// closes the controller before the runtime.
func newHarness(t *testing.T, sb pigruntimetest.Sandbox, cfg Config) *harness {
	t.Helper()
	rt := pigruntimetest.NewRuntime(t, sb)
	faux, model := pigruntimetest.RegisterFaux(t, rt, fauxProvider)
	if cfg.Model == "" {
		cfg.Model = model
	}
	return buildHarness(t, rt, faux, sb, cfg)
}

// newUnsetHarness is a harness whose model is unset, in an environment with no
// ambient credential. The faux provider, which holds a credential, is
// registered only when withFaux is set.
func newUnsetHarness(t *testing.T, cfg Config, withFaux bool) *harness {
	t.Helper()
	pigruntimetest.ClearEnv(t)
	sb := pigruntimetest.NewSandbox(t)
	rt := pigruntimetest.NewRuntime(t, sb)
	var faux *ai.FauxProviderHandle
	if withFaux {
		faux, _ = pigruntimetest.RegisterFaux(t, rt, fauxProvider)
	}
	cfg.Model = ""
	cfg.ConfigFile = "/kopi/config.json"
	return buildHarness(t, rt, faux, sb, cfg)
}

func buildHarness(t *testing.T, rt *pigruntime.Runtime, faux *ai.FauxProviderHandle, sb pigruntimetest.Sandbox, cfg Config) *harness {
	t.Helper()
	cfg.Workspace = sb.Workspace
	if cfg.JoinTimeout == 0 {
		cfg.JoinTimeout = waitLimit
	}
	var tasks *fakeTasks
	if cfg.Tasks == nil {
		tasks = &fakeTasks{}
		cfg.Tasks = tasks
	}
	c := NewController(rt, cfg)
	rec := &recorder{}
	c.Subscribe(rec.add)
	t.Cleanup(func() { assert.NoError(t, c.Close(context.Background())) })
	// A new session's first prompt carries a mode line. It would show in every
	// test that reads the user messages the model received, so the harness
	// marks it told; the tests of the line itself call showModeLine first.
	mute := c.Subscribe(func(ev event.Event) {
		if _, ok := ev.(event.SessionChanged); ok {
			c.queue.Lock()
			c.main.told = modeLine(c.mode(), c.cfg.Plans.Path())
			c.queue.Unlock()
		}
	})
	return &harness{c: c, faux: faux, rec: rec, tasks: tasks, showModeLine: mute}
}

func openHarness(t *testing.T, cfg Config) (*harness, pigruntimetest.Sandbox) {
	t.Helper()
	sb := pigruntimetest.NewSandbox(t)
	h := newHarness(t, sb, cfg)
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	return h, sb
}

func wait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting")
	}
}

func (h *harness) turn(t *testing.T, prompt string) *Work {
	t.Helper()
	w, err := h.c.Submit(t.Context(), Main, prompt)
	require.NoError(t, err)
	require.NotNil(t, w)
	wait(t, w.Done())
	return w
}

func testTool(name string, c policy.Capability, run func(context.Context, json.RawMessage) (string, error)) tool.Tool {
	return tool.Tool{
		Name:        name,
		Label:       name,
		Description: "Test-only tool.",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
		Capability:  c,
		Run: func(ctx context.Context, args json.RawMessage, admit func(policy.Target) (policy.Mode, error)) (tool.Result, error) {
			if _, err := admit(policy.Target{}); err != nil {
				return tool.Result{}, err
			}
			out, err := run(ctx, args)
			return tool.Result{Text: out}, err
		},
	}
}

func execTool(ran *atomic.Int32) tool.Tool {
	return testTool("Exec", policy.Exec, func(context.Context, json.RawMessage) (string, error) {
		ran.Add(1)
		return execOutput, nil
	})
}

// releaseTool blocks until release is closed, ignoring its ctx.
func releaseTool(started chan<- struct{}, release <-chan struct{}) tool.Tool {
	return testTool("Hold", policy.Read, func(context.Context, json.RawMessage) (string, error) {
		close(started)
		<-release
		return "released", nil
	})
}

func readTool(t *testing.T, sb pigruntimetest.Sandbox) tool.Tool {
	t.Helper()
	ws, err := tool.NewWorkspace(sb.Workspace, nil)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, ws.Close()) })
	return tool.NewRead(ws)
}

func call(name, id string) pigruntimetest.Call {
	return pigruntimetest.Call{Name: name, ID: id}
}

func receive(t *testing.T, requests <-chan pigruntimetest.Request) pigruntimetest.Request {
	t.Helper()
	select {
	case req := <-requests:
		return req
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for a provider request")
		return pigruntimetest.Request{}
	}
}

func TestTurnStreamsTextThenEnds(t *testing.T) {
	h, _ := openHarness(t, Config{})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("hello from the faux model")})

	w := h.turn(t, "hi")

	evs := untimed(h.rec.all())
	var text strings.Builder
	for _, d := range ofType[event.TextDelta](evs) {
		text.WriteString(d.Text)
	}
	assert.Equal(t, "hello from the faux model", text.String())
	assert.Greater(t, len(ofType[event.TextDelta](evs)), 1, "the reply streams in several deltas")
	assert.Equal(t, event.TurnEnd{WorkID: w.ID(), Outcome: event.Outcome{Status: event.Completed}}, evs[len(evs)-1])
	assert.Equal(t, event.Outcome{Status: event.Completed}, w.Outcome())
	assert.Equal(t, "hello from the faux model", w.Reply())
}

func TestTurnStartOpensTurnAndTurnEndIsTimed(t *testing.T) {
	h, _ := openHarness(t, Config{})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("hello from the faux model")})

	w := h.turn(t, "  //hi there ")

	evs := h.rec.all()
	startAt := slices.IndexFunc(evs, func(ev event.Event) bool { _, ok := ev.(event.TurnStart); return ok })
	textAt := slices.IndexFunc(evs, func(ev event.Event) bool { _, ok := ev.(event.TextDelta); return ok })
	require.GreaterOrEqual(t, startAt, 0)
	assert.Less(t, startAt, textAt)
	assert.Equal(t, event.TurnStart{WorkID: w.ID(), Prompt: "/hi there"}, evs[startAt])
	ends := ofType[event.TurnEnd](evs)
	require.Len(t, ends, 1)
	assert.Equal(t, w.ID(), ends[0].WorkID)
	assert.Positive(t, ends[0].Elapsed)
}

func TestDeniedCallIsClassified(t *testing.T) {
	var ran atomic.Int32
	h, _ := openHarness(t, Config{Tools: []tool.Tool{execTool(&ran)}})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Exec", "call-1")), pigruntimetest.Text("ok")})

	w := h.turn(t, "run it")

	evs := h.rec.all()
	startAt := slices.Index(evs, event.Event(event.TurnStart{WorkID: w.ID(), Prompt: "run it"}))
	toolAt := slices.IndexFunc(evs, func(ev event.Event) bool { _, ok := ev.(event.ToolStart); return ok })
	require.GreaterOrEqual(t, startAt, 0)
	assert.Less(t, startAt, toolAt)
	starts := ofType[event.ToolStart](evs)
	require.Len(t, starts, 1)
	assert.Equal(t, "Exec", starts[0].Title)
	assert.Equal(t, fallbackAction, starts[0].Action)
	ends := ofType[event.ToolEnd](evs)
	require.Len(t, ends, 1)
	assert.True(t, ends[0].IsError)
	assert.Equal(t, event.EndDenied, ends[0].Kind)
	assert.Equal(t, "denied · plan mode needs the sandbox, which is off or unavailable", ends[0].Summary)
	assert.Zero(t, ran.Load())
}

func TestReadResultReachesNextRequest(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	require.NoError(t, os.WriteFile(filepath.Join(sb.Workspace, "notes.txt"), []byte("kopi fixture 42\n"), 0o600))
	h := newHarness(t, sb, Config{Tools: []tool.Tool{readTool(t, sb)}})
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{
		pigruntimetest.ToolCalls(pigruntimetest.Call{Name: "Read", ID: "call-1", Args: map[string]any{"file_path": "notes.txt"}}),
		pigruntimetest.Capture(t, requests, "read it"),
	})

	h.turn(t, "read notes.txt")

	evs := h.rec.all()
	assert.Equal(t, []event.ToolStart{{
		CallID: "call-1", Name: "Read", Args: `{"file_path":"notes.txt"}`,
		Title: "Read notes.txt", Action: tool.Action{Verb: "Read", One: "file", Many: "files"},
	}}, ofType[event.ToolStart](evs))
	ends := ofType[event.ToolEnd](evs)
	require.Len(t, ends, 1)
	assert.False(t, ends[0].IsError)
	assert.Contains(t, ends[0].Output, "kopi fixture 42")
	assert.Equal(t, "1 line", ends[0].Summary)
	req := receive(t, requests)
	require.Len(t, req.ToolResults, 1)
	assert.Contains(t, req.ToolResults[0], "kopi fixture 42")
}

func TestResumeDeliversDisplayedHistory(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	require.NoError(t, os.WriteFile(filepath.Join(sb.Workspace, "notes.txt"), []byte("kopi fixture 42\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(sb.Workspace, "edit.txt"), []byte("alpha\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(sb.Workspace, "secret.txt"), []byte("alpha\n"), 0o600))
	ws, err := tool.NewWorkspace(sb.Workspace, nil)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, ws.Close()) })
	read, edit := readTool(t, sb), tool.NewEdit(ws)
	rules, err := policy.NewRules(sb.Workspace, sb.Home, tool.Specs([]tool.Tool{edit}), policy.Source{Name: "test", Allow: []string{"Edit"}, Deny: []string{"Edit(secret.txt)"}})
	require.NoError(t, err)
	cfg := Config{Tools: []tool.Tool{read, edit}, Rules: rules, Mode: policy.Execute}
	h := newHarness(t, sb, cfg)
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	changed := ofType[event.SessionChanged](h.rec.all())
	require.Len(t, changed, 1)
	assert.Nil(t, changed[0].History)
	h.faux.SetResponses([]ai.FauxResponseStep{
		pigruntimetest.ToolCalls(pigruntimetest.Call{Name: "Read", ID: "call-1", Args: map[string]any{"file_path": "notes.txt"}}),
		pigruntimetest.ToolCalls(editCall("call-2", "secret.txt")),
		pigruntimetest.ToolCalls(editCall("call-3", "edit.txt")),
		pigruntimetest.Text("done"),
	})
	h.turn(t, "read notes.txt")
	id := h.c.Status().SessionID
	require.NoError(t, h.c.Close(t.Context()))

	fresh := newHarness(t, sb, cfg)
	require.NoError(t, fresh.c.Open(t.Context(), OpenRequest{ResumeID: id}))
	changed = ofType[event.SessionChanged](fresh.rec.all())
	require.Len(t, changed, 1)
	history := changed[0].History
	require.NotEmpty(t, history)
	assert.Equal(t, event.TurnStart{Prompt: "read notes.txt"}, history[0])
	starts := ofType[event.ToolStart](history)
	require.Len(t, starts, 3)
	assert.Equal(t, event.ToolStart{
		CallID: "call-1", Name: "Read", Args: `{"file_path":"notes.txt"}`,
		Title: "Read notes.txt", Action: tool.Action{Verb: "Read", One: "file", Many: "files"},
	}, starts[0])
	assert.NotEmpty(t, starts[2].Done)
	assert.NotEmpty(t, starts[2].Change)
	ends := ofType[event.ToolEnd](history)
	require.Len(t, ends, 3)
	assert.Contains(t, ends[0].Output, "kopi fixture 42")
	assert.Equal(t, "1 line", ends[0].Summary)
	assert.Equal(t, event.EndDenied, ends[1].Kind)
	assert.Equal(t, "denied by rule Edit(secret.txt) (test)", ends[1].Summary)
	assert.Equal(t, event.EndRan, ends[2].Kind)
	assert.False(t, ends[2].IsError)
}

// In execute mode a non-Read call with no rule deciding it asks first.
func TestExecToolFollowsMode(t *testing.T) {
	var ran atomic.Int32
	h, _ := openHarness(t, Config{Tools: []tool.Tool{execTool(&ran)}})
	approvals := subscribeTo[event.ApprovalRequested](h.c)
	requests := make(chan pigruntimetest.Request, 1)
	turn := func(asks bool) string {
		h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Exec", "call-1")), pigruntimetest.Capture(t, requests, "ok")})
		w, err := h.c.Submit(t.Context(), Main, "run it")
		require.NoError(t, err)
		if asks {
			assert.True(t, h.c.Answer(receiveEvent(t, approvals).Token, policy.Decision{Answer: policy.Allow}))
		}
		wait(t, w.Done())
		req := receive(t, requests)
		require.NotEmpty(t, req.ToolResults)
		return req.ToolResults[len(req.ToolResults)-1]
	}

	denied := turn(false)
	assert.Contains(t, denied, `tool "Exec" needs execute mode; the session is in plan mode`)
	assert.Zero(t, ran.Load())

	h.c.EnterExecute()
	assert.Equal(t, execOutput, turn(true))
	assert.Equal(t, int32(1), ran.Load())

	h.c.EnterPlan()
	assert.Contains(t, turn(false), policy.ErrDenied.Error())
	assert.Equal(t, int32(1), ran.Load())
	assert.Len(t, ofType[event.ApprovalRequested](h.rec.all()), 1, "plan mode refuses before asking")

	assert.Equal(t, []event.ModeChanged{{Mode: policy.Execute}, {Mode: policy.Plan}}, ofType[event.ModeChanged](h.rec.all()))
}

func TestPlanSandboxedExecToolRunsInPlanWithoutAsking(t *testing.T) {
	var ran atomic.Int32
	exec := execTool(&ran)
	exec.Sandboxed, exec.PlanSandboxed = true, true
	h, _ := openHarness(t, Config{Tools: []tool.Tool{exec}})
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Exec", "call-1")), pigruntimetest.Capture(t, requests, "ok")})

	h.turn(t, "run it")

	req := receive(t, requests)
	require.NotEmpty(t, req.ToolResults)
	assert.Equal(t, execOutput, req.ToolResults[len(req.ToolResults)-1])
	assert.Equal(t, int32(1), ran.Load())
	assert.Equal(t, policy.Plan, h.c.Status().Mode)
	assert.Empty(t, ofType[event.ApprovalRequested](h.rec.all()))
}

// PiG runs a batch containing a sequential (non-Read) tool one call at a
// time, so the Exec call's before-hook runs only after the blocking call ends
// and the call is denied there; Admit's revocation is covered in pigruntime.
func TestPlanCommandStopsLaterExecCallInSameTurn(t *testing.T) {
	var ran atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	h, _ := openHarness(t, Config{Tools: []tool.Tool{releaseTool(started, release), execTool(&ran)}})
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{
		pigruntimetest.ToolCalls(call("Hold", "call-1"), call("Exec", "call-2")),
		pigruntimetest.Capture(t, requests, "done"),
	})
	h.c.EnterExecute()

	w, err := h.c.Submit(t.Context(), Main, "hold then exec")
	require.NoError(t, err)
	wait(t, started)
	_, err = h.c.Submit(t.Context(), Main, "/plan")
	require.NoError(t, err)
	close(release)
	wait(t, w.Done())

	assert.Zero(t, ran.Load())
	req := receive(t, requests)
	require.Len(t, req.ToolResults, 2)
	assert.Equal(t, "released", req.ToolResults[0])
	assert.Contains(t, req.ToolResults[1], `tool "Exec" needs execute mode; the session is in plan mode`)
	assert.Equal(t, policy.Plan, h.c.Status().Mode)
}

func TestProviderFailureEndsTurnFailed(t *testing.T) {
	h, _ := openHarness(t, Config{})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ProviderError("invalid request: bad thing")})

	w := h.turn(t, "go")

	want := event.Outcome{Status: event.Failed, Message: "invalid request: bad thing"}
	assert.Equal(t, want, w.Outcome())
	assert.Empty(t, w.Reply())
	assert.Equal(t, []event.TurnEnd{{WorkID: w.ID(), Outcome: want}}, ofType[event.TurnEnd](untimed(h.rec.all())))
}

func TestInterruptDuringToolEndsInterruptedAndPersists(t *testing.T) {
	started := make(chan struct{})
	block := testTool("Block", policy.Read, func(ctx context.Context, _ json.RawMessage) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	})
	h, sb := openHarness(t, Config{Tools: []tool.Tool{block}})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Block", "call-1")), pigruntimetest.Text("unreached")})
	assert.False(t, h.c.Interrupt(Main), "nothing to interrupt yet")

	w, err := h.c.Submit(t.Context(), Main, "block")
	require.NoError(t, err)
	wait(t, started)
	assert.True(t, h.c.Interrupt(Main))
	wait(t, w.Done())

	assert.Equal(t, event.Outcome{Status: event.Interrupted}, w.Outcome())
	assert.Empty(t, w.Reply())
	assert.Equal(t, []event.TurnEnd{{WorkID: w.ID(), Outcome: event.Outcome{Status: event.Interrupted}}}, ofType[event.TurnEnd](untimed(h.rec.all())))
	id := h.c.Status().SessionID
	require.NoError(t, h.c.Close(t.Context()))

	fresh := newHarness(t, sb, Config{Tools: []tool.Tool{block}})
	require.NoError(t, fresh.c.Open(t.Context(), OpenRequest{ResumeID: id}))
	changed := ofType[event.SessionChanged](fresh.rec.all())
	require.Len(t, changed, 1)
	assert.Equal(t, id, changed[0].SessionID)
	assert.True(t, changed[0].Resumed)
	// user, tool call, tool result and PiG's synthesized error message
	assert.Equal(t, 4, changed[0].MessageCount)

	requests := make(chan pigruntimetest.Request, 1)
	fresh.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "resumed")})
	fresh.turn(t, "carry on")
	req := receive(t, requests)
	assert.Equal(t, []string{"block", "carry on"}, req.UserTexts)
	assert.Equal(t, []string{context.Canceled.Error()}, req.ToolResults)
	assert.Contains(t, req.Raw, `"call-1"`)
}

// blockOnFirstTurnEnd holds event delivery at the first TurnEnd until unblock
// is closed, keeping the finished turn registered in that window.
func blockOnFirstTurnEnd(c *Controller) (reached, unblock chan struct{}) {
	reached, unblock = make(chan struct{}), make(chan struct{})
	var once sync.Once
	c.Subscribe(func(ev event.Event) {
		if _, ok := ev.(event.TurnEnd); ok {
			once.Do(func() {
				close(reached)
				<-unblock
			})
		}
	})
	return reached, unblock
}

func TestInterruptAfterTurnFinishedSparesNextTurn(t *testing.T) {
	h, _ := openHarness(t, Config{})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("first"), pigruntimetest.Text("second")})
	reached, unblock := blockOnFirstTurnEnd(h.c)

	w, err := h.c.Submit(t.Context(), Main, "first")
	require.NoError(t, err)
	wait(t, reached)
	// The TurnEnd is delivered under the lock Interrupt takes.
	interrupted := make(chan bool, 1)
	go func() { interrupted <- h.c.Interrupt(Main) }()
	close(unblock)
	wait(t, w.Done())
	assert.Equal(t, event.Completed, w.Outcome().Status)
	select {
	case ok := <-interrupted:
		assert.False(t, ok, "the turn was deregistered with its TurnEnd")
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for Interrupt")
	}

	assert.Equal(t, event.Outcome{Status: event.Completed}, h.turn(t, "second").Outcome())
	assert.Equal(t, 2, h.faux.CallCount())
}

func TestSubmitAsTurnFinishesStartsNextTurn(t *testing.T) {
	h, _ := openHarness(t, Config{})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("first"), pigruntimetest.Text("second")})
	reached, unblock := blockOnFirstTurnEnd(h.c)

	w, err := h.c.Submit(t.Context(), Main, "first")
	require.NoError(t, err)
	wait(t, reached)
	// The TurnEnd is delivered under the lock Submit takes to queue input.
	type submitted struct {
		w   *Work
		err error
	}
	second := make(chan submitted, 1)
	go func() {
		w, err := h.c.Submit(context.Background(), Main, "second")
		second <- submitted{w, err}
	}()
	close(unblock)
	wait(t, w.Done())
	var next *Work
	select {
	case got := <-second:
		require.NoError(t, got.err)
		require.NotNil(t, got.w, "the prompt starts a turn instead of queueing behind the finished one")
		next = got.w
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for Submit")
	}
	wait(t, next.Done())

	assert.NotEqual(t, w.ID(), next.ID())
	assert.Equal(t, event.Outcome{Status: event.Completed}, w.Outcome())
	assert.Equal(t, event.Outcome{Status: event.Completed}, next.Outcome())
	assert.Equal(t, 2, h.faux.CallCount())
	if queues := ofType[event.QueueChanged](h.rec.all()); len(queues) > 0 {
		assert.Empty(t, queues[len(queues)-1].Pending)
	}
}

// A second send on a session already running a turn is rejected by PiG.
func TestRejectedSendIsHandedBackOnce(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	h, _ := openHarness(t, Config{Tools: []tool.Tool{releaseTool(started, release)}})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Hold", "call-1")), pigruntimetest.Text("done")})
	running, err := h.c.Submit(t.Context(), Main, "hold")
	require.NoError(t, err)
	wait(t, started)

	ctx, cancel := context.WithCancel(t.Context())
	w := &Work{id: "turn-rejected", base: t.Context(), done: make(chan struct{}), cancel: cancel}
	h.c.main.runTurn(ctx, h.c.main.sess, h.c.main.emit, w, []pigruntime.Prompt{{Text: "rejected"}}, nil)
	wait(t, w.Done())
	close(release)
	wait(t, running.Done())

	assert.Equal(t, event.Failed, w.Outcome().Status)
	assert.Contains(t, w.Outcome().Message, "failed to send prompt")
	var rejected []event.TurnEnd
	for _, end := range ofType[event.TurnEnd](untimed(h.rec.all())) {
		if end.WorkID == w.ID() {
			rejected = append(rejected, end)
		}
	}
	assert.Equal(t, []event.TurnEnd{{WorkID: w.ID(), Outcome: w.Outcome(), Unsent: []event.Input{{Text: "rejected"}}}}, rejected)
	assert.Empty(t, ofType[event.Error](h.rec.all()))
	assert.Equal(t, event.Outcome{Status: event.Completed}, running.Outcome())
}

func TestFailedResumeAsTurnFinishesKeepsSessionUsable(t *testing.T) {
	h, _ := openHarness(t, Config{})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("first"), pigruntimetest.Text("second")})
	reached, unblock := blockOnFirstTurnEnd(h.c)
	id := h.c.Status().SessionID

	w, err := h.c.Submit(t.Context(), Main, "first")
	require.NoError(t, err)
	wait(t, reached)
	// The TurnEnd is delivered under the lock the switch takes to stop the turn.
	resumed := make(chan error, 1)
	go func() { resumed <- h.c.Resume(context.Background(), "no-such-session") }()
	close(unblock)
	wait(t, w.Done())
	select {
	case err := <-resumed:
		require.ErrorIs(t, err, pigruntime.ErrSessionNotFound)
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for Resume")
	}

	require.ErrorIs(t, h.c.Resume(t.Context(), "no-such-session"), pigruntime.ErrSessionNotFound)
	assert.Equal(t, id, h.c.Status().SessionID)
	assert.Equal(t, event.Outcome{Status: event.Completed}, h.turn(t, "second").Outcome())
	assert.Equal(t, 2, h.faux.CallCount())
}

func TestPromptWhileBusyIsQueuedAndCommandRuns(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	h, _ := openHarness(t, Config{Tools: []tool.Tool{releaseTool(started, release)}})
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Hold", "call-1")), pigruntimetest.Capture(t, requests, "done")})

	w, err := h.c.Submit(t.Context(), Main, "hold")
	require.NoError(t, err)
	wait(t, started)

	got, err := h.c.Submit(t.Context(), Main, "another prompt")
	require.NoError(t, err)
	assert.Nil(t, got)
	got, err = h.c.Submit(t.Context(), Main, "/execute")
	require.NoError(t, err)
	assert.Nil(t, got)
	assert.True(t, h.c.Status().Busy)
	assert.Equal(t, policy.Execute, h.c.Status().Mode)

	close(release)
	wait(t, w.Done())
	assert.False(t, h.c.Status().Busy)
	assert.Equal(t, []string{"hold", "another prompt", "The session is in execute mode."}, receive(t, requests).UserTexts)
	evs := untimed(h.rec.all())
	queuedAt := slices.IndexFunc(evs, func(ev event.Event) bool {
		return assert.ObjectsAreEqual(event.QueueChanged{Pending: []string{"another prompt"}}, ev)
	})
	sentAt := slices.Index(evs, event.Event(event.UserMessage{Text: "another prompt"}))
	require.GreaterOrEqual(t, queuedAt, 0)
	assert.Less(t, queuedAt, sentAt)
	assert.Equal(t, []event.TurnEnd{{WorkID: w.ID(), Outcome: event.Outcome{Status: event.Completed}}}, ofType[event.TurnEnd](evs))
}

func TestQueuedInputIsDeliveredAtBoundary(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	h, _ := openHarness(t, Config{Tools: []tool.Tool{releaseTool(started, release)}})
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Hold", "call-1")), pigruntimetest.Capture(t, requests, "done")})

	w, err := h.c.Submit(t.Context(), Main, "hold")
	require.NoError(t, err)
	wait(t, started)
	_, err = h.c.Submit(t.Context(), Main, "next")
	require.NoError(t, err)
	close(release)
	wait(t, w.Done())

	assert.Equal(t, []string{"hold", "next"}, receive(t, requests).UserTexts)
	queueing := slices.DeleteFunc(h.rec.all(), func(ev event.Event) bool {
		switch ev.(type) {
		case event.QueueChanged, event.UserMessage:
			return false
		}
		return true
	})
	assert.Equal(t, []event.Event{
		event.QueueChanged{Pending: []string{"next"}},
		event.QueueChanged{Pending: []string{}},
		event.UserMessage{Text: "next"},
	}, queueing)
}

// blockTool blocks until its ctx is cancelled.
func blockTool(started chan<- struct{}) tool.Tool {
	return testTool("Block", policy.Read, func(ctx context.Context, _ json.RawMessage) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	})
}

// turnEnds delivers every TurnEnd, untimed.
func turnEnds(c *Controller) <-chan event.TurnEnd {
	ends := make(chan event.TurnEnd, 8)
	c.Subscribe(func(ev event.Event) {
		if end, ok := ev.(event.TurnEnd); ok {
			end.Elapsed = 0
			ends <- end
		}
	})
	return ends
}

func receiveEnd(t *testing.T, ends <-chan event.TurnEnd) event.TurnEnd {
	t.Helper()
	select {
	case end := <-ends:
		return end
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for TurnEnd")
		return event.TurnEnd{}
	}
}

// lifecycle keeps the TurnStart and TurnEnd events.
func lifecycle(evs []event.Event) []event.Event {
	return slices.DeleteFunc(evs, func(ev event.Event) bool {
		switch ev.(type) {
		case event.TurnStart, event.TurnEnd:
			return false
		}
		return true
	})
}

func TestInterruptHandsQueuedInputBack(t *testing.T) {
	started := make(chan struct{})
	h, _ := openHarness(t, Config{Tools: []tool.Tool{blockTool(started)}})
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Block", "call-1")), pigruntimetest.Capture(t, requests, "fresh")})

	w, err := h.c.Submit(t.Context(), Main, "block")
	require.NoError(t, err)
	wait(t, started)
	img := pngFile(t)
	got, err := h.c.Submit(t.Context(), Main, "look at [image #1]", img)
	require.NoError(t, err)
	assert.Nil(t, got)
	assert.True(t, h.c.Interrupt(Main))
	wait(t, w.Done())

	assert.Equal(t, []event.TurnEnd{{
		WorkID:  w.ID(),
		Outcome: event.Outcome{Status: event.Interrupted},
		Unsent:  []event.Input{{Text: "look at [image #1]", Images: []string{img}}},
	}}, ofType[event.TurnEnd](untimed(h.rec.all())))

	h.turn(t, "after")
	req := receive(t, requests)
	assert.Equal(t, []string{"block", "after"}, req.UserTexts)
	assert.NotContains(t, req.Raw, "look at")
}

func TestFailedResumeCommandHandsQueuedInputBack(t *testing.T) {
	started := make(chan struct{})
	h, _ := openHarness(t, Config{Tools: []tool.Tool{blockTool(started)}})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Block", "call-1")), pigruntimetest.Text("after")})
	id := h.c.Status().SessionID
	errs := subscribeTo[event.Error](h.c)

	w, err := h.c.Submit(t.Context(), Main, "block")
	require.NoError(t, err)
	wait(t, started)
	img := pngFile(t)
	_, err = h.c.Submit(t.Context(), Main, "queued [image #1]", img)
	require.NoError(t, err)
	_, err = h.c.Submit(t.Context(), Main, "/resume no-such-session")
	require.NoError(t, err)
	require.ErrorIs(t, receiveEvent(t, errs).Err, pigruntime.ErrSessionNotFound)
	wait(t, w.Done())
	assertSwitchDone(t, h)

	evs := untimed(h.rec.all())
	assert.Equal(t, []event.TurnEnd{{
		WorkID: w.ID(), Outcome: event.Outcome{Status: event.Interrupted},
		Unsent: []event.Input{{Text: "queued [image #1]", Images: []string{img}}},
	}}, ofType[event.TurnEnd](evs))
	endAt := slices.IndexFunc(evs, func(ev event.Event) bool { _, ok := ev.(event.TurnEnd); return ok })
	errorAt := slices.IndexFunc(evs, func(ev event.Event) bool { _, ok := ev.(event.Error); return ok })
	assert.Less(t, endAt, errorAt)
	assert.Len(t, ofType[event.SessionChanged](evs), 1)
	assert.Equal(t, id, h.c.Status().SessionID)
	assert.Equal(t, event.Completed, h.turn(t, "after").Outcome().Status)
}

func TestAsyncResumeCapturesInputBeforeWaitingForLife(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	h, _ := openHarness(t, Config{Tools: []tool.Tool{releaseTool(started, release)}})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Hold", "call-1")), pigruntimetest.Text("after")})
	errs := subscribeTo[event.Error](h.c)
	w, err := h.c.Submit(t.Context(), Main, "hold")
	require.NoError(t, err)
	wait(t, started)
	require.True(t, h.c.Interrupt(Main))
	img := pngFile(t)
	_, err = h.c.Submit(t.Context(), Main, "after interrupt [image #1]", img)
	require.NoError(t, err)

	// The command must capture the stopped turn before its starter gets life.
	h.c.life.Lock()
	_, err = h.c.Submit(t.Context(), Main, "/resume no-such-session")
	assert.NoError(t, err)
	close(release)
	wait(t, w.Done())
	h.c.queue.Lock()
	assert.Empty(t, h.c.main.pending)
	assert.True(t, w.switchHold)
	assert.NotNil(t, w.terminal)
	h.c.queue.Unlock()
	assert.Empty(t, ofType[event.TurnEnd](h.rec.all()))
	h.c.life.Unlock()

	require.ErrorIs(t, receiveEvent(t, errs).Err, pigruntime.ErrSessionNotFound)
	assertSwitchDone(t, h)
	assert.Equal(t, []event.TurnEnd{{
		WorkID: w.ID(), Outcome: event.Outcome{Status: event.Interrupted},
		Unsent: []event.Input{{Text: "after interrupt [image #1]", Images: []string{img}}},
	}}, ofType[event.TurnEnd](untimed(h.rec.all())))
	assert.Equal(t, event.Completed, h.turn(t, "after").Outcome().Status)
}

func TestSyncResumeHoldsSettlementWhileCapturingInput(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	h, _ := openHarness(t, Config{Tools: []tool.Tool{releaseTool(started, release)}})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Hold", "call-1"))})
	w, err := h.c.Submit(t.Context(), Main, "hold")
	require.NoError(t, err)
	wait(t, started)
	require.True(t, h.c.Interrupt(Main))
	img := pngFile(t)
	_, err = h.c.Submit(t.Context(), Main, "later [image #1]", img)
	require.NoError(t, err)
	capturing, proceed := make(chan struct{}), make(chan struct{})
	h.c.Subscribe(func(ev event.Event) {
		if q, ok := ev.(event.QueueChanged); ok && len(q.Pending) == 0 {
			// Queue delivery occurs inside the initiating transaction.
			assert.True(t, w.switchHold)
			close(capturing)
			<-proceed
		}
	})
	result := make(chan error, 1)
	go func() { result <- h.c.Resume(t.Context(), "no-such-session") }()
	wait(t, capturing)
	close(release)
	select {
	case <-w.Done():
		t.Error("settlement escaped the initiating queue transaction")
	default:
	}
	close(proceed)
	select {
	case err := <-result:
		require.ErrorIs(t, err, pigruntime.ErrSessionNotFound)
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for Resume")
	}
	wait(t, w.Done())
	assert.Equal(t, []event.TurnEnd{{
		WorkID: w.ID(), Outcome: event.Outcome{Status: event.Interrupted},
		Unsent: []event.Input{{Text: "later [image #1]", Images: []string{img}}},
	}}, ofType[event.TurnEnd](untimed(h.rec.all())))
}

func TestCloseDiscardsDeregisteredSwitchWorkBeforeDelayedStart(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	h, _ := openHarness(t, Config{Tools: []tool.Tool{releaseTool(started, release)}})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Hold", "call-1"))})
	w, err := h.c.Submit(t.Context(), Main, "hold")
	require.NoError(t, err)
	wait(t, started)
	require.True(t, h.c.Interrupt(Main))
	_, err = h.c.Submit(t.Context(), Main, "later [image #1]", pngFile(t))
	require.NoError(t, err)
	op, err := h.c.beginSwitch(nil)
	require.NoError(t, err)
	// Delay the async starter explicitly so Close wins life after settlement.
	start := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		<-start
		h.c.life.Lock()
		defer h.c.life.Unlock()
		err := h.c.startSwitch(t.Context(), OpenRequest{ResumeID: "no-such-session"}, op)
		if err != nil {
			h.c.relay.emit(event.Error{Err: err})
		}
		result <- err
	}()
	close(release)
	wait(t, w.Done())
	require.False(t, h.c.Status().Busy)
	require.NoError(t, h.c.Close(t.Context()))
	delivered := len(h.rec.all())
	close(start)
	select {
	case err := <-result:
		require.ErrorIs(t, err, ErrNoSession)
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for delayed start")
	}
	h.c.queue.Lock()
	assert.True(t, w.discard)
	assert.False(t, w.switchHold)
	assert.Nil(t, w.terminal)
	h.c.queue.Unlock()
	assert.Len(t, h.rec.all(), delivered)
	assert.Empty(t, ofType[event.TurnEnd](h.rec.all()))
	assert.Len(t, ofType[event.SessionChanged](h.rec.all()), 1)
}

func TestSwitchJoinTimeoutHandsQueuedInputBack(t *testing.T) {
	started, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	block := testTool("Hold", policy.Read, func(ctx context.Context, _ json.RawMessage) (string, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		<-release
		return "released", nil
	})
	h, _ := openHarness(t, Config{Tools: []tool.Tool{block}})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Hold", "call-1")), pigruntimetest.Text("after")})
	id := h.c.Status().SessionID
	w, err := h.c.Submit(t.Context(), Main, "hold")
	require.NoError(t, err)
	wait(t, started)
	img := pngFile(t)
	_, err = h.c.Submit(t.Context(), Main, "queued [image #1]", img)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- h.c.Resume(ctx, "no-such-session") }()
	wait(t, cancelled)
	cancel()
	// Release the tool even if an assertion fails, so cleanup can join it.
	select {
	case err := <-result:
		assert.ErrorIs(t, err, ErrWorkStillRunning)
	case <-time.After(waitLimit):
		t.Error("timed out waiting for Resume")
	}
	assert.Empty(t, ofType[event.TurnEnd](h.rec.all()))
	close(release)
	wait(t, w.Done())

	assert.Equal(t, []event.TurnEnd{{
		WorkID: w.ID(), Outcome: event.Outcome{Status: event.Interrupted},
		Unsent: []event.Input{{Text: "queued [image #1]", Images: []string{img}}},
	}}, ofType[event.TurnEnd](untimed(h.rec.all())))
	assert.Len(t, ofType[event.SessionChanged](h.rec.all()), 1)
	assert.Equal(t, id, h.c.Status().SessionID)
	assert.Equal(t, event.Completed, h.turn(t, "after").Outcome().Status)
}

func TestInterruptBeforeSendHandsOriginalAndQueuedInputBack(t *testing.T) {
	h, _ := openHarness(t, Config{})
	originalImages := []string{pngFile(t), pngFile(t)}
	laterImage := pngFile(t)
	original := pigruntime.Prompt{
		Text: "original [image #1] [image #2]",
		Images: []pigruntime.Image{
			{Path: originalImages[0]},
			{Path: originalImages[1]},
		},
	}
	later := pigruntime.Prompt{Text: "later [image #1]", Images: []pigruntime.Image{{Path: laterImage}}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	w := &Work{
		id:       "turn-1",
		base:     t.Context(),
		done:     make(chan struct{}),
		cancel:   cancel,
		handBack: []pigruntime.Prompt{later},
		stopped:  true,
	}
	h.c.main.work = w

	h.c.main.runTurn(ctx, h.c.main.sess, h.c.main.emit, w, []pigruntime.Prompt{original}, nil)
	wait(t, w.Done())

	assert.Zero(t, h.faux.CallCount())
	assert.Equal(t, event.Outcome{Status: event.Interrupted}, w.Outcome())
	assert.Equal(t, []event.TurnEnd{{
		WorkID:  w.ID(),
		Outcome: event.Outcome{Status: event.Interrupted},
		Unsent: []event.Input{
			{Text: original.Text, Images: originalImages},
			{Text: later.Text, Images: []string{laterImage}},
		},
	}}, ofType[event.TurnEnd](untimed(h.rec.all())))
	assert.False(t, h.c.Status().Busy)
}

func TestSendNowContinuesTurnWithQueuedAndDraft(t *testing.T) {
	started := make(chan struct{})
	h, _ := openHarness(t, Config{Tools: []tool.Tool{blockTool(started)}})
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Block", "call-1")), pigruntimetest.Capture(t, requests, "joined")})

	w, err := h.c.Submit(t.Context(), Main, "block")
	require.NoError(t, err)
	wait(t, started)
	got, err := h.c.Submit(t.Context(), Main, "q")
	require.NoError(t, err)
	assert.Nil(t, got)
	require.NoError(t, h.c.SendNow(t.Context(), Main, "d"))
	wait(t, w.Done())

	assert.Equal(t, []string{"block", "q\n\nd"}, receive(t, requests).UserTexts)
	assert.Equal(t, event.Outcome{Status: event.Completed}, w.Outcome())
	evs := untimed(h.rec.all())
	// One Work throughout, and nothing between the interrupted send's end and
	// the joined send's start, so the controller never went idle.
	assert.Equal(t, []event.Event{
		event.TurnStart{WorkID: w.ID(), Prompt: "block"},
		event.TurnEnd{WorkID: w.ID(), Outcome: event.Outcome{Status: event.Interrupted}},
		event.TurnStart{WorkID: w.ID(), Prompt: "q\n\nd"},
		event.TurnEnd{WorkID: w.ID(), Outcome: event.Outcome{Status: event.Completed}},
	}, lifecycle(slices.Clone(evs)))
	endAt := slices.IndexFunc(evs, func(ev event.Event) bool {
		return assert.ObjectsAreEqual(event.TurnEnd{WorkID: w.ID(), Outcome: event.Outcome{Status: event.Interrupted}}, ev)
	})
	require.GreaterOrEqual(t, endAt, 0)
	assert.Equal(t, event.TurnStart{WorkID: w.ID(), Prompt: "q\n\nd"}, evs[endAt+1])
}

func TestSendNowWithNothingToSendInterruptsNothing(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	h, _ := openHarness(t, Config{Tools: []tool.Tool{releaseTool(started, release)}})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Hold", "call-1")), pigruntimetest.Text("done")})

	w, err := h.c.Submit(t.Context(), Main, "hold")
	require.NoError(t, err)
	wait(t, started)
	require.NoError(t, h.c.SendNow(t.Context(), Main, ""))
	require.NoError(t, h.c.SendNow(t.Context(), Main, "  "))
	close(release)
	wait(t, w.Done())

	assert.Equal(t, []event.TurnEnd{{WorkID: w.ID(), Outcome: event.Outcome{Status: event.Completed}}}, ofType[event.TurnEnd](untimed(h.rec.all())))
	assert.Equal(t, 2, h.faux.CallCount())
}

func TestSendNowAtIdleSubmits(t *testing.T) {
	h, _ := openHarness(t, Config{})
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "ok")})
	ends := turnEnds(h.c)

	require.NoError(t, h.c.SendNow(t.Context(), Main, ""))
	assert.Empty(t, ofType[event.TurnStart](h.rec.all()), "a blank send-now at idle starts nothing")
	require.NoError(t, h.c.SendNow(t.Context(), Main, "hi"))

	assert.Equal(t, []string{"hi"}, receive(t, requests).UserTexts)
	end := receiveEnd(t, ends)
	assert.Equal(t, event.Outcome{Status: event.Completed}, end.Outcome)
	assert.Equal(t, []event.TurnStart{{WorkID: end.WorkID, Prompt: "hi"}}, ofType[event.TurnStart](h.rec.all()))
}

func TestTakeQueuedEmptiesQueueAndTurnContinues(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	h, _ := openHarness(t, Config{Tools: []tool.Tool{releaseTool(started, release)}})
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Hold", "call-1")), pigruntimetest.Capture(t, requests, "done")})
	assert.Nil(t, h.c.TakeQueued(Main), "nothing runs")

	w, err := h.c.Submit(t.Context(), Main, "hold")
	require.NoError(t, err)
	wait(t, started)
	img := pngFile(t)
	_, err = h.c.Submit(t.Context(), Main, "q1")
	require.NoError(t, err)
	_, err = h.c.Submit(t.Context(), Main, "q2 [image #1]", img)
	require.NoError(t, err)
	assert.Equal(t, []event.Input{{Text: "q1"}, {Text: "q2 [image #1]", Images: []string{img}}}, h.c.TakeQueued(Main))
	assert.Empty(t, h.c.TakeQueued(Main))
	close(release)
	wait(t, w.Done())

	assert.Equal(t, []string{"hold"}, receive(t, requests).UserTexts)
	evs := untimed(h.rec.all())
	assert.Equal(t, []event.TurnEnd{{WorkID: w.ID(), Outcome: event.Outcome{Status: event.Completed}}}, ofType[event.TurnEnd](evs))
	queues := ofType[event.QueueChanged](evs)
	require.NotEmpty(t, queues)
	assert.Empty(t, queues[len(queues)-1].Pending)
}

func TestTakeQueuedDuringSendIsNeverDelivered(t *testing.T) {
	const queued = "queued-3f7a"
	started, release := make(chan struct{}), make(chan struct{})
	h, _ := openHarness(t, Config{Tools: []tool.Tool{releaseTool(started, release)}})
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Hold", "call-1")), pigruntimetest.Capture(t, requests, "done")})

	w, err := h.c.Submit(t.Context(), Main, "hold")
	require.NoError(t, err)
	wait(t, started)
	_, err = h.c.Submit(t.Context(), Main, queued)
	require.NoError(t, err)
	assert.Equal(t, []event.Input{{Text: queued}}, h.c.TakeQueued(Main))
	evs := h.rec.all()
	assert.Equal(t, event.QueueChanged{Pending: []string{}}, evs[len(evs)-1])
	close(release)
	wait(t, w.Done())

	req := receive(t, requests)
	assert.Equal(t, []string{"hold"}, req.UserTexts)
	assert.NotContains(t, req.Raw, queued)
	assert.Equal(t, 2, h.faux.CallCount())
}

func TestSessionSwitchDiscardsQueuedInput(t *testing.T) {
	const queued = "queued-5e1d"
	for _, tc := range []struct {
		name      string
		resume    bool
		wantAfter []string
	}{
		{name: "new", wantAfter: []string{"after"}},
		{name: "resume", resume: true, wantAfter: []string{"first", "after"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started := make(chan struct{})
			h, _ := openHarness(t, Config{Tools: []tool.Tool{blockTool(started)}})
			switchTo := h.c.NewSession
			if tc.resume {
				h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("first answer")})
				h.turn(t, "first")
				id := h.c.Status().SessionID
				switchTo = func(ctx context.Context) error { return h.c.Resume(ctx, id) }
				require.NoError(t, h.c.NewSession(t.Context()))
			}
			requests := make(chan pigruntimetest.Request, 2)
			h.faux.SetResponses([]ai.FauxResponseStep{
				pigruntimetest.ToolCalls(call("Block", "call-1")),
				pigruntimetest.Capture(t, requests, "fresh"),
				pigruntimetest.Capture(t, requests, "back"),
			})
			ends := turnEnds(h.c)
			oldID := h.c.Status().SessionID

			w, err := h.c.Submit(t.Context(), Main, "block")
			require.NoError(t, err)
			wait(t, started)
			img := pngFile(t)
			_, err = h.c.Submit(t.Context(), Main, queued, img)
			require.NoError(t, err)
			require.NoError(t, switchTo(t.Context()))

			assert.Equal(t, event.TurnEnd{WorkID: w.ID(), Outcome: event.Outcome{Status: event.Interrupted}}, receiveEnd(t, ends))
			evs := h.rec.all()
			workEnds := slices.DeleteFunc(ofType[event.TurnEnd](evs), func(end event.TurnEnd) bool { return end.WorkID != w.ID() })
			require.Len(t, workEnds, 1)
			endAt := slices.IndexFunc(evs, func(ev event.Event) bool {
				end, ok := ev.(event.TurnEnd)
				return ok && end.WorkID == w.ID()
			})
			changedAt := slices.IndexFunc(evs[endAt+1:], func(ev event.Event) bool { _, ok := ev.(event.SessionChanged); return ok })
			assert.GreaterOrEqual(t, changedAt, 0, "the terminal event precedes SessionChanged")
			h.turn(t, "after")
			req := receive(t, requests)
			assert.Equal(t, tc.wantAfter, req.UserTexts)
			assert.NotContains(t, req.Raw, queued)

			require.NoError(t, h.c.Resume(t.Context(), oldID))
			h.turn(t, "check")
			req = receive(t, requests)
			assert.Equal(t, []string{"block", "check"}, req.UserTexts)
			assert.NotContains(t, req.Raw, queued, "the old session never received it")
		})
	}
}

// widePNGFile is wider than PiG's default longest side of 2000 pixels.
func widePNGFile(t *testing.T) (path string, data []byte) {
	t.Helper()
	var b bytes.Buffer
	require.NoError(t, png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 6000, 40))))
	path = filepath.Join(t.TempDir(), "wide.png")
	require.NoError(t, os.WriteFile(path, b.Bytes(), 0o600))
	return path, b.Bytes()
}

func TestSteeredImageIsResizedAndKeepsOrder(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	h, _ := openHarness(t, Config{Tools: []tool.Tool{releaseTool(started, release)}})
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Hold", "call-1")), pigruntimetest.Capture(t, requests, "done")})
	path, data := widePNGFile(t)

	w, err := h.c.Submit(t.Context(), Main, "hold")
	require.NoError(t, err)
	wait(t, started)
	_, err = h.c.Submit(t.Context(), Main, "wide [image #1]", path)
	require.NoError(t, err)
	_, err = h.c.Submit(t.Context(), Main, "after")
	require.NoError(t, err)
	close(release)
	wait(t, w.Done())

	req := receive(t, requests)
	assert.Equal(t, []string{"hold", "wide [image #1]", "after"}, req.UserTexts)
	assert.Equal(t, 1, strings.Count(req.Raw, `"type":"image"`))
	assert.NotContains(t, req.Raw, base64.StdEncoding.EncodeToString(data), "the image was resized")
	assert.Equal(t, []event.TurnEnd{{WorkID: w.ID(), Outcome: event.Outcome{Status: event.Completed}}}, ofType[event.TurnEnd](untimed(h.rec.all())))
}

// Input given after an interrupt, while the stopped turn is still finishing,
// starts the next turn instead of being handed back with the stopped one's.
func TestInputAfterInterruptStartsNextTurn(t *testing.T) {
	for name, now := range map[string]bool{"submit": false, "send-now": true} {
		t.Run(name, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			h, _ := openHarness(t, Config{Tools: []tool.Tool{releaseTool(started, release)}})
			requests := make(chan pigruntimetest.Request, 1)
			h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Hold", "call-1")), pigruntimetest.Capture(t, requests, "fresh")})
			ends := turnEnds(h.c)

			w, err := h.c.Submit(t.Context(), Main, "hold")
			require.NoError(t, err)
			wait(t, started)
			_, err = h.c.Submit(t.Context(), Main, "before")
			require.NoError(t, err)
			require.True(t, h.c.Interrupt(Main))
			if now {
				require.NoError(t, h.c.SendNow(t.Context(), Main, "after"))
			} else {
				got, err := h.c.Submit(t.Context(), Main, "after")
				require.NoError(t, err)
				assert.Nil(t, got)
			}
			close(release)

			old, fresh := receiveEnd(t, ends), receiveEnd(t, ends)
			assert.Equal(t, event.TurnEnd{WorkID: w.ID(), Outcome: event.Outcome{Status: event.Interrupted}, Unsent: []event.Input{{Text: "before"}}}, old)
			assert.NotEqual(t, w.ID(), fresh.WorkID)
			assert.Equal(t, event.Outcome{Status: event.Completed}, fresh.Outcome)
			assert.Equal(t, []string{"hold", "after"}, receive(t, requests).UserTexts)
			assert.Equal(t, []event.Event{
				event.TurnStart{WorkID: w.ID(), Prompt: "hold"},
				old,
				event.TurnStart{WorkID: fresh.WorkID, Prompt: "after"},
				fresh,
			}, lifecycle(untimed(h.rec.all())))
		})
	}
}

func TestSecondInterruptHandsBackLaterInput(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	h, _ := openHarness(t, Config{Tools: []tool.Tool{releaseTool(started, release)}})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Hold", "call-1")), pigruntimetest.Text("unused")})
	ends := turnEnds(h.c)

	w, err := h.c.Submit(t.Context(), Main, "hold")
	require.NoError(t, err)
	wait(t, started)
	_, err = h.c.Submit(t.Context(), Main, "before")
	require.NoError(t, err)
	require.True(t, h.c.Interrupt(Main))
	got, err := h.c.Submit(t.Context(), Main, "after")
	require.NoError(t, err)
	assert.Nil(t, got)
	require.True(t, h.c.Interrupt(Main))
	close(release)

	assert.Equal(t, event.TurnEnd{WorkID: w.ID(), Outcome: event.Outcome{Status: event.Interrupted}, Unsent: []event.Input{{Text: "before"}, {Text: "after"}}}, receiveEnd(t, ends))
	wait(t, w.Done())
	assert.Equal(t, []event.Event{
		event.TurnStart{WorkID: w.ID(), Prompt: "hold"},
		event.TurnEnd{WorkID: w.ID(), Outcome: event.Outcome{Status: event.Interrupted}, Unsent: []event.Input{{Text: "before"}, {Text: "after"}}},
	}, lifecycle(untimed(h.rec.all())))
	assert.Equal(t, 1, h.faux.CallCount())
}

func TestBadImageRefusesInput(t *testing.T) {
	store := historyStore(t)
	started, release := make(chan struct{}), make(chan struct{})
	h, _ := openHarness(t, Config{Tools: []tool.Tool{releaseTool(started, release)}, History: store})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Hold", "call-1")), pigruntimetest.Text("done")})
	bad := badImages(t)

	for name, path := range bad {
		_, err := h.c.Submit(t.Context(), Main, "look [image #1]", path)
		require.ErrorIs(t, err, ErrBadImage, name)
		require.ErrorIs(t, h.c.SendNow(t.Context(), Main, "look [image #1]", path), ErrBadImage, name)
	}
	assert.Zero(t, h.faux.CallCount())
	assert.Empty(t, ofType[event.TurnStart](h.rec.all()))

	w, err := h.c.Submit(t.Context(), Main, "hold")
	require.NoError(t, err)
	wait(t, started)
	for name, path := range bad {
		_, err := h.c.Submit(t.Context(), Main, "queued [image #1]", path)
		require.ErrorIs(t, err, ErrBadImage, name)
	}
	assert.Nil(t, h.c.TakeQueued(Main))
	close(release)
	wait(t, w.Done())
	assert.Equal(t, []string{"hold"}, h.c.History())
	assert.Empty(t, ofType[event.QueueChanged](h.rec.all()))
}

func TestControlOnUnknownTargetValidatesImagesBeforeRecordingAndRunning(t *testing.T) {
	for _, now := range []bool{false, true} {
		t.Run(map[bool]string{false: "submit", true: "send-now"}[now], func(t *testing.T) {
			h, _ := openHarness(t, Config{History: historyStore(t)})
			submit := func(images ...string) error {
				if now {
					return h.c.SendNow(t.Context(), "t99", "/execute", images...)
				}
				w, err := h.c.Submit(t.Context(), "t99", "/execute", images...)
				assert.Nil(t, w)
				return err
			}

			require.ErrorIs(t, submit(filepath.Join(t.TempDir(), "missing.png")), ErrBadImage)
			assert.Empty(t, h.c.History())
			assert.Equal(t, policy.Plan, h.c.Status().Mode)
			require.NoError(t, submit())
			assert.Equal(t, []string{"/execute"}, h.c.History())
			assert.Equal(t, policy.Execute, h.c.Status().Mode)

			require.NoError(t, h.c.Close(t.Context()))
			require.ErrorIs(t, submit(), ErrNoSession)
			w, err := h.c.Submit(t.Context(), "t99", "  ")
			require.NoError(t, err)
			assert.Nil(t, w)
		})
	}
}

func TestImageReachesProvider(t *testing.T) {
	h, _ := openHarness(t, Config{})
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "a square")})
	path := pngFile(t)
	data, err := os.ReadFile(path)
	require.NoError(t, err)

	w, err := h.c.Submit(t.Context(), Main, "what is [image #1]", path)
	require.NoError(t, err)
	wait(t, w.Done())

	req := receive(t, requests)
	assert.Equal(t, []string{"what is [image #1]"}, req.UserTexts)
	assert.Contains(t, req.Raw, base64.StdEncoding.EncodeToString(data))
	assert.Equal(t, []event.TurnStart{{WorkID: w.ID(), Prompt: "what is [image #1]"}}, ofType[event.TurnStart](h.rec.all()))
}

func TestToggleModeSwitchesBothWays(t *testing.T) {
	h, _ := openHarness(t, Config{})
	h.c.ToggleMode()
	assert.Equal(t, policy.Execute, h.c.Status().Mode)
	h.c.ToggleMode()
	assert.Equal(t, policy.Plan, h.c.Status().Mode)
	assert.Equal(t, []event.ModeChanged{{Mode: policy.Execute}, {Mode: policy.Plan}}, ofType[event.ModeChanged](h.rec.all()))

	require.NoError(t, h.c.Close(t.Context()))
	delivered := len(h.rec.all())
	mode, gen := h.c.authority.Snapshot()
	h.c.ToggleMode()
	gotMode, gotGen := h.c.authority.Snapshot()
	assert.Equal(t, mode, gotMode)
	assert.Equal(t, gen, gotGen)
	assert.Len(t, h.rec.all(), delivered)
}

func historyStore(t *testing.T) *history.Store {
	t.Helper()
	s, err := history.Open(filepath.Join(t.TempDir(), "history.jsonl"), "/work", 100)
	require.NoError(t, err)
	return s
}

func TestInputIsRecordedInHistory(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	h, _ := openHarness(t, Config{Tools: []tool.Tool{releaseTool(started, release)}, History: historyStore(t)})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Hold", "call-1")), pigruntimetest.Text("done")})

	w, err := h.c.Submit(t.Context(), Main, "hold")
	require.NoError(t, err)
	wait(t, started)
	for _, input := range []string{"queued", "/plan", "/plan", "  "} {
		_, err := h.c.Submit(t.Context(), Main, input)
		require.NoError(t, err, input)
	}
	_, err = h.c.Submit(t.Context(), Main, "/nope")
	require.ErrorIs(t, err, ErrUnknownCommand)
	require.NoError(t, h.c.SendNow(t.Context(), Main, ""))
	h.c.ToggleMode()
	close(release)
	wait(t, w.Done())

	assert.Equal(t, []string{"hold", "queued", "/plan", "/nope"}, h.c.History())
}

func TestUnwritableHistoryNeverFailsSubmit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	store, err := history.Open(path, "/work", 100)
	require.NoError(t, err)
	// A directory where the file belongs makes every append fail.
	require.NoError(t, os.Mkdir(path, 0o700))
	h, _ := openHarness(t, Config{History: store})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("answer")})

	assert.Equal(t, event.Outcome{Status: event.Completed}, h.turn(t, "hello").Outcome())
}

func TestHistoryIsNilWithoutStore(t *testing.T) {
	h, _ := openHarness(t, Config{})
	_, err := h.c.Submit(t.Context(), Main, "/plan")
	require.NoError(t, err)
	assert.Nil(t, h.c.History())
}

func TestFilesListsWorkspaceEntries(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "src"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "src", "main.go"), nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), nil, 0o600))
	ws, err := tool.NewWorkspace(dir, nil)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, ws.Close()) })
	h := newHarness(t, pigruntimetest.NewSandbox(t), Config{Files: ws})

	cs, err := h.c.Files()
	require.NoError(t, err)
	assert.Equal(t, []Candidate{{Value: "notes.txt"}, {Value: "src/"}, {Value: "src/main.go"}}, cs)

	cs, err = newHarness(t, pigruntimetest.NewSandbox(t), Config{}).c.Files()
	require.NoError(t, err)
	assert.Nil(t, cs)
}

func TestUnknownCommandNeverReachesProvider(t *testing.T) {
	h, _ := openHarness(t, Config{})
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "ok")})

	w, err := h.c.Submit(t.Context(), Main, "/frobnicate now")
	require.ErrorIs(t, err, ErrUnknownCommand)
	assert.Contains(t, err.Error(), "/frobnicate")
	assert.Nil(t, w)
	w, err = h.c.Submit(t.Context(), Main, "   ")
	require.NoError(t, err)
	assert.Nil(t, w)
	assert.Zero(t, h.faux.CallCount())

	h.turn(t, "//hello")
	req := receive(t, requests)
	assert.Equal(t, []string{"/hello"}, req.UserTexts)
}

func TestPromptIsNeverACommand(t *testing.T) {
	h, _ := openHarness(t, Config{History: historyStore(t)})
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "ok")})

	w, err := h.c.Prompt(t.Context(), "/execute now")
	require.NoError(t, err)
	require.NotNil(t, w)
	wait(t, w.Done())

	assert.Equal(t, []string{"/execute now"}, receive(t, requests).UserTexts)
	assert.Equal(t, policy.Plan, h.c.Status().Mode)
	assert.Empty(t, ofType[event.ModeChanged](h.rec.all()))
	assert.Empty(t, h.c.History())
}

func TestModelOutputCannotChangeMode(t *testing.T) {
	echo := testTool("Echo", policy.Read, func(context.Context, json.RawMessage) (string, error) {
		return "/execute", nil
	})
	h, _ := openHarness(t, Config{Tools: []tool.Tool{echo}})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Echo", "call-1")), pigruntimetest.Text("/execute")})

	h.turn(t, "try to escalate")

	evs := h.rec.all()
	assert.Equal(t, "/execute", ofType[event.ToolEnd](evs)[0].Output)
	assert.Equal(t, policy.Plan, h.c.Status().Mode)
	assert.Empty(t, ofType[event.ModeChanged](evs))
}

func TestNewSessionIsRefusedWhileWorkWillNotStop(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	h, _ := openHarness(t, Config{Tools: []tool.Tool{releaseTool(started, release)}, JoinTimeout: 50 * time.Millisecond})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Hold", "call-1")), pigruntimetest.Text("done")})
	oldID := h.c.Status().SessionID

	w, err := h.c.Submit(t.Context(), Main, "hold")
	require.NoError(t, err)
	wait(t, started)

	require.ErrorIs(t, h.c.NewSession(t.Context()), ErrWorkStillRunning)
	status := h.c.Status()
	assert.Equal(t, oldID, status.SessionID)
	assert.True(t, status.Busy)

	close(release)
	wait(t, w.Done())
	require.NoError(t, h.c.NewSession(t.Context()))

	evs := untimed(h.rec.all())
	changedAt := slices.IndexFunc(evs, func(ev event.Event) bool {
		changed, ok := ev.(event.SessionChanged)
		return ok && changed.SessionID != oldID
	})
	require.Positive(t, changedAt)
	assert.Equal(t, event.SessionChanged{SessionID: h.c.Status().SessionID}, evs[changedAt])
	assert.NotEqual(t, oldID, h.c.Status().SessionID)
	assert.Len(t, evs, changedAt+1, "no event may follow the switch")
	assert.Equal(t, []event.TurnEnd{{WorkID: w.ID(), Outcome: event.Outcome{Status: event.Interrupted}}}, ofType[event.TurnEnd](evs))
	assert.Empty(t, ofType[event.Error](evs))
}

func TestNewSessionStopsRunningTurn(t *testing.T) {
	started := make(chan struct{})
	block := testTool("Block", policy.Read, func(ctx context.Context, _ json.RawMessage) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	})
	h, _ := openHarness(t, Config{Tools: []tool.Tool{block}})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Block", "call-1")), pigruntimetest.Text("unreached")})
	oldID := h.c.Status().SessionID

	w, err := h.c.Submit(t.Context(), Main, "block")
	require.NoError(t, err)
	wait(t, started)
	require.NoError(t, h.c.NewSession(t.Context()))

	select {
	case <-w.Done():
	default:
		t.Fatal("NewSession returned before the turn ended")
	}
	assert.Equal(t, event.Outcome{Status: event.Interrupted}, w.Outcome())
	newID := h.c.Status().SessionID
	assert.NotEqual(t, oldID, newID)
	evs := untimed(h.rec.all())
	endAt := slices.IndexFunc(evs, func(ev event.Event) bool {
		return assert.ObjectsAreEqual(event.TurnEnd{WorkID: w.ID(), Outcome: event.Outcome{Status: event.Interrupted}}, ev)
	})
	// SessionChanged holds a slice, so it cannot be found with ==.
	changedAt := slices.IndexFunc(evs, func(ev event.Event) bool {
		return assert.ObjectsAreEqual(event.SessionChanged{SessionID: newID}, ev)
	})
	require.GreaterOrEqual(t, endAt, 0)
	assert.Less(t, endAt, changedAt)

	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("fresh")})
	assert.Equal(t, event.Outcome{Status: event.Completed}, h.turn(t, "after switch").Outcome())
}

func TestPromptDuringSessionSwitchIsRefused(t *testing.T) {
	h, _ := openHarness(t, Config{})
	oldID := h.c.Status().SessionID
	reached, unblock := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.c.Subscribe(func(ev event.Event) {
		if changed, ok := ev.(event.SessionChanged); ok && changed.SessionID != oldID {
			once.Do(func() {
				close(reached)
				<-unblock
			})
		}
	})

	_, err := h.c.Submit(t.Context(), Main, "/new")
	require.NoError(t, err)
	wait(t, reached)
	w, err := h.c.Submit(t.Context(), Main, "too early")
	close(unblock)
	assert.ErrorIs(t, err, ErrBusy)
	assert.Nil(t, w)
	assertSwitchDone(t, h)
	assert.Zero(t, h.faux.CallCount())
}

// assertSwitchDone waits for a switch started by a command to finish: start
// clears the active operation before it releases life.
func assertSwitchDone(t *testing.T, h *harness) {
	t.Helper()
	h.c.life.Lock()
	defer h.c.life.Unlock()
	h.c.mu.Lock()
	defer h.c.mu.Unlock()
	assert.Nil(t, h.c.switching)
}

// subscribeTo delivers every event of type T.
func subscribeTo[T event.Event](c *Controller) <-chan T {
	ch := make(chan T, 8)
	c.Subscribe(func(ev event.Event) {
		if e, ok := ev.(T); ok {
			ch <- e
		}
	})
	return ch
}

func receiveEvent[T event.Event](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(waitLimit):
		var zero T
		t.Fatalf("timed out waiting for %T", zero)
		return zero
	}
}

func TestSwitchCommandReturnsAtOnceAndRefusesInputUntilDone(t *testing.T) {
	h, _ := openHarness(t, Config{})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("fresh")})
	oldID := h.c.Status().SessionID
	changes := subscribeTo[event.SessionChanged](h.c)

	// Holding life keeps the switch from starting; asserts only, so a failure
	// cannot leave it held.
	h.c.life.Lock()
	_, err := h.c.Submit(t.Context(), Main, "/new")
	assert.NoError(t, err)
	w, err := h.c.Submit(t.Context(), Main, "too early")
	assert.ErrorIs(t, err, ErrBusy)
	assert.Nil(t, w)
	_, err = h.c.Submit(t.Context(), Main, "/new")
	assert.ErrorIs(t, err, ErrBusy)
	h.c.life.Unlock()

	changed := receiveEvent(t, changes)
	assert.NotEqual(t, oldID, changed.SessionID)
	assertSwitchDone(t, h)
	assert.Equal(t, changed.SessionID, h.c.Status().SessionID)
	assert.Equal(t, event.Outcome{Status: event.Completed}, h.turn(t, "after switch").Outcome())
	assert.Equal(t, 1, h.faux.CallCount())
	assert.Len(t, ofType[event.SessionChanged](h.rec.all()), 2, "the refused /new switched nothing")
}

func TestFailedResumeCommandArrivesAsError(t *testing.T) {
	h, _ := openHarness(t, Config{})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("still here")})
	id := h.c.Status().SessionID
	errs := subscribeTo[event.Error](h.c)

	_, err := h.c.Submit(t.Context(), Main, "/resume no-such-session")
	require.NoError(t, err)
	require.ErrorIs(t, receiveEvent(t, errs).Err, pigruntime.ErrSessionNotFound)

	assert.Equal(t, id, h.c.Status().SessionID)
	assert.Equal(t, event.Outcome{Status: event.Completed}, h.turn(t, "after").Outcome())
}

func TestSessionStartEntersPlanByDefault(t *testing.T) {
	h, sb := openHarness(t, Config{})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("first answer")})
	h.turn(t, "first question")
	savedID := h.c.Status().SessionID

	h.c.EnterExecute()
	require.NoError(t, h.c.NewSession(t.Context()))
	assert.Equal(t, policy.Plan, h.c.Status().Mode)
	assert.Equal(t, []event.ModeChanged{{Mode: policy.Execute}, {Mode: policy.Plan}}, ofType[event.ModeChanged](h.rec.all()))

	h.c.EnterExecute()
	require.NoError(t, h.c.Close(t.Context()))

	fresh := newHarness(t, sb, Config{})
	require.NoError(t, fresh.c.Open(t.Context(), OpenRequest{ResumeID: savedID}))
	changed := ofType[event.SessionChanged](fresh.rec.all())
	require.Len(t, changed, 1)
	assert.Equal(t, savedID, changed[0].SessionID)
	assert.True(t, changed[0].Resumed)
	assert.Positive(t, changed[0].MessageCount)
	assert.Equal(t, policy.Plan, fresh.c.Status().Mode)
}

func TestResumeEntersPlanByDefault(t *testing.T) {
	h, _ := openHarness(t, Config{})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("first answer")})
	h.turn(t, "first question")
	savedID := h.c.Status().SessionID
	require.NoError(t, h.c.NewSession(t.Context()))

	h.c.EnterExecute()
	require.NoError(t, h.c.Resume(t.Context(), savedID))

	status := h.c.Status()
	assert.Equal(t, savedID, status.SessionID)
	assert.Equal(t, policy.Plan, status.Mode)
	assert.Positive(t, status.MessageCount)
}

func TestNewAndResumeCommandsStartInConfiguredMode(t *testing.T) {
	h, _ := openHarness(t, Config{Mode: policy.Execute})
	assert.Equal(t, policy.Execute, h.c.Status().Mode)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("first answer")})
	h.turn(t, "first question")
	savedID := h.c.Status().SessionID
	changes := subscribeTo[event.SessionChanged](h.c)

	h.c.EnterPlan()
	_, err := h.c.Submit(t.Context(), Main, "/new")
	require.NoError(t, err)
	assert.NotEqual(t, savedID, receiveEvent(t, changes).SessionID)
	assertSwitchDone(t, h)
	assert.Equal(t, policy.Execute, h.c.Status().Mode)

	h.c.EnterPlan()
	_, err = h.c.Submit(t.Context(), Main, "/resume "+savedID)
	require.NoError(t, err)
	assert.Equal(t, savedID, receiveEvent(t, changes).SessionID)
	assertSwitchDone(t, h)
	assert.Equal(t, policy.Execute, h.c.Status().Mode)
	assert.Equal(t, []event.ModeChanged{
		{Mode: policy.Plan}, {Mode: policy.Execute}, {Mode: policy.Plan}, {Mode: policy.Execute},
	}, ofType[event.ModeChanged](h.rec.all()))
}

func TestNewAndResumeCommandsDropExecuteInPlanConfig(t *testing.T) {
	h, _ := openHarness(t, Config{Mode: policy.Plan})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("first answer")})
	h.turn(t, "first question")
	savedID := h.c.Status().SessionID
	changes := subscribeTo[event.SessionChanged](h.c)

	h.c.EnterExecute()
	_, err := h.c.Submit(t.Context(), Main, "/new")
	require.NoError(t, err)
	assert.NotEqual(t, savedID, receiveEvent(t, changes).SessionID)
	assertSwitchDone(t, h)
	assert.Equal(t, policy.Plan, h.c.Status().Mode)

	h.c.EnterExecute()
	_, err = h.c.Submit(t.Context(), Main, "/resume "+savedID)
	require.NoError(t, err)
	assert.Equal(t, savedID, receiveEvent(t, changes).SessionID)
	assertSwitchDone(t, h)
	assert.Equal(t, policy.Plan, h.c.Status().Mode)
}

func TestBareDenyRemovesTool(t *testing.T) {
	var ran atomic.Int32
	sb := pigruntimetest.NewSandbox(t)
	read := readTool(t, sb)
	rules, err := policy.NewRules(sb.Workspace, sb.Home, map[string]policy.ToolSpec{"Read": {}, "Exec": {}}, policy.Source{Name: "test", Deny: []string{"Exec"}})
	require.NoError(t, err)
	h := newHarness(t, sb, Config{Tools: []tool.Tool{read, execTool(&ran)}, Rules: rules})
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "done")})

	h.turn(t, "what can you use?")

	req := receive(t, requests)
	assert.Equal(t, []string{"Read", "AskUserQuestion"}, req.ToolNames)
	assert.Equal(t, joinPrompt(buildPrompt(sb.Workspace, []tool.Tool{read, tool.NewAskUserQuestion(nil)}, nil, nil, false, nil)), req.SystemPrompt)
	assert.NotContains(t, req.SystemPrompt, "Exec")
}

func TestStatusReportsShell(t *testing.T) {
	var ran atomic.Int32
	sb := pigruntimetest.NewSandbox(t)
	exec := execTool(&ran)
	exec.Sandboxed = true
	h := newHarness(t, sb, Config{Tools: []tool.Tool{readTool(t, sb), exec}})
	s := h.c.Status()
	assert.True(t, s.Shell)
	assert.True(t, s.Sandboxed)

	unsandboxedSB := pigruntimetest.NewSandbox(t)
	unsandboxed := execTool(&ran)
	unsandboxed.Sandboxed = false
	s = newHarness(t, unsandboxedSB, Config{Tools: []tool.Tool{readTool(t, unsandboxedSB), unsandboxed}}).c.Status()
	assert.True(t, s.Shell)
	assert.False(t, s.Sandboxed)

	denySB := pigruntimetest.NewSandbox(t)
	rules, err := policy.NewRules(denySB.Workspace, denySB.Home, map[string]policy.ToolSpec{"Exec": {}}, policy.Source{Name: "test", Deny: []string{"Exec"}})
	require.NoError(t, err)
	denied := newHarness(t, denySB, Config{Tools: []tool.Tool{exec}, Rules: rules})
	assert.False(t, denied.c.Status().Shell)
}

func TestOpenEmitsConfigErrorsAfterTheSession(t *testing.T) {
	first, second := errors.New("first"), errors.New("second")
	h, _ := openHarness(t, Config{Errors: []error{first, second}})

	evs := h.rec.all()
	i := slices.IndexFunc(evs, func(ev event.Event) bool { _, ok := ev.(event.SessionChanged); return ok })
	require.GreaterOrEqual(t, i, 0)
	assert.Equal(t, []event.Error{{Err: first}, {Err: second}}, ofType[event.Error](evs[i:]))
	assert.Empty(t, ofType[event.Error](evs[:i]))

	require.NoError(t, h.c.NewSession(t.Context()))
	assert.Len(t, ofType[event.Error](h.rec.all()), 2, "a later session does not repeat them")
}

func TestOpenUnsetModelReportsTheDefaultAfterTheSession(t *testing.T) {
	first := errors.New("first")
	h := newUnsetHarness(t, Config{Errors: []error{first}}, true)

	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))

	const using = `Using kopi-faux/faux-1. Set "model" in /kopi/config.json to choose another.`
	assert.Equal(t, "kopi-faux/faux-1", h.c.Model())
	assert.Equal(t, "kopi-faux/faux-1", h.c.SmallModel())
	assert.Equal(t, "kopi-faux/faux-1", h.c.Status().Model)
	evs := h.rec.all()
	i := slices.IndexFunc(evs, func(ev event.Event) bool { _, ok := ev.(event.SessionChanged); return ok })
	require.GreaterOrEqual(t, i, 0)
	assert.Equal(t, []event.Event{event.Notice{Text: using}, event.Error{Err: first}}, evs[i+1:])
}

func TestOpenUnsetModelWithoutCredentialFails(t *testing.T) {
	h := newUnsetHarness(t, Config{}, false)

	require.ErrorIs(t, h.c.Open(t.Context(), OpenRequest{}), pigruntime.ErrNoCredential)
	assert.Empty(t, h.c.Model())
}

func TestOpenConfiguredModelReportsNoNotice(t *testing.T) {
	h, _ := openHarness(t, Config{ConfigFile: "/kopi/config.json"})

	assert.Empty(t, ofType[event.Notice](h.rec.all()))
	assert.Equal(t, "kopi-faux/faux-1", h.c.Model())
}

func TestSmallModelFollowsConfigThenSessionModel(t *testing.T) {
	h, _ := openHarness(t, Config{SmallModel: "other/small-1"})
	assert.Equal(t, "other/small-1", h.c.SmallModel())
	assert.Equal(t, "kopi-faux/faux-1", h.c.Model())
}

func searchSandbox(t *testing.T) pigruntimetest.Sandbox {
	t.Helper()
	sb := pigruntimetest.NewSandbox(t)
	models := `{"providers":{"kopi-search":{"baseUrl":"http://127.0.0.1:1","api":"openai-responses","apiKey":"synthetic-key","models":[{"id":"search-1"}]}}}`
	require.NoError(t, os.WriteFile(filepath.Join(sb.AgentDir, "models.json"), []byte(models), 0o600))
	return sb
}

func TestWebSearchIsOfferedForASearchingSmallModel(t *testing.T) {
	h := newHarness(t, searchSandbox(t), Config{SmallModel: "kopi-search/search-1"})
	assert.Equal(t, []string{"AskUserQuestion", "WebSearch"}, toolNames(h.c.tools))
}

func TestWebSearchIsAbsentForANonSearchingModel(t *testing.T) {
	h := newHarness(t, searchSandbox(t), Config{})
	assert.Equal(t, []string{"AskUserQuestion"}, toolNames(h.c.tools))
}

func TestWebSearchIsAbsentWhenRulesRemoveIt(t *testing.T) {
	sb := searchSandbox(t)
	rules, err := policy.NewRules(sb.Workspace, sb.Home, map[string]policy.ToolSpec{"WebSearch": {}}, policy.Source{Name: "test", Deny: []string{"WebSearch"}})
	require.NoError(t, err)

	h := newHarness(t, sb, Config{SmallModel: "kopi-search/search-1", Rules: rules})

	assert.Equal(t, []string{"AskUserQuestion"}, toolNames(h.c.tools))
}

func serverTool(name string) tool.Tool {
	t := testTool(name, policy.Read, func(context.Context, json.RawMessage) (string, error) { return "", nil })
	t.Deferred = true
	return t
}

func toolNames(tools []tool.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	return names
}

func servers(statuses ...pigruntime.ServerStatus) func(context.Context) ([]pigruntime.ServerStatus, error) {
	return func(context.Context) ([]pigruntime.ServerStatus, error) { return statuses, nil }
}

func TestOpenWaitsForServersAndAddsTheirTools(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	read, mcp := readTool(t, sb), serverTool("mcp__s__t")
	h := newHarness(t, sb, Config{
		Tools:   []tool.Tool{read},
		Servers: servers(pigruntime.ServerStatus{Name: "s", Tools: []tool.Tool{mcp}, Instructions: "use t wisely"}),
	})
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	requests := make(chan pigruntimetest.Request, 2)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "done"), pigruntimetest.Capture(t, requests, "done")})

	h.turn(t, "what can you use?")

	req := receive(t, requests)
	assert.Equal(t, []string{"Read", "AskUserQuestion", "ToolSearch"}, req.ToolNames)
	search := tool.NewToolSearch(nil, nil, nil)
	assert.Equal(t, joinPrompt(buildPrompt(sb.Workspace, []tool.Tool{read, tool.NewAskUserQuestion(nil), search, mcp}, nil, []mcpServer{{name: "s", instructions: "use t wisely", tools: []string{mcp.Name}}}, false, nil)), req.SystemPrompt)
	assert.Contains(t, req.SystemPrompt, "use t wisely")
	assert.Contains(t, req.SystemPrompt, "MCP server s tools: mcp__s__t.")

	require.NoError(t, h.c.NewSession(t.Context()))
	h.turn(t, "and now?")
	again := receive(t, requests)
	assert.Equal(t, req.ToolNames, again.ToolNames, "a later session keeps the server tools")
	assert.Equal(t, req.SystemPrompt, again.SystemPrompt)
}

func TestOpenReportsServerFailuresAsErrors(t *testing.T) {
	first, broken := errors.New("first"), errors.New("no such command")
	h, _ := openHarness(t, Config{
		Errors: []error{first},
		Servers: servers(
			pigruntime.ServerStatus{Name: "broken", Err: broken},
			pigruntime.ServerStatus{Name: "s", Tools: []tool.Tool{serverTool("mcp__s__t")}, Skipped: []string{"bad: name longer than 64 characters"}},
		),
	})

	errs := ofType[event.Error](h.rec.all())
	require.Len(t, errs, 2)
	assert.Equal(t, first, errs[0].Err)
	require.ErrorIs(t, errs[1].Err, broken)
	assert.EqualError(t, errs[1].Err, "MCP server broken failed to start: no such command")
	assert.Equal(t, []event.Notice{{Text: "MCP server s: skipped tool bad: name longer than 64 characters"}}, ofType[event.Notice](h.rec.all()))
}

func TestOpenFailsWhenServersAreInterrupted(t *testing.T) {
	interrupted := errors.New("startup was interrupted")
	h := newHarness(t, pigruntimetest.NewSandbox(t), Config{
		Servers: func(context.Context) ([]pigruntime.ServerStatus, error) { return nil, interrupted },
	})

	require.ErrorIs(t, h.c.Open(t.Context(), OpenRequest{}), interrupted)

	assert.Empty(t, h.c.Status().SessionID)
	assert.Empty(t, ofType[event.SessionChanged](h.rec.all()))
}

// heldServers is a Servers that blocks until release is closed, then returns
// statuses and err; cancelled is closed if its ctx ends first.
type heldServers struct {
	statuses           []pigruntime.ServerStatus
	err                error
	release, cancelled chan struct{}
}

func holdServers(statuses ...pigruntime.ServerStatus) *heldServers {
	return &heldServers{statuses: statuses, release: make(chan struct{}), cancelled: make(chan struct{})}
}

func (s *heldServers) wait(ctx context.Context) ([]pigruntime.ServerStatus, error) {
	select {
	case <-s.release:
		return s.statuses, s.err
	case <-ctx.Done():
		close(s.cancelled)
		return nil, ctx.Err()
	}
}

func TestStartQueuesPromptsUntilTheSessionOpens(t *testing.T) {
	srv := holdServers()
	h := newHarness(t, pigruntimetest.NewSandbox(t), Config{Servers: srv.wait})
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "both answered")})
	queued := subscribeTo[event.QueueChanged](h.c)
	starts := subscribeTo[event.TurnStart](h.c)
	ends := turnEnds(h.c)

	require.NoError(t, h.c.Start(t.Context(), OpenRequest{}))
	for i, want := range [][]string{{"first"}, {"first", "second"}} {
		w, err := h.c.Submit(t.Context(), Main, want[i])
		require.NoError(t, err)
		assert.Nil(t, w)
		assert.Equal(t, want, receiveEvent(t, queued).Pending)
	}
	status := h.c.Status()
	assert.Empty(t, status.SessionID)
	assert.False(t, status.Busy)
	assert.False(t, h.c.Interrupt(Main))

	close(srv.release)
	assert.Equal(t, "first\n\nsecond", receiveEvent(t, starts).Prompt)
	assert.Empty(t, receiveEvent(t, queued).Pending)
	assert.Equal(t, event.Outcome{Status: event.Completed}, receiveEnd(t, ends).Outcome)
	assert.Equal(t, []string{"first\n\nsecond"}, receive(t, requests).UserTexts)
	assert.Equal(t, 1, h.faux.CallCount())
	assert.NotEmpty(t, h.c.Status().SessionID)
}

func TestSendNowWhileStartingQueues(t *testing.T) {
	srv := holdServers()
	h := newHarness(t, pigruntimetest.NewSandbox(t), Config{Servers: srv.wait})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("answered")})
	queued := subscribeTo[event.QueueChanged](h.c)
	starts := subscribeTo[event.TurnStart](h.c)
	ends := turnEnds(h.c)

	require.NoError(t, h.c.Start(t.Context(), OpenRequest{}))
	require.NoError(t, h.c.SendNow(t.Context(), Main, "now"))
	assert.Equal(t, []string{"now"}, receiveEvent(t, queued).Pending)

	close(srv.release)
	assert.Equal(t, "now", receiveEvent(t, starts).Prompt)
	assert.Equal(t, event.Outcome{Status: event.Completed}, receiveEnd(t, ends).Outcome)
}

func TestStartEmitsConnectingActivityBeforeTheSession(t *testing.T) {
	first := errors.New("first")
	srv := holdServers(pigruntime.ServerStatus{Name: "broken", Err: errors.New("no such command")})
	h := newHarness(t, pigruntimetest.NewSandbox(t), Config{Errors: []error{first}, Servers: srv.wait})
	changes := subscribeTo[event.SessionChanged](h.c)

	require.NoError(t, h.c.Start(t.Context(), OpenRequest{}))
	assert.Equal(t, []event.Activity{{Text: "Connecting to MCP servers…"}}, ofType[event.Activity](h.rec.all()))
	close(srv.release)
	receiveEvent(t, changes)
	assertSwitchDone(t, h)

	evs := h.rec.all()
	i := slices.IndexFunc(evs, func(ev event.Event) bool { _, ok := ev.(event.SessionChanged); return ok })
	require.GreaterOrEqual(t, i, 0)
	assert.Equal(t, []event.Activity{{Text: "Connecting to MCP servers…"}, {}}, ofType[event.Activity](evs[:i]))
	assert.Empty(t, ofType[event.Activity](evs[i:]))
	assert.Empty(t, ofType[event.Error](evs[:i]))
	errs := ofType[event.Error](evs[i:])
	require.Len(t, errs, 2)
	assert.Equal(t, first, errs[0].Err)
	assert.EqualError(t, errs[1].Err, "MCP server broken failed to start: no such command")

	bare := newHarness(t, pigruntimetest.NewSandbox(t), Config{})
	changes = subscribeTo[event.SessionChanged](bare.c)
	require.NoError(t, bare.c.Start(t.Context(), OpenRequest{}))
	receiveEvent(t, changes)
	assertSwitchDone(t, bare)
	assert.Empty(t, ofType[event.Activity](bare.rec.all()), "without servers there is nothing to connect to")
}

func TestModeChosenWhileStartingCarriesIntoTheSession(t *testing.T) {
	srv := holdServers()
	h := newHarness(t, pigruntimetest.NewSandbox(t), Config{Mode: policy.Plan, Servers: srv.wait})
	changes := subscribeTo[event.SessionChanged](h.c)

	require.NoError(t, h.c.Start(t.Context(), OpenRequest{}))
	assert.Equal(t, policy.Plan, h.c.Status().Mode)
	_, err := h.c.Submit(t.Context(), Main, "/execute")
	require.NoError(t, err)
	h.c.ToggleMode()
	h.c.ToggleMode()
	assert.Equal(t, policy.Execute, h.c.Status().Mode)

	close(srv.release)
	receiveEvent(t, changes)
	assertSwitchDone(t, h)
	assert.Equal(t, policy.Execute, h.c.Status().Mode)
	assert.Equal(t, []event.ModeChanged{{Mode: policy.Execute}, {Mode: policy.Plan}, {Mode: policy.Execute}}, ofType[event.ModeChanged](h.rec.all()))
}

func TestNewAfterFailedStartOpensInConfiguredMode(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	prior := newHarness(t, sb, Config{})
	require.NoError(t, prior.c.Open(t.Context(), OpenRequest{}))
	prior.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("hi")})
	prior.turn(t, "hi")
	id := prior.c.Status().SessionID
	require.NoError(t, prior.c.Close(t.Context()))

	srv := holdServers()
	h := newHarness(t, sb, Config{Mode: policy.Plan, Servers: srv.wait})
	errs := subscribeTo[event.Error](h.c)
	changes := subscribeTo[event.SessionChanged](h.c)
	require.NoError(t, h.c.Start(t.Context(), OpenRequest{ResumeID: id}))
	// The session passes the check, then is gone by the time it is opened.
	require.NoError(t, os.RemoveAll(filepath.Join(sb.AgentDir, "sessions")))
	close(srv.release)
	require.ErrorIs(t, receiveEvent(t, errs).Err, pigruntime.ErrSessionNotFound)
	assertSwitchDone(t, h)

	h.c.ToggleMode()
	assert.Equal(t, policy.Execute, h.c.Status().Mode)
	_, err := h.c.Submit(t.Context(), Main, "/new")
	require.NoError(t, err)
	receiveEvent(t, changes)
	assertSwitchDone(t, h)
	assert.Equal(t, policy.Plan, h.c.Status().Mode)
}

func TestSwitchCommandsAreBusyWhileStarting(t *testing.T) {
	srv := holdServers()
	h := newHarness(t, pigruntimetest.NewSandbox(t), Config{Servers: srv.wait})
	changes := subscribeTo[event.SessionChanged](h.c)

	require.NoError(t, h.c.Start(t.Context(), OpenRequest{}))
	for _, cmd := range []string{"/new", "/resume some-session"} {
		_, err := h.c.Submit(t.Context(), Main, cmd)
		assert.ErrorIs(t, err, ErrBusy, cmd)
	}

	close(srv.release)
	receiveEvent(t, changes)
	assertSwitchDone(t, h)
	assert.Len(t, ofType[event.SessionChanged](h.rec.all()), 1)
}

func TestTakeQueuedWhileStarting(t *testing.T) {
	srv := holdServers()
	h := newHarness(t, pigruntimetest.NewSandbox(t), Config{Servers: srv.wait})
	changes := subscribeTo[event.SessionChanged](h.c)

	require.NoError(t, h.c.Start(t.Context(), OpenRequest{}))
	_, err := h.c.Submit(t.Context(), Main, "take me back")
	require.NoError(t, err)
	assert.Equal(t, []event.Input{{Text: "take me back"}}, h.c.TakeQueued(Main))
	assert.Nil(t, h.c.TakeQueued(Main))
	assert.Equal(t, []event.QueueChanged{{Pending: []string{"take me back"}}, {Pending: []string{}}}, ofType[event.QueueChanged](h.rec.all()))

	close(srv.release)
	receiveEvent(t, changes)
	assertSwitchDone(t, h)
	assert.False(t, h.c.Status().Busy)
	assert.Zero(t, h.faux.CallCount())
}

func TestCloseWhileStartingEndsTheWait(t *testing.T) {
	srv := holdServers()
	h := newHarness(t, pigruntimetest.NewSandbox(t), Config{Servers: srv.wait})
	ignore := goleak.IgnoreCurrent()

	require.NoError(t, h.c.Start(t.Context(), OpenRequest{}))
	_, err := h.c.Submit(t.Context(), Main, "never sent")
	require.NoError(t, err)
	require.NoError(t, h.c.Close(t.Context()))
	delivered := len(h.rec.all())
	wait(t, srv.cancelled)
	goleak.VerifyNone(t, ignore)

	assert.Nil(t, h.c.main.sess)
	assert.Empty(t, h.c.Status().SessionID)
	assert.Empty(t, ofType[event.SessionChanged](h.rec.all()))
	assert.Len(t, h.rec.all(), delivered, "no event may be delivered after Close returned")
	assert.Zero(t, h.faux.CallCount())
}

func TestStartReportsFailureAndKeepsQueuedInput(t *testing.T) {
	interrupted := errors.New("startup was interrupted")
	srv := holdServers()
	srv.err = interrupted
	h := newHarness(t, pigruntimetest.NewSandbox(t), Config{Servers: srv.wait})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("fresh")})
	errs := subscribeTo[event.Error](h.c)
	changes := subscribeTo[event.SessionChanged](h.c)

	require.NoError(t, h.c.Start(t.Context(), OpenRequest{}))
	_, err := h.c.Submit(t.Context(), Main, "keep me")
	require.NoError(t, err)
	close(srv.release)
	require.ErrorIs(t, receiveEvent(t, errs).Err, interrupted)
	assertSwitchDone(t, h)
	evs := h.rec.all()
	ended := slices.Index(evs, event.Event(event.Activity{}))
	failed := slices.IndexFunc(evs, func(ev event.Event) bool { _, ok := ev.(event.Error); return ok })
	require.GreaterOrEqual(t, ended, 0)
	assert.Less(t, ended, failed, "the activity ends before the failure is reported")

	assert.Empty(t, h.c.Status().SessionID)
	_, err = h.c.Submit(t.Context(), Main, "no session yet")
	assert.ErrorIs(t, err, ErrNoSession)
	assert.Equal(t, []event.Input{{Text: "keep me"}}, h.c.TakeQueued(Main))

	_, err = h.c.Submit(t.Context(), Main, "/new")
	require.NoError(t, err)
	changed := receiveEvent(t, changes)
	assertSwitchDone(t, h)
	assert.Equal(t, changed.SessionID, h.c.Status().SessionID)
	assert.Equal(t, event.Outcome{Status: event.Completed}, h.turn(t, "after").Outcome())
}

func TestNewAfterFailedStartSendsQueuedInput(t *testing.T) {
	srv := holdServers()
	srv.err = errors.New("startup was interrupted")
	h := newHarness(t, pigruntimetest.NewSandbox(t), Config{Servers: srv.wait})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("kept")})
	errs := subscribeTo[event.Error](h.c)
	starts := subscribeTo[event.TurnStart](h.c)
	ends := turnEnds(h.c)

	require.NoError(t, h.c.Start(t.Context(), OpenRequest{}))
	_, err := h.c.Submit(t.Context(), Main, "keep me")
	require.NoError(t, err)
	close(srv.release)
	require.ErrorIs(t, receiveEvent(t, errs).Err, srv.err)
	assertSwitchDone(t, h)

	_, err = h.c.Submit(t.Context(), Main, "/new")
	require.NoError(t, err)
	assert.Equal(t, "keep me", receiveEvent(t, starts).Prompt)
	assert.Equal(t, event.Outcome{Status: event.Completed}, receiveEnd(t, ends).Outcome)
	assert.NotEmpty(t, h.c.Status().SessionID)
}

func TestStartupEventsWaitForTheFirstSessionThatOpens(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	prior := newHarness(t, sb, Config{})
	require.NoError(t, prior.c.Open(t.Context(), OpenRequest{}))
	prior.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("hi")})
	prior.turn(t, "hi")
	id := prior.c.Status().SessionID
	require.NoError(t, prior.c.Close(t.Context()))

	first, broken := errors.New("first"), errors.New("no such command")
	srv := holdServers(pigruntime.ServerStatus{Name: "broken", Err: broken})
	h := newHarness(t, sb, Config{Errors: []error{first}, Servers: srv.wait})
	errs := subscribeTo[event.Error](h.c)
	require.NoError(t, h.c.Start(t.Context(), OpenRequest{ResumeID: id}))
	// The session passes the check, then is gone by the time it is opened.
	require.NoError(t, os.RemoveAll(filepath.Join(sb.AgentDir, "sessions")))
	close(srv.release)
	require.ErrorIs(t, receiveEvent(t, errs).Err, pigruntime.ErrSessionNotFound)
	assertSwitchDone(t, h)
	require.Len(t, ofType[event.Error](h.rec.all()), 1)

	require.NoError(t, h.c.NewSession(t.Context()))
	evs := h.rec.all()
	i := slices.IndexFunc(evs, func(ev event.Event) bool { _, ok := ev.(event.SessionChanged); return ok })
	require.GreaterOrEqual(t, i, 0)
	after := ofType[event.Error](evs[i:])
	require.Len(t, after, 2)
	assert.Equal(t, first, after[0].Err)
	assert.ErrorIs(t, after[1].Err, broken)

	require.NoError(t, h.c.NewSession(t.Context()))
	assert.Len(t, ofType[event.Error](h.rec.all()), 3, "a later session does not repeat them")
}

func TestBareDenyRemovesServerTool(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	read, kept, denied := readTool(t, sb), serverTool("mcp__s__kept"), serverTool("mcp__s__denied")
	rules, err := policy.NewRules(sb.Workspace, sb.Home, map[string]policy.ToolSpec{"Read": {}}, policy.Source{Name: "test", Deny: []string{denied.Name}})
	require.NoError(t, err)
	h := newHarness(t, sb, Config{
		Tools:   []tool.Tool{read},
		Rules:   rules,
		Servers: servers(pigruntime.ServerStatus{Name: "s", Tools: []tool.Tool{denied, kept}}),
	})
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "done")})

	h.turn(t, "what can you use?")

	req := receive(t, requests)
	assert.Equal(t, []string{"Read", "AskUserQuestion", "ToolSearch"}, req.ToolNames)
	assert.Equal(t, []string{"Read", "AskUserQuestion", "ToolSearch", kept.Name}, toolNames(h.c.tools))
	assert.NotContains(t, req.SystemPrompt, denied.Name)
}

func TestBareDenyRemovesToolSearch(t *testing.T) {
	for _, deny := range []string{"ToolSearch", "*"} {
		t.Run(deny, func(t *testing.T) {
			sb := pigruntimetest.NewSandbox(t)
			mcp := serverTool("mcp__s__t")
			rules, err := policy.NewRules(sb.Workspace, sb.Home, tool.Specs([]tool.Tool{tool.NewToolSearch(nil, nil, nil)}), policy.Source{Name: "test", Deny: []string{deny}})
			require.NoError(t, err)
			h := newHarness(t, sb, Config{
				Rules:   rules,
				Servers: servers(pigruntime.ServerStatus{Name: "s", Tools: []tool.Tool{mcp}}),
			})
			require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
			requests := make(chan pigruntimetest.Request, 1)
			h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "done")})

			h.turn(t, "what can you use?")

			req := receive(t, requests)
			assert.NotContains(t, req.ToolNames, "ToolSearch")
			assert.NotContains(t, req.SystemPrompt, mcp.Name)
			assert.NotContains(t, toolNames(h.c.tools), "ToolSearch")
			assert.Equal(t, deny == "ToolSearch", slices.Contains(toolNames(h.c.tools), "AskUserQuestion"))
		})
	}
}

func TestToolSearchFollowsPermissionRules(t *testing.T) {
	for _, effect := range []string{"allow", "ask"} {
		t.Run(effect, func(t *testing.T) {
			sb := pigruntimetest.NewSandbox(t)
			mcp := serverTool("mcp__s__t")
			source := policy.Source{Name: "test", Allow: []string{"ToolSearch"}}
			if effect == "ask" {
				source.Allow, source.Ask = nil, []string{"ToolSearch"}
			}
			rules, err := policy.NewRules(sb.Workspace, sb.Home, tool.Specs([]tool.Tool{tool.NewToolSearch(nil, nil, nil)}), source)
			require.NoError(t, err)
			h := newHarness(t, sb, Config{
				Rules: rules, Unattended: true,
				Servers: servers(pigruntime.ServerStatus{Name: "s", Tools: []tool.Tool{mcp}}),
			})
			require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
			requests := make(chan pigruntimetest.Request, 1)
			h.faux.SetResponses([]ai.FauxResponseStep{
				pigruntimetest.ToolCalls(pigruntimetest.Call{Name: "ToolSearch", ID: "call-1", Args: map[string]any{"query": mcp.Name}}),
				pigruntimetest.Capture(t, requests, "done"),
			})

			h.turn(t, "load t")

			req := receive(t, requests)
			assert.Contains(t, req.ToolNames, "ToolSearch")
			require.Len(t, req.ToolResults, 1)
			if effect == "ask" {
				assert.Contains(t, req.ToolResults[0], `rule ToolSearch (test) requires approval`)
				assert.NotContains(t, req.ToolNames, mcp.Name)
			} else {
				assert.Contains(t, req.ToolResults[0], "Loaded 1 tool")
				assert.Contains(t, req.ToolNames, mcp.Name)
			}
		})
	}
}

func TestOpenDefersServerTools(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	read, a, b := readTool(t, sb), serverTool("mcp__s__a"), serverTool("mcp__s__b")
	h := newHarness(t, sb, Config{
		Tools:   []tool.Tool{read},
		Servers: servers(pigruntime.ServerStatus{Name: "s", Tools: []tool.Tool{a, b}}),
	})
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	require.Equal(t, []string{"Read", "AskUserQuestion", "ToolSearch", a.Name, b.Name}, toolNames(h.c.tools))
	for i, deferred := range []bool{false, false, false, true, true} {
		assert.Equal(t, deferred, h.c.tools[i].Deferred, h.c.tools[i].Name)
	}
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{
		pigruntimetest.ToolCalls(pigruntimetest.Call{Name: "ToolSearch", ID: "call-1", Args: map[string]any{"query": a.Name}}),
		pigruntimetest.Capture(t, requests, "done"),
	})

	h.turn(t, "load a")

	ends := ofType[event.ToolEnd](h.rec.all())
	require.Len(t, ends, 1)
	assert.False(t, ends[0].IsError, "ToolSearch runs in plan mode without asking")
	assert.ElementsMatch(t, []string{"Read", "AskUserQuestion", "ToolSearch", a.Name}, receive(t, requests).ToolNames)
}

func TestNoToolSearchWithoutServerTools(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	read := readTool(t, sb)
	h := newHarness(t, sb, Config{Tools: []tool.Tool{read}})
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	assert.Equal(t, []string{"Read", "AskUserQuestion"}, toolNames(h.c.tools))
	assert.Contains(t, joinPrompt(h.c.prompt), "\nTools available: Read, AskUserQuestion.\n")
	assert.NotContains(t, joinPrompt(h.c.prompt), "deferred")

	denySB := pigruntimetest.NewSandbox(t)
	denied := serverTool("mcp__s__denied")
	rules, err := policy.NewRules(denySB.Workspace, denySB.Home, map[string]policy.ToolSpec{"Read": {}}, policy.Source{Name: "test", Deny: []string{denied.Name}})
	require.NoError(t, err)
	h = newHarness(t, denySB, Config{
		Tools:   []tool.Tool{readTool(t, denySB)},
		Rules:   rules,
		Servers: servers(pigruntime.ServerStatus{Name: "s", Tools: []tool.Tool{denied}}),
	})
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	assert.Equal(t, []string{"Read", "AskUserQuestion"}, toolNames(h.c.tools))
	assert.Contains(t, joinPrompt(h.c.prompt), "\nTools available: Read, AskUserQuestion.\n")
	assert.NotContains(t, joinPrompt(h.c.prompt), "deferred")
	assert.NotContains(t, joinPrompt(h.c.prompt), "MCP server s tools")
}

func TestResumeWithoutIDIsMissingArgument(t *testing.T) {
	h, _ := openHarness(t, Config{})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("still here")})
	id := h.c.Status().SessionID

	_, err := h.c.Submit(t.Context(), Main, "/resume")
	require.ErrorIs(t, err, ErrMissingArgument)

	assert.Equal(t, id, h.c.Status().SessionID)
	assert.Equal(t, event.Outcome{Status: event.Completed}, h.turn(t, "after").Outcome())
}

func TestOpenErrorsPassThrough(t *testing.T) {
	codex := ai.GetBuiltinModels("openai-codex")
	require.NotEmpty(t, codex)
	for _, tc := range []struct {
		name  string
		model string
		req   OpenRequest
		want  error
		// unattended skips the first-run login that Start would run.
		unattended bool
	}{
		{name: "continue without sessions", req: OpenRequest{Continue: true}, want: pigruntime.ErrSessionNotFound},
		{name: "resume a missing session", req: OpenRequest{ResumeID: "no-such-session"}, want: pigruntime.ErrSessionNotFound},
		{name: "unknown model", model: fauxProvider + "/missing", want: pigruntime.ErrUnknownModel},
		{name: "missing credential", model: "openai-codex/" + codex[0].ID, want: pigruntime.ErrNoCredential, unattended: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, pigruntimetest.NewSandbox(t), Config{
				Model:      tc.model,
				Unattended: tc.unattended,
				Servers: func(context.Context) ([]pigruntime.ServerStatus, error) {
					t.Error("the servers were waited for before the check")
					return nil, nil
				},
			})
			require.ErrorIs(t, h.c.Open(t.Context(), tc.req), tc.want)
			require.ErrorIs(t, h.c.Start(t.Context(), tc.req), tc.want)
			_, err := h.c.Submit(t.Context(), Main, "hello")
			assert.ErrorIs(t, err, ErrNoSession)
			assert.Empty(t, h.rec.all())
		})
	}
}

func TestStatusReportsContextUsageAndBranch(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	h := newHarness(t, sb, Config{})
	assert.Empty(t, h.c.Status().Branch, "the sandbox workspace is in no repository")
	status := h.c.Status()
	assert.Zero(t, status.ContextTokens)
	assert.Zero(t, status.ContextWindow)

	require.NoError(t, os.MkdirAll(filepath.Join(sb.Workspace, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sb.Workspace, ".git", "HEAD"), []byte("ref: refs/heads/feature/status\n"), 0o600))
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("counted")})
	h.turn(t, "count me")

	status = h.c.Status()
	tokens, window, ok := h.c.main.sess.ContextUsage()
	require.True(t, ok)
	assert.Positive(t, tokens)
	assert.Equal(t, tokens, status.ContextTokens)
	assert.Equal(t, window, status.ContextWindow)
	assert.Equal(t, "feature/status", status.Branch)
}

func TestAgentsReportEffortAndUsage(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	require.NoError(t, os.WriteFile(filepath.Join(sb.Workspace, "notes.txt"), []byte("kopi fixture 42\n"), 0o600))
	rt := pigruntimetest.NewRuntime(t, sb)
	thinker := ai.NewFauxProvider(ai.FauxConfig{ProviderID: fauxProvider, Models: []ai.FauxModelDefinition{{ID: "thinker", Reasoning: true}}})
	require.NoError(t, rt.RegisterProvider(thinker.Provider()))
	h := buildHarness(t, rt, thinker, sb, Config{Model: fauxProvider + "/thinker", Tools: []tool.Tool{readTool(t, sb)}})
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	h.faux.SetResponses([]ai.FauxResponseStep{
		pigruntimetest.ToolCalls(pigruntimetest.Call{Name: "Read", ID: "call-1", Args: map[string]any{"file_path": "notes.txt"}}),
		pigruntimetest.Text("counted"),
	})
	h.turn(t, "count me")

	main := h.c.Agents()[0]
	toolCalls, cost := h.c.main.sess.Usage()
	assert.Equal(t, h.c.main.sess.Effort(), main.Effort)
	assert.NotEmpty(t, main.Effort)
	assert.Equal(t, 1, main.ToolCalls)
	assert.Equal(t, toolCalls, main.ToolCalls)
	assert.Equal(t, cost, main.Cost)
}

func TestQuitReturnsErrQuit(t *testing.T) {
	h, _ := openHarness(t, Config{})
	_, err := h.c.Submit(t.Context(), Main, "/quit")
	assert.ErrorIs(t, err, ErrQuit)
}

func TestTurnUsesNoAmbientState(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	sb.PlantDecoys(t)
	require.NoError(t, os.WriteFile(filepath.Join(sb.Workspace, "notes.txt"), []byte("kopi fixture 42\n"), 0o600))
	before := pigruntimetest.Snapshot(t, sb.Home, sb.Workspace)
	tools := []tool.Tool{readTool(t, sb)}
	h := newHarness(t, sb, Config{Tools: tools})
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{
		pigruntimetest.ToolCalls(pigruntimetest.Call{Name: "Read", ID: "call-1", Args: map[string]any{"file_path": "notes.txt"}}),
		pigruntimetest.Capture(t, requests, "done"),
	})

	h.turn(t, "read notes.txt")
	require.NoError(t, h.c.Close(t.Context()))

	req := receive(t, requests)
	assert.Equal(t, []string{"Read", "AskUserQuestion"}, req.ToolNames)
	assert.Equal(t, joinPrompt(buildPrompt(sb.Workspace, append(tools, tool.NewAskUserQuestion(nil)), nil, nil, false, nil)), req.SystemPrompt)
	for _, marker := range pigruntimetest.DecoyMarkers {
		assert.NotContains(t, req.Raw, marker)
	}
	assert.Equal(t, before, pigruntimetest.Snapshot(t, sb.Home, sb.Workspace))
}

func TestTurnSendsConfiguredInstructions(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	global := instruction.Group{Dir: sb.KopiHome, Global: true, Files: []instruction.File{{Path: filepath.Join(sb.KopiHome, "AGENTS.md"), Text: "GLOBAL_RULE"}}}
	project := instruction.Group{Dir: sb.Workspace, Files: []instruction.File{{Path: filepath.Join(sb.Workspace, "AGENTS.md"), Text: "PROJECT_RULE"}}}
	h := newHarness(t, sb, Config{Instructions: []instruction.Group{global, project}})
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "done")})

	h.turn(t, "hello")

	req := receive(t, requests)
	assert.Equal(t, joinPrompt(buildPrompt(sb.Workspace, []tool.Tool{tool.NewAskUserQuestion(nil)}, []instruction.Group{global, project}, nil, false, nil)), req.SystemPrompt)
	g, p := strings.Index(req.SystemPrompt, "GLOBAL_RULE"), strings.Index(req.SystemPrompt, "PROJECT_RULE")
	require.GreaterOrEqual(t, g, 0)
	assert.Greater(t, p, g, "the global group comes before the project's")
}

func TestTurnRetainsGlobalAndProjectSharingDirectoryAcrossResume(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	repo := filepath.Dir(sb.KopiHome)
	require.NoError(t, os.Mkdir(filepath.Join(repo, ".git"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("PROJECT_SHARED_DIR"), 0o600))
	global := filepath.Join(sb.KopiHome, "AGENTS.md")
	require.NoError(t, os.WriteFile(global, []byte("GLOBAL_SHARED_DIR"), 0o600))
	sb.Workspace = sb.KopiHome
	groups, errs := instruction.Load(global, []instruction.Root{{Dir: sb.Workspace, Trusted: true}}, nil)
	require.Empty(t, errs)
	require.Len(t, groups, 2)
	cfg := Config{Instructions: groups}
	h := newHarness(t, sb, cfg)
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "done")})
	h.turn(t, "hello")
	first := receive(t, requests).SystemPrompt
	id := h.c.Status().SessionID
	require.NoError(t, h.c.Close(t.Context()))

	fresh := newHarness(t, sb, cfg)
	require.NoError(t, fresh.c.Open(t.Context(), OpenRequest{ResumeID: id}))
	fresh.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "resumed")})
	fresh.turn(t, "again")
	resumed := receive(t, requests).SystemPrompt
	for _, prompt := range []string{first, resumed} {
		assert.Equal(t, 1, strings.Count(prompt, "GLOBAL_SHARED_DIR"))
		assert.Equal(t, 1, strings.Count(prompt, "PROJECT_SHARED_DIR"))
		assert.Less(t, strings.Index(prompt, "GLOBAL_SHARED_DIR"), strings.Index(prompt, "PROJECT_SHARED_DIR"))
	}
	assert.Equal(t, first, resumed)
}

func TestCloseJoinsRunningTurn(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	started := make(chan struct{})
	block := testTool("Block", policy.Read, func(ctx context.Context, _ json.RawMessage) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	})
	h := newHarness(t, sb, Config{Tools: []tool.Tool{block}})
	ignore := goleak.IgnoreCurrent()
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Block", "call-1")), pigruntimetest.Text("unreached")})

	w, err := h.c.Submit(t.Context(), Main, "block")
	require.NoError(t, err)
	wait(t, started)
	require.NoError(t, h.c.Close(t.Context()))

	select {
	case <-w.Done():
	default:
		t.Fatal("Close returned before the turn ended")
	}
	assert.Equal(t, event.Interrupted, w.Outcome().Status)
	goleak.VerifyNone(t, ignore)
}

func TestCloseDiscardsQueuedInput(t *testing.T) {
	const queued = "queued-9b2c"
	started := make(chan struct{})
	tools := []tool.Tool{blockTool(started)}
	h, sb := openHarness(t, Config{Tools: tools})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Block", "call-1")), pigruntimetest.Text("unreached")})
	id := h.c.Status().SessionID

	w, err := h.c.Submit(t.Context(), Main, "block")
	require.NoError(t, err)
	wait(t, started)
	_, err = h.c.Submit(t.Context(), Main, queued, pngFile(t))
	require.NoError(t, err)
	require.NoError(t, h.c.Close(t.Context()))

	assert.Equal(t, []event.TurnEnd{{WorkID: w.ID(), Outcome: event.Outcome{Status: event.Interrupted}}}, ofType[event.TurnEnd](untimed(h.rec.all())))

	fresh := newHarness(t, sb, Config{Tools: tools})
	require.NoError(t, fresh.c.Open(t.Context(), OpenRequest{ResumeID: id}))
	requests := make(chan pigruntimetest.Request, 1)
	fresh.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "resumed")})
	fresh.turn(t, "check")
	req := receive(t, requests)
	assert.Equal(t, []string{"block", "check"}, req.UserTexts)
	assert.NotContains(t, req.Raw, queued)
}

func TestCloseKeepsSessionWhileWorkWillNotStop(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	h, _ := openHarness(t, Config{Tools: []tool.Tool{releaseTool(started, release)}, JoinTimeout: 50 * time.Millisecond})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Hold", "call-1")), pigruntimetest.Text("done")})
	id := h.c.Status().SessionID

	w, err := h.c.Submit(t.Context(), Main, "hold")
	require.NoError(t, err)
	wait(t, started)
	_, err = h.c.Submit(t.Context(), Main, "queued [image #1]", pngFile(t))
	require.NoError(t, err)

	// Assertions only until the tool is released, so a failure cannot hang the test.
	assert.ErrorIs(t, h.c.Close(t.Context()), pigruntime.ErrJoinTimeout)
	assert.ErrorIs(t, h.c.Close(t.Context()), pigruntime.ErrJoinTimeout)
	status := h.c.Status()
	assert.Equal(t, id, status.SessionID)
	assert.True(t, status.Busy)
	_, err = h.c.Submit(t.Context(), Main, "after close")
	assert.ErrorIs(t, err, ErrNoSession)
	assertInertAfterClose(t, h)
	delivered := len(h.rec.all())

	close(release)
	wait(t, w.Done())
	h.c.queue.Lock()
	assert.True(t, w.discard, "Close's discard survives its join timeout")
	assert.False(t, w.switchHold)
	assert.Nil(t, w.terminal)
	h.c.queue.Unlock()
	assert.Len(t, h.rec.all(), delivered, "no event may be delivered after Close returned")
	require.NoError(t, h.c.Close(t.Context()))
	assert.Empty(t, h.c.Status().SessionID)
}

// assertInertAfterClose checks that a closed controller refuses commands,
// keeps its mode and delivers nothing.
func assertInertAfterClose(t *testing.T, h *harness) {
	t.Helper()
	delivered := len(h.rec.all())
	mode, gen := h.c.authority.Snapshot()
	for _, cmd := range []string{"/execute", "/plan", "/new", "/resume", "/resume no-such-session"} {
		_, err := h.c.Submit(t.Context(), Main, cmd)
		assert.ErrorIs(t, err, ErrNoSession, cmd)
	}
	cs, err := h.c.Arguments("resume")
	require.NoError(t, err)
	assert.Nil(t, cs)
	h.c.EnterExecute()
	h.c.EnterPlan()
	gotMode, gotGen := h.c.authority.Snapshot()
	assert.Equal(t, mode, gotMode)
	assert.Equal(t, gen, gotGen)
	assert.Len(t, h.rec.all(), delivered, "no event may be delivered after Close returned")
}

func TestClosedControllerIsInert(t *testing.T) {
	h, _ := openHarness(t, Config{})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("answer")})
	h.turn(t, "hello")
	require.NoError(t, h.c.Close(t.Context()))
	assertInertAfterClose(t, h)
}

func skillConfig(prefix string, names ...string) Config {
	var skills []skill.Skill
	for _, n := range names {
		skills = append(skills, skill.Skill{Name: n, Description: "does " + n, Body: "body of " + n, Dir: "/skills/" + n})
	}
	return Config{Skills: skills, SkillPrefix: prefix}
}

func skillText(name string) string {
	return skill.Text(skill.Skill{Name: name, Body: "body of " + name, Dir: "/skills/" + name})
}

func TestTypedSkillsLoadWithTheirPrompt(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		want        []string
	}{
		{"leading", "/skill:a do it", []string{"/skill:a do it", skillText("a")}},
		{"mid prompt", "use /skill:b now", []string{"use /skill:b now", skillText("b")}},
		{"twice", "/skill:a and /skill:a", []string{"/skill:a and /skill:a", skillText("a")}},
		{"two in order", "/skill:b then /skill:a", []string{"/skill:b then /skill:a", skillText("b"), skillText("a")}},
		{"escaped", "//skill:a", []string{"/skill:a"}},
		{"multiline", "/skill:a\nmore", []string{"/skill:a\nmore", skillText("a")}},
		{"unknown mid prompt", "use /skill:z", []string{"use /skill:z"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := openHarness(t, skillConfig("skill", "a", "b"))
			requests := make(chan pigruntimetest.Request, 1)
			h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "ok")})

			h.turn(t, tc.input)

			assert.Equal(t, tc.want, receive(t, requests).UserTexts)
		})
	}
}

func TestUnknownLeadingSkillIsUnknownCommand(t *testing.T) {
	h, _ := openHarness(t, skillConfig("skill", "a"))

	_, err := h.c.Submit(t.Context(), Main, "/skill:nope x")

	require.ErrorIs(t, err, ErrUnknownCommand)
	assert.Zero(t, h.faux.CallCount())
}

func TestSkillNamedLikeControlCommandIsNotTypable(t *testing.T) {
	h, _ := openHarness(t, skillConfig("", "plan", "a"))
	h.c.EnterExecute()

	w, err := h.c.Submit(t.Context(), Main, "/plan")

	require.NoError(t, err)
	assert.Nil(t, w)
	assert.Equal(t, policy.Plan, h.c.Status().Mode)
	assert.Equal(t, []Command{{Name: "a", Summary: "does a", Skill: true}}, h.c.Commands()[len(commandTable()):])
	errs := ofType[event.Error](h.rec.all())
	require.Len(t, errs, 1)
	assert.EqualError(t, errs[0].Err, "skill plan cannot be typed: /plan is a control command")
}

func TestPromptLoadsSkillsButRunsNoCommand(t *testing.T) {
	h, _ := openHarness(t, skillConfig("skill", "a"))
	requests := make(chan pigruntimetest.Request, 2)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "ok"), pigruntimetest.Capture(t, requests, "ok")})

	for _, text := range []string{"/skill:a x", "/plan"} {
		w, err := h.c.Prompt(t.Context(), text)
		require.NoError(t, err)
		wait(t, w.Done())
	}

	assert.Equal(t, []string{"/skill:a x", skillText("a")}, receive(t, requests).UserTexts[:2])
	assert.Contains(t, receive(t, requests).UserTexts, "/plan")
}

func TestQueuedPromptKeepsItsSkill(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	cfg := skillConfig("skill", "a")
	cfg.Tools = []tool.Tool{releaseTool(started, release)}
	h, _ := openHarness(t, cfg)
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Hold", "call-1")), pigruntimetest.Capture(t, requests, "done")})

	w, err := h.c.Submit(t.Context(), Main, "hold")
	require.NoError(t, err)
	wait(t, started)
	_, err = h.c.Submit(t.Context(), Main, "/skill:a next")
	require.NoError(t, err)
	close(release)
	wait(t, w.Done())

	assert.Equal(t, []string{"hold", "/skill:a next", skillText("a")}, receive(t, requests).UserTexts)
}

func TestCommandsListSkillsAfterControlCommands(t *testing.T) {
	c := NewController(nil, skillConfig("skill", "b", "a"))

	cmds := c.Commands()

	n := len(commandTable())
	assert.Equal(t, []Command{
		{Name: "skill:a", Summary: "does a", Skill: true},
		{Name: "skill:b", Summary: "does b", Skill: true},
	}, cmds[n:])
	assert.Equal(t, []Span{{Start: 0, End: 8, Kind: KindCommand}}, c.Spans("/skill:a"))
	assert.Nil(t, c.Spans("/skill:z"))
}

// modeHarness opens a harness whose sessions have a plan file and tell the
// model their mode, the way a real first prompt does.
func modeHarness(t *testing.T) *harness {
	t.Helper()
	sb := pigruntimetest.NewSandbox(t)
	plans := tool.NewPlans(filepath.Join(sb.Home, "plans"))
	h := newHarness(t, sb, Config{Plans: plans})
	h.showModeLine()
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	return h
}

func (h *harness) planLine() string {
	return "The session is in plan mode. Its plan file is " + h.c.PlanFile() + "; Read, Write and Edit work on that file in both modes."
}

// userTexts sends each prompt as its own turn and returns the user messages the
// model read in each.
func (h *harness) userTexts(t *testing.T, prompts ...string) [][]string {
	t.Helper()
	requests := make(chan pigruntimetest.Request, len(prompts))
	var steps []ai.FauxResponseStep
	for range prompts {
		steps = append(steps, pigruntimetest.Capture(t, requests, "ok"))
	}
	h.faux.SetResponses(steps)
	var out [][]string
	for _, p := range prompts {
		h.turn(t, p)
		// The model reads the whole conversation: keep what this prompt added.
		texts := receive(t, requests).UserTexts
		out = append(out, texts[h.read:])
		h.read = len(texts)
	}
	return out
}

func TestFirstPromptCarriesTheModeLineOnce(t *testing.T) {
	h := modeHarness(t)
	assert.Equal(t, [][]string{{"hi", h.planLine()}, {"again"}}, h.userTexts(t, "hi", "again"))
}

func TestModeLineFollowsTheNextPromptAfterAChange(t *testing.T) {
	h := modeHarness(t)
	got := h.userTexts(t, "one")
	h.c.ToggleMode()
	got = append(got, h.userTexts(t, "two")...)
	h.c.ToggleMode()
	h.c.ToggleMode()
	got = append(got, h.userTexts(t, "three")...)

	assert.Equal(t, [][]string{{"one", h.planLine()}, {"two", "The session is in execute mode."}, {"three"}}, got)
}

func TestNewSessionGetsTheModeLineAgain(t *testing.T) {
	h := modeHarness(t)
	h.userTexts(t, "one")
	oldPath := h.c.PlanFile()
	require.NoError(t, h.c.NewSession(t.Context()))
	require.NotEqual(t, oldPath, h.c.PlanFile())
	h.read = 0

	assert.Equal(t, [][]string{{"two", h.planLine()}}, h.userTexts(t, "two"))
}

func TestResumedSessionRestoresItsPlanFile(t *testing.T) {
	h := modeHarness(t)
	h.userTexts(t, "one")
	writePlan(t, h, "# Plan\n")
	oldPath, oldID := h.c.PlanFile(), h.c.Status().SessionID
	require.NoError(t, h.c.NewSession(t.Context()))
	require.NotEqual(t, oldPath, h.c.PlanFile())
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{ResumeID: oldID}))
	h.read = 0

	assert.Equal(t, oldPath, h.c.PlanFile())
	assert.Equal(t, [][]string{{"one", h.planLine(), "two", h.planLine()}}, h.userTexts(t, "two"), "the resumed history, then the line told again")
}

func TestHeadlessPromptCarriesTheModeLine(t *testing.T) {
	h := modeHarness(t)
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "ok")})

	w, err := h.c.Prompt(t.Context(), "hi")
	require.NoError(t, err)
	wait(t, w.Done())

	assert.Equal(t, []string{"hi", h.planLine()}, receive(t, requests).UserTexts)
}

func TestModeLineIsToldAgainWhenItsPromptWasNotDelivered(t *testing.T) {
	c := &Controller{authority: policy.NewAuthority()}
	c.main = &runner{c: c}
	line := modeLine(policy.Plan, "")
	sent := c.main.tell([]pigruntime.Prompt{{Text: "a"}})
	assert.Equal(t, line, sent[0].Mode)
	assert.Empty(t, c.main.tell([]pigruntime.Prompt{{Text: "b"}})[0].Mode)

	c.main.untell(sent)
	assert.Equal(t, line, c.main.tell([]pigruntime.Prompt{{Text: "c"}})[0].Mode)
}

func TestOpenReportsPlanFileSelectionFailure(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	blocker := filepath.Join(sb.Home, "blocker")
	require.NoError(t, os.WriteFile(blocker, nil, 0o600))
	h := newHarness(t, sb, Config{Plans: tool.NewPlans(filepath.Join(blocker, "plans"))})

	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))

	errs := ofType[event.Error](h.rec.all())
	require.Len(t, errs, 1)
	assert.ErrorContains(t, errs[0].Err, "failed to select the plan file")
	assert.Empty(t, h.c.PlanFile())
}
