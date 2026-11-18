package app

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/pigruntime"
	"github.com/keyute/kopi/internal/pigruntime/pigruntimetest"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/tool"
)

// fakeTasks is a task manager whose tasks the test sets; end reports a task's
// end through the registered callback, synchronously.
type fakeTasks struct {
	mu           sync.Mutex
	onChange     func(tool.TaskInfo)
	onForeground func()
	infos        []tool.TaskInfo
	fg           bool
	detaches     int
	stopAlls     int
	stops        []string
	stop         func(tool.TaskInfo) (tool.TaskInfo, error)
	stopAll      func(int)
	agents       *tool.Tasks
}

func (f *fakeTasks) OnChange(fn func(tool.TaskInfo)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onChange = fn
}

func (f *fakeTasks) OnForeground(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onForeground = fn
}

// foreground sets whether a command runs in the foreground and signals it.
func (f *fakeTasks) foreground(fg bool) {
	f.mu.Lock()
	f.fg = fg
	fn := f.onForeground
	f.mu.Unlock()
	fn()
}

func (f *fakeTasks) Detach() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.detaches++
	return f.fg
}

func (f *fakeTasks) Foreground() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fg
}

func (f *fakeTasks) List() []tool.TaskInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.infos)
}

func (f *fakeTasks) Output(id string) (tool.TaskInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := slices.IndexFunc(f.infos, func(info tool.TaskInfo) bool { return info.ID == id })
	if i < 0 {
		return tool.TaskInfo{}, fmt.Errorf("failed to find task %q: %w", id, tool.ErrUnknownTask)
	}
	return f.infos[i], nil
}

func (f *fakeTasks) Stop(id string) (tool.TaskInfo, error) {
	f.mu.Lock()
	f.stops = append(f.stops, id)
	i := slices.IndexFunc(f.infos, func(info tool.TaskInfo) bool { return info.ID == id })
	if i < 0 {
		f.mu.Unlock()
		return tool.TaskInfo{}, fmt.Errorf("failed to find task %q: %w", id, tool.ErrUnknownTask)
	}
	if f.infos[i].Status == tool.TaskRunning {
		f.infos[i].Status = tool.TaskStopped
		f.infos[i].Ended = f.infos[i].Started.Add(3 * time.Second)
	}
	info, stop, agents := f.infos[i], f.stop, f.agents
	f.mu.Unlock()
	if info.Agent != "" {
		info, _ = agents.Stop(id)
		f.set(info)
	}
	if stop != nil {
		return stop(info)
	}
	return info, nil
}

// StartAgent starts the task in a real registry and mirrors it into infos;
// Stop runs the task's stop func there.
func (f *fakeTasks) StartAgent(agent, description string, stop func()) *tool.AgentTask {
	f.mu.Lock()
	if f.agents == nil {
		f.agents = tool.NewTasks()
	}
	agents := f.agents
	f.mu.Unlock()
	at := agents.StartAgent(agent, description, stop)
	info, _ := agents.Output(at.ID())
	f.set(info)
	return at
}

func (f *fakeTasks) StopAll() {
	f.mu.Lock()
	f.stopAlls++
	n, stopAll := f.stopAlls, f.stopAll
	f.infos = nil
	f.mu.Unlock()
	if stopAll != nil {
		stopAll(n)
	}
}

func (f *fakeTasks) set(info tool.TaskInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i := slices.IndexFunc(f.infos, func(in tool.TaskInfo) bool { return in.ID == info.ID }); i >= 0 {
		f.infos[i] = info
		return
	}
	f.infos = append(f.infos, info)
}

func (f *fakeTasks) end(info tool.TaskInfo) {
	f.set(info)
	f.mu.Lock()
	fn := f.onChange
	f.mu.Unlock()
	fn(info)
}

func (f *fakeTasks) counts() (detaches, stopAlls int, stops []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.detaches, f.stopAlls, slices.Clone(f.stops)
}

var taskStart = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func runningTask(id string) tool.TaskInfo {
	return tool.TaskInfo{ID: id, Command: "make " + id, Started: taskStart, Status: tool.TaskRunning, Output: "built " + id + "\n"}
}

func endedTask(id string) tool.TaskInfo {
	info := runningTask(id)
	info.Status, info.Reason, info.Ended = tool.TaskCompleted, "exit code 0", taskStart.Add(12*time.Second)
	return info
}

func endText(id string) string {
	return "Background task " + id + " completed (exit code 0): make " + id + "\nbuilt " + id
}

func (h *harness) queuedNotices() []pigruntime.Notice {
	h.c.queue.Lock()
	defer h.c.queue.Unlock()
	return slices.Clone(h.c.notices)
}

// idle reports whether no turn is registered; it waits out a queue section
// in progress, such as the one delivering a TurnEnd.
func (h *harness) idle() bool {
	h.c.queue.Lock()
	defer h.c.queue.Unlock()
	h.c.mu.Lock()
	defer h.c.mu.Unlock()
	return h.c.turn == nil
}

// gated answers with text once gate is closed.
func gated(gate <-chan struct{}, text string) ai.FauxResponseStep {
	return ai.FauxFactoryStep(func(ai.TranscriptContext, ai.StreamOptions, *ai.FauxProviderState, *ai.Model) (ai.AssistantMessage, error) {
		<-gate
		return ai.FauxAssistantMessage(ai.FauxContentString(text), ai.FauxAssistantMessageOptions{}), nil
	})
}

func TestDelayedStopTaskAcrossReplacement(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprintf("failed=%t", failed), func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			f := &fakeTasks{stop: func(info tool.TaskInfo) (tool.TaskInfo, error) {
				close(entered)
				<-release
				return info, nil
			}}
			h, _ := openHarness(t, Config{Tasks: f, Wake: true})
			f.set(runningTask("t1"))
			result := make(chan error, 1)
			go func() { result <- h.c.StopTask("t1") }()
			wait(t, entered)
			if failed {
				require.ErrorIs(t, h.c.Resume(t.Context(), "no-such-session"), pigruntime.ErrSessionNotFound)
			} else {
				require.NoError(t, h.c.NewSession(t.Context()))
			}
			close(release)
			require.NoError(t, receiveError(t, result))
			assert.True(t, h.idle())
			if failed {
				queued := h.queuedNotices()
				require.Len(t, queued, 1)
				assert.Equal(t, string(tool.TaskStopped), queued[0].Shown.Status)
			} else {
				assert.Empty(t, h.queuedNotices())
			}
		})
	}
}

func receiveError(t *testing.T, results <-chan error) error {
	t.Helper()
	select {
	case err := <-results:
		return err
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for result")
		return nil
	}
}

func TestCloseBoundsTaskCleanupAndReusesPendingSweep(t *testing.T) {
	for _, callerCancel := range []bool{false, true} {
		t.Run(fmt.Sprintf("caller-cancel=%t", callerCancel), func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			f := &fakeTasks{stopAll: func(n int) {
				if n == 1 {
					close(entered)
					<-release
				}
			}}
			h, _ := openHarness(t, Config{Tasks: f, JoinTimeout: 50 * time.Millisecond})
			id := h.c.Status().SessionID
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- h.c.Close(ctx) }()
			wait(t, entered)
			if callerCancel {
				cancel()
			}
			require.ErrorIs(t, receiveError(t, result), pigruntime.ErrJoinTimeout)
			assert.Equal(t, id, h.c.Status().SessionID)
			assert.Empty(t, f.List(), "cleanup may clear the list before its callbacks drain")
			delivered := len(h.rec.all())
			cancelled, stop := context.WithCancel(t.Context())
			stop()
			require.ErrorIs(t, h.c.Close(cancelled), pigruntime.ErrJoinTimeout)
			_, sweeps, _ := f.counts()
			assert.Equal(t, 1, sweeps, "retry must not overlap a pending sweep")
			f.end(endedTask("late"))
			assert.Len(t, h.rec.all(), delivered)
			close(release)
			require.NoError(t, h.c.Close(t.Context()))
			assert.Empty(t, h.c.Status().SessionID)
			assert.Empty(t, f.List())
			_, sweeps, _ = f.counts()
			assert.Equal(t, 2, sweeps, "a fresh sweep drains adoption after pending cleanup")
			assert.Len(t, h.rec.all(), delivered)
		})
	}
}

// The tool adopts only after the first sweep has emptied the manager. This
// controls the same boundary as asynchronous abort without relying on PiG timing.
func TestCloseDrainsTaskAdoptedAfterFirstSweep(t *testing.T) {
	started, swept := make(chan struct{}), make(chan struct{})
	f := &fakeTasks{}
	var w *Work
	f.stopAll = func(n int) {
		if n == 1 {
			close(swept)
		} else {
			select {
			case <-w.Done():
			default:
				t.Error("final sweep preceded turn join")
			}
			f.end(endedTask("callback"))
			f.mu.Lock()
			f.infos = nil
			f.mu.Unlock()
		}
	}
	adopt := testTool("Adopt", policy.Read, func(context.Context, json.RawMessage) (string, error) {
		close(started)
		<-swept
		f.set(runningTask("late"))
		return "adopted", nil
	})
	h, _ := openHarness(t, Config{Tasks: f, Tools: []tool.Tool{adopt}})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Adopt", "call-1")), pigruntimetest.Text("done")})
	var err error
	w, err = h.c.Submit(t.Context(), "adopt")
	require.NoError(t, err)
	wait(t, started)
	require.NoError(t, h.c.Close(t.Context()))
	assert.Empty(t, f.List())
	_, sweeps, _ := f.counts()
	assert.Equal(t, 2, sweeps)
	assert.Empty(t, h.c.Status().SessionID)
	delivered := len(h.rec.all())
	f.end(endedTask("after"))
	assert.Len(t, h.rec.all(), delivered)
}

func TestCloseRetryJoinsPendingFinalSweep(t *testing.T) {
	started, swept := make(chan struct{}), make(chan struct{})
	final, release := make(chan struct{}), make(chan struct{})
	f := &fakeTasks{stopAll: func(n int) {
		switch n {
		case 1:
			close(swept)
		case 2:
			close(final)
			<-release
		}
	}}
	adopt := testTool("Adopt", policy.Read, func(context.Context, json.RawMessage) (string, error) {
		close(started)
		<-swept
		f.set(runningTask("late"))
		return "adopted", nil
	})
	h, _ := openHarness(t, Config{Tasks: f, Tools: []tool.Tool{adopt}, JoinTimeout: 50 * time.Millisecond})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Adopt", "call-1")), pigruntimetest.Text("done")})
	w, err := h.c.Submit(t.Context(), "adopt")
	require.NoError(t, err)
	wait(t, started)
	id := h.c.Status().SessionID
	result := make(chan error, 1)
	go func() { result <- h.c.Close(t.Context()) }()
	wait(t, final)
	require.ErrorIs(t, receiveError(t, result), pigruntime.ErrJoinTimeout)
	wait(t, w.Done())
	assert.Equal(t, id, h.c.Status().SessionID)
	assert.Empty(t, f.List())
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, h.c.Close(cancelled), pigruntime.ErrJoinTimeout)
	_, sweeps, _ := f.counts()
	assert.Equal(t, 2, sweeps)
	close(release)
	require.NoError(t, h.c.Close(t.Context()))
	_, sweeps, _ = f.counts()
	assert.Equal(t, 2, sweeps, "retry awaits the invisible pending final sweep")
	assert.Empty(t, h.c.Status().SessionID)
}

func TestTaskEndWakesIdleController(t *testing.T) {
	h, _ := openHarness(t, Config{Wake: true})
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "noted")})
	ends := turnEnds(h.c)

	h.tasks.end(endedTask("t1"))

	end := receiveEnd(t, ends)
	assert.Equal(t, event.Completed, end.Outcome.Status)
	texts := receive(t, requests).UserTexts
	require.NotEmpty(t, texts)
	assert.Equal(t, endText("t1"), texts[len(texts)-1])
	assert.True(t, h.idle())
	evs := h.rec.all()
	assert.Equal(t, []event.TurnStart{{WorkID: end.WorkID}}, ofType[event.TurnStart](evs))
	assert.Equal(t, []event.TaskEnded{{ID: "t1", Command: "make t1", Status: "completed", Elapsed: 12 * time.Second}}, ofType[event.TaskEnded](evs))
	assert.Len(t, ofType[event.TasksChanged](evs), 1)
	assert.Empty(t, ofType[event.QueueChanged](evs), "notices are not queued input")
	assert.Empty(t, h.queuedNotices())
}

func TestTaskEndDuringTurnIsSteered(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	h, _ := openHarness(t, Config{Wake: true, Tools: []tool.Tool{releaseTool(started, release)}})
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Hold", "call-1")), pigruntimetest.Capture(t, requests, "done")})

	w, err := h.c.Submit(t.Context(), "hold")
	require.NoError(t, err)
	wait(t, started)
	h.tasks.end(endedTask("t1"))
	close(release)
	wait(t, w.Done())

	assert.Equal(t, []string{"hold", endText("t1")}, receive(t, requests).UserTexts)
	assert.Equal(t, 2, h.faux.CallCount())
	assert.True(t, h.idle())
	evs := untimed(h.rec.all())
	assert.Equal(t, []event.Event{
		event.TurnStart{WorkID: w.ID(), Prompt: "hold"},
		event.TurnEnd{WorkID: w.ID(), Outcome: event.Outcome{Status: event.Completed}},
	}, lifecycle(slices.Clone(evs)))
	assert.Len(t, ofType[event.TaskEnded](evs), 1)
}

// The task ends while the model writes its final reply: the notice reaches the
// model exactly once, within the same Work.
func TestTaskEndDuringFinalReplyIsDeliveredOnce(t *testing.T) {
	h, _ := openHarness(t, Config{Wake: true})
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{
		ai.FauxFactoryStep(func(ai.TranscriptContext, ai.StreamOptions, *ai.FauxProviderState, *ai.Model) (ai.AssistantMessage, error) {
			h.tasks.end(endedTask("t1"))
			return ai.FauxAssistantMessage(ai.FauxContentString("final"), ai.FauxAssistantMessageOptions{}), nil
		}),
		pigruntimetest.Capture(t, requests, "noted"),
	})

	w := h.turn(t, "go")

	assert.Equal(t, []string{"go", endText("t1")}, receive(t, requests).UserTexts)
	assert.True(t, h.idle())
	assert.Equal(t, 2, h.faux.CallCount())
	for _, start := range ofType[event.TurnStart](h.rec.all()) {
		assert.Equal(t, w.ID(), start.WorkID)
	}
	assert.Len(t, ofType[event.TaskEnded](h.rec.all()), 1)
}

func TestTaskEndDuringFinalReplyWithoutWakeWaitsForNextPrompt(t *testing.T) {
	h, _ := openHarness(t, Config{Wake: false})
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{
		ai.FauxFactoryStep(func(ai.TranscriptContext, ai.StreamOptions, *ai.FauxProviderState, *ai.Model) (ai.AssistantMessage, error) {
			h.tasks.end(endedTask("t1"))
			return ai.FauxAssistantMessage(ai.FauxContentString("final"), ai.FauxAssistantMessageOptions{}), nil
		}),
		pigruntimetest.Capture(t, requests, "noted"),
	})

	w := h.turn(t, "go")

	assert.Equal(t, "final", w.Reply())
	require.Equal(t, 1, h.faux.CallCount())
	assert.True(t, h.idle())
	assert.Len(t, ofType[event.TurnStart](h.rec.all()), 1)
	require.Len(t, h.queuedNotices(), 1)

	h.turn(t, "next")

	assert.Equal(t, []string{"go", "next", endText("t1")}, receive(t, requests).UserTexts)
	assert.Equal(t, 2, h.faux.CallCount())
	assert.True(t, h.idle())
	assert.Empty(t, h.queuedNotices())
	assert.Len(t, ofType[event.TurnStart](h.rec.all()), 2)
	assert.Len(t, ofType[event.TaskEnded](h.rec.all()), 1)
}

// The task ends while the finished Work delivers its TurnEnd: whether the end
// lands before or after the Work deregisters, the notice starts exactly one
// new turn.
func TestTaskEndAsTurnEndsStartsOneTurn(t *testing.T) {
	h, _ := openHarness(t, Config{Wake: true})
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("first"), pigruntimetest.Capture(t, requests, "noted")})
	reached, unblock := blockOnFirstTurnEnd(h.c)
	ends := turnEnds(h.c)

	w, err := h.c.Submit(t.Context(), "first")
	require.NoError(t, err)
	wait(t, reached)
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		h.tasks.end(endedTask("t1"))
	}()
	close(unblock)
	wait(t, w.Done())
	wait(t, ended)

	assert.Equal(t, w.ID(), receiveEnd(t, ends).WorkID)
	wake := receiveEnd(t, ends)
	assert.NotEqual(t, w.ID(), wake.WorkID)
	texts := receive(t, requests).UserTexts
	assert.Equal(t, endText("t1"), texts[len(texts)-1])
	assert.True(t, h.idle())
	assert.Equal(t, 2, h.faux.CallCount())
	assert.Len(t, ofType[event.TurnStart](h.rec.all()), 2)
}

func TestTaskEndAfterInterruptWaitsForNextPrompt(t *testing.T) {
	started := make(chan struct{})
	h, _ := openHarness(t, Config{Wake: true, Tools: []tool.Tool{blockTool(started)}})
	requests := make(chan pigruntimetest.Request, 1)
	gate := make(chan struct{})
	ends := turnEnds(h.c)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Block", "call-1")), pigruntimetest.Capture(t, requests, "next"), gated(gate, "woken")})

	w, err := h.c.Submit(t.Context(), "block")
	require.NoError(t, err)
	wait(t, started)
	require.True(t, h.c.Interrupt())
	wait(t, w.Done())
	h.tasks.end(endedTask("t1"))

	assert.True(t, h.idle(), "a task end after Interrupt does not wake the model")
	assert.Equal(t, 1, h.faux.CallCount())
	assert.Empty(t, h.c.TakeQueued())
	for _, end := range ofType[event.TurnEnd](h.rec.all()) {
		assert.Empty(t, end.Unsent)
	}

	h.turn(t, "next")
	assert.Equal(t, []string{"block", "next", endText("t1")}, receive(t, requests).UserTexts)

	h.tasks.end(endedTask("t2"))
	assert.False(t, h.idle(), "a human prompt lifts the hold")

	close(gate)
	for range 3 {
		receiveEnd(t, ends)
	}
	assert.True(t, h.idle())
}

func TestTaskEndWithoutWakeRidesWithNextPrompt(t *testing.T) {
	h, _ := openHarness(t, Config{})
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "ok")})

	h.tasks.end(endedTask("t1"))
	assert.True(t, h.idle())
	assert.Zero(t, h.faux.CallCount())

	h.turn(t, "hi")
	assert.Equal(t, []string{"hi", endText("t1")}, receive(t, requests).UserTexts)
	assert.Empty(t, h.queuedNotices())
}

// A notice the failed send never handed over does not restart the Work.
func TestFailedSendDoesNotWakeForNotice(t *testing.T) {
	h, _ := openHarness(t, Config{Wake: true})
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{
		ai.FauxFactoryStep(func(ai.TranscriptContext, ai.StreamOptions, *ai.FauxProviderState, *ai.Model) (ai.AssistantMessage, error) {
			h.tasks.end(endedTask("t1"))
			return ai.FauxAssistantMessage(ai.FauxContentString(""), ai.FauxAssistantMessageOptions{StopReason: string(ai.StopReasonError), ErrorMessage: "invalid request: bad thing"}), nil
		}),
		pigruntimetest.Capture(t, requests, "ok"),
	})

	w := h.turn(t, "go")

	assert.Equal(t, event.Failed, w.Outcome().Status)
	assert.True(t, h.idle())
	assert.Equal(t, 1, h.faux.CallCount())
	require.Len(t, h.queuedNotices(), 1)

	h.turn(t, "again")
	texts := receive(t, requests).UserTexts
	assert.Equal(t, []string{"again", endText("t1")}, texts[len(texts)-2:])
}

func TestStopTaskQueuesNonWakingNotice(t *testing.T) {
	h, _ := openHarness(t, Config{Wake: true})
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "ok")})
	h.tasks.set(runningTask("t1"))
	h.tasks.set(endedTask("t2"))

	require.NoError(t, h.c.StopTask("t1"))
	assert.True(t, h.idle(), "a human stop does not wake the model")
	require.NoError(t, h.c.StopTask("t2"))
	require.ErrorIs(t, h.c.StopTask("t9"), tool.ErrUnknownTask)
	_, _, stops := h.tasks.counts()
	assert.Equal(t, []string{"t1"}, stops, "an ended task is not stopped again")
	queued := h.queuedNotices()
	require.Len(t, queued, 1)
	assert.Equal(t, string(tool.TaskStopped), queued[0].Shown.Status)

	h.turn(t, "hi")
	want := "Background task t1 stopped by the user: make t1\nbuilt t1"
	assert.Equal(t, []string{"hi", want}, receive(t, requests).UserTexts)
	assert.Equal(t, []event.TaskEnded{{ID: "t1", Command: "make t1", Status: "stopped", Elapsed: 3 * time.Second}}, ofType[event.TaskEnded](h.rec.all()))
}

func TestModelStoppedTaskQueuesNothing(t *testing.T) {
	h, _ := openHarness(t, Config{Wake: true})
	stopped := runningTask("t1")
	stopped.Status, stopped.Ended = tool.TaskStopped, taskStart.Add(time.Second)

	h.tasks.end(runningTask("t1"))
	h.tasks.end(stopped)

	assert.True(t, h.idle())
	assert.Empty(t, h.queuedNotices())
	assert.Len(t, ofType[event.TasksChanged](h.rec.all()), 2)
}

func TestTasksEndingTogetherJoinOneTurn(t *testing.T) {
	h, _ := openHarness(t, Config{Wake: true})
	gate := make(chan struct{})
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{gated(gate, "first"), pigruntimetest.Capture(t, requests, "all")})
	ends := turnEnds(h.c)

	for _, id := range []string{"t1", "t2", "t3"} {
		h.tasks.end(endedTask(id))
	}
	close(gate)

	receiveEnd(t, ends)
	assert.Equal(t, []string{endText("t1"), endText("t2"), endText("t3")}, receive(t, requests).UserTexts)
	assert.True(t, h.idle())
	assert.Equal(t, 2, h.faux.CallCount())
	assert.Len(t, ofType[event.TurnStart](h.rec.all()), 1)
	assert.Len(t, ofType[event.TaskEnded](h.rec.all()), 3)
}

func TestTaskEndDuringApprovalWaitsForBoundary(t *testing.T) {
	var ran atomic.Int32
	h, _ := openHarness(t, Config{Wake: true, Tools: []tool.Tool{execTool(&ran)}})
	approvals := subscribeTo[event.ApprovalRequested](h.c)
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Exec", "call-1")), pigruntimetest.Capture(t, requests, "ok")})
	h.c.EnterExecute()

	w, err := h.c.Submit(t.Context(), "run it")
	require.NoError(t, err)
	token := receiveEvent(t, approvals).Token
	h.tasks.end(endedTask("t1"))
	assert.Equal(t, policy.Execute, h.c.Status().Mode)
	assert.True(t, h.c.Answer(token, policy.Decision{Answer: policy.Allow}))
	wait(t, w.Done())

	req := receive(t, requests)
	assert.Equal(t, []string{"run it", endText("t1")}, req.UserTexts)
	assert.Equal(t, []string{execOutput}, req.ToolResults)
	assert.Equal(t, int32(1), ran.Load())
	assert.Equal(t, []event.ModeChanged{{Mode: policy.Execute}}, ofType[event.ModeChanged](h.rec.all()))
	assert.Len(t, ofType[event.TurnStart](h.rec.all()), 1)
}

func TestSendNowCarriesQueuedNotices(t *testing.T) {
	started := make(chan struct{})
	h, _ := openHarness(t, Config{Tools: []tool.Tool{blockTool(started)}})
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Block", "call-1")), pigruntimetest.Capture(t, requests, "joined")})

	w, err := h.c.Submit(t.Context(), "block")
	require.NoError(t, err)
	wait(t, started)
	h.tasks.end(endedTask("t1"))
	require.NoError(t, h.c.SendNow(t.Context(), "now"))
	wait(t, w.Done())

	assert.Equal(t, []string{"block", "now", endText("t1")}, receive(t, requests).UserTexts)
}

func TestSessionSwitchDropsNoticesAndStopsTasks(t *testing.T) {
	h, _ := openHarness(t, Config{})
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "fresh")})

	oldID := h.c.Status().SessionID
	changes := subscribeTo[event.SessionChanged](h.c)
	h.tasks.end(endedTask("t1"))
	require.Len(t, h.queuedNotices(), 1)

	// Holding life keeps the switch under way; asserts only, so a failure
	// cannot leave it held.
	h.c.life.Lock()
	_, err := h.c.Submit(t.Context(), "/new")
	assert.NoError(t, err)
	h.tasks.end(endedTask("t2"))
	assert.Len(t, h.queuedNotices(), 2, "kept until the switch commits")
	h.c.life.Unlock()
	assert.NotEqual(t, oldID, receiveEvent(t, changes).SessionID)
	assertSwitchDone(t, h)

	_, stopAlls, _ := h.tasks.counts()
	assert.Equal(t, 1, stopAlls)
	assert.Empty(t, h.queuedNotices())
	assert.Len(t, ofType[event.TasksChanged](h.rec.all()), 3)

	require.ErrorIs(t, h.c.Resume(t.Context(), "no-such-session"), pigruntime.ErrSessionNotFound)
	_, stopAlls, _ = h.tasks.counts()
	assert.Equal(t, 1, stopAlls, "a failed open keeps the session's tasks")

	h.turn(t, "after")
	assert.Equal(t, []string{"after"}, receive(t, requests).UserTexts)
}

func TestCloseStopsTasksAndDropsLaterEnds(t *testing.T) {
	h, _ := openHarness(t, Config{Wake: true})
	h.tasks.end(endedTask("t0"))
	require.NoError(t, h.c.Close(t.Context()))
	_, stopAlls, _ := h.tasks.counts()
	assert.Equal(t, 1, stopAlls)
	delivered := len(h.rec.all())

	h.tasks.end(endedTask("t1"))
	assert.True(t, h.idle())
	assert.Empty(t, h.queuedNotices())
	assert.Len(t, h.rec.all(), delivered)
}

func TestCloseStopsTasksWhenWorkWillNotStop(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	h, _ := openHarness(t, Config{Tools: []tool.Tool{releaseTool(started, release)}, JoinTimeout: 50 * time.Millisecond})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Hold", "call-1")), pigruntimetest.Text("done")})

	w, err := h.c.Submit(t.Context(), "hold")
	require.NoError(t, err)
	wait(t, started)
	assert.ErrorIs(t, h.c.Close(t.Context()), pigruntime.ErrJoinTimeout)
	_, stopAlls, _ := h.tasks.counts()
	assert.Equal(t, 1, stopAlls)
	close(release)
	wait(t, w.Done())
}

func TestBackgroundDelegatesUntilClose(t *testing.T) {
	h, _ := openHarness(t, Config{})
	assert.False(t, h.c.Background())
	h.tasks.mu.Lock()
	h.tasks.fg = true
	h.tasks.mu.Unlock()
	assert.True(t, h.c.Background())
	require.NoError(t, h.c.Close(t.Context()))
	assert.False(t, h.c.Background())
	detaches, _, _ := h.tasks.counts()
	assert.Equal(t, 2, detaches)
}

func TestStatusCountsRunningTasks(t *testing.T) {
	h, _ := openHarness(t, Config{})
	assert.Zero(t, h.c.Status().Shells)
	assert.Zero(t, h.c.Status().Agents)
	assert.False(t, h.c.Status().Foreground)
	h.tasks.set(runningTask("t1"))
	h.tasks.set(endedTask("t2"))
	h.tasks.set(runningTask("t3"))
	h.tasks.mu.Lock()
	h.tasks.fg = true
	h.tasks.mu.Unlock()

	s := h.c.Status()
	assert.Equal(t, 2, s.Shells)
	assert.Zero(t, s.Agents)
	assert.True(t, s.Foreground)
	assert.Len(t, h.c.Tasks(), 3)
	info, err := h.c.TaskOutput("t2")
	require.NoError(t, err)
	assert.Equal(t, endedTask("t2"), info)
}

func TestNoticeText(t *testing.T) {
	h, _ := openHarness(t, Config{})
	var out strings.Builder
	for i := 1; i <= 25; i++ {
		fmt.Fprintf(&out, "line %d\n\n  \n", i)
	}
	long := strings.Repeat("é", 90)
	info := tool.TaskInfo{
		ID: "t7", Command: long + "\nsecond line", Started: taskStart, Ended: taskStart.Add(90 * time.Second),
		Status: tool.TaskFailed, Reason: "exit status 2", Output: out.String(),
	}

	h.tasks.end(info)

	queued := h.queuedNotices()
	require.Len(t, queued, 1)
	var tail []string
	for i := 6; i <= 25; i++ {
		tail = append(tail, fmt.Sprintf("line %d", i))
	}
	want := "Background task t7 failed (exit status 2): " + strings.Repeat("é", 80) + "\n" + strings.Join(tail, "\n")
	assert.Equal(t, want, queued[0].Text)
	assert.Equal(t, event.TaskEnded{ID: "t7", Command: info.Command, Status: "failed", Elapsed: 90 * time.Second}, queued[0].Shown)

	info.Output = ""
	info.ID = "t8"
	h.tasks.end(info)
	queued = h.queuedNotices()
	require.Len(t, queued, 2)
	assert.Equal(t, "Background task t8 failed (exit status 2): "+strings.Repeat("é", 80), queued[1].Text)
}

func TestForegroundSignalReportsTasksChanged(t *testing.T) {
	h, _ := openHarness(t, Config{})
	changes := subscribeTo[event.TasksChanged](h.c)

	h.tasks.foreground(true)
	receiveEvent(t, changes)
	assert.True(t, h.c.Status().Foreground)
	h.tasks.foreground(false)
	receiveEvent(t, changes)
	assert.False(t, h.c.Status().Foreground)

	require.NoError(t, h.c.Close(t.Context()))
	delivered := len(h.rec.all())
	h.tasks.foreground(true)
	assert.Len(t, h.rec.all(), delivered, "no event after Close")
}

const stopText = "Background task t1 stopped by the user: make t1\nbuilt t1"

// A human-stop notice given back undelivered stays non-waking: a send-now
// cancel with nothing else to send ends the Work.
func TestUndeliveredStopNoticeDoesNotContinueTurn(t *testing.T) {
	h, _ := openHarness(t, Config{Wake: true})
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "ok")})
	h.tasks.set(runningTask("t1"))
	require.NoError(t, h.c.StopTask("t1"))
	h.c.queue.Lock()
	notices := h.c.takeNotices()
	h.c.queue.Unlock()
	require.Len(t, notices, 1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	w := &Work{id: "turn-1", done: make(chan struct{}), cancel: cancel}
	h.c.mu.Lock()
	h.c.turn = w
	h.c.mu.Unlock()

	h.c.runTurn(t.Context(), ctx, h.c.sess, h.c.emit, w, nil, notices)
	wait(t, w.Done())

	assert.Zero(t, h.faux.CallCount())
	assert.True(t, h.idle())
	queued := h.queuedNotices()
	require.Len(t, queued, 1)
	assert.Equal(t, string(tool.TaskStopped), queued[0].Shown.Status)
	h.turn(t, "hi")
	assert.Equal(t, []string{"hi", stopText}, receive(t, requests).UserTexts)
}

func TestRequeueKeepsStopNoticesNonWaking(t *testing.T) {
	h, _ := openHarness(t, Config{Wake: true})
	stopped := runningTask("t1")
	stopped.Status = tool.TaskStopped
	stop, end := notice(stopped, "stop"), notice(endedTask("t2"), "end")
	h.c.queue.Lock()
	defer h.c.queue.Unlock()

	h.c.requeue([]pigruntime.Notice{stop})
	assert.False(t, h.c.waking())

	h.c.takeNotices()
	h.c.requeue([]pigruntime.Notice{stop, end})
	assert.Equal(t, []pigruntime.Notice{stop, end}, h.c.notices)
	assert.True(t, h.c.waking())
}

// A human stop during the final reply is not handed over alone, so the run
// makes no extra request for it.
func TestStopNoticeAloneIsNotHandedOver(t *testing.T) {
	h, _ := openHarness(t, Config{Wake: true})
	requests := make(chan pigruntimetest.Request, 1)
	h.tasks.set(runningTask("t1"))
	h.faux.SetResponses([]ai.FauxResponseStep{
		ai.FauxFactoryStep(func(ai.TranscriptContext, ai.StreamOptions, *ai.FauxProviderState, *ai.Model) (ai.AssistantMessage, error) {
			assert.NoError(t, h.c.StopTask("t1"))
			return ai.FauxAssistantMessage(ai.FauxContentString("final"), ai.FauxAssistantMessageOptions{}), nil
		}),
		pigruntimetest.Capture(t, requests, "ok"),
	})

	w := h.turn(t, "go")

	assert.Equal(t, "final", w.Reply())
	assert.Equal(t, 1, h.faux.CallCount())
	assert.True(t, h.idle())
	require.Len(t, h.queuedNotices(), 1)
	h.turn(t, "next")
	assert.Equal(t, []string{"go", "next", stopText}, receive(t, requests).UserTexts)
}

func TestStopNoticeIsHandedOverWithWakingNotice(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	h, _ := openHarness(t, Config{Wake: true, Tools: []tool.Tool{releaseTool(started, release)}})
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Hold", "call-1")), pigruntimetest.Capture(t, requests, "done")})
	h.tasks.set(runningTask("t1"))

	w, err := h.c.Submit(t.Context(), "hold")
	require.NoError(t, err)
	wait(t, started)
	require.NoError(t, h.c.StopTask("t1"))
	h.tasks.end(endedTask("t2"))
	close(release)
	wait(t, w.Done())

	assert.Equal(t, []string{"hold", stopText, endText("t2")}, receive(t, requests).UserTexts)
	assert.Equal(t, 2, h.faux.CallCount())
	assert.Empty(t, h.queuedNotices())
}

func TestFailedSwitchKeepsNotices(t *testing.T) {
	h, _ := openHarness(t, Config{Wake: true})
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "ok")})
	errs := subscribeTo[event.Error](h.c)
	h.tasks.set(runningTask("t1"))
	require.NoError(t, h.c.StopTask("t1"))

	// Holding life keeps the switch under way; asserts only, so a failure
	// cannot leave it held.
	h.c.life.Lock()
	_, err := h.c.Submit(t.Context(), "/resume no-such-session")
	assert.NoError(t, err)
	h.tasks.end(endedTask("t2"))
	assert.True(t, h.idle(), "no turn starts during a switch")
	h.c.life.Unlock()
	require.ErrorIs(t, receiveEvent(t, errs).Err, pigruntime.ErrSessionNotFound)
	assertSwitchDone(t, h)

	assert.True(t, h.idle())
	assert.Len(t, h.queuedNotices(), 2)
	_, stopAlls, _ := h.tasks.counts()
	assert.Zero(t, stopAlls)
	h.turn(t, "next")
	assert.Equal(t, []string{"next", stopText, endText("t2")}, receive(t, requests).UserTexts)
}

func TestTaskEndDuringLoginWakesAfterLogin(t *testing.T) {
	h := openLoginHarness(t, Config{Wake: true})
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "noted")})
	ends := turnEnds(h.c)
	run, _ := h.startLogin(t)

	h.tasks.end(endedTask("t1"))

	assert.True(t, h.idle(), "no turn starts during a login")
	assert.Len(t, h.queuedNotices(), 1)
	assert.Empty(t, ofType[event.TurnStart](h.rec.all()))

	require.True(t, h.c.Interrupt())
	<-run.done

	end := receiveEnd(t, ends)
	assert.Equal(t, event.Completed, end.Outcome.Status)
	texts := receive(t, requests).UserTexts
	require.NotEmpty(t, texts)
	assert.Equal(t, endText("t1"), texts[len(texts)-1])
	assert.Empty(t, h.queuedNotices())
}

func endedAgent(id string) tool.TaskInfo {
	info := endedTask(id)
	info.Agent = "reviewer"
	return info
}

func TestAgentEndWakesWithoutWake(t *testing.T) {
	for _, wake := range []bool{false, true} {
		t.Run(fmt.Sprintf("wake=%t", wake), func(t *testing.T) {
			h, _ := openHarness(t, Config{Wake: wake})
			requests := make(chan pigruntimetest.Request, 1)
			h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "noted")})
			ends := turnEnds(h.c)

			h.tasks.end(endedAgent("t1"))

			assert.Equal(t, event.Completed, receiveEnd(t, ends).Outcome.Status)
			texts := receive(t, requests).UserTexts
			assert.Equal(t, endText("t1"), texts[len(texts)-1])
			assert.Equal(t, "reviewer", ofType[event.TaskEnded](h.rec.all())[0].Agent)
		})
	}
}

func TestAgentEndContinuesTurnWithoutWake(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	h, _ := openHarness(t, Config{Tools: []tool.Tool{releaseTool(started, release)}})
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Hold", "call-1")), pigruntimetest.Capture(t, requests, "done")})

	w, err := h.c.Submit(t.Context(), "hold")
	require.NoError(t, err)
	wait(t, started)
	h.tasks.end(endedAgent("t1"))
	close(release)
	wait(t, w.Done())

	assert.Equal(t, []string{"hold", endText("t1")}, receive(t, requests).UserTexts)
	assert.Equal(t, 2, h.faux.CallCount())
}

func TestWakingIsPerNotice(t *testing.T) {
	stoppedAgent := endedAgent("t1")
	stoppedAgent.Status = tool.TaskStopped
	for _, tc := range []struct {
		name   string
		wake   bool
		notice tool.TaskInfo
		want   bool
	}{
		{"shell without wake", false, endedTask("t1"), false},
		{"shell with wake", true, endedTask("t1"), true},
		{"agent without wake", false, endedAgent("t1"), true},
		{"agent with wake", true, endedAgent("t1"), true},
		{"stopped agent without wake", false, stoppedAgent, false},
		{"stopped agent with wake", true, stoppedAgent, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := openHarness(t, Config{Wake: tc.wake})
			h.c.queue.Lock()
			defer h.c.queue.Unlock()
			h.c.notices = []pigruntime.Notice{notice(tc.notice, "head")}
			assert.Equal(t, tc.want, h.c.waking())
		})
	}
}

func TestStopTaskOnAgentQueuesAgentNotice(t *testing.T) {
	h, _ := openHarness(t, Config{Wake: true})
	var at *tool.AgentTask
	at = h.tasks.StartAgent("reviewer", "check the diff\nmore", func() { at.End(tool.TaskStopped, "", "") })
	assert.Equal(t, 1, h.c.Status().Agents)

	require.NoError(t, h.c.StopTask(at.ID()))

	assert.True(t, h.idle(), "a human stop does not wake the model")
	assert.Zero(t, h.faux.CallCount())
	assert.Zero(t, h.c.Status().Agents)
	queued := h.queuedNotices()
	require.Len(t, queued, 1)
	assert.Equal(t, "Agent "+at.ID()+" (reviewer) stopped by the user: check the diff\n(no output)", queued[0].Text)
	assert.Equal(t, "reviewer", queued[0].Shown.Agent)
	assert.Equal(t, string(tool.TaskStopped), queued[0].Shown.Status)
}

func TestStatusCountsAgentsSeparately(t *testing.T) {
	h, _ := openHarness(t, Config{})
	h.tasks.StartAgent("reviewer", "a", func() {})
	h.tasks.StartAgent("explorer", "b", func() {})
	done := h.tasks.StartAgent("explorer", "c", func() {})
	done.End(tool.TaskCompleted, "", "ok")
	h.tasks.set(tool.TaskInfo{ID: "s1", Command: "make", Status: tool.TaskRunning})
	h.tasks.set(func() tool.TaskInfo { i, _ := h.tasks.agents.Output(done.ID()); return i }())

	s := h.c.Status()
	assert.Equal(t, 2, s.Agents)
	assert.Equal(t, 1, s.Shells)
}
