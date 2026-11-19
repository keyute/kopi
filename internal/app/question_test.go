package app

import (
	"context"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/pigruntime/pigruntimetest"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/tool"
)

func questionCall(id string) pigruntimetest.Call {
	option := func(label string) map[string]any { return map[string]any{"label": label, "description": label + " it"} }
	return pigruntimetest.Call{Name: "AskUserQuestion", ID: id, Args: map[string]any{"questions": []any{
		map[string]any{"question": "Which one?", "header": "Pick", "options": []any{option("Red"), option("Blue")}},
	}}}
}

var picked = []tool.Answer{{Picks: []tool.Pick{{Option: 0}}}}

func TestQuestionAnswerReachesModel(t *testing.T) {
	h, _ := openHarness(t, Config{})
	asked := subscribeTo[event.QuestionRequested](h.c)
	ended := subscribeTo[event.QuestionEnded](h.c)
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(questionCall("call-1")), pigruntimetest.Capture(t, requests, "done")})

	w, err := h.c.Submit(t.Context(), "ask me")
	require.NoError(t, err)
	req := receiveEvent(t, asked)
	require.Len(t, req.Questions, 1)
	assert.Equal(t, "Which one?", req.Questions[0].Question)
	assert.True(t, h.c.AnswerQuestion(req.Token, []tool.Answer{{Picks: []tool.Pick{{Option: 1}}}}))
	wait(t, w.Done())

	assert.Equal(t, event.QuestionEnded{Token: req.Token}, receiveEvent(t, ended))
	assert.Equal(t, event.Outcome{Status: event.Completed}, w.Outcome())
	results := receive(t, requests).ToolResults
	require.Len(t, results, 1)
	assert.Contains(t, results[0], "The user answered:")
	assert.Contains(t, results[0], `"Blue"`)
}

func TestQuestionDismissalStopsTurnAndModelSeesIt(t *testing.T) {
	rules, err := policy.NewRules(t.TempDir(), t.TempDir(), map[string]policy.ToolSpec{"Edit": {}, "Write": {}}, policy.Source{Name: "test", Allow: []string{"Write"}})
	require.NoError(t, err)
	h, sb := editHarness(t, Config{Rules: rules})
	asked := subscribeTo[event.QuestionRequested](h.c)
	ended := subscribeTo[event.QuestionEnded](h.c)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(questionCall("call-1"), writeCall("call-2", "b.txt")), pigruntimetest.Text("unreached")})

	w, err := h.c.Submit(t.Context(), "ask me")
	require.NoError(t, err)
	req := receiveEvent(t, asked)
	queued, err := h.c.Submit(t.Context(), "queued meanwhile")
	require.NoError(t, err)
	assert.Nil(t, queued)
	assert.True(t, h.c.AnswerQuestion(req.Token, nil))
	wait(t, w.Done())

	assert.Equal(t, event.QuestionEnded{Token: req.Token}, receiveEvent(t, ended))
	assert.Equal(t, event.Outcome{Status: event.Interrupted}, w.Outcome())
	turnEnds := ofType[event.TurnEnd](h.rec.all())
	require.Len(t, turnEnds, 1)
	assert.Equal(t, []event.Input{{Text: "queued meanwhile"}}, turnEnds[0].Unsent)
	assert.Equal(t, "alpha\n", fileText(t, sb, "b.txt"), "the following rule-allowed tool must not run")
	assert.Equal(t, 1, h.faux.CallCount(), "the dismissal stops the turn before another model request")
	ends := ofType[event.ToolEnd](h.rec.all())
	require.Len(t, ends, 1)
	assert.False(t, ends[0].IsError)

	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "understood")})
	h.turn(t, "carry on")
	results := receive(t, requests).ToolResults
	require.NotEmpty(t, results)
	assert.Contains(t, results[0], "dismissed")
}

func TestQuestionClaimedDismissalSurvivesCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		rec := &recorder{}
		c := &Controller{turn: &Work{sendCtx: ctx, cancel: cancel}, emit: rec.add}
		done := make(chan struct{})
		var answers []tool.Answer
		var err error
		go func() {
			answers, err = c.askQuestion(ctx, nil)
			close(done)
		}()

		synctest.Wait()
		asked := ofType[event.QuestionRequested](rec.all())
		require.Len(t, asked, 1)
		c.queue.Lock()
		c.mu.Lock()
		p := c.question
		require.NotNil(t, p)
		p.claimed = true
		c.question = nil
		c.mu.Unlock()
		c.queue.Unlock()
		// Hold delivery after AnswerQuestion claims the slot, forcing cancellation
		// to reach askQuestion before the buffered answer is available.
		defer close(p.answer)
		p.work.cancel()
		synctest.Wait()
		select {
		case <-done:
			assert.Fail(t, "askQuestion returned before the claimed answer was delivered")
		default:
		}

		p.answer <- nil
		synctest.Wait()
		<-done
		require.NoError(t, err)
		assert.Nil(t, answers)
		assert.Equal(t, []event.QuestionEnded{{Token: asked[0].Token}}, ofType[event.QuestionEnded](rec.all()))
	})
}

func TestQuestionCancelledByInterruptEnds(t *testing.T) {
	h, _ := openHarness(t, Config{})
	asked := subscribeTo[event.QuestionRequested](h.c)
	ended := subscribeTo[event.QuestionEnded](h.c)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(questionCall("call-1")), pigruntimetest.Text("unreached")})

	w, err := h.c.Submit(t.Context(), "ask me")
	require.NoError(t, err)
	req := receiveEvent(t, asked)
	assert.True(t, h.c.Interrupt())

	assert.Equal(t, event.QuestionEnded{Token: req.Token}, receiveEvent(t, ended))
	wait(t, w.Done())
	assert.Equal(t, event.Outcome{Status: event.Interrupted}, w.Outcome())
	assert.False(t, h.c.AnswerQuestion(req.Token, nil), "the cancelled question is gone")
}

func TestQuestionCancelledBySessionSwitchEnds(t *testing.T) {
	h, _ := openHarness(t, Config{})
	asked := subscribeTo[event.QuestionRequested](h.c)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(questionCall("call-1")), pigruntimetest.Text("unreached")})
	oldID := h.c.Status().SessionID

	w, err := h.c.Submit(t.Context(), "ask me")
	require.NoError(t, err)
	req := receiveEvent(t, asked)
	require.NoError(t, h.c.NewSession(t.Context()))

	wait(t, w.Done())
	evs := h.rec.all()
	endedAt := slices.Index(evs, event.Event(event.QuestionEnded{Token: req.Token}))
	changedAt := slices.IndexFunc(evs, func(ev event.Event) bool {
		changed, ok := ev.(event.SessionChanged)
		return ok && changed.SessionID != oldID
	})
	require.GreaterOrEqual(t, endedAt, 0)
	assert.Less(t, endedAt, changedAt)
	assert.False(t, h.c.AnswerQuestion(req.Token, nil))
}

func TestQuestionStaleAnswerIsIgnored(t *testing.T) {
	h, _ := openHarness(t, Config{})
	asked := subscribeTo[event.QuestionRequested](h.c)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(questionCall("call-1")), pigruntimetest.Text("done")})
	assert.False(t, h.c.AnswerQuestion(0, nil), "nothing is pending yet")

	w, err := h.c.Submit(t.Context(), "ask me")
	require.NoError(t, err)
	req := receiveEvent(t, asked)
	assert.False(t, h.c.AnswerQuestion(req.Token+1, nil))
	assert.True(t, h.c.AnswerQuestion(req.Token, picked))
	assert.False(t, h.c.AnswerQuestion(req.Token, picked))
	wait(t, w.Done())

	assert.Equal(t, event.Outcome{Status: event.Completed}, w.Outcome())
}

func TestQuestionCallsAreAskedOneAtATime(t *testing.T) {
	h, _ := openHarness(t, Config{})
	asked := subscribeTo[event.QuestionRequested](h.c)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(questionCall("call-1"), questionCall("call-2")), pigruntimetest.Text("done")})

	w, err := h.c.Submit(t.Context(), "ask me twice")
	require.NoError(t, err)
	first := receiveEvent(t, asked)
	assert.True(t, h.c.AnswerQuestion(first.Token, picked))
	second := receiveEvent(t, asked)
	assert.NotEqual(t, first.Token, second.Token)
	assert.True(t, h.c.AnswerQuestion(second.Token, picked))
	wait(t, w.Done())

	evs := h.rec.all()
	firstEnded := slices.Index(evs, event.Event(event.QuestionEnded{Token: first.Token}))
	secondAsked := slices.IndexFunc(evs, func(ev event.Event) bool {
		q, ok := ev.(event.QuestionRequested)
		return ok && q.Token == second.Token
	})
	require.GreaterOrEqual(t, firstEnded, 0)
	assert.Less(t, firstEnded, secondAsked)
}

func TestQuestionNeedsNoApprovalInPlanMode(t *testing.T) {
	h, _ := openHarness(t, Config{})
	asked := subscribeTo[event.QuestionRequested](h.c)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(questionCall("call-1")), pigruntimetest.Text("done")})

	w, err := h.c.Submit(t.Context(), "ask me")
	require.NoError(t, err)
	mode, _ := h.c.authority.Snapshot()
	require.Equal(t, policy.Plan, mode)
	req := receiveEvent(t, asked)
	assert.True(t, h.c.AnswerQuestion(req.Token, nil))
	wait(t, w.Done())

	assert.Empty(t, ofType[event.ApprovalRequested](h.rec.all()))
}

func TestQuestionToolIsAttendedOnly(t *testing.T) {
	h, _ := openHarness(t, Config{})
	assert.Contains(t, toolNames(h.c.tools), "AskUserQuestion")

	h, _ = openHarness(t, Config{Unattended: true})
	assert.NotContains(t, toolNames(h.c.tools), "AskUserQuestion")

	sb := pigruntimetest.NewSandbox(t)
	rules, err := policy.NewRules(sb.Workspace, sb.Home, tool.Specs([]tool.Tool{tool.NewAskUserQuestion(nil)}), policy.Source{Name: "test", Deny: []string{"AskUserQuestion"}})
	require.NoError(t, err)
	h = newHarness(t, sb, Config{Rules: rules})
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	assert.NotContains(t, toolNames(h.c.tools), "AskUserQuestion")
}
