//go:build unix

package app

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/pigruntime/pigruntimetest"
	"github.com/keyute/kopi/internal/tool"
)

// gatedCmd is a shell command that prints output and exits once release is
// called. started closes when its process runs and dead when its process group
// is gone, killed or not.
type gatedCmd struct {
	name, command, output string
	release               func()
	started, dead         <-chan struct{}
}

type userHarness struct {
	*harness
	requests chan pigruntimetest.Request
	// changed holds one pending signal, however many events arrived.
	changed chan struct{}
	names   map[string]string
	tb      *testing.T
}

func newUserHarness(t *testing.T, cfg Config) *userHarness {
	t.Helper()
	sb := pigruntimetest.NewSandbox(t)
	cfg.Tasks = tool.NewTasks()
	cfg.UserShell = tool.NewShell(sb.Workspace, nil)
	cfg.Wake = true
	h := newHarness(t, sb, cfg)
	u := &userHarness{harness: h, requests: make(chan pigruntimetest.Request, 8), changed: make(chan struct{}, 1), names: map[string]string{}, tb: t}
	h.c.Subscribe(func(event.Event) {
		select {
		case u.changed <- struct{}{}:
		default:
		}
	})
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	return u
}

// cmd returns a gated command that prints output.
func (u *userHarness) cmd(name, output string) gatedCmd {
	t := u.tb
	dir := t.TempDir()
	in, alive := filepath.Join(dir, "input"), filepath.Join(dir, "alive")
	require.NoError(t, syscall.Mkfifo(in, 0o600))
	require.NoError(t, syscall.Mkfifo(alive, 0o600))
	f, err := os.OpenFile(in, os.O_RDWR, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	started, dead := make(chan struct{}), make(chan struct{})
	go func() {
		r, err := os.Open(alive)
		if err != nil {
			return
		}
		defer r.Close()
		close(started)
		_, _ = io.Copy(io.Discard, r)
		close(dead)
	}()
	command := "(exec 3>'" + alive + "'; read x <'" + in + "'; echo " + output + ") & wait"
	u.names[command] = name
	return gatedCmd{name: name, command: command, output: output, started: started, dead: dead, release: func() { _, _ = f.WriteString("go\n") }}
}

func (u *userHarness) submit(t *testing.T, text string) {
	t.Helper()
	_, err := u.c.Submit(t.Context(), Main, text)
	require.NoError(t, err)
}

func (u *userHarness) run(t *testing.T, c gatedCmd) {
	t.Helper()
	u.submit(t, "!"+c.command)
}

// until waits for cond, which changes only together with an event.
func (u *userHarness) until(t *testing.T, cond func() bool) {
	t.Helper()
	for !cond() {
		select {
		case <-u.changed:
		case <-time.After(waitLimit):
			t.Fatal("timed out waiting for the state to change")
		}
	}
}

// shape describes the queue: prompts by text, commands as !name:state, a handed
// entry with a trailing *.
func (u *userHarness) shape() []string {
	u.c.queue.Lock()
	defer u.c.queue.Unlock()
	out := []string{}
	for _, e := range u.c.main.queued {
		s := e.prompt.Text
		if e.cmd != nil {
			s = "!" + u.names[e.cmd.text] + ":" + string(e.cmd.state)
		}
		if e.handed {
			s += "*"
		}
		out = append(out, s)
	}
	return out
}

func (u *userHarness) untilShape(t *testing.T, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	u.until(t, func() bool { return assert.ObjectsAreEqual(want, u.shape()) })
}

func (u *userHarness) commandEvents(state event.CommandState) int {
	n := 0
	for _, e := range ofType[event.UserCommand](u.rec.all()) {
		if e.State == state {
			n++
		}
	}
	return n
}

// trace lists the turn starts, the commands and prompts the model read and the
// turn ends, in order.
func (u *userHarness) trace() []string {
	var out []string
	for _, ev := range u.rec.all() {
		switch e := ev.(type) {
		case event.TurnStart:
			out = append(out, "start:"+e.Prompt)
		case event.UserCommand:
			out = append(out, "cmd:"+u.names[e.Command]+":"+string(e.State))
		case event.UserMessage:
			out = append(out, "msg:"+e.Text)
		case event.TurnEnd:
			out = append(out, "end")
		}
	}
	return out
}

// assertDrawnRightAfterTheQueueDrops checks that each prompt and command the
// model read follows the QueueChanged that removed it, back to back.
func assertDrawnRightAfterTheQueueDrops(t *testing.T, evs []event.Event) {
	t.Helper()
	for i, ev := range evs {
		switch ev.(type) {
		case event.UserMessage, event.UserCommand:
			require.Positive(t, i)
			assert.IsType(t, event.QueueChanged{}, evs[i-1])
		}
	}
}

func wantNotice(t *testing.T, got string, c gatedCmd, head string) {
	t.Helper()
	assert.True(t, strings.HasPrefix(got, "The user ran a shell command"+head), got)
	assert.Contains(t, got, "\n$ "+c.command)
	if c.output != "" && !strings.Contains(head, "stopped") && !strings.Contains(head, "moved") {
		assert.Contains(t, got, c.output)
	}
}

const (
	exited  = " (exit code 0"
	stopped = " and stopped it"
	moved   = " and moved it to the background as task "
)

func TestPromptBehindARunningCommandWaitsAndOneRequestReadsBoth(t *testing.T) {
	u := newUserHarness(t, Config{})
	a := u.cmd("a", "out-a")
	u.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, u.requests, "ok")})
	ends := turnEnds(u.c)

	u.run(t, a)
	wait(t, a.started)
	u.submit(t, "P")
	assert.Zero(t, u.faux.CallCount())
	assert.Empty(t, ofType[event.TurnStart](u.rec.all()), "no turn starts until the command ends")
	assert.Equal(t, []string{"!a:running", "P"}, u.shape())

	a.release()
	assert.Equal(t, event.Completed, receiveEnd(t, ends).Outcome.Status)
	req := receive(t, u.requests)
	require.Len(t, req.UserTexts, 2)
	wantNotice(t, req.UserTexts[0], a, exited)
	assert.Equal(t, "P", req.UserTexts[1])
	assert.Equal(t, []string{"start:", "cmd:a:exited", "msg:P", "end"}, u.trace())
	assertDrawnRightAfterTheQueueDrops(t, u.rec.all())
	assert.Empty(t, u.shape())
}

func TestMidTurnCommandsAndPromptReachTheModelInTypingOrder(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	u := newUserHarness(t, Config{Tools: []tool.Tool{releaseTool(started, release)}})
	a, b, c := u.cmd("a", "out-a"), u.cmd("b", "out-b"), u.cmd("c", "out-c")
	u.faux.SetResponses([]ai.FauxResponseStep{
		pigruntimetest.ToolCalls(call("Hold", "call-1")),
		pigruntimetest.Capture(t, u.requests, "done"),
		pigruntimetest.Capture(t, u.requests, "b-ack"),
		pigruntimetest.Capture(t, u.requests, "c-ack"),
	})
	ends := turnEnds(u.c)

	u.submit(t, "hold")
	wait(t, started)
	u.run(t, a)
	wait(t, a.started)
	a.release()
	u.untilShape(t, "!a:exited")
	u.submit(t, "P")
	u.run(t, b)
	wait(t, b.started)
	u.run(t, c)
	assert.Equal(t, []string{"!a:exited", "P", "!b:running", "!c:queued"}, u.shape())

	close(release)
	receiveEnd(t, ends)
	req := receive(t, u.requests)
	require.Len(t, req.UserTexts, 3)
	assert.Equal(t, "hold", req.UserTexts[0])
	wantNotice(t, req.UserTexts[1], a, exited)
	assert.Equal(t, "P", req.UserTexts[2])
	assert.Equal(t, []string{"!b:running", "!c:queued"}, u.shape(), "what is behind the running command stays")

	b.release()
	wait(t, c.started)
	receiveEnd(t, ends)
	req = receive(t, u.requests)
	wantNotice(t, req.UserTexts[len(req.UserTexts)-1], b, exited)
	c.release()
	receiveEnd(t, ends)
	req = receive(t, u.requests)
	wantNotice(t, req.UserTexts[len(req.UserTexts)-1], c, exited)
	assert.Equal(t, []string{"start:hold", "cmd:a:exited", "msg:P", "end", "start:", "cmd:b:exited", "end", "start:", "cmd:c:exited", "end"}, u.trace())
	assertDrawnRightAfterTheQueueDrops(t, u.rec.all())
}

func TestCommandFinishingBeforeTheLastBoundaryExtendsTheRunOnce(t *testing.T) {
	u := newUserHarness(t, Config{})
	a := u.cmd("a", "out-a")
	gate := make(chan struct{})
	u.faux.SetResponses([]ai.FauxResponseStep{gated(gate, "done"), pigruntimetest.Capture(t, u.requests, "ack")})
	ends := turnEnds(u.c)

	w, err := u.c.Submit(t.Context(), Main, "go")
	require.NoError(t, err)
	require.NotNil(t, w)
	u.run(t, a)
	wait(t, a.started)
	a.release()
	u.untilShape(t, "!a:exited")
	close(gate)

	wait(t, w.Done())
	assert.Equal(t, event.Completed, receiveEnd(t, ends).Outcome.Status)
	req := receive(t, u.requests)
	require.Len(t, req.UserTexts, 2)
	wantNotice(t, req.UserTexts[1], a, exited)
	assert.Equal(t, 1, u.commandEvents(event.CommandExited), "read exactly once")
	assert.Equal(t, 2, u.faux.CallCount(), "the same run goes on with a reply")
	assert.Empty(t, ends, "one turn")
	assert.Empty(t, u.shape())
}

func TestCommandFinishingWithNoTurnWakesTheModel(t *testing.T) {
	u := newUserHarness(t, Config{})
	a := u.cmd("a", "out-a")
	u.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, u.requests, "ack")})
	ends := turnEnds(u.c)

	u.run(t, a)
	wait(t, a.started)
	assert.Zero(t, u.faux.CallCount())
	a.release()
	receiveEnd(t, ends)
	req := receive(t, u.requests)
	require.Len(t, req.UserTexts, 1)
	wantNotice(t, req.UserTexts[0], a, exited)
	assert.Equal(t, []string{"start:", "cmd:a:exited", "end"}, u.trace())
}

func TestEscapeMidTurnKeepsTheCommandsAndSilencesThem(t *testing.T) {
	started := make(chan struct{})
	u := newUserHarness(t, Config{Tools: []tool.Tool{blockTool(started)}})
	a, b, c, d := u.cmd("a", "out-a"), u.cmd("b", "out-b"), u.cmd("c", "out-c"), u.cmd("d", "out-d")
	u.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Block", "call-1")), pigruntimetest.Capture(t, u.requests, "d-ack")})
	ends := turnEnds(u.c)

	u.submit(t, "block")
	wait(t, started)
	u.run(t, a)
	wait(t, a.started)
	a.release()
	u.untilShape(t, "!a:exited")
	u.run(t, b)
	wait(t, b.started)
	u.run(t, c)
	u.submit(t, "P")
	require.True(t, u.c.Interrupt(Main))

	end := receiveEnd(t, ends)
	assert.Equal(t, []event.Input{{Text: "P"}}, end.Unsent, "only the prompt is handed back")
	u.until(t, func() bool { return u.commandEvents(event.CommandExited) == 1 })
	assert.Equal(t, 1, u.faux.CallCount(), "a is recorded with no request")
	assert.Equal(t, []string{"!b:running", "!c:queued"}, u.shape())

	b.release()
	wait(t, c.started)
	u.until(t, func() bool { return u.commandEvents(event.CommandExited) == 2 })
	c.release()
	u.until(t, func() bool { return u.commandEvents(event.CommandExited) == 3 })
	u.untilShape(t)
	assert.Equal(t, 1, u.faux.CallCount(), "a command that ends after the stop is silent")

	u.run(t, d)
	wait(t, d.started)
	d.release()
	receiveEnd(t, ends)
	req := receive(t, u.requests)
	require.Len(t, req.UserTexts, 5)
	assert.Equal(t, "block", req.UserTexts[0])
	for i, cmd := range []gatedCmd{a, b, c, d} {
		wantNotice(t, req.UserTexts[i+1], cmd, exited)
	}
}

func TestEscapeAtIdleStopsTheRunningCommandAndDropsTheRest(t *testing.T) {
	u := newUserHarness(t, Config{})
	a, b := u.cmd("a", "out-a"), u.cmd("b", "out-b")
	unsents := subscribeTo[event.Unsent](u.c)

	u.run(t, a)
	wait(t, a.started)
	u.run(t, b)
	u.submit(t, "P")
	require.True(t, u.c.Interrupt(Main))

	assert.Equal(t, event.Unsent{Inputs: []event.Input{{Text: "P"}}, Dropped: 1}, receiveEvent(t, unsents))
	wait(t, a.dead)
	u.until(t, func() bool { return u.commandEvents(event.CommandStopped) == 1 })
	u.untilShape(t)
	assert.Zero(t, u.faux.CallCount(), "the stopped command is recorded with no request")
	select {
	case <-b.started:
		t.Fatal("the queued command ran")
	default:
	}
	assert.False(t, u.c.Interrupt(Main), "nothing left to stop")
}

func TestBackgroundingTheCommandLetsTheRestGoOn(t *testing.T) {
	for _, midTurn := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "mid-turn"}[midTurn], func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			u := newUserHarness(t, Config{Tools: []tool.Tool{releaseTool(started, release)}})
			a, b := u.cmd("a", "out-a"), u.cmd("b", "out-b")
			steps := []ai.FauxResponseStep{pigruntimetest.Capture(t, u.requests, "first"), pigruntimetest.Capture(t, u.requests, "second")}
			if midTurn {
				steps = slices.Concat([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Hold", "call-1"))}, steps)
			}
			u.faux.SetResponses(steps)
			ends := turnEnds(u.c)

			if midTurn {
				u.submit(t, "hold")
				wait(t, started)
			}
			u.run(t, a)
			wait(t, a.started)
			u.submit(t, "P")
			u.run(t, b)
			require.True(t, u.c.Background())
			wait(t, b.started)
			if midTurn {
				close(release)
			}
			receiveEnd(t, ends)
			req := receive(t, u.requests)
			texts := req.UserTexts
			require.GreaterOrEqual(t, len(texts), 2)
			wantNotice(t, texts[len(texts)-2], a, moved)
			assert.Equal(t, "P", texts[len(texts)-1])
			assert.Contains(t, texts[len(texts)-2], "t1")

			// The task's end is the ordinary notice, after the record.
			a.release()
			receiveEnd(t, ends)
			texts = receive(t, u.requests).UserTexts
			assert.Contains(t, texts[len(texts)-1], "Background task t1 completed")
			assert.Equal(t, []string{"!b:running"}, u.shape())
		})
	}
}

func TestSendNowBehindARunningCommandDoesNotCancelTheSend(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	u := newUserHarness(t, Config{Tools: []tool.Tool{releaseTool(started, release)}})
	a := u.cmd("a", "out-a")
	u.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Hold", "call-1")), pigruntimetest.Capture(t, u.requests, "done")})
	ends := turnEnds(u.c)

	u.submit(t, "hold")
	wait(t, started)
	u.run(t, a)
	wait(t, a.started)
	require.NoError(t, u.c.SendNow(t.Context(), Main, "P"))
	u.c.queue.Lock()
	assert.NoError(t, u.c.main.work.sendCtx.Err(), "the running send is not cancelled")
	u.c.queue.Unlock()
	assert.Equal(t, []string{"!a:running", "P"}, u.shape())

	a.release()
	u.untilShape(t, "!a:exited", "P")
	close(release)
	receiveEnd(t, ends)
	req := receive(t, u.requests)
	require.Len(t, req.UserTexts, 3)
	wantNotice(t, req.UserTexts[1], a, exited)
	assert.Equal(t, "P", req.UserTexts[2])
	assert.Equal(t, 2, u.faux.CallCount())
}

func TestNewSessionClearsTheQueueAndKillsTheRunningCommand(t *testing.T) {
	started := make(chan struct{})
	u := newUserHarness(t, Config{Tools: []tool.Tool{blockTool(started)}})
	a, b, c := u.cmd("a", "out-a"), u.cmd("b", "out-b"), u.cmd("c", "out-c")
	u.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Block", "call-1"))})

	u.submit(t, "block")
	wait(t, started)
	u.run(t, a)
	wait(t, a.started)
	a.release()
	u.untilShape(t, "!a:exited")
	u.run(t, b)
	wait(t, b.started)
	u.run(t, c)

	require.NoError(t, u.c.NewSession(t.Context()))
	wait(t, b.dead)
	assert.Empty(t, u.shape())
	assert.Equal(t, 1, u.faux.CallCount(), "nothing is delivered to the new session")
	assert.Empty(t, ofType[event.UserCommand](u.rec.all()))
	evs := u.rec.all()
	changed := slices.IndexFunc(evs, func(ev event.Event) bool { _, ok := ev.(event.SessionChanged); return ok })
	require.GreaterOrEqual(t, changed, 0)
	assert.Contains(t, evs[changed:], event.Event(event.QueueChanged{Pending: []event.Queued{}}))
	select {
	case <-c.started:
		t.Fatal("the queued command ran")
	default:
	}
}

func TestCloseKillsTheRunningCommandAndReturns(t *testing.T) {
	u := newUserHarness(t, Config{})
	a, b := u.cmd("a", "out-a"), u.cmd("b", "out-b")
	u.run(t, a)
	wait(t, a.started)
	u.run(t, b)

	closed := make(chan error, 1)
	go func() { closed <- u.c.Close(context.Background()) }()
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(waitLimit):
		t.Fatal("Close did not return")
	}
	wait(t, a.dead)
	assert.Empty(t, u.shape())
	select {
	case <-b.started:
		t.Fatal("the queued command ran")
	default:
	}
}

func TestFailingProviderGetsNoWakeLoopFromCommands(t *testing.T) {
	failing := func(gate <-chan struct{}) ai.FauxResponseStep {
		return ai.FauxFactoryStep(func(ai.TranscriptContext, ai.StreamOptions, *ai.FauxProviderState, *ai.Model) (ai.AssistantMessage, error) {
			<-gate
			return ai.FauxAssistantMessage(ai.FauxContentString(""), ai.FauxAssistantMessageOptions{StopReason: string(ai.StopReasonError), ErrorMessage: "boom"}), nil
		})
	}
	t.Run("ready command waits for the failed turn", func(t *testing.T) {
		u := newUserHarness(t, Config{})
		a := u.cmd("a", "out-a")
		gate := make(chan struct{})
		u.faux.SetResponses([]ai.FauxResponseStep{failing(gate)})
		ends := turnEnds(u.c)

		w, err := u.c.Submit(t.Context(), Main, "go")
		require.NoError(t, err)
		require.NotNil(t, w)
		u.run(t, a)
		wait(t, a.started)
		a.release()
		u.untilShape(t, "!a:exited")
		close(gate)

		assert.Equal(t, event.Failed, receiveEnd(t, ends).Outcome.Status)
		u.until(t, func() bool { return u.commandEvents(event.CommandExited) == 1 })
		u.untilShape(t)
		assert.Equal(t, 1, u.faux.CallCount(), "recorded with no request")
		assert.Empty(t, ends)
	})
	t.Run("command handed over before the failure", func(t *testing.T) {
		started, release := make(chan struct{}), make(chan struct{})
		u := newUserHarness(t, Config{Tools: []tool.Tool{releaseTool(started, release)}})
		a := u.cmd("a", "out-a")
		gate := make(chan struct{})
		close(gate)
		u.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Hold", "call-1")), failing(gate)})
		ends := turnEnds(u.c)

		u.submit(t, "hold")
		wait(t, started)
		u.run(t, a)
		wait(t, a.started)
		a.release()
		u.untilShape(t, "!a:exited")
		close(release)

		assert.Equal(t, event.Failed, receiveEnd(t, ends).Outcome.Status)
		u.untilShape(t)
		assert.Equal(t, 2, u.faux.CallCount())
		assert.Equal(t, 1, u.commandEvents(event.CommandExited))
		assert.Empty(t, ends)
	})
}

// A stopped command waits at the head only while a turn runs when the stop and
// the turn overlap; the test puts one there.
func TestStoppedCommandAtTheHeadIsNotHandedAtABoundary(t *testing.T) {
	inject := func(u *userHarness, es ...*entry) {
		u.c.queue.Lock()
		u.c.main.queued = append(u.c.main.queued, es...)
		u.c.queue.Unlock()
	}
	stopped := func() *entry {
		return &entry{cmd: &userCommand{text: "sleep 9", state: event.CommandStopped}}
	}
	t.Run("alone", func(t *testing.T) {
		started, release := make(chan struct{}), make(chan struct{})
		u := newUserHarness(t, Config{Tools: []tool.Tool{releaseTool(started, release)}})
		u.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Hold", "call-1")), pigruntimetest.Capture(t, u.requests, "done")})
		ends := turnEnds(u.c)
		u.submit(t, "hold")
		wait(t, started)
		inject(u, stopped())
		close(release)
		receiveEnd(t, ends)
		u.until(t, func() bool { return u.commandEvents(event.CommandStopped) == 1 })
		assert.Equal(t, []string{"hold"}, receive(t, u.requests).UserTexts)
		assert.Equal(t, 2, u.faux.CallCount())
		trace := u.trace()
		assert.Equal(t, "end", trace[len(trace)-2])
		assert.Equal(t, "cmd::stopped", trace[len(trace)-1], "recorded after the turn")
	})
	t.Run("with a prompt typed meanwhile", func(t *testing.T) {
		started, release := make(chan struct{}), make(chan struct{})
		u := newUserHarness(t, Config{Tools: []tool.Tool{releaseTool(started, release)}})
		u.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Hold", "call-1")), pigruntimetest.Capture(t, u.requests, "done")})
		ends := turnEnds(u.c)
		u.submit(t, "hold")
		wait(t, started)
		inject(u, stopped())
		u.submit(t, "P")
		close(release)
		receiveEnd(t, ends)
		texts := receive(t, u.requests).UserTexts
		require.Len(t, texts, 3)
		assert.True(t, strings.HasPrefix(texts[1], "The user ran a shell command and stopped it"))
		assert.Equal(t, "P", texts[2])
	})
}

func TestShellInputIsACommandOnlyWithAShellAndAtTheStart(t *testing.T) {
	t.Run("without a shell it is a prompt", func(t *testing.T) {
		h, _ := openHarness(t, Config{})
		requests := make(chan pigruntimetest.Request, 1)
		h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "ok")})
		h.turn(t, "!x")
		assert.Equal(t, []string{"!x"}, receive(t, requests).UserTexts)
	})
	t.Run("Prompt is verbatim", func(t *testing.T) {
		u := newUserHarness(t, Config{})
		u.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, u.requests, "ok")})
		w, err := u.c.Prompt(t.Context(), "!x")
		require.NoError(t, err)
		require.NotNil(t, w)
		wait(t, w.Done())
		assert.Equal(t, []string{"!x"}, receive(t, u.requests).UserTexts)
	})
	t.Run("leading space is a prompt", func(t *testing.T) {
		u := newUserHarness(t, Config{})
		u.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, u.requests, "ok")})
		w, err := u.c.Submit(t.Context(), Main, " !x")
		require.NoError(t, err)
		require.NotNil(t, w)
		wait(t, w.Done())
		assert.Equal(t, []string{"!x"}, receive(t, u.requests).UserTexts)
	})
	t.Run("recorded in history, bare bang ignored", func(t *testing.T) {
		u := newUserHarness(t, Config{History: historyStore(t)})
		u.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, u.requests, "ok")})
		ends := turnEnds(u.c)
		w, err := u.c.Submit(t.Context(), Main, "!")
		require.NoError(t, err)
		assert.Nil(t, w)
		w, err = u.c.Submit(t.Context(), Main, "!   ")
		require.NoError(t, err)
		assert.Nil(t, w)
		assert.Empty(t, u.c.History())
		assert.Empty(t, ofType[event.QueueChanged](u.rec.all()))

		w, err = u.c.Submit(t.Context(), Main, "!echo hi")
		require.NoError(t, err)
		assert.Nil(t, w)
		receiveEnd(t, ends)
		assert.Equal(t, []string{"!echo hi"}, u.c.History())
		texts := receive(t, u.requests).UserTexts
		require.Len(t, texts, 1)
		assert.Contains(t, texts[0], "$ echo hi\nhi")
	})
}

func TestUserCommandNoticeText(t *testing.T) {
	for _, tc := range []struct {
		name string
		u    userCommand
		want string
	}{
		{"exited", userCommand{text: "make", state: event.CommandExited, reason: "exit code 1", elapsed: 12 * time.Second, output: "boom\n"}, "The user ran a shell command (exit code 1, 12s):\n$ make\nboom\n"},
		{"exited minutes", userCommand{text: "make", state: event.CommandExited, reason: "exit code 0", elapsed: 65 * time.Second}, "The user ran a shell command (exit code 0, 1m5s):\n$ make"},
		{"exited under a second", userCommand{text: "true", state: event.CommandExited, reason: "exit code 0", elapsed: 300 * time.Millisecond}, "The user ran a shell command (exit code 0):\n$ true"},
		{"stopped", userCommand{text: "sleep 99", state: event.CommandStopped, elapsed: 12 * time.Second}, "The user ran a shell command and stopped it after 12s:\n$ sleep 99"},
		{"stopped under a second", userCommand{text: "sleep 99", state: event.CommandStopped, elapsed: time.Second / 2}, "The user ran a shell command and stopped it:\n$ sleep 99"},
		{"moved", userCommand{text: "make", state: event.CommandMoved, taskID: "t3", file: "/out/t3.txt"}, "The user ran a shell command and moved it to the background as task t3" + tool.BackgroundTail("/out/t3.txt", true) + "\n$ make"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := tc.u.notice(true)
			assert.Equal(t, tc.want, n.Text)
			shown, ok := n.Shown.(event.UserCommand)
			require.True(t, ok)
			assert.Equal(t, tc.u.state, shown.State)
			assert.Equal(t, tc.u.text, shown.Command)
		})
	}
	assert.Equal(t, "exit 3", exitText("exit code 3"))
	assert.Equal(t, "signal: killed", exitText("signal: killed"))
}

func TestCommandThatStartsMidTurnIsReportedRunning(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	u := newUserHarness(t, Config{Tools: []tool.Tool{releaseTool(started, release)}})
	a, b := u.cmd("a", "out-a"), u.cmd("b", "out-b")
	u.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Hold", "call-1")), pigruntimetest.Text("done")})
	ends := turnEnds(u.c)

	u.submit(t, "hold")
	wait(t, started)
	u.run(t, a)
	wait(t, a.started)
	u.run(t, b)
	a.release()
	u.untilShape(t, "!a:exited", "!b:running")

	queues := ofType[event.QueueChanged](u.rec.all())
	pending := queues[len(queues)-1].Pending
	require.Len(t, pending, 2)
	assert.Equal(t, event.CommandRunning, pending[1].Command.State)
	assert.False(t, pending[1].Command.Started.IsZero())
	close(release)
	receiveEnd(t, ends)
}

func TestPromptHandedBackKeepsItsEscape(t *testing.T) {
	started := make(chan struct{})
	u := newUserHarness(t, Config{Tools: []tool.Tool{blockTool(started)}})
	u.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(call("Block", "call-1"))})
	ends := turnEnds(u.c)

	u.submit(t, "block")
	wait(t, started)
	u.submit(t, " !make clean")
	assert.Equal(t, []event.Input{{Text: " !make clean"}}, u.c.TakeQueued(Main))
	u.submit(t, " !make clean")
	u.submit(t, "plain")
	require.True(t, u.c.Interrupt(Main))
	assert.Equal(t, []event.Input{{Text: " !make clean"}, {Text: "plain"}}, receiveEnd(t, ends).Unsent)
}
