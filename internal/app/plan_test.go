package app

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/pigruntime/pigruntimetest"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/subagent"
	"github.com/keyute/kopi/internal/tool"
)

func exitPlanCall(id string) pigruntimetest.Call {
	return pigruntimetest.Call{Name: "ExitPlanMode", ID: id, Args: map[string]any{}}
}

func writePlan(t *testing.T, h *harness, text string) {
	t.Helper()
	require.NoError(t, os.WriteFile(h.c.PlanFile(), []byte(text), 0o600))
}

// planCall opens a session with a plan file, writes plan to it and submits a
// prompt whose first response calls ExitPlanMode, then waits for the request.
func planCall(t *testing.T, h *harness, plan string, then ...ai.FauxResponseStep) (*Work, event.PlanRequested) {
	t.Helper()
	asked := subscribeTo[event.PlanRequested](h.c)
	writePlan(t, h, plan)
	h.faux.SetResponses(append([]ai.FauxResponseStep{pigruntimetest.ToolCalls(exitPlanCall("call-1"))}, then...))
	w, err := h.c.Submit(t.Context(), Main, "plan it")
	require.NoError(t, err)
	return w, receiveEvent(t, asked)
}

func TestPlanRequestCarriesPathAndPlan(t *testing.T) {
	h := modeHarness(t)
	_, req := planCall(t, h, "# Plan\n", pigruntimetest.Text("unreached"))

	assert.Equal(t, h.c.PlanFile(), req.Path)
	assert.Equal(t, "# Plan\n", req.Plan)
	assert.NotZero(t, req.Token)
}

func TestPlanApprovalSwitchesToExecuteAndTurnContinues(t *testing.T) {
	h := modeHarness(t)
	requests, next := make(chan pigruntimetest.Request, 1), make(chan pigruntimetest.Request, 1)
	w, req := planCall(t, h, "# Plan\n", pigruntimetest.Capture(t, requests, "done"), pigruntimetest.Capture(t, next, "ok"))
	ended := subscribeTo[event.PlanEnded](h.c)

	assert.True(t, h.c.AnswerPlan(req.Token, tool.PlanAnswer{Approved: true}))
	wait(t, w.Done())

	assert.Equal(t, event.PlanEnded{Token: req.Token}, receiveEvent(t, ended))
	assert.Equal(t, []event.ModeChanged{{Mode: policy.Execute}}, ofType[event.ModeChanged](h.rec.all()))
	assert.Equal(t, event.Outcome{Status: event.Completed}, w.Outcome())
	assert.Equal(t, []string{"The user approved the plan. The session is in execute mode."}, receive(t, requests).ToolResults)

	h.turn(t, "go on")
	assert.Equal(t, []string{"plan it", h.planLine(), "go on"}, receive(t, next).UserTexts, "no mode line follows an approved plan")
}

func TestPlanFeedbackKeepsPlanModeAndReachesModel(t *testing.T) {
	h := modeHarness(t)
	requests := make(chan pigruntimetest.Request, 1)
	w, req := planCall(t, h, "# Plan\n", pigruntimetest.Capture(t, requests, "revising"))

	assert.True(t, h.c.AnswerPlan(req.Token, tool.PlanAnswer{Feedback: "  smaller steps \n"}))
	wait(t, w.Done())

	assert.Empty(t, ofType[event.ModeChanged](h.rec.all()))
	assert.Equal(t, policy.Plan, h.c.Status().Mode)
	assert.Equal(t, event.Outcome{Status: event.Completed}, w.Outcome())
	assert.Equal(t, []string{"The user asked for changes to the plan. The session is in plan mode. Their feedback:\nsmaller steps"}, receive(t, requests).ToolResults)
}

func TestPlanDismissalStopsTurnAndModelSeesIt(t *testing.T) {
	h := modeHarness(t)
	w, req := planCall(t, h, "# Plan\n", pigruntimetest.Text("unreached"))
	ended := subscribeTo[event.PlanEnded](h.c)

	queued, err := h.c.Submit(t.Context(), Main, "queued meanwhile")
	require.NoError(t, err)
	assert.Nil(t, queued)
	assert.True(t, h.c.AnswerPlan(req.Token, tool.PlanAnswer{Feedback: " "}))
	wait(t, w.Done())

	assert.Equal(t, event.PlanEnded{Token: req.Token}, receiveEvent(t, ended))
	assert.Equal(t, event.Outcome{Status: event.Interrupted}, w.Outcome())
	assert.Equal(t, policy.Plan, h.c.Status().Mode)
	turnEnds := ofType[event.TurnEnd](h.rec.all())
	require.Len(t, turnEnds, 1)
	assert.Equal(t, []event.Input{{Text: "queued meanwhile"}}, turnEnds[0].Unsent)
	assert.Equal(t, 1, h.faux.CallCount())

	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{pigruntimetest.Capture(t, requests, "understood")})
	h.turn(t, "carry on")
	assert.Equal(t, []string{"The user dismissed the plan without answering. The session is in plan mode."}, receive(t, requests).ToolResults)
}

func TestPlanCancelledByInterruptLeavesModeAlone(t *testing.T) {
	h := modeHarness(t)
	w, req := planCall(t, h, "# Plan\n", pigruntimetest.Text("unreached"))
	ended := subscribeTo[event.PlanEnded](h.c)

	assert.True(t, h.c.Interrupt(Main))
	wait(t, w.Done())

	assert.Equal(t, event.PlanEnded{Token: req.Token}, receiveEvent(t, ended))
	assert.Equal(t, event.Outcome{Status: event.Interrupted}, w.Outcome())
	assert.Equal(t, policy.Plan, h.c.Status().Mode)
	assert.Empty(t, ofType[event.ModeChanged](h.rec.all()))
	assert.False(t, h.c.AnswerPlan(req.Token, tool.PlanAnswer{Approved: true}), "the cancelled plan is gone")
}

func TestPlanCancelledBySessionSwitchLeavesModeAlone(t *testing.T) {
	h := modeHarness(t)
	w, req := planCall(t, h, "# Plan\n", pigruntimetest.Text("unreached"))

	oldID := h.c.Status().SessionID

	require.NoError(t, h.c.NewSession(t.Context()))
	wait(t, w.Done())

	evs := h.rec.all()
	ended := event.Event(event.PlanEnded{Token: req.Token})
	assert.Len(t, slices.DeleteFunc(slices.Clone(evs), func(ev event.Event) bool { return ev != ended }), 1)
	changedAt := slices.IndexFunc(evs, func(ev event.Event) bool {
		changed, ok := ev.(event.SessionChanged)
		return ok && changed.SessionID != oldID
	})
	require.GreaterOrEqual(t, changedAt, 0)
	assert.Less(t, slices.Index(evs, ended), changedAt)
	assert.False(t, h.c.AnswerPlan(req.Token, tool.PlanAnswer{Approved: true}))
	assert.Equal(t, policy.Plan, h.c.Status().Mode)
}

func TestPlanStaleTokenIsIgnored(t *testing.T) {
	h := modeHarness(t)
	assert.False(t, h.c.AnswerPlan(0, tool.PlanAnswer{Approved: true}), "nothing is pending yet")
	w, req := planCall(t, h, "# Plan\n", pigruntimetest.Text("done"))

	assert.False(t, h.c.AnswerPlan(req.Token+1, tool.PlanAnswer{Approved: true}))
	assert.False(t, h.c.RefreshPlan(req.Token+1))
	assert.Equal(t, policy.Plan, h.c.Status().Mode)
	assert.True(t, h.c.AnswerPlan(req.Token, tool.PlanAnswer{Feedback: "no"}))
	assert.False(t, h.c.AnswerPlan(req.Token, tool.PlanAnswer{Approved: true}))
	wait(t, w.Done())
}

func TestRefreshPlanRepeatsTheRequestWithTheEditedText(t *testing.T) {
	h := modeHarness(t)
	requests := make(chan pigruntimetest.Request, 1)
	w, req := planCall(t, h, "# Plan\n", pigruntimetest.Capture(t, requests, "done"))

	writePlan(t, h, "# Edited\n")
	assert.True(t, h.c.RefreshPlan(req.Token))
	repeated := ofType[event.PlanRequested](h.rec.all())
	require.Len(t, repeated, 2)
	assert.Equal(t, event.PlanRequested{Token: req.Token, Path: req.Path, Plan: "# Edited\n"}, repeated[1])

	assert.True(t, h.c.AnswerPlan(req.Token, tool.PlanAnswer{Approved: true}))
	wait(t, w.Done())
	assert.Equal(t, []string{"The user approved the plan. The user edited the plan file before answering. The session is in execute mode."}, receive(t, requests).ToolResults)
	assert.False(t, h.c.RefreshPlan(req.Token), "the answered plan is gone")
}

func TestExitPlanModeToolIsAttendedOnlyAndNeverForAgents(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	plans := tool.NewPlans(sb.Home + "/plans")

	h, _ := openHarness(t, Config{})
	assert.NotContains(t, toolNames(h.c.tools), "ExitPlanMode", "without a plan file")

	h = newHarness(t, sb, Config{Plans: plans})
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	assert.Contains(t, toolNames(h.c.tools), "ExitPlanMode")

	h, _ = openHarness(t, Config{Plans: plans, Unattended: true})
	assert.NotContains(t, toolNames(h.c.tools), "ExitPlanMode")

	rules, err := policy.NewRules(sb.Workspace, sb.Home, tool.Specs([]tool.Tool{tool.NewExitPlanMode(nil, nil)}), policy.Source{Name: "test", Deny: []string{"ExitPlanMode"}})
	require.NoError(t, err)
	h = newHarness(t, pigruntimetest.NewSandbox(t), Config{Plans: plans, Rules: rules})
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	assert.NotContains(t, toolNames(h.c.tools), "ExitPlanMode")

	a := newAgentHarness(t, pigruntimetest.NewSandbox(t), Config{Plans: plans, AgentTools: []tool.Tool{readTool(t, sb)}}, subagent.Definition{Name: "explorer", Description: "explorer", Model: childProvider + "/faux-1"})
	assert.Contains(t, toolNames(a.c.tools), "ExitPlanMode")
	require.Len(t, a.c.agents, 1)
	assert.Equal(t, []string{"Read"}, toolNames(a.c.agents[0].tools))
}

func TestPlanModeWritesOnlyThePlanFile(t *testing.T) {
	sb := pigruntimetest.NewSandbox(t)
	plans := tool.NewPlans(filepath.Join(sb.Home, "plans"))
	ws, err := tool.NewWorkspace(sb.Workspace, []string{sb.Home})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, ws.Close()) })
	rootWS := ws.WithPlans(plans)
	h := newHarness(t, sb, Config{Plans: plans, Tools: []tool.Tool{tool.NewWrite(rootWS), tool.NewEdit(rootWS)}})
	require.NoError(t, h.c.Open(t.Context(), OpenRequest{}))
	other := filepath.Join(filepath.Dir(h.c.PlanFile()), "other.md")
	requests := make(chan pigruntimetest.Request, 1)
	h.faux.SetResponses([]ai.FauxResponseStep{
		pigruntimetest.ToolCalls(writeCall("w1", h.c.PlanFile()), writeCall("w2", other)),
		pigruntimetest.Capture(t, requests, "done"),
	})

	w, err := h.c.Submit(t.Context(), Main, "write")
	require.NoError(t, err)
	wait(t, w.Done())

	got, err := os.ReadFile(h.c.PlanFile())
	require.NoError(t, err)
	assert.Equal(t, "gamma\n", string(got))
	assert.NoFileExists(t, other)
	assert.Empty(t, ofType[event.ApprovalRequested](h.rec.all()))
	results := receive(t, requests).ToolResults
	require.Len(t, results, 2)
	assert.NotContains(t, results[0], "denied")
	assert.Contains(t, results[1], policy.ErrDenied.Error())
}

func TestRefreshPlanIgnoresAnUnreadablePlanFile(t *testing.T) {
	h := modeHarness(t)
	w, req := planCall(t, h, "# Plan\n", pigruntimetest.Text("done"))

	require.NoError(t, os.Remove(h.c.PlanFile()))
	assert.False(t, h.c.RefreshPlan(req.Token), "the file is missing")
	require.NoError(t, os.Mkdir(h.c.PlanFile(), 0o700))
	assert.False(t, h.c.RefreshPlan(req.Token), "the file is a directory")

	assert.Len(t, ofType[event.PlanRequested](h.rec.all()), 1)
	assert.True(t, h.c.AnswerPlan(req.Token, tool.PlanAnswer{Feedback: "no"}), "the plan is still pending")
	wait(t, w.Done())
}

func TestPlanApprovalSurvivesCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		rec := &recorder{}
		c := &Controller{authority: policy.NewAuthority()}
		c.main = &runner{c: c, work: &Work{sendCtx: ctx, cancel: cancel}, emit: rec.add}
		type result struct {
			answer tool.PlanAnswer
			mode   policy.Mode
			err    error
		}
		done := make(chan result)
		go func() {
			var r result
			r.answer, r.mode, r.err = c.reviewPlan(ctx, "# Plan\n")
			done <- r
		}()

		synctest.Wait()
		asked := ofType[event.PlanRequested](rec.all())
		require.Len(t, asked, 1)
		require.Equal(t, policy.Plan, c.mode())
		assert.True(t, c.AnswerPlan(asked[0].Token, tool.PlanAnswer{Approved: true}))
		cancel()
		synctest.Wait()

		r := <-done
		require.NoError(t, r.err)
		assert.True(t, r.answer.Approved)
		assert.Equal(t, policy.Execute, r.mode)
		assert.Equal(t, policy.Execute, c.mode())
	})
}
