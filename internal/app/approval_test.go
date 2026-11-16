package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/pigruntime"
	"github.com/keyute/kopi/internal/pigruntime/pigruntimetest"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/tool"
)

// editHarness opens an execute-mode session with Edit and Write over the
// sandbox workspace, which holds a.txt and b.txt.
func editHarness(t *testing.T, cfg Config) (*harness, pigruntimetest.Sandbox) {
	t.Helper()
	sb := pigruntimetest.NewSandbox(t)
	for _, name := range []string{"a.txt", "b.txt"} {
		require.NoError(t, os.WriteFile(filepath.Join(sb.Workspace, name), []byte("alpha\n"), 0o600))
	}
	ws, err := tool.NewWorkspace(sb.Workspace, nil)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, ws.Close()) })
	cfg.Tools = []tool.Tool{tool.NewEdit(ws), tool.NewWrite(ws)}
	h := newHarness(t, sb, cfg)
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	h.c.EnterExecute()
	return h, sb
}

func editCall(id, path string) pigruntimetest.Call {
	return pigruntimetest.Call{Name: "Edit", ID: id, Args: map[string]any{"file_path": path, "old_string": "alpha", "new_string": "beta"}}
}

func writeCall(id, path string) pigruntimetest.Call {
	return pigruntimetest.Call{Name: "Write", ID: id, Args: map[string]any{"file_path": path, "content": "gamma\n"}}
}

func fileText(t *testing.T, sb pigruntimetest.Sandbox, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(sb.Workspace, name))
	require.NoError(t, err)
	return string(b)
}

func TestApprovalYesRunsEdit(t *testing.T) {
	h, sb := editHarness(t, Config{})
	approvals, ended := subscribeTo[event.ApprovalRequested](h.c), subscribeTo[event.ApprovalEnded](h.c)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(editCall("call-1", "a.txt")), pigruntimetest.Text("done")})

	w, err := h.c.Submit(t.Context(), "edit a.txt")
	require.NoError(t, err)
	req := receiveEvent(t, approvals)
	assert.Equal(t, "call-1", req.CallID)
	assert.Contains(t, req.Title, "a.txt")
	assert.Equal(t, []tool.PreviewLine{{Kind: tool.Removed, Text: "alpha"}, {Kind: tool.Added, Text: "beta"}}, req.Preview)
	assert.Empty(t, req.Rule)
	assert.True(t, req.Edits)
	assert.True(t, h.c.Answer(req.Token, policy.Decision{Answer: policy.Allow}))
	wait(t, w.Done())

	assert.Equal(t, event.ApprovalEnded{Token: req.Token}, receiveEvent(t, ended))
	assert.Equal(t, event.Outcome{Status: event.Completed}, w.Outcome())
	assert.Equal(t, "beta\n", fileText(t, sb, "a.txt"))
}

func TestApprovalAllowEditsSkipsLaterPrompts(t *testing.T) {
	h, sb := editHarness(t, Config{})
	approvals := subscribeTo[event.ApprovalRequested](h.c)
	h.faux.SetResponses([]ai.FauxResponseStep{
		pigruntimetest.ToolCalls(editCall("call-1", "a.txt")),
		pigruntimetest.ToolCalls(writeCall("call-2", "b.txt")),
		pigruntimetest.Text("done"),
	})

	w, err := h.c.Submit(t.Context(), "edit both")
	require.NoError(t, err)
	req := receiveEvent(t, approvals)
	assert.True(t, h.c.Answer(req.Token, policy.Decision{Answer: policy.AllowEdits}))
	wait(t, w.Done())

	assert.Equal(t, event.Outcome{Status: event.Completed}, w.Outcome())
	assert.Equal(t, "beta\n", fileText(t, sb, "a.txt"))
	assert.Equal(t, "gamma\n", fileText(t, sb, "b.txt"))
	evs := h.rec.all()
	assert.Len(t, ofType[event.ApprovalRequested](evs), 1)
	assert.Equal(t, []event.ApprovalEnded{{Token: req.Token}}, ofType[event.ApprovalEnded](evs))
}

func TestApprovalNoInterruptsTurnAndModelSeesRejection(t *testing.T) {
	for _, reason := range []string{"", " \n\t"} {
		t.Run(fmt.Sprintf("reason=%q", reason), func(t *testing.T) {
			rules, err := policy.NewRules(t.TempDir(), t.TempDir(), map[string]policy.ToolSpec{"Edit": {}, "Write": {}}, policy.Source{Name: "test", Allow: []string{"Write"}})
			require.NoError(t, err)
			h, sb := editHarness(t, Config{Rules: rules})
			approvals, ended := subscribeTo[event.ApprovalRequested](h.c), subscribeTo[event.ApprovalEnded](h.c)
			h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(editCall("call-1", "a.txt"), writeCall("call-2", "b.txt")), pigruntimetest.Text("unreached")})

			w, err := h.c.Submit(t.Context(), "edit a.txt")
			require.NoError(t, err)
			req := receiveEvent(t, approvals)
			queued, err := h.c.Submit(t.Context(), "queued meanwhile")
			require.NoError(t, err)
			assert.Nil(t, queued)
			assert.True(t, h.c.Answer(req.Token, policy.Decision{Answer: policy.Reject, Reason: reason}))
			wait(t, w.Done())

			assert.Equal(t, event.ApprovalEnded{Token: req.Token}, receiveEvent(t, ended))
			assert.Equal(t, event.Outcome{Status: event.Interrupted}, w.Outcome())
			ends := ofType[event.TurnEnd](h.rec.all())
			require.Len(t, ends, 1)
			assert.Equal(t, []event.Input{{Text: "queued meanwhile"}}, ends[0].Unsent)
			assert.Equal(t, "alpha\n", fileText(t, sb, "a.txt"))
			assert.Equal(t, "alpha\n", fileText(t, sb, "b.txt"), "the following rule-allowed tool must not run")
			assert.Equal(t, 1, h.faux.CallCount(), "the rejection stops the turn before another model request")

			requests := make(chan pigruntimetest.Request, 1)
			h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "understood")})
			h.turn(t, "carry on")
			results := receive(t, requests).ToolResults
			require.Len(t, results, 1)
			assert.Contains(t, results[0], policy.ErrRejected.Error())
			assert.Contains(t, results[0], "stop and wait")
		})
	}
}

func TestApprovalNoWithReasonContinuesTurn(t *testing.T) {
	h, sb := editHarness(t, Config{})
	approvals := subscribeTo[event.ApprovalRequested](h.c)
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{
		pigruntimetest.ToolCalls(editCall("call-1", "a.txt"), editCall("call-2", "b.txt")),
		pigruntimetest.Capture(t, requests, "done"),
	})

	w, err := h.c.Submit(t.Context(), "edit both")
	require.NoError(t, err)
	first := receiveEvent(t, approvals)
	assert.Equal(t, "call-1", first.CallID)
	require.True(t, h.c.Answer(first.Token, policy.Decision{Answer: policy.Reject, Reason: "  leave a.txt alone\n"}))
	second := receiveEvent(t, approvals)
	assert.Equal(t, "call-2", second.CallID)
	require.True(t, h.c.Answer(second.Token, policy.Decision{Answer: policy.Allow}))
	wait(t, w.Done())

	assert.Equal(t, event.Outcome{Status: event.Completed}, w.Outcome())
	assert.Equal(t, "alpha\n", fileText(t, sb, "a.txt"))
	assert.Equal(t, "beta\n", fileText(t, sb, "b.txt"))
	results := receive(t, requests).ToolResults
	require.Len(t, results, 2)
	assert.Contains(t, results[0], policy.ErrRejected.Error())
	assert.Contains(t, results[0], "with this feedback:\nleave a.txt alone")
	assert.NotContains(t, results[0], "alone\n")
	assert.NotContains(t, results[0], "stop and wait")
}

func TestApprovalNoWithReasonKeepsQueuedInput(t *testing.T) {
	h, _ := editHarness(t, Config{})
	approvals := subscribeTo[event.ApprovalRequested](h.c)
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{
		pigruntimetest.ToolCalls(editCall("call-1", "a.txt")),
		pigruntimetest.Capture(t, requests, "done"),
	})

	w, err := h.c.Submit(t.Context(), "edit a.txt")
	require.NoError(t, err)
	req := receiveEvent(t, approvals)
	_, err = h.c.Submit(t.Context(), "queued meanwhile")
	require.NoError(t, err)
	require.True(t, h.c.Answer(req.Token, policy.Decision{Answer: policy.Reject, Reason: "use b.txt"}))
	wait(t, w.Done())

	assert.Equal(t, event.Outcome{Status: event.Completed}, w.Outcome())
	for _, end := range ofType[event.TurnEnd](h.rec.all()) {
		assert.Empty(t, end.Unsent)
	}
	assert.Contains(t, receive(t, requests).Raw, "queued meanwhile")
}

// answers are the distinct Decisions a human can give.
var answers = []policy.Decision{
	{Answer: policy.Allow},
	{Answer: policy.AllowEdits},
	{Answer: policy.Reject},
	{Answer: policy.Reject, Reason: "use b.txt"},
}

func TestApprovalClaimedAnswerSurvivesCancellation(t *testing.T) {
	for _, want := range []policy.Decision{{Answer: policy.Reject}, {Answer: policy.Reject, Reason: "use b.txt"}} {
		t.Run(fmt.Sprintf("reason=%q", want.Reason), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				rec := &recorder{}
				c := &Controller{turn: &Work{sendCtx: ctx, cancel: cancel}, emit: rec.add}
				approve := c.approver(newDisplay(nil))
				done := make(chan struct{})
				var answer policy.Decision
				var err error
				go func() {
					answer, err = approve(ctx, policy.Request{CallID: "call-1", Tool: "Edit"})
					close(done)
				}()

				synctest.Wait()
				requests := ofType[event.ApprovalRequested](rec.all())
				require.Len(t, requests, 1)
				c.queue.Lock()
				c.mu.Lock()
				p := c.approval
				require.NotNil(t, p)
				p.claimed = true
				c.approval = nil
				c.mu.Unlock()
				c.queue.Unlock()
				// Hold delivery after Answer claims the slot, forcing cancellation to
				// reach the approver before the buffered answer is available.
				defer close(p.answer)
				p.work.cancel()
				synctest.Wait()
				select {
				case <-done:
					assert.Fail(t, "approver returned before the claimed answer was delivered")
				default:
				}

				p.answer <- want
				synctest.Wait()
				<-done
				require.NoError(t, err)
				require.Equal(t, want, answer)
				assert.Equal(t, []event.ApprovalEnded{{Token: requests[0].Token}}, ofType[event.ApprovalEnded](rec.all()))
			})
		})
	}
}

func TestApprovalCancellationBeforeAnswer(t *testing.T) {
	for _, interrupt := range []bool{false, true} {
		for _, a := range answers {
			t.Run(fmt.Sprintf("interrupt=%t/answer=%d/reason=%q", interrupt, a.Answer, a.Reason), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					rec := &recorder{}
					c := &Controller{turn: &Work{sendCtx: ctx, cancel: cancel}, emit: rec.add}
					done := make(chan struct{})
					go func() {
						answer, err := c.approver(newDisplay(nil))(ctx, policy.Request{Tool: "Edit"})
						assert.Equal(t, policy.Decision{}, answer)
						assert.ErrorIs(t, err, context.Canceled)
						close(done)
					}()
					synctest.Wait()
					requests := ofType[event.ApprovalRequested](rec.all())
					require.Len(t, requests, 1)
					if interrupt {
						require.True(t, c.Interrupt())
					} else {
						cancel()
					}
					// Do not yield to the approver's cancellation cleanup before answering.
					assert.False(t, c.Answer(requests[0].Token, a))
					synctest.Wait()
					<-done
					assert.Equal(t, []event.ApprovalEnded{{Token: requests[0].Token}}, ofType[event.ApprovalEnded](rec.all()))
				})
			})
		}
	}
}

func TestApprovalRegistrationAfterCancellation(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		t.Run(fmt.Sprintf("stopped=%t", stopped), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			rec := &recorder{}
			c := &Controller{turn: &Work{sendCtx: ctx, cancel: func() {}}, emit: rec.add}
			if stopped {
				require.True(t, c.Interrupt())
			} else {
				cancel()
			}
			answer, err := c.approver(newDisplay(nil))(ctx, policy.Request{Tool: "Edit"})
			assert.Equal(t, policy.Decision{}, answer)
			assert.ErrorIs(t, err, context.Canceled)
			assert.Empty(t, rec.all())
			assert.Nil(t, c.approval)
		})
	}
}

func TestApprovalAnswerBeforeCancellation(t *testing.T) {
	for _, a := range answers {
		t.Run(fmt.Sprintf("answer=%d/reason=%q", a.Answer, a.Reason), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				rec := &recorder{}
				c := &Controller{turn: &Work{sendCtx: ctx, cancel: cancel}, emit: rec.add}
				done := make(chan struct{})
				go func() {
					answer, err := c.approver(newDisplay(nil))(ctx, policy.Request{Tool: "Edit"})
					assert.Equal(t, a, answer)
					assert.NoError(t, err)
					close(done)
				}()
				synctest.Wait()
				requests := ofType[event.ApprovalRequested](rec.all())
				require.Len(t, requests, 1)
				require.True(t, c.Answer(requests[0].Token, a))
				cancel()
				synctest.Wait()
				<-done
			})
		})
	}
}

func TestApprovalSendCancellationBeforeForwarding(t *testing.T) {
	for _, sendNow := range []bool{false, true} {
		for _, registerLate := range []bool{false, true} {
			t.Run(fmt.Sprintf("sendNow=%t/registerLate=%t", sendNow, registerLate), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					parentCtx, cancelParent := context.WithCancel(t.Context())
					defer cancelParent()
					sendCtx, cancelSend := context.WithCancel(parentCtx)
					defer cancelSend()
					approvalCtx, cancelApproval := context.WithCancel(t.Context())
					defer cancelApproval()
					rec := &recorder{}
					w := &Work{sendCtx: sendCtx, cancel: cancelSend}
					c := &Controller{sess: &pigruntime.Session{}, turn: w, emit: rec.add}
					approve := c.approver(newDisplay(nil))
					cancel := func() {
						if sendNow {
							require.NoError(t, c.SendNow(t.Context(), "continue"))
							assert.False(t, w.stopped)
							assert.Equal(t, w, c.turn)
						} else {
							cancelParent()
						}
						require.ErrorIs(t, sendCtx.Err(), context.Canceled)
						require.NoError(t, approvalCtx.Err(), "PiG has not forwarded cancellation")
					}
					if registerLate {
						cancel()
						answer, err := approve(approvalCtx, policy.Request{Tool: "Edit"})
						assert.Equal(t, policy.Decision{}, answer)
						assert.ErrorIs(t, err, context.Canceled)
						assert.Empty(t, ofType[event.ApprovalRequested](rec.all()))
						assert.Nil(t, c.approval)
						return
					}
					done := make(chan struct{})
					go func() {
						answer, err := approve(approvalCtx, policy.Request{Tool: "Edit"})
						assert.Equal(t, policy.Decision{}, answer)
						assert.ErrorIs(t, err, context.Canceled)
						close(done)
					}()
					synctest.Wait()
					requests := ofType[event.ApprovalRequested](rec.all())
					require.Len(t, requests, 1)
					cancel()
					for _, a := range answers {
						assert.False(t, c.Answer(requests[0].Token, a))
					}
					cancelApproval()
					synctest.Wait()
					<-done
				})
			})
		}
	}
}

func TestApprovalSendNowStillAsksOnFollowingSend(t *testing.T) {
	h, sb := editHarness(t, Config{})
	approvals := subscribeTo[event.ApprovalRequested](h.c)
	h.faux.SetResponses([]ai.FauxResponseStep{
		pigruntimetest.ToolCalls(editCall("call-1", "a.txt")),
		pigruntimetest.ToolCalls(editCall("call-2", "b.txt")),
		pigruntimetest.Text("done"),
	})
	w, err := h.c.Submit(t.Context(), "edit a.txt")
	require.NoError(t, err)
	first := receiveEvent(t, approvals)
	require.NoError(t, h.c.SendNow(t.Context(), "edit b.txt instead"))
	assert.False(t, h.c.Answer(first.Token, policy.Decision{Answer: policy.AllowEdits}))
	second := receiveEvent(t, approvals)
	assert.Equal(t, "call-2", second.CallID)
	assert.Greater(t, second.Token, first.Token)
	require.True(t, h.c.Answer(second.Token, policy.Decision{Answer: policy.Allow}))
	wait(t, w.Done())
	assert.Equal(t, event.Outcome{Status: event.Completed}, w.Outcome())
	assert.Equal(t, "alpha\n", fileText(t, sb, "a.txt"))
	assert.Equal(t, "beta\n", fileText(t, sb, "b.txt"))
	starts := ofType[event.TurnStart](h.rec.all())
	require.Len(t, starts, 2)
	assert.Equal(t, w.ID(), starts[0].WorkID)
	assert.Equal(t, w.ID(), starts[1].WorkID)
}

func TestApprovalCancelledAllowEditsDoesNotSkipNextTurn(t *testing.T) {
	h, sb := editHarness(t, Config{})
	approvals := subscribeTo[event.ApprovalRequested](h.c)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(editCall("call-1", "a.txt"))})
	w, err := h.c.Submit(t.Context(), "edit a.txt")
	require.NoError(t, err)
	first := receiveEvent(t, approvals)
	require.True(t, h.c.Interrupt())
	assert.False(t, h.c.Answer(first.Token, policy.Decision{Answer: policy.AllowEdits}))
	wait(t, w.Done())
	assert.Equal(t, "alpha\n", fileText(t, sb, "a.txt"))

	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(editCall("call-2", "b.txt")), pigruntimetest.Text("done")})
	w, err = h.c.Submit(t.Context(), "edit b.txt")
	require.NoError(t, err)
	second := receiveEvent(t, approvals)
	assert.Equal(t, "call-2", second.CallID)
	require.True(t, h.c.Answer(second.Token, policy.Decision{Answer: policy.Allow}))
	wait(t, w.Done())
	assert.Equal(t, "beta\n", fileText(t, sb, "b.txt"))
}

func TestApprovalCancelledByInterruptEnds(t *testing.T) {
	h, sb := editHarness(t, Config{})
	approvals, ended := subscribeTo[event.ApprovalRequested](h.c), subscribeTo[event.ApprovalEnded](h.c)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(editCall("call-1", "a.txt")), pigruntimetest.Text("unreached")})

	w, err := h.c.Submit(t.Context(), "edit a.txt")
	require.NoError(t, err)
	req := receiveEvent(t, approvals)
	assert.True(t, h.c.Interrupt())

	assert.Equal(t, event.ApprovalEnded{Token: req.Token}, receiveEvent(t, ended))
	wait(t, w.Done())
	assert.Equal(t, event.Outcome{Status: event.Interrupted}, w.Outcome())
	assert.False(t, h.c.Answer(req.Token, policy.Decision{Answer: policy.Allow}), "the cancelled approval is gone")
	assert.Equal(t, "alpha\n", fileText(t, sb, "a.txt"))
}

func TestApprovalCancelledBySessionSwitchEnds(t *testing.T) {
	h, sb := editHarness(t, Config{})
	approvals := subscribeTo[event.ApprovalRequested](h.c)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(editCall("call-1", "a.txt")), pigruntimetest.Text("unreached")})
	oldID := h.c.Status().SessionID

	w, err := h.c.Submit(t.Context(), "edit a.txt")
	require.NoError(t, err)
	req := receiveEvent(t, approvals)
	require.NoError(t, h.c.NewSession(t.Context()))

	wait(t, w.Done())
	evs := h.rec.all()
	endedAt := slices.Index(evs, event.Event(event.ApprovalEnded{Token: req.Token}))
	changedAt := slices.IndexFunc(evs, func(ev event.Event) bool {
		changed, ok := ev.(event.SessionChanged)
		return ok && changed.SessionID != oldID
	})
	require.GreaterOrEqual(t, endedAt, 0)
	assert.Less(t, endedAt, changedAt)
	assert.False(t, h.c.Answer(req.Token, policy.Decision{Answer: policy.Allow}))
	assert.Equal(t, "alpha\n", fileText(t, sb, "a.txt"))
}

func TestApprovalStaleAnswerIsIgnored(t *testing.T) {
	h, sb := editHarness(t, Config{})
	approvals := subscribeTo[event.ApprovalRequested](h.c)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(editCall("call-1", "a.txt")), pigruntimetest.Text("done")})
	assert.False(t, h.c.Answer(0, policy.Decision{Answer: policy.Allow}), "nothing is pending yet")

	w, err := h.c.Submit(t.Context(), "edit a.txt")
	require.NoError(t, err)
	req := receiveEvent(t, approvals)
	assert.False(t, h.c.Answer(req.Token+1, policy.Decision{Answer: policy.Reject}))
	assert.True(t, h.c.Answer(req.Token, policy.Decision{Answer: policy.Allow}))
	assert.False(t, h.c.Answer(req.Token, policy.Decision{Answer: policy.Reject}))
	wait(t, w.Done())

	assert.Equal(t, event.Outcome{Status: event.Completed}, w.Outcome())
	assert.Equal(t, "beta\n", fileText(t, sb, "a.txt"))
}

func TestApprovalAllowsEachEditInBatch(t *testing.T) {
	h, sb := editHarness(t, Config{})
	approvals, ended := subscribeTo[event.ApprovalRequested](h.c), subscribeTo[event.ApprovalEnded](h.c)
	h.faux.SetResponses([]ai.FauxResponseStep{
		pigruntimetest.ToolCalls(editCall("call-1", "a.txt"), editCall("call-2", "b.txt")),
		pigruntimetest.Text("done"),
	})

	w, err := h.c.Submit(t.Context(), "edit both")
	require.NoError(t, err)
	first := receiveEvent(t, approvals)
	assert.Equal(t, "call-1", first.CallID)
	require.True(t, h.c.Answer(first.Token, policy.Decision{Answer: policy.Allow}))
	assert.Equal(t, event.ApprovalEnded{Token: first.Token}, receiveEvent(t, ended))
	second := receiveEvent(t, approvals)
	assert.Equal(t, "call-2", second.CallID)
	assert.Greater(t, second.Token, first.Token)
	assert.Equal(t, "alpha\n", fileText(t, sb, "b.txt"))
	require.True(t, h.c.Answer(second.Token, policy.Decision{Answer: policy.Allow}))
	assert.Equal(t, event.ApprovalEnded{Token: second.Token}, receiveEvent(t, ended))
	wait(t, w.Done())

	assert.Equal(t, event.Outcome{Status: event.Completed}, w.Outcome())
	assert.Equal(t, "beta\n", fileText(t, sb, "a.txt"))
	assert.Equal(t, "beta\n", fileText(t, sb, "b.txt"))
	approvalEvents := slices.DeleteFunc(h.rec.all(), func(ev event.Event) bool {
		switch ev.(type) {
		case event.ApprovalRequested, event.ApprovalEnded:
			return false
		}
		return true
	})
	assert.Equal(t, []event.Event{first, event.ApprovalEnded{Token: first.Token}, second, event.ApprovalEnded{Token: second.Token}}, approvalEvents)
}

func TestApprovalReusedCallIDCannotReuseAnswer(t *testing.T) {
	for _, transition := range []string{"model message", "new session", "resume"} {
		t.Run(transition, func(t *testing.T) {
			h, sb := editHarness(t, Config{})
			approvals := subscribeTo[event.ApprovalRequested](h.c)
			id := h.c.Status().SessionID
			steps := []ai.FauxResponseStep{pigruntimetest.ToolCalls(editCall("reused", "a.txt"))}
			if transition != "model message" {
				steps = append(steps, pigruntimetest.Text("first done"))
			}
			steps = append(steps, pigruntimetest.ToolCalls(editCall("reused", "b.txt")), pigruntimetest.Text("done"))
			h.faux.SetResponses(steps)

			w, err := h.c.Submit(t.Context(), "edit a.txt")
			require.NoError(t, err)
			first := receiveEvent(t, approvals)
			require.True(t, h.c.Answer(first.Token, policy.Decision{Answer: policy.Allow}))
			if transition != "model message" {
				wait(t, w.Done())
				assert.Equal(t, event.Outcome{Status: event.Completed}, w.Outcome())
				require.NoError(t, h.c.NewSession(t.Context()))
				if transition == "resume" {
					require.NoError(t, h.c.Resume(t.Context(), id))
				}
				assert.Equal(t, policy.Plan, h.c.Status().Mode)
				h.c.EnterExecute()
				w, err = h.c.Submit(t.Context(), "edit b.txt")
				require.NoError(t, err)
			}
			second := receiveEvent(t, approvals)
			assert.Equal(t, first.CallID, second.CallID)
			assert.Greater(t, second.Token, first.Token)
			assert.Equal(t, "beta\n", fileText(t, sb, "a.txt"))
			assert.False(t, h.c.Answer(first.Token, policy.Decision{Answer: policy.Allow}))
			assert.False(t, h.c.Answer(first.Token, policy.Decision{Answer: policy.AllowEdits}))
			assert.False(t, h.c.Answer(first.Token, policy.Decision{Answer: policy.Reject}))
			assert.False(t, h.c.Answer(second.Token+1, policy.Decision{Answer: policy.Allow}))
			assert.Equal(t, "alpha\n", fileText(t, sb, "b.txt"))
			require.True(t, h.c.Answer(second.Token, policy.Decision{Answer: policy.Allow}))
			wait(t, w.Done())
			assert.Equal(t, event.Outcome{Status: event.Completed}, w.Outcome())
			assert.Equal(t, "beta\n", fileText(t, sb, "b.txt"))
		})
	}
}

func TestApprovalAllowEditsResetsAcrossSessions(t *testing.T) {
	for _, transition := range []string{"new session", "resume"} {
		t.Run(transition, func(t *testing.T) {
			h, sb := editHarness(t, Config{})
			approvals := subscribeTo[event.ApprovalRequested](h.c)
			id := h.c.Status().SessionID
			h.faux.SetResponses([]ai.FauxResponseStep{
				pigruntimetest.ToolCalls(editCall("call-1", "a.txt")),
				pigruntimetest.Text("granted"),
				pigruntimetest.ToolCalls(editCall("call-2", "b.txt")),
				pigruntimetest.Text("done"),
			})
			w, err := h.c.Submit(t.Context(), "edit a.txt")
			require.NoError(t, err)
			first := receiveEvent(t, approvals)
			require.True(t, h.c.Answer(first.Token, policy.Decision{Answer: policy.AllowEdits}))
			wait(t, w.Done())
			assert.Equal(t, event.Outcome{Status: event.Completed}, w.Outcome())
			assert.Equal(t, "beta\n", fileText(t, sb, "a.txt"))

			if transition == "resume" {
				require.NoError(t, h.c.Resume(t.Context(), id))
			} else {
				require.NoError(t, h.c.NewSession(t.Context()))
			}
			assert.Equal(t, policy.Plan, h.c.Status().Mode)
			h.c.EnterExecute()
			w, err = h.c.Submit(t.Context(), "edit b.txt")
			require.NoError(t, err)
			second := receiveEvent(t, approvals)
			assert.Greater(t, second.Token, first.Token)
			assert.Equal(t, "call-2", second.CallID)
			assert.Equal(t, "alpha\n", fileText(t, sb, "b.txt"))
			require.True(t, h.c.Answer(second.Token, policy.Decision{Answer: policy.Allow}))
			wait(t, w.Done())
			assert.Equal(t, event.Outcome{Status: event.Completed}, w.Outcome())
			assert.Equal(t, "beta\n", fileText(t, sb, "b.txt"))
			assert.Len(t, ofType[event.ApprovalRequested](h.rec.all()), 2)
		})
	}
}

func TestUnattendedDeniesAskWithHint(t *testing.T) {
	h, sb := editHarness(t, Config{Unattended: true})
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(editCall("call-1", "a.txt")), pigruntimetest.Capture(t, requests, "done")})

	h.turn(t, "edit a.txt")

	results := receive(t, requests).ToolResults
	require.Len(t, results, 1)
	assert.Contains(t, results[0], "needs approval")
	assert.Contains(t, results[0], "--allowed-tools")
	assert.Contains(t, results[0], policy.ErrDenied.Error())
	evs := h.rec.all()
	assert.Empty(t, ofType[event.ApprovalRequested](evs))
	assert.Empty(t, ofType[event.ApprovalEnded](evs))
	assert.Equal(t, "alpha\n", fileText(t, sb, "a.txt"))
}
