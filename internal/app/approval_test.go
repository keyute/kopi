package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/classifier"
	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/pigruntime"
	"github.com/keyute/kopi/internal/pigruntime/pigruntimetest"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/tool"
	"github.com/keyute/kopi/internal/trust"
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

	w, err := h.c.Submit(t.Context(), Main, "edit a.txt")
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

func TestApprovalReadRefusesRemovedDirectory(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	cfg, _, _ := dirConfig(t, sb, Config{})
	rules, err := policy.NewRules(sb.Workspace, sb.Home, tool.Specs(cfg.Tools), policy.Source{Name: "test", Ask: []string{"Read"}})
	require.NoError(t, err)
	cfg.Rules = rules
	h := newHarness(t, sb, cfg)
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	dir := canonicalTemp(t)
	path := filepath.Join(dir, "file.txt")
	const contents = "synthetic private contents"
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	decide(t, h.c, dir, trust.Untrusted)
	require.NoError(t, h.c.AddDir(dir))

	approvals := subscribeTo[event.ApprovalRequested](h.c)
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{
		pigruntimetest.ToolCalls(pigruntimetest.Call{Name: "Read", ID: "call-1", Args: map[string]any{"file_path": path}}),
		pigruntimetest.Capture(t, requests, "done"),
	})
	w, err := h.c.Submit(t.Context(), Main, "read the file")
	require.NoError(t, err)
	req := receiveEvent(t, approvals)
	assert.Equal(t, "call-1", req.CallID)
	require.NoError(t, h.c.RemoveDir(dir))
	require.True(t, h.c.Answer(req.Token, policy.Decision{Answer: policy.Allow}))
	wait(t, w.Done())

	assert.Equal(t, event.Outcome{Status: event.Completed}, w.Outcome())
	results := receive(t, requests).ToolResults
	require.Len(t, results, 1)
	assert.Contains(t, results[0], tool.ErrOutsideWorkspace.Error())
	assert.NotContains(t, results[0], contents)
}

func TestApprovalAllowEditsSkipsLaterPrompts(t *testing.T) {
	h, sb := editHarness(t, Config{})
	approvals := subscribeTo[event.ApprovalRequested](h.c)
	h.faux.SetResponses([]ai.FauxResponseStep{
		pigruntimetest.ToolCalls(editCall("call-1", "a.txt")),
		pigruntimetest.ToolCalls(writeCall("call-2", "b.txt")),
		pigruntimetest.Text("done"),
	})

	w, err := h.c.Submit(t.Context(), Main, "edit both")
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

			w, err := h.c.Submit(t.Context(), Main, "edit a.txt")
			require.NoError(t, err)
			req := receiveEvent(t, approvals)
			queued, err := h.c.Submit(t.Context(), Main, "queued meanwhile")
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
			require.Len(t, results, 2)
			assert.Contains(t, results[0], policy.ErrRejected.Error())
			assert.Contains(t, results[0], "stop and wait")
			assert.Contains(t, results[1], "Operation aborted")
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

	w, err := h.c.Submit(t.Context(), Main, "edit both")
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

	w, err := h.c.Submit(t.Context(), Main, "edit a.txt")
	require.NoError(t, err)
	req := receiveEvent(t, approvals)
	_, err = h.c.Submit(t.Context(), Main, "queued meanwhile")
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
				c := &Controller{}
				c.main = &runner{c: c, work: &Work{sendCtx: ctx, cancel: cancel}, emit: rec.add, bound: rec.add}
				approve := c.approver(newDisplay(nil), c.main)
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
				require.Len(t, c.approvals, 1)
				p := c.approvals[0]
				p.claimed = true
				c.approvals = nil
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
					c := &Controller{}
					c.main = &runner{c: c, work: &Work{sendCtx: ctx, cancel: cancel}, emit: rec.add, bound: rec.add}
					done := make(chan struct{})
					go func() {
						answer, err := c.approver(newDisplay(nil), c.main)(ctx, policy.Request{Tool: "Edit"})
						assert.Equal(t, policy.Decision{}, answer)
						assert.ErrorIs(t, err, context.Canceled)
						close(done)
					}()
					synctest.Wait()
					requests := ofType[event.ApprovalRequested](rec.all())
					require.Len(t, requests, 1)
					if interrupt {
						require.True(t, c.Interrupt(Main))
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
			c := &Controller{}
			c.main = &runner{c: c, work: &Work{sendCtx: ctx, cancel: func() {}}, emit: rec.add, bound: rec.add}
			if stopped {
				require.True(t, c.Interrupt(Main))
			} else {
				cancel()
			}
			answer, err := c.approver(newDisplay(nil), c.main)(ctx, policy.Request{Tool: "Edit"})
			assert.Equal(t, policy.Decision{}, answer)
			assert.ErrorIs(t, err, context.Canceled)
			assert.Empty(t, rec.all())
			assert.Empty(t, c.approvals)
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
				c := &Controller{}
				c.main = &runner{c: c, work: &Work{sendCtx: ctx, cancel: cancel}, emit: rec.add, bound: rec.add}
				done := make(chan struct{})
				go func() {
					answer, err := c.approver(newDisplay(nil), c.main)(ctx, policy.Request{Tool: "Edit"})
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
					c := &Controller{}
					c.main = &runner{c: c, sess: &pigruntime.Session{}, work: w, emit: rec.add, bound: rec.add}
					approve := c.approver(newDisplay(nil), c.main)
					cancel := func() {
						if sendNow {
							require.NoError(t, c.SendNow(t.Context(), Main, "continue"))
							assert.False(t, w.stopped)
							assert.Equal(t, w, c.main.work)
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
						assert.Empty(t, c.approvals)
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
	w, err := h.c.Submit(t.Context(), Main, "edit a.txt")
	require.NoError(t, err)
	first := receiveEvent(t, approvals)
	require.NoError(t, h.c.SendNow(t.Context(), Main, "edit b.txt instead"))
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
	w, err := h.c.Submit(t.Context(), Main, "edit a.txt")
	require.NoError(t, err)
	first := receiveEvent(t, approvals)
	require.True(t, h.c.Interrupt(Main))
	assert.False(t, h.c.Answer(first.Token, policy.Decision{Answer: policy.AllowEdits}))
	wait(t, w.Done())
	assert.Equal(t, "alpha\n", fileText(t, sb, "a.txt"))

	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(editCall("call-2", "b.txt")), pigruntimetest.Text("done")})
	w, err = h.c.Submit(t.Context(), Main, "edit b.txt")
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

	w, err := h.c.Submit(t.Context(), Main, "edit a.txt")
	require.NoError(t, err)
	req := receiveEvent(t, approvals)
	assert.True(t, h.c.Interrupt(Main))

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

	w, err := h.c.Submit(t.Context(), Main, "edit a.txt")
	require.NoError(t, err)
	req := receiveEvent(t, approvals)
	require.NoError(t, h.c.NewSession(t.Context()))

	wait(t, w.Done())
	evs := h.rec.all()
	ended := event.Event(event.ApprovalEnded{Token: req.Token})
	assert.Len(t, slices.DeleteFunc(slices.Clone(evs), func(ev event.Event) bool { return ev != ended }), 1)
	endedAt := slices.Index(evs, ended)
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

	w, err := h.c.Submit(t.Context(), Main, "edit a.txt")
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

	w, err := h.c.Submit(t.Context(), Main, "edit both")
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

			w, err := h.c.Submit(t.Context(), Main, "edit a.txt")
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
				w, err = h.c.Submit(t.Context(), Main, "edit b.txt")
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
			w, err := h.c.Submit(t.Context(), Main, "edit a.txt")
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
			w, err = h.c.Submit(t.Context(), Main, "edit b.txt")
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

const (
	replyAllow = `{"decision":"allow","reason":"fine"}`
	replyDeny  = `{"decision":"deny","reason":"too risky"}`
	replyAsk   = `{"decision":"ask","reason":"unclear intent"}`
)

// reviewing returns a Classifier whose stages both reply and send every prompt
// they get on prompts.
func reviewing(reply string, prompts chan<- string) *classifier.Classifier {
	answer := func(_ context.Context, _, prompt string) (string, error) {
		prompts <- prompt
		return reply, nil
	}
	return classifier.New(answer, answer, nil)
}

// probeTool is a Read tool that asks by default, counting its runs.
func probeTool(ran *atomic.Int32) tool.Tool {
	t := testTool("Probe", policy.Read, func(context.Context, json.RawMessage) (string, error) {
		ran.Add(1)
		return "probed", nil
	})
	t.Ask = true
	return t
}

func probeCall(id string, n int) pigruntimetest.Call {
	return pigruntimetest.Call{Name: "Probe", ID: id, Args: map[string]any{"n": n}}
}

// reviewInput is the JSON message a review receives.
type reviewInput struct {
	Task    string
	History []struct{ Tool, Args string }
	Action  struct{ Tool string }
}

func parseReview(t *testing.T, prompt string) reviewInput {
	t.Helper()
	var in reviewInput
	require.NoError(t, json.Unmarshal([]byte(prompt), &in))
	return in
}

func receivePrompt(t *testing.T, prompts <-chan string) string {
	t.Helper()
	select {
	case p := <-prompts:
		return p
	case <-time.After(waitLimit):
		t.Fatal("timed out waiting for a classifier prompt")
		return ""
	}
}

func TestClassifierAllowRunsCallWithoutAsking(t *testing.T) {
	var ran atomic.Int32
	prompts := make(chan string, 8)
	h, _ := openHarness(t, Config{Tools: []tool.Tool{probeTool(&ran)}, Classifier: reviewing(replyAllow, prompts)})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(probeCall("call-1", 1)), pigruntimetest.Text("done")})

	h.turn(t, "probe it")

	assert.Equal(t, int32(1), ran.Load())
	assert.Empty(t, ofType[event.ApprovalRequested](h.rec.all()))
	assert.Equal(t, "Probe", parseReview(t, receivePrompt(t, prompts)).Action.Tool)
}

func TestClassifierDenyFailsCallAndTurnContinues(t *testing.T) {
	var ran atomic.Int32
	h, _ := openHarness(t, Config{Tools: []tool.Tool{probeTool(&ran)}, Classifier: reviewing(replyDeny, make(chan string, 8))})
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(probeCall("call-1", 1)), pigruntimetest.Capture(t, requests, "done")})

	w := h.turn(t, "probe it")

	assert.Equal(t, event.Outcome{Status: event.Completed}, w.Outcome())
	assert.Zero(t, ran.Load())
	assert.Empty(t, ofType[event.ApprovalRequested](h.rec.all()))
	results := receive(t, requests).ToolResults
	require.Len(t, results, 1)
	assert.Contains(t, results[0], "too risky")
}

func TestClassifierAskSendsCallToHumanWithReview(t *testing.T) {
	var ran atomic.Int32
	h, _ := openHarness(t, Config{Tools: []tool.Tool{probeTool(&ran)}, Classifier: reviewing(replyAsk, make(chan string, 8))})
	approvals := subscribeTo[event.ApprovalRequested](h.c)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(probeCall("call-1", 1)), pigruntimetest.Text("done")})

	w, err := h.c.Submit(t.Context(), Main, "probe it")
	require.NoError(t, err)
	req := receiveEvent(t, approvals)
	assert.Equal(t, &policy.Review{Verdict: policy.VerdictAsk, Reason: "unclear intent"}, req.Review)
	assert.Empty(t, req.Rule)
	assert.True(t, h.c.Answer(req.Token, policy.Decision{Answer: policy.Allow}))
	wait(t, w.Done())

	assert.Equal(t, int32(1), ran.Load())
}

func TestClassifierThirdDenialGoesToHuman(t *testing.T) {
	var ran atomic.Int32
	h, _ := openHarness(t, Config{Tools: []tool.Tool{probeTool(&ran)}, Classifier: reviewing(replyDeny, make(chan string, 16))})
	approvals := subscribeTo[event.ApprovalRequested](h.c)
	h.faux.SetResponses([]ai.FauxResponseStep{
		pigruntimetest.ToolCalls(probeCall("call-1", 1), probeCall("call-2", 2), probeCall("call-3", 3)),
		pigruntimetest.Text("done"),
	})

	w, err := h.c.Submit(t.Context(), Main, "probe it")
	require.NoError(t, err)
	req := receiveEvent(t, approvals)
	assert.Equal(t, "call-3", req.CallID)
	assert.Equal(t, &policy.Review{Verdict: policy.VerdictDeny, Reason: "too risky"}, req.Review)
	assert.True(t, h.c.Answer(req.Token, policy.Decision{Answer: policy.Reject, Reason: "no"}))
	wait(t, w.Done())

	assert.Zero(t, ran.Load())
	assert.Len(t, ofType[event.ApprovalRequested](h.rec.all()), 1)
}

func TestClassifierSeesPromptAndEarlierCallsButNotTheCallUnderReview(t *testing.T) {
	var ran atomic.Int32
	prompts := make(chan string, 8)
	h, _ := openHarness(t, Config{Tools: []tool.Tool{probeTool(&ran)}, Classifier: reviewing(replyAllow, prompts)})
	h.faux.SetResponses([]ai.FauxResponseStep{
		pigruntimetest.ToolCalls(probeCall("call-1", 1)),
		pigruntimetest.ToolCalls(probeCall("call-2", 2)),
		pigruntimetest.Text("done"),
	})

	h.turn(t, "probe twice")

	first, second := parseReview(t, receivePrompt(t, prompts)), parseReview(t, receivePrompt(t, prompts))
	assert.Empty(t, first.History)
	assert.Equal(t, "probe twice", second.Task)
	require.Len(t, second.History, 1)
	assert.Equal(t, "Probe", second.History[0].Tool)
	assert.JSONEq(t, `{"n":1}`, second.History[0].Args)
}

func TestClassifierTaskIncludesPromptQueuedBehindRunningTurn(t *testing.T) {
	var ran atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	prompts := make(chan string, 8)
	h, _ := openHarness(t, Config{Tools: []tool.Tool{releaseTool(started, release), probeTool(&ran)}, Classifier: reviewing(replyAllow, prompts)})
	h.faux.SetResponses([]ai.FauxResponseStep{
		pigruntimetest.ToolCalls(call("Hold", "call-1")),
		pigruntimetest.ToolCalls(probeCall("call-2", 2)),
		pigruntimetest.Text("done"),
	})

	w, err := h.c.Submit(t.Context(), Main, "first")
	require.NoError(t, err)
	wait(t, started)
	queued, err := h.c.Submit(t.Context(), Main, "queued")
	require.NoError(t, err)
	assert.Nil(t, queued)
	close(release)
	wait(t, w.Done())

	assert.Equal(t, "first\n\nqueued", parseReview(t, receivePrompt(t, prompts)).Task)
}

func TestClassifierRecordFollowsTheSession(t *testing.T) {
	var ran atomic.Int32
	prompts := make(chan string, 8)
	cfg := Config{Tools: []tool.Tool{probeTool(&ran)}, Classifier: reviewing(replyAllow, prompts)}
	h, sb := openHarness(t, cfg)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(probeCall("call-1", 1)), pigruntimetest.Text("done")})
	h.turn(t, "earlier")
	receivePrompt(t, prompts)
	id := h.c.Status().SessionID

	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(probeCall("call-2", 2)), pigruntimetest.Text("done")})
	require.NoError(t, h.c.NewSession(t.Context()))
	h.turn(t, "fresh")
	fresh := parseReview(t, receivePrompt(t, prompts))
	assert.Equal(t, "fresh", fresh.Task)
	assert.Empty(t, fresh.History)
	require.NoError(t, h.c.Close(t.Context()))

	resumed := newHarness(t, sb, cfg)
	require.NoError(t, resumed.c.Open(t.Context(), OpenRequest{ResumeID: id}))
	resumed.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(probeCall("call-3", 3)), pigruntimetest.Text("done")})
	resumed.turn(t, "later")
	in := parseReview(t, receivePrompt(t, prompts))
	assert.Equal(t, "earlier\n\nlater", in.Task)
	require.Len(t, in.History, 1)
	assert.JSONEq(t, `{"n":1}`, in.History[0].Args)
}

func TestClassifierUnattended(t *testing.T) {
	t.Run("allow runs the call", func(t *testing.T) {
		var ran atomic.Int32
		h, _ := openHarness(t, Config{Unattended: true, Tools: []tool.Tool{probeTool(&ran)}, Classifier: reviewing(replyAllow, make(chan string, 8))})
		h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(probeCall("call-1", 1)), pigruntimetest.Text("done")})

		h.turn(t, "probe it")

		assert.Equal(t, int32(1), ran.Load())
	})
	t.Run("ask denies the call", func(t *testing.T) {
		var ran atomic.Int32
		h, _ := openHarness(t, Config{Unattended: true, Tools: []tool.Tool{probeTool(&ran)}, Classifier: reviewing(replyAsk, make(chan string, 8))})
		requests := make(chan pigruntimetest.Request, 1)
		h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(probeCall("call-1", 1)), pigruntimetest.Capture(t, requests, "done")})

		h.turn(t, "probe it")

		assert.Zero(t, ran.Load())
		results := receive(t, requests).ToolResults
		require.Len(t, results, 1)
		assert.Contains(t, results[0], "the approval classifier could not decide")
	})
}

func TestClassifierInterruptWhileReviewingEndsTurnWithoutApproval(t *testing.T) {
	var ran atomic.Int32
	started := make(chan struct{})
	block := func(ctx context.Context, _, _ string) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}
	h, _ := openHarness(t, Config{Tools: []tool.Tool{probeTool(&ran)}, Classifier: classifier.New(block, block, nil)})
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(probeCall("call-1", 1)), pigruntimetest.Text("unreached")})

	w, err := h.c.Submit(t.Context(), Main, "probe it")
	require.NoError(t, err)
	wait(t, started)
	assert.True(t, h.c.Interrupt(Main))
	wait(t, w.Done())

	assert.Equal(t, event.Outcome{Status: event.Interrupted}, w.Outcome())
	assert.Zero(t, ran.Load())
	assert.Empty(t, ofType[event.ApprovalRequested](h.rec.all()))
}

func TestClassifierDoesNotReviewUnruledEdits(t *testing.T) {
	prompts := make(chan string, 8)
	h, sb := editHarness(t, Config{Classifier: reviewing(replyDeny, prompts)})
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(editCall("call-1", "a.txt")), pigruntimetest.Capture(t, requests, "done")})

	h.turn(t, "edit a.txt")

	assert.Equal(t, "beta\n", fileText(t, sb, "a.txt"))
	assert.Empty(t, ofType[event.ApprovalRequested](h.rec.all()))
	assert.Empty(t, prompts)
	assert.NotContains(t, receive(t, requests).SystemPrompt, "classifier")
}

// askingChildren opens a harness whose subagents ask about every Probe call.
func askingChildren(t *testing.T, cfg Config) (*agentHarness, *atomic.Int32) {
	t.Helper()
	ran := new(atomic.Int32)
	cfg.AgentTools = []tool.Tool{probeTool(ran)}
	a := newAgentHarness(t, pigruntimetest.NewSandbox(t), cfg, agentDef("worker"))
	a.rootAcks(4)
	return a, ran
}

func TestChildAskIsAdmittedWithoutARootTurnAndLabelled(t *testing.T) {
	a, ran := askingChildren(t, Config{})
	approvals, ended := subscribeTo[event.ApprovalRequested](a.c), subscribeTo[event.ApprovalEnded](a.c)
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(probeCall("p1", 1)), pigruntimetest.Text("done")})

	id := a.launch(t, "worker", "probe it", "go")
	req := receiveEvent(t, approvals)

	assert.Equal(t, "worker", req.Agent)
	assert.Equal(t, id, req.TaskID)
	assert.Equal(t, "p1", req.CallID)
	assert.False(t, a.c.Status().Busy, "no root turn is running")
	assert.True(t, a.c.Answer(req.Token, policy.Decision{Answer: policy.Allow}))
	a.untilStatus(t, id, tool.TaskCompleted)
	assert.Equal(t, event.ApprovalEnded{Token: req.Token}, receiveEvent(t, ended))
	assert.Equal(t, int32(1), ran.Load())
}

func TestChildAsksQueueFirstInFirstOutAndAreAnsweredByToken(t *testing.T) {
	a, ran := askingChildren(t, Config{})
	approvals := subscribeTo[event.ApprovalRequested](a.c)
	a.child.SetResponses([]ai.FauxResponseStep{
		pigruntimetest.ToolCalls(probeCall("p1", 1)), pigruntimetest.ToolCalls(probeCall("p2", 2)),
		pigruntimetest.Text("done"), pigruntimetest.Text("done"),
	})

	first := a.launch(t, "worker", "one", "go")
	req1 := receiveEvent(t, approvals)
	second := a.launch(t, "worker", "two", "go")
	req2 := receiveEvent(t, approvals)

	assert.Equal(t, first, req1.TaskID)
	assert.Equal(t, second, req2.TaskID)
	a.c.mu.Lock()
	require.Len(t, a.c.approvals, 2)
	assert.Equal(t, []uint64{req1.Token, req2.Token}, []uint64{a.c.approvals[0].token, a.c.approvals[1].token})
	a.c.mu.Unlock()
	assert.False(t, a.c.Answer(req2.Token+1, policy.Decision{Answer: policy.Allow}))
	assert.True(t, a.c.Answer(req2.Token, policy.Decision{Answer: policy.Allow}))
	a.untilStatus(t, second, tool.TaskCompleted)
	assert.Equal(t, tool.TaskRunning, a.info(t, first).Status, "the older ask still waits")
	assert.True(t, a.c.Answer(req1.Token, policy.Decision{Answer: policy.Allow}))
	a.untilStatus(t, first, tool.TaskCompleted)
	assert.Equal(t, int32(2), ran.Load())
}

func TestChildRejectWithoutReasonStopsOnlyThatChild(t *testing.T) {
	a, ran := askingChildren(t, Config{})
	approvals := subscribeTo[event.ApprovalRequested](a.c)
	a.child.SetResponses([]ai.FauxResponseStep{
		pigruntimetest.ToolCalls(probeCall("p1", 1)), pigruntimetest.ToolCalls(probeCall("p2", 2)), pigruntimetest.Text("done"),
	})
	first := a.launch(t, "worker", "one", "go")
	req1 := receiveEvent(t, approvals)
	second := a.launch(t, "worker", "two", "go")
	req2 := receiveEvent(t, approvals)

	assert.True(t, a.c.Answer(req1.Token, policy.Decision{Answer: policy.Reject}))
	a.untilStatus(t, first, tool.TaskStopped)
	a.until(t, func() bool { return a.live() == 1 })

	queued := a.queuedNotices()
	require.Len(t, queued, 1, "counting the child out and its notice are one step")
	assert.True(t, strings.HasPrefix(queued[0].Text, fmt.Sprintf("Agent %s (worker) stopped by the user: one", first)), queued[0].Text)
	assert.Equal(t, string(tool.TaskStopped), queued[0].Shown.(event.TaskEnded).Status)
	assert.True(t, a.idle(), "the stop wakes nobody")
	assert.Equal(t, tool.TaskRunning, a.info(t, second).Status)
	assert.True(t, a.c.Answer(req2.Token, policy.Decision{Answer: policy.Allow}))
	a.untilStatus(t, second, tool.TaskCompleted)
	assert.Equal(t, int32(1), ran.Load())
}

func TestChildRejectWithReasonLetsTheChildGoOn(t *testing.T) {
	a, ran := askingChildren(t, Config{})
	approvals := subscribeTo[event.ApprovalRequested](a.c)
	requests := make(chan pigruntimetest.Request, 1)
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(probeCall("p1", 1)), pigruntimetest.Capture(t, requests, "done")})

	id := a.launch(t, "worker", "one", "go")
	req := receiveEvent(t, approvals)
	assert.True(t, a.c.Answer(req.Token, policy.Decision{Answer: policy.Reject, Reason: "use b"}))
	a.untilStatus(t, id, tool.TaskCompleted)

	results := receive(t, requests).ToolResults
	require.Len(t, results, 1)
	assert.Contains(t, results[0], "use b")
	assert.Zero(t, ran.Load())
}

func TestUnattendedDeniesAChildsAsk(t *testing.T) {
	a, ran := askingChildren(t, Config{Unattended: true})
	requests := make(chan pigruntimetest.Request, 1)
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(probeCall("p1", 1)), pigruntimetest.Capture(t, requests, "done")})

	id := a.launch(t, "worker", "one", "go")
	a.untilStatus(t, id, tool.TaskCompleted)

	results := receive(t, requests).ToolResults
	require.Len(t, results, 1)
	assert.Contains(t, results[0], "cannot ask for it")
	assert.Zero(t, ran.Load())
}

func TestCloseCancelsAChildsPendingAsk(t *testing.T) {
	a, ran := askingChildren(t, Config{})
	approvals, ended := subscribeTo[event.ApprovalRequested](a.c), subscribeTo[event.ApprovalEnded](a.c)
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(probeCall("p1", 1)), pigruntimetest.Text("unreached")})
	a.launch(t, "worker", "one", "go")
	req := receiveEvent(t, approvals)

	require.NoError(t, a.c.Close(t.Context()))

	assert.Equal(t, event.ApprovalEnded{Token: req.Token}, receiveEvent(t, ended))
	assert.False(t, a.c.Answer(req.Token, policy.Decision{Answer: policy.Allow}))
	assert.Empty(t, a.c.approvals)
	assert.Zero(t, ran.Load())
}

func TestSessionSwitchEndsAChildsPendingAskOnceBeforeSessionChanged(t *testing.T) {
	a, ran := askingChildren(t, Config{})
	approvals := subscribeTo[event.ApprovalRequested](a.c)
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(probeCall("p1", 1)), pigruntimetest.Text("unreached")})
	oldID := a.c.Status().SessionID
	a.launch(t, "worker", "one", "go")
	req := receiveEvent(t, approvals)

	require.NoError(t, a.c.NewSession(t.Context()))

	evs := a.rec.all()
	changedAt := slices.IndexFunc(evs, func(ev event.Event) bool {
		changed, ok := ev.(event.SessionChanged)
		return ok && changed.SessionID != oldID
	})
	require.GreaterOrEqual(t, changedAt, 0)
	ended := event.Event(event.ApprovalEnded{Token: req.Token})
	assert.Len(t, slices.DeleteFunc(slices.Clone(evs), func(ev event.Event) bool { return ev != ended }), 1)
	assert.Less(t, slices.Index(evs, ended), changedAt)
	assert.False(t, a.c.Answer(req.Token, policy.Decision{Answer: policy.Allow}))
	assert.Empty(t, a.c.approvals)
	assert.Zero(t, ran.Load())
}

func TestEditsGrantFromAChildCoversTheRoot(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	for _, name := range []string{"a.txt", "b.txt"} {
		require.NoError(t, os.WriteFile(filepath.Join(sb.Workspace, name), []byte("alpha\n"), 0o600))
	}
	ws, err := tool.NewWorkspace(sb.Workspace, nil)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, ws.Close()) })
	edits := []tool.Tool{tool.NewEdit(ws), tool.NewWrite(ws)}
	a := newAgentHarness(t, sb, Config{Tools: edits, AgentTools: edits}, agentDef("worker"))
	a.c.EnterExecute()
	approvals := subscribeTo[event.ApprovalRequested](a.c)
	a.child.SetResponses([]ai.FauxResponseStep{pigruntimetest.ToolCalls(editCall("c1", "a.txt")), pigruntimetest.Text("edited")})
	a.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Text("ack"), pigruntimetest.ToolCalls(editCall("r1", "b.txt")), pigruntimetest.Text("done")})
	ends := turnEnds(a.c)

	a.launch(t, "worker", "edit a", "go")
	req := receiveEvent(t, approvals)
	require.True(t, a.c.Answer(req.Token, policy.Decision{Answer: policy.AllowEdits}))
	receiveEnd(t, ends)
	w, err := a.c.Submit(t.Context(), Main, "edit b.txt")
	require.NoError(t, err)
	wait(t, w.Done())

	assert.Equal(t, "beta\n", fileText(t, sb, "b.txt"))
	assert.Len(t, ofType[event.ApprovalRequested](a.rec.all()), 1, "one grant covers the root and its children")
}
