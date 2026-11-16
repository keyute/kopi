package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
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
	"github.com/keyute/kopi/internal/pigruntime"
	"github.com/keyute/kopi/internal/pigruntime/pigruntimetest"
	"github.com/keyute/kopi/internal/policy"
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

func lastNotice(t *testing.T, rec *recorder) string {
	t.Helper()
	notices := ofType[event.Notice](rec.all())
	require.NotEmpty(t, notices)
	return notices[len(notices)-1].Text
}

type harness struct {
	c    *Controller
	faux *ai.FauxProviderHandle
	rec  *recorder
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
	cfg.Workspace = sb.Workspace
	if cfg.JoinTimeout == 0 {
		cfg.JoinTimeout = waitLimit
	}
	c := NewController(rt, cfg)
	rec := &recorder{}
	c.Subscribe(rec.add)
	t.Cleanup(func() { assert.NoError(t, c.Close(context.Background())) })
	return &harness{c: c, faux: faux, rec: rec}
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
	w, err := h.c.Submit(t.Context(), prompt)
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
		Run:         run,
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

func TestDeniedCallHasNoSummary(t *testing.T) {
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
	assert.Empty(t, ends[0].Summary)
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
	read := readTool(t, sb)
	h := newHarness(t, sb, Config{Tools: []tool.Tool{read}})
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	changed := ofType[event.SessionChanged](h.rec.all())
	require.Len(t, changed, 1)
	assert.Nil(t, changed[0].History)
	h.faux.SetResponses([]ai.FauxResponseStep{
		pigruntimetest.ToolCalls(pigruntimetest.Call{Name: "Read", ID: "call-1", Args: map[string]any{"file_path": "notes.txt"}}),
		pigruntimetest.Text("read it"),
	})
	h.turn(t, "read notes.txt")
	id := h.c.Status().SessionID
	require.NoError(t, h.c.Close(t.Context()))

	fresh := newHarness(t, sb, Config{Tools: []tool.Tool{read}})
	require.NoError(t, fresh.c.Open(t.Context(), OpenRequest{ResumeID: id}))
	changed = ofType[event.SessionChanged](fresh.rec.all())
	require.Len(t, changed, 1)
	history := changed[0].History
	require.NotEmpty(t, history)
	assert.Equal(t, event.TurnStart{Prompt: "read notes.txt"}, history[0])
	assert.Equal(t, []event.ToolStart{{
		CallID: "call-1", Name: "Read", Args: `{"file_path":"notes.txt"}`,
		Title: "Read notes.txt", Action: tool.Action{Verb: "Read", One: "file", Many: "files"},
	}}, ofType[event.ToolStart](history))
	ends := ofType[event.ToolEnd](history)
	require.Len(t, ends, 1)
	assert.Contains(t, ends[0].Output, "kopi fixture 42")
	assert.Equal(t, "1 line", ends[0].Summary)
}

// In execute mode a non-Read call with no rule deciding it asks first.
func TestExecToolFollowsMode(t *testing.T) {
	var ran atomic.Int32
	h, _ := openHarness(t, Config{Tools: []tool.Tool{execTool(&ran)}})
	approvals := subscribeTo[event.ApprovalRequested](h.c)
	requests := make(chan pigruntimetest.Request, 1)
	turn := func(asks bool) string {
		h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Exec", "call-1")), pigruntimetest.Capture(t, requests, "ok")})
		w, err := h.c.Submit(t.Context(), "run it")
		require.NoError(t, err)
		if asks {
			assert.True(t, h.c.Answer(receiveEvent(t, approvals).Token, policy.Allow))
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

	w, err := h.c.Submit(t.Context(), "hold then exec")
	require.NoError(t, err)
	wait(t, started)
	_, err = h.c.Submit(t.Context(), "/plan")
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
	assert.False(t, h.c.Interrupt(), "nothing to interrupt yet")

	w, err := h.c.Submit(t.Context(), "block")
	require.NoError(t, err)
	wait(t, started)
	assert.True(t, h.c.Interrupt())
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

	w, err := h.c.Submit(t.Context(), "first")
	require.NoError(t, err)
	wait(t, reached)
	// The TurnEnd is delivered under the lock Interrupt takes.
	interrupted := make(chan bool, 1)
	go func() { interrupted <- h.c.Interrupt() }()
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

	w, err := h.c.Submit(t.Context(), "first")
	require.NoError(t, err)
	wait(t, reached)
	// The TurnEnd is delivered under the lock Submit takes to queue input.
	type submitted struct {
		w   *Work
		err error
	}
	second := make(chan submitted, 1)
	go func() {
		w, err := h.c.Submit(context.Background(), "second")
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
	running, err := h.c.Submit(t.Context(), "hold")
	require.NoError(t, err)
	wait(t, started)

	ctx, cancel := context.WithCancel(t.Context())
	w := &Work{id: "turn-rejected", done: make(chan struct{}), cancel: cancel}
	h.c.runTurn(t.Context(), ctx, h.c.sess, h.c.emit, w, []pigruntime.Prompt{{Text: "rejected"}})
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

	w, err := h.c.Submit(t.Context(), "first")
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

func TestPromptWhileBusyIsQueuedAndStatusWorks(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	h, _ := openHarness(t, Config{Tools: []tool.Tool{releaseTool(started, release)}})
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Hold", "call-1")), pigruntimetest.Capture(t, requests, "done")})

	w, err := h.c.Submit(t.Context(), "hold")
	require.NoError(t, err)
	wait(t, started)

	got, err := h.c.Submit(t.Context(), "another prompt")
	require.NoError(t, err)
	assert.Nil(t, got)
	got, err = h.c.Submit(t.Context(), "/status")
	require.NoError(t, err)
	assert.Nil(t, got)
	assert.Contains(t, lastNotice(t, h.rec), "turn: running")
	assert.True(t, h.c.Status().Busy)

	close(release)
	wait(t, w.Done())
	assert.False(t, h.c.Status().Busy)
	assert.Equal(t, []string{"hold", "another prompt"}, receive(t, requests).UserTexts)
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

	w, err := h.c.Submit(t.Context(), "hold")
	require.NoError(t, err)
	wait(t, started)
	_, err = h.c.Submit(t.Context(), "next")
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

	w, err := h.c.Submit(t.Context(), "block")
	require.NoError(t, err)
	wait(t, started)
	img := pngFile(t)
	got, err := h.c.Submit(t.Context(), "look at [image #1]", img)
	require.NoError(t, err)
	assert.Nil(t, got)
	assert.True(t, h.c.Interrupt())
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

	w, err := h.c.Submit(t.Context(), "block")
	require.NoError(t, err)
	wait(t, started)
	img := pngFile(t)
	_, err = h.c.Submit(t.Context(), "queued [image #1]", img)
	require.NoError(t, err)
	_, err = h.c.Submit(t.Context(), "/resume no-such-session")
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
	w, err := h.c.Submit(t.Context(), "hold")
	require.NoError(t, err)
	wait(t, started)
	require.True(t, h.c.Interrupt())
	img := pngFile(t)
	_, err = h.c.Submit(t.Context(), "after interrupt [image #1]", img)
	require.NoError(t, err)

	// The command must capture the stopped turn before its starter gets life.
	h.c.life.Lock()
	_, err = h.c.Submit(t.Context(), "/resume no-such-session")
	assert.NoError(t, err)
	close(release)
	wait(t, w.Done())
	h.c.queue.Lock()
	assert.Empty(t, h.c.pending)
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
	w, err := h.c.Submit(t.Context(), "hold")
	require.NoError(t, err)
	wait(t, started)
	require.True(t, h.c.Interrupt())
	img := pngFile(t)
	_, err = h.c.Submit(t.Context(), "later [image #1]", img)
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
	w, err := h.c.Submit(t.Context(), "hold")
	require.NoError(t, err)
	wait(t, started)
	require.True(t, h.c.Interrupt())
	_, err = h.c.Submit(t.Context(), "later [image #1]", pngFile(t))
	require.NoError(t, err)
	op, err := h.c.beginSwitch()
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
	w, err := h.c.Submit(t.Context(), "hold")
	require.NoError(t, err)
	wait(t, started)
	img := pngFile(t)
	_, err = h.c.Submit(t.Context(), "queued [image #1]", img)
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
		done:     make(chan struct{}),
		cancel:   cancel,
		handBack: []pigruntime.Prompt{later},
		stopped:  true,
	}
	h.c.turn = w

	h.c.runTurn(t.Context(), ctx, h.c.sess, h.c.emit, w, []pigruntime.Prompt{original})
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

	w, err := h.c.Submit(t.Context(), "block")
	require.NoError(t, err)
	wait(t, started)
	got, err := h.c.Submit(t.Context(), "q")
	require.NoError(t, err)
	assert.Nil(t, got)
	require.NoError(t, h.c.SendNow(t.Context(), "d"))
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

	w, err := h.c.Submit(t.Context(), "hold")
	require.NoError(t, err)
	wait(t, started)
	require.NoError(t, h.c.SendNow(t.Context(), ""))
	require.NoError(t, h.c.SendNow(t.Context(), "  "))
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

	require.NoError(t, h.c.SendNow(t.Context(), ""))
	assert.Empty(t, ofType[event.TurnStart](h.rec.all()), "a blank send-now at idle starts nothing")
	require.NoError(t, h.c.SendNow(t.Context(), "hi"))

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
	assert.Nil(t, h.c.TakeQueued(), "nothing runs")

	w, err := h.c.Submit(t.Context(), "hold")
	require.NoError(t, err)
	wait(t, started)
	img := pngFile(t)
	_, err = h.c.Submit(t.Context(), "q1")
	require.NoError(t, err)
	_, err = h.c.Submit(t.Context(), "q2 [image #1]", img)
	require.NoError(t, err)
	assert.Equal(t, []event.Input{{Text: "q1"}, {Text: "q2 [image #1]", Images: []string{img}}}, h.c.TakeQueued())
	assert.Empty(t, h.c.TakeQueued())
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

	w, err := h.c.Submit(t.Context(), "hold")
	require.NoError(t, err)
	wait(t, started)
	_, err = h.c.Submit(t.Context(), queued)
	require.NoError(t, err)
	assert.Equal(t, []event.Input{{Text: queued}}, h.c.TakeQueued())
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

			w, err := h.c.Submit(t.Context(), "block")
			require.NoError(t, err)
			wait(t, started)
			img := pngFile(t)
			_, err = h.c.Submit(t.Context(), queued, img)
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

	w, err := h.c.Submit(t.Context(), "hold")
	require.NoError(t, err)
	wait(t, started)
	_, err = h.c.Submit(t.Context(), "wide [image #1]", path)
	require.NoError(t, err)
	_, err = h.c.Submit(t.Context(), "after")
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

			w, err := h.c.Submit(t.Context(), "hold")
			require.NoError(t, err)
			wait(t, started)
			_, err = h.c.Submit(t.Context(), "before")
			require.NoError(t, err)
			require.True(t, h.c.Interrupt())
			if now {
				require.NoError(t, h.c.SendNow(t.Context(), "after"))
			} else {
				got, err := h.c.Submit(t.Context(), "after")
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

	w, err := h.c.Submit(t.Context(), "hold")
	require.NoError(t, err)
	wait(t, started)
	_, err = h.c.Submit(t.Context(), "before")
	require.NoError(t, err)
	require.True(t, h.c.Interrupt())
	got, err := h.c.Submit(t.Context(), "after")
	require.NoError(t, err)
	assert.Nil(t, got)
	require.True(t, h.c.Interrupt())
	close(release)

	assert.Equal(t, event.TurnEnd{WorkID: w.ID(), Outcome: event.Outcome{Status: event.Interrupted}, Unsent: []event.Input{{Text: "before"}, {Text: "after"}}}, receiveEnd(t, ends))
	wait(t, w.Done())
	assert.Equal(t, []event.Event{
		event.TurnStart{WorkID: w.ID(), Prompt: "hold"},
		event.TurnEnd{WorkID: w.ID(), Outcome: event.Outcome{Status: event.Interrupted}, Unsent: []event.Input{{Text: "before"}, {Text: "after"}}},
	}, lifecycle(untimed(h.rec.all())))
	assert.Equal(t, 1, h.faux.CallCount())
}

func TestSettle(t *testing.T) {
	a, b, c, d := pigruntime.Prompt{Text: "a"}, pigruntime.Prompt{Text: "b"}, pigruntime.Prompt{Text: "c"}, pigruntime.Prompt{Text: "d"}
	completed, interrupted, failed := event.Outcome{Status: event.Completed}, event.Outcome{Status: event.Interrupted}, event.Outcome{Status: event.Failed, Message: "boom"}
	ps := func(p ...pigruntime.Prompt) []pigruntime.Prompt { return p }
	for _, tc := range []struct {
		name                           string
		stopped, cancelled, sent       bool
		outcome                        event.Outcome
		undelivered, handBack, pending []pigruntime.Prompt
		wantNext, wantUnsent           []pigruntime.Prompt
	}{
		{name: "completed alone ends", sent: true, outcome: completed},
		{name: "completed continues with undelivered then pending", sent: true, outcome: completed, undelivered: ps(b), pending: ps(c), wantNext: ps(b, c)},
		{name: "stopped hands back unsent parts, undelivered and hand-back", stopped: true, cancelled: true, outcome: interrupted, undelivered: ps(b), handBack: ps(c), wantUnsent: ps(a, b, c)},
		{name: "stopped keeps pending for the next turn", stopped: true, cancelled: true, sent: true, outcome: interrupted, handBack: ps(b), pending: ps(c), wantUnsent: ps(b)},
		{name: "stopped after completing hands back", stopped: true, cancelled: true, sent: true, outcome: completed, handBack: ps(b), wantUnsent: ps(b)},
		{name: "send-now continues with pending", cancelled: true, sent: true, outcome: interrupted, undelivered: ps(b), pending: ps(c, d), wantNext: ps(b, c, d)},
		{name: "send-now before the prompt was read resends it", cancelled: true, outcome: interrupted, pending: ps(b), wantNext: ps(a, b)},
		{name: "send-now continues after a failure", cancelled: true, sent: true, outcome: failed, pending: ps(b), wantNext: ps(b)},
		{name: "failed hands everything back", sent: true, outcome: failed, undelivered: ps(b), pending: ps(c), wantUnsent: ps(b, c)},
		{name: "rejected prompt is handed back", outcome: failed, pending: ps(b), wantUnsent: ps(a, b)},
		{name: "interrupted by the provider hands pending back", sent: true, outcome: interrupted, pending: ps(b), wantUnsent: ps(b)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next, unsent := settle(tc.stopped, tc.cancelled, tc.outcome, tc.sent, ps(a), tc.undelivered, tc.handBack, tc.pending)
			assert.Equal(t, tc.wantNext, next)
			assert.Equal(t, tc.wantUnsent, unsent)
		})
	}
}

func TestPullHandsNothingToCancelledSend(t *testing.T) {
	a := pigruntime.Prompt{Text: "a"}
	c := &Controller{pending: []pigruntime.Prompt{a}}
	emit := func(event.Event) {}

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	assert.Empty(t, c.pull(cancelled, emit)())
	assert.Equal(t, []pigruntime.Prompt{a}, c.pending)

	assert.Equal(t, []pigruntime.Prompt{a}, c.pull(t.Context(), emit)())
	assert.Empty(t, c.pending)
}

func TestJoinedPromptsKeepOrder(t *testing.T) {
	i1, i2 := pigruntime.Image{Path: "/1.png"}, pigruntime.Image{Path: "/2.png"}
	got := joined([]pigruntime.Prompt{{Text: "one [image #1]", Images: []pigruntime.Image{i1}}, {Text: "two"}, {Text: "three [image #2]", Images: []pigruntime.Image{i2}}})
	assert.Equal(t, pigruntime.Prompt{Text: "one [image #1]\n\ntwo\n\nthree [image #2]", Images: []pigruntime.Image{i1, i2}}, got)
}

func TestBadImageRefusesInput(t *testing.T) {
	store := historyStore(t)
	started, release := make(chan struct{}), make(chan struct{})
	h, _ := openHarness(t, Config{Tools: []tool.Tool{releaseTool(started, release)}, History: store})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Hold", "call-1")), pigruntimetest.Text("done")})
	bad := badImages(t)

	for name, path := range bad {
		_, err := h.c.Submit(t.Context(), "look [image #1]", path)
		require.ErrorIs(t, err, ErrBadImage, name)
		require.ErrorIs(t, h.c.SendNow(t.Context(), "look [image #1]", path), ErrBadImage, name)
	}
	assert.Zero(t, h.faux.CallCount())
	assert.Empty(t, ofType[event.TurnStart](h.rec.all()))

	w, err := h.c.Submit(t.Context(), "hold")
	require.NoError(t, err)
	wait(t, started)
	for name, path := range bad {
		_, err := h.c.Submit(t.Context(), "queued [image #1]", path)
		require.ErrorIs(t, err, ErrBadImage, name)
	}
	assert.Nil(t, h.c.TakeQueued())
	close(release)
	wait(t, w.Done())
	assert.Equal(t, []string{"hold"}, h.c.History())
	assert.Empty(t, ofType[event.QueueChanged](h.rec.all()))
}

func TestImageReachesProvider(t *testing.T) {
	h, _ := openHarness(t, Config{})
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "a square")})
	path := pngFile(t)
	data, err := os.ReadFile(path)
	require.NoError(t, err)

	w, err := h.c.Submit(t.Context(), "what is [image #1]", path)
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

	w, err := h.c.Submit(t.Context(), "hold")
	require.NoError(t, err)
	wait(t, started)
	for _, input := range []string{"queued", "/status", "/status", "  "} {
		_, err := h.c.Submit(t.Context(), input)
		require.NoError(t, err, input)
	}
	_, err = h.c.Submit(t.Context(), "/nope")
	require.ErrorIs(t, err, ErrUnknownCommand)
	require.NoError(t, h.c.SendNow(t.Context(), ""))
	h.c.ToggleMode()
	close(release)
	wait(t, w.Done())

	assert.Equal(t, []string{"hold", "queued", "/status", "/nope"}, h.c.History())
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
	_, err := h.c.Submit(t.Context(), "/status")
	require.NoError(t, err)
	assert.Nil(t, h.c.History())
}

func TestUnknownCommandNeverReachesProvider(t *testing.T) {
	h, _ := openHarness(t, Config{})
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "ok")})

	w, err := h.c.Submit(t.Context(), "/frobnicate now")
	require.ErrorIs(t, err, ErrUnknownCommand)
	assert.Contains(t, err.Error(), "/frobnicate")
	assert.Nil(t, w)
	w, err = h.c.Submit(t.Context(), "   ")
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

	w, err := h.c.Submit(t.Context(), "hold")
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

	w, err := h.c.Submit(t.Context(), "block")
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

	_, err := h.c.Submit(t.Context(), "/new")
	require.NoError(t, err)
	wait(t, reached)
	w, err := h.c.Submit(t.Context(), "too early")
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
	_, err := h.c.Submit(t.Context(), "/new")
	assert.NoError(t, err)
	w, err := h.c.Submit(t.Context(), "too early")
	assert.ErrorIs(t, err, ErrBusy)
	assert.Nil(t, w)
	_, err = h.c.Submit(t.Context(), "/new")
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

	_, err := h.c.Submit(t.Context(), "/resume no-such-session")
	require.NoError(t, err)
	require.ErrorIs(t, receiveEvent(t, errs).Err, pigruntime.ErrSessionNotFound)

	assert.Equal(t, id, h.c.Status().SessionID)
	assert.Equal(t, event.Outcome{Status: event.Completed}, h.turn(t, "after").Outcome())
}

func TestSessionStartAlwaysEntersPlan(t *testing.T) {
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

func TestResumeEntersPlan(t *testing.T) {
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

func TestOpenRequestModeIsHonouredAndNewReturnsToPlan(t *testing.T) {
	h := newHarness(t, pigruntimetest.NewSandbox(t), Config{})
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{Mode: policy.Execute}))
	assert.Equal(t, policy.Execute, h.c.Status().Mode)
	oldID := h.c.Status().SessionID
	changes := subscribeTo[event.SessionChanged](h.c)

	_, err := h.c.Submit(t.Context(), "/new")
	require.NoError(t, err)
	assert.NotEqual(t, oldID, receiveEvent(t, changes).SessionID)
	assertSwitchDone(t, h)

	assert.Equal(t, policy.Plan, h.c.Status().Mode)
	assert.Equal(t, []event.ModeChanged{{Mode: policy.Execute}, {Mode: policy.Plan}}, ofType[event.ModeChanged](h.rec.all()))
}

func TestNewAndResumeCommandsStartInConfiguredMode(t *testing.T) {
	h, _ := openHarness(t, Config{Mode: policy.Execute})
	assert.Equal(t, policy.Plan, h.c.Status().Mode)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("first answer")})
	h.turn(t, "first question")
	savedID := h.c.Status().SessionID
	changes := subscribeTo[event.SessionChanged](h.c)

	_, err := h.c.Submit(t.Context(), "/new")
	require.NoError(t, err)
	assert.NotEqual(t, savedID, receiveEvent(t, changes).SessionID)
	assertSwitchDone(t, h)
	assert.Equal(t, policy.Execute, h.c.Status().Mode)

	h.c.EnterPlan()
	_, err = h.c.Submit(t.Context(), "/resume "+savedID)
	require.NoError(t, err)
	assert.Equal(t, savedID, receiveEvent(t, changes).SessionID)
	assertSwitchDone(t, h)
	assert.Equal(t, policy.Execute, h.c.Status().Mode)
}

func TestBareDenyRemovesTool(t *testing.T) {
	var ran atomic.Int32
	sb := pigruntimetest.NewSandbox(t)
	read := readTool(t, sb)
	rules, err := policy.NewRules(sb.Workspace, sb.Home, []string{"Read", "Exec"}, policy.Source{Name: "test", Deny: []string{"Exec"}})
	require.NoError(t, err)
	h := newHarness(t, sb, Config{Tools: []tool.Tool{read, execTool(&ran)}, Rules: rules})
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "done")})

	h.turn(t, "what can you use?")

	req := receive(t, requests)
	assert.Equal(t, []string{"Read"}, req.ToolNames)
	assert.Equal(t, systemPrompt(sb.Workspace, []tool.Tool{read}), req.SystemPrompt)
	assert.NotContains(t, req.SystemPrompt, "Exec")
	assert.Equal(t, []ToolStatus{{"Read", policy.Read, true}}, h.c.Status().Tools)
}

func TestPromptMentionsWriteAndEditOnlyWhenAvailable(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	ws, err := tool.NewWorkspace(sb.Workspace, nil)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, ws.Close()) })
	read, write, edit := tool.NewRead(ws), tool.NewWrite(ws), tool.NewEdit(ws)
	const sentence = "Write and Edit work only in execute mode"

	assert.Contains(t, systemPrompt(sb.Workspace, []tool.Tool{read, edit}), sentence)
	rules, err := policy.NewRules(sb.Workspace, sb.Home, []string{"Read", "Write", "Edit"}, policy.Source{Name: "test", Deny: []string{"Write", "Edit"}})
	require.NoError(t, err)
	h := newHarness(t, sb, Config{Tools: []tool.Tool{read, write, edit}, Rules: rules})
	assert.NotContains(t, h.c.prompt, sentence)
	assert.Contains(t, h.c.prompt, "Tools available: Read.\n")
}

func TestResumeWithoutIDListsSessions(t *testing.T) {
	h, _ := openHarness(t, Config{})
	notices := subscribeTo[event.Notice](h.c)
	_, err := h.c.Submit(t.Context(), "/resume")
	require.NoError(t, err)
	assert.Equal(t, "no saved sessions for this workspace", receiveEvent(t, notices).Text)

	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("answer")})
	h.turn(t, "list me")
	_, err = h.c.Submit(t.Context(), "/resume")
	require.NoError(t, err)
	notice := receiveEvent(t, notices).Text
	assert.Contains(t, notice, h.c.Status().SessionID)
	assert.Contains(t, notice, "list me")
	assert.NotContains(t, notice, "messages")
}

func TestOpenErrorsPassThrough(t *testing.T) {
	codex := ai.GetBuiltinModels("openai-codex")
	require.NotEmpty(t, codex)
	for _, tc := range []struct {
		name  string
		model string
		req   OpenRequest
		want  error
	}{
		{name: "continue without sessions", req: OpenRequest{Continue: true}, want: pigruntime.ErrSessionNotFound},
		{name: "unknown model", model: fauxProvider + "/missing", want: pigruntime.ErrUnknownModel},
		{name: "missing credential", model: "openai-codex/" + codex[0].ID, want: pigruntime.ErrNoCredential},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, pigruntimetest.NewSandbox(t), Config{Model: tc.model})
			require.ErrorIs(t, h.c.Open(t.Context(), tc.req), tc.want)
			_, err := h.c.Submit(t.Context(), "hello")
			assert.ErrorIs(t, err, ErrNoSession)
		})
	}
}

func TestStatusReportsToolsHonestly(t *testing.T) {
	var ran atomic.Int32
	sb := pigruntimetest.NewSandbox(t)
	h := newHarness(t, sb, Config{Tools: []tool.Tool{readTool(t, sb), execTool(&ran)}})
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))

	status := h.c.Status()
	assert.Equal(t, []ToolStatus{{"Read", policy.Read, true}, {"Exec", policy.Exec, false}}, status.Tools)
	assert.Equal(t, sb.Workspace, status.Workspace)
	assert.Equal(t, fauxProvider+"/faux-1", status.Model)

	_, err := h.c.Submit(t.Context(), "/status")
	require.NoError(t, err)
	notice := lastNotice(t, h.rec)
	for _, want := range []string{
		"mode: plan", "model: " + status.Model, "workspace: " + sb.Workspace, "session: " + status.SessionID + " (0 messages)",
		"turn: idle", "Read (read): allowed", "Exec (exec): not allowed in plan mode", approvalNote,
	} {
		assert.Contains(t, notice, want)
	}

	h.c.EnterExecute()
	assert.Equal(t, []ToolStatus{{"Read", policy.Read, true}, {"Exec", policy.Exec, true}}, h.c.Status().Tools)
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
	tokens, window, ok := h.c.sess.ContextUsage()
	require.True(t, ok)
	assert.Positive(t, tokens)
	assert.Equal(t, tokens, status.ContextTokens)
	assert.Equal(t, window, status.ContextWindow)
	assert.Equal(t, "feature/status", status.Branch)
}

func TestHelpListsCommands(t *testing.T) {
	h, _ := openHarness(t, Config{})
	_, err := h.c.Submit(t.Context(), "/help")
	require.NoError(t, err)
	notice := lastNotice(t, h.rec)
	for _, cmd := range h.c.Commands() {
		assert.Contains(t, notice, cmd.Usage)
	}
	_, err = h.c.Submit(t.Context(), "/quit")
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
	assert.Equal(t, []string{"Read"}, req.ToolNames)
	assert.Equal(t, systemPrompt(sb.Workspace, tools), req.SystemPrompt)
	for _, marker := range pigruntimetest.DecoyMarkers {
		assert.NotContains(t, req.Raw, marker)
	}
	assert.Equal(t, before, pigruntimetest.Snapshot(t, sb.Home, sb.Workspace))
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

	w, err := h.c.Submit(t.Context(), "block")
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

	w, err := h.c.Submit(t.Context(), "block")
	require.NoError(t, err)
	wait(t, started)
	_, err = h.c.Submit(t.Context(), queued, pngFile(t))
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

	w, err := h.c.Submit(t.Context(), "hold")
	require.NoError(t, err)
	wait(t, started)
	_, err = h.c.Submit(t.Context(), "queued [image #1]", pngFile(t))
	require.NoError(t, err)

	// Assertions only until the tool is released, so a failure cannot hang the test.
	assert.ErrorIs(t, h.c.Close(t.Context()), pigruntime.ErrJoinTimeout)
	assert.ErrorIs(t, h.c.Close(t.Context()), pigruntime.ErrJoinTimeout)
	status := h.c.Status()
	assert.Equal(t, id, status.SessionID)
	assert.True(t, status.Busy)
	_, err = h.c.Submit(t.Context(), "after close")
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
	for _, cmd := range []string{"/execute", "/status", "/help", "/plan", "/new", "/resume", "/resume no-such-session"} {
		_, err := h.c.Submit(t.Context(), cmd)
		assert.ErrorIs(t, err, ErrNoSession, cmd)
	}
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
