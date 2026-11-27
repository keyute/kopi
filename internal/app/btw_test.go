package app

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/pigruntime"
	"github.com/keyute/kopi/internal/pigruntime/pigruntimetest"
	"github.com/keyute/kopi/internal/tool"
)

// sideEvents delivers the side question events in the order they were sent.
func sideEvents(h *harness) <-chan event.Event {
	ch := make(chan event.Event, 16)
	h.c.Subscribe(func(ev event.Event) {
		switch ev.(type) {
		case event.SideQuestionAsked, event.SideQuestionEnded:
			ch <- ev
		}
	})
	return ch
}

func nextSide(t *testing.T, ch <-chan event.Event) event.Event {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for a side question event")
		return nil
	}
}

func nextEnded(t *testing.T, ch <-chan event.Event) event.SideQuestionEnded {
	t.Helper()
	ended, ok := nextSide(t, ch).(event.SideQuestionEnded)
	require.True(t, ok)
	return ended
}

func sideAsked(t *testing.T, ch <-chan event.Event) event.SideQuestionAsked {
	t.Helper()
	asked, ok := nextSide(t, ch).(event.SideQuestionAsked)
	require.True(t, ok)
	return asked
}

// heldHarness is a harness whose model holds each request until release is
// called, which the cleanup also does so the test server can close.
func heldHarness(t *testing.T) (h *harness, reached chan struct{}, release func()) {
	t.Helper()
	sb := pigruntimetest.NewSandbox(t)
	reached = make(chan struct{}, 4)
	gate := make(chan struct{})
	release = sync.OnceFunc(func() { close(gate) })
	h = newHarness(t, sb, Config{Model: sb.HeldPricedModel(t, reached, gate)})
	t.Cleanup(release)
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	return h, reached, release
}

func TestBtwWithoutAQuestionIsRefused(t *testing.T) {
	h, _ := openHarness(t, Config{})
	side := sideEvents(h)

	_, err := h.c.Submit(t.Context(), Main, "/btw")

	require.ErrorIs(t, err, ErrMissingArgument)
	assert.Empty(t, side)
}

func TestBtwWithoutASessionIsRefused(t *testing.T) {
	h := newHarness(t, pigruntimetest.NewSandbox(t), Config{})

	assert.ErrorIs(t, h.c.btwCommand(t.Context(), "q"), ErrNoSession)
}

func TestBtwDuringAModelChangeIsRefused(t *testing.T) {
	h, _ := openHarness(t, Config{})
	h.c.mu.Lock()
	h.c.changingModel = true
	h.c.mu.Unlock()

	assert.ErrorIs(t, h.c.btwCommand(t.Context(), "q"), ErrBusy)
}

func TestBtwAnswersWithoutEnteringTheSession(t *testing.T) {
	h, _ := openHarness(t, Config{})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("side answer")})
	side := sideEvents(h)
	before := h.c.main.sess.MessageCount()

	_, err := h.c.Submit(t.Context(), Main, "/btw  what now?")
	require.NoError(t, err)

	asked := sideAsked(t, side)
	assert.Equal(t, "what now?", asked.Question)
	ended := nextEnded(t, side)
	assert.Equal(t, event.SideQuestionEnded{ID: asked.ID, Answer: "side answer"}, ended)
	assert.Equal(t, before, h.c.main.sess.MessageCount())
	assert.Empty(t, side)
}

func TestBtwWorksWhileATurnRuns(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	h, _ := openHarness(t, Config{Tools: []tool.Tool{releaseTool(started, release)}})
	h.faux.SetResponses([]ai.FauxResponseStep{
		pigruntimetest.ToolCalls(call("Hold", "call-1")),
		pigruntimetest.Text("side answer"),
		pigruntimetest.Text("done"),
	})
	side := sideEvents(h)
	w, err := h.c.Submit(t.Context(), Main, "hold")
	require.NoError(t, err)
	wait(t, started)

	_, err = h.c.Submit(t.Context(), Main, "/btw q")
	require.NoError(t, err)
	asked := sideAsked(t, side)
	ended := nextEnded(t, side)
	close(release)
	wait(t, w.Done())

	assert.Equal(t, event.SideQuestionEnded{ID: asked.ID, Answer: "side answer"}, ended)
	assert.Equal(t, event.Completed, w.Outcome().Status)
	assert.Equal(t, "done", w.Reply())
}

func TestStopSideQuestionCancelsTheRunningOne(t *testing.T) {
	h, reached, _ := heldHarness(t)
	side := sideEvents(h)
	require.NoError(t, h.c.btwCommand(t.Context(), "q"))
	asked := sideAsked(t, side)
	wait(t, reached)

	h.c.StopSideQuestion(asked.ID)

	ended := nextEnded(t, side)
	assert.Equal(t, asked.ID, ended.ID)
	assert.ErrorIs(t, ended.Err, context.Canceled)
	assert.Empty(t, side)
}

func TestStopSideQuestionIgnoresAnotherIDAndAnEndedOne(t *testing.T) {
	h, reached, release := heldHarness(t)
	side := sideEvents(h)
	require.NoError(t, h.c.btwCommand(t.Context(), "q"))
	asked := sideAsked(t, side)
	wait(t, reached)

	h.c.StopSideQuestion(asked.ID + 1)
	release()
	ended := nextEnded(t, side)
	h.c.StopSideQuestion(asked.ID)

	assert.NoError(t, ended.Err)
	assert.Equal(t, "priced answer", ended.Answer)
	assert.Empty(t, side)
}

func TestASecondBtwCancelsTheFirst(t *testing.T) {
	h, reached, release := heldHarness(t)
	side := sideEvents(h)
	require.NoError(t, h.c.btwCommand(t.Context(), "first"))
	first := sideAsked(t, side)
	wait(t, reached)

	require.NoError(t, h.c.btwCommand(t.Context(), "second"))
	second := sideAsked(t, side)
	wait(t, reached)
	release()

	ended := map[int]event.SideQuestionEnded{}
	for range 2 {
		e := nextEnded(t, side)
		ended[e.ID] = e
	}
	assert.NotEqual(t, first.ID, second.ID)
	require.Len(t, ended, 2)
	assert.ErrorIs(t, ended[first.ID].Err, context.Canceled)
	assert.NoError(t, ended[second.ID].Err)
	assert.Empty(t, side)
}

func TestASessionSwitchCancelsASideQuestion(t *testing.T) {
	h, reached, _ := heldHarness(t)
	side := sideEvents(h)
	require.NoError(t, h.c.btwCommand(t.Context(), "q"))
	asked := sideAsked(t, side)
	wait(t, reached)

	require.NoError(t, h.c.NewSession(t.Context()))

	ended := nextEnded(t, side)
	assert.Equal(t, asked.ID, ended.ID)
	assert.ErrorIs(t, ended.Err, context.Canceled)
	assert.Zero(t, h.c.Status().TotalCost)
	assert.Empty(t, side)
}

func TestCloseCancelsASideQuestion(t *testing.T) {
	h, reached, _ := heldHarness(t)
	side := sideEvents(h)
	require.NoError(t, h.c.btwCommand(t.Context(), "q"))
	asked := sideAsked(t, side)
	wait(t, reached)

	closed := make(chan error, 1)
	go func() { closed <- h.c.Close(t.Context()) }()
	require.NoError(t, receiveErr(t, closed))

	ended := nextEnded(t, side)
	assert.Equal(t, asked.ID, ended.ID)
	assert.ErrorIs(t, ended.Err, context.Canceled)
	assert.Empty(t, side)
}

func TestBtwChargesTheTotal(t *testing.T) {
	h, _, release := heldHarness(t)
	release()
	side := sideEvents(h)

	require.NoError(t, h.c.btwCommand(t.Context(), "q"))
	sideAsked(t, side)
	require.NoError(t, nextEnded(t, side).Err)

	assert.Equal(t, pigruntimetest.PricedModelCost, h.c.Status().TotalCost)
	assert.Equal(t, pigruntimetest.PricedModelCost, pigruntime.Usage{Models: h.c.Usage().Models}.Cost())
}

func TestBtwSendsTheQuestionUnderTheWrapper(t *testing.T) {
	h, _ := openHarness(t, Config{})
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "side answer")})
	side := sideEvents(h)

	require.NoError(t, h.c.btwCommand(t.Context(), "what now?"))
	sideAsked(t, side)
	require.NoError(t, nextEnded(t, side).Err)

	assert.Equal(t, []string{sideQuestionPrefix + "\n\n" + "what now?"}, receive(t, requests).UserTexts)
}
