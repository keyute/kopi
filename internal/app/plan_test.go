package app

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/instruction"
	"github.com/keyute/kopi/internal/pigruntime"
	"github.com/keyute/kopi/internal/pigruntime/pigruntimetest"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/subagent"
	"github.com/keyute/kopi/internal/tool"
	"github.com/keyute/kopi/internal/trust"
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
	assert.Equal(t, []string{"The user approved the plan. The session is in execute mode. Carry out the plan now."}, receive(t, requests).ToolResults)

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
	assert.Equal(t, []string{"The user approved the plan. The user edited the plan file before answering. The session is in execute mode. Carry out the plan now."}, receive(t, requests).ToolResults)
	assert.False(t, h.c.RefreshPlan(req.Token), "the answered plan is gone")
}

func TestModeLineSteersToThePlanOnlyWhereItCanBeApproved(t *testing.T) {
	assert.Equal(t, "Plan mode is on: do not change the workspace. Research the request; if you end with changes to propose, asked for or not, write them as a plan to /plans/s.md"+
		" (Read, Write and Edit work on that file in both modes) and call ExitPlanMode for the user's approval instead of ending your turn."+
		" With no changes to propose, just answer.", modeLine(policy.Plan, "/plans/s.md", true))
	assert.Equal(t, "The session is in plan mode. Its plan file is /plans/s.md; Read, Write and Edit work on that file in both modes.",
		modeLine(policy.Plan, "/plans/s.md", false), "ExitPlanMode is not offered")
	assert.Equal(t, "The session is in plan mode.", modeLine(policy.Plan, "", true))
	assert.Equal(t, "The session is in execute mode.", modeLine(policy.Execute, "/plans/s.md", true))
}

func TestRecallNamesThePlanFileInExecuteModeOnceItHasContent(t *testing.T) {
	h := modeHarness(t)
	assert.Equal(t, h.planLine(), h.c.recall())

	h.c.EnterExecute()
	assert.Equal(t, "The session is in execute mode.", h.c.recall(), "no plan was written")
	writePlan(t, h, " \n")
	assert.Equal(t, "The session is in execute mode.", h.c.recall(), "the plan is blank")
	writePlan(t, h, "# Plan\n")
	assert.Equal(t, "The session is in execute mode. Its plan file is "+h.c.PlanFile()+".", h.c.recall())

	h, _ = openHarness(t, Config{})
	h.c.EnterExecute()
	assert.Equal(t, "The session is in execute mode.", h.c.recall(), "the session has no plan file")
}

func TestCompactionRecallsTheModeLine(t *testing.T) {
	const execLine = "The session is in execute mode."
	for _, tc := range []struct {
		name string
		// between runs after the first prompt, before the compaction.
		between func(h *harness)
		want    func(h *harness) []string
	}{
		{"plan mode", func(*harness) {}, func(h *harness) []string { return []string{"kept prompt", h.planLine(), "next"} }},
		{"execute mode with a plan", func(h *harness) {
			h.c.EnterExecute()
			writePlan(t, h, "# Plan\n")
		}, func(h *harness) []string {
			return []string{"kept prompt", execLine, execLine + " Its plan file is " + h.c.PlanFile() + ".", "next"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := modeHarness(t)
			// Together the two prompts pass PiG's compaction threshold on the faux
			// model's window; the second alone is more than PiG keeps verbatim, so the
			// first, and the mode line sent with it, is summarised.
			old, kept := strings.Repeat("a", 240_000), strings.Repeat("b", 240_000)
			requests := make(chan pigruntimetest.Request, 1)
			h.faux.SetResponses([]ai.FauxResponseStep{
				pigruntimetest.Text("first"), pigruntimetest.Text("second"), pigruntimetest.Text("the summary"), pigruntimetest.Capture(t, requests, "third"),
			})

			h.turn(t, old)
			tc.between(h)
			h.turn(t, kept)
			h.turn(t, "next")

			texts := receive(t, requests).UserTexts
			require.NotEmpty(t, texts)
			assert.Contains(t, texts[0], "the summary")
			texts = texts[1:]
			for i, text := range texts {
				if text == kept {
					texts[i] = "kept prompt"
				}
			}
			assert.Equal(t, tc.want(h), texts)
		})
	}
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
	assert.Equal(t, []string{"Read", "WebSearch"}, toolNames(a.c.agents[0].tools))
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

func TestAddedDirectoryIsToldOnceAndAgainToANewSessionAndAResumedOne(t *testing.T) {
	d := newDirHarness(t, Config{}, true)
	dir := otherDir(t)
	decide(t, d.c, dir, trust.Trusted)
	text := told(d.sb.Workspace, dir)
	require.NoError(t, d.c.AddDir(dir))

	assert.Equal(t, [][]string{{"one", text}, {"two"}}, d.userTexts(t, "one", "two"))
	savedID := d.c.Status().SessionID

	require.NoError(t, d.c.NewSession(t.Context()))
	d.read = 0
	assert.Equal(t, [][]string{{"three", text}}, d.userTexts(t, "three"), "the new session has not been told")

	sessions := subscribeTo[event.SessionChanged](d.c)
	_, err := d.c.Submit(t.Context(), Main, "/resume "+savedID)
	require.NoError(t, err)
	assert.Equal(t, savedID, receiveEvent(t, sessions).SessionID)
	assertSwitchDone(t, d.harness)
	d.read = 0
	got := d.userTexts(t, "four")
	require.Len(t, got, 1)
	assert.Equal(t, []string{"four", text}, got[0][len(got[0])-2:], "the resumed session is told again")
	assert.ErrorIs(t, d.ws.CanAdd(dir), tool.ErrInWorkspace)
}

func TestModeLineAndDirectoriesAreOfferedUntilRead(t *testing.T) {
	c := &Controller{authority: policy.NewAuthority()}
	c.cfg.Workspace = "/ws"
	c.main = &runner{c: c}
	c.dirs = []addedDir{{dir: tool.Dir{Real: "/other"}}}
	want := modeLine(policy.Plan, "", false) + "\n" + "The workspace also includes /other. Give a path there as an absolute path; relative paths and Bash still start from /ws."

	first := c.main.tell([]pigruntime.Prompt{{Text: "a"}})
	assert.Equal(t, want, first[0].Mode)
	assert.Equal(t, want, c.main.tell([]pigruntime.Prompt{{Text: "b"}})[0].Mode, "handed over and not read is offered again")

	first[0].ModeRead()
	assert.Empty(t, c.main.tell([]pigruntime.Prompt{{Text: "c"}})[0].Mode)

	c.dirs = append(c.dirs, addedDir{dir: tool.Dir{Real: "/another"}})
	assert.Equal(t, "The workspace also includes /another. Give a path there as an absolute path; relative paths and Bash still start from /ws.", c.main.tell([]pigruntime.Prompt{{Text: "d"}})[0].Mode, "only what is new")
}

func TestOneReadRecordsEverythingItsPromptCarried(t *testing.T) {
	c := &Controller{authority: policy.NewAuthority()}
	c.cfg.Workspace = "/ws"
	c.main = &runner{c: c}
	c.main.tell([]pigruntime.Prompt{{Text: "a"}})[0].ModeRead()
	c.dirs = []addedDir{{dir: tool.Dir{Real: "/one"}}}
	c.authority.EnterExecute()
	both := c.main.tell([]pigruntime.Prompt{{Text: "b"}})
	require.Equal(t, modeLine(policy.Execute, "", false)+"\n"+c.dirsText(0, 1, true), both[0].Mode)
	both[0].ModeRead()
	assert.Empty(t, c.main.tell([]pigruntime.Prompt{{Text: "c"}})[0].Mode)
}

func TestReadingOnlyDirectoriesLeavesTheModeLineAlone(t *testing.T) {
	c := &Controller{authority: policy.NewAuthority()}
	c.cfg.Workspace = "/ws"
	c.main = &runner{c: c}
	c.main.tell([]pigruntime.Prompt{{Text: "a"}})[0].ModeRead()
	c.dirs = []addedDir{{dir: tool.Dir{Real: "/one"}}}
	dirsOnly := c.main.tell([]pigruntime.Prompt{{Text: "b"}})
	require.Equal(t, c.dirsText(0, 1, true), dirsOnly[0].Mode)
	dirsOnly[0].ModeRead()
	assert.Equal(t, modeLine(policy.Plan, "", false), c.main.told)

	c.authority.EnterExecute()
	modeOnly := c.main.tell([]pigruntime.Prompt{{Text: "c"}})
	require.Equal(t, modeLine(policy.Execute, "", false), modeOnly[0].Mode)
	assert.Equal(t, modeOnly[0].Mode, c.main.tell([]pigruntime.Prompt{{Text: "d"}})[0].Mode, "not read")
	assert.Equal(t, 1, c.main.dirsTold)
}

func TestDirectoryIsToldOnceWhenTheSendCompactsBeforeItsPromptIsRead(t *testing.T) {
	d := newDirHarness(t, Config{}, true)
	dir := canonicalTemp(t)
	decide(t, d.c, dir, trust.Untrusted)
	old, kept := strings.Repeat("a", 240_000), strings.Repeat("b", 240_000)
	// An aborted turn is not checked for compaction at its end, so the next send's
	// own check compacts, before its prompt is in the session.
	aborted := ai.FauxAssistantMessage(ai.FauxContentString("second"), ai.FauxAssistantMessageOptions{StopReason: string(ai.StopReasonAborted)})
	requests := make(chan pigruntimetest.Request, 1)
	d.faux.SetResponses([]ai.FauxResponseStep{
		pigruntimetest.Text("first"), aborted, pigruntimetest.Text("the summary"), pigruntimetest.Capture(t, requests, "third"),
	})

	d.turn(t, old)
	d.turn(t, kept)
	require.NoError(t, d.c.AddDir(dir))
	d.turn(t, "next")

	text := inJSON(t, "The workspace also includes "+dir+". Give a path there as an absolute path; relative paths and Bash still start from "+d.sb.Workspace+".")
	raw := receive(t, requests).Raw
	assert.Contains(t, raw, "the summary")
	assert.Equal(t, 1, strings.Count(raw, text), "told once, not also recalled")
}

func TestOnlyTheDirectoriesOfThePromptReadAreNotOfferedAgain(t *testing.T) {
	c := &Controller{authority: policy.NewAuthority()}
	c.cfg.Workspace = "/ws"
	c.main = &runner{c: c}
	c.dirs = []addedDir{{dir: tool.Dir{Real: "/one"}}}
	first := c.main.tell([]pigruntime.Prompt{{Text: "a"}})
	c.dirs = append(c.dirs, addedDir{dir: tool.Dir{Real: "/two"}})
	second := c.main.tell([]pigruntime.Prompt{{Text: "b"}})
	require.Contains(t, second[0].Mode, "/one", "the first was not read yet")
	require.Contains(t, second[0].Mode, "/two")

	first[0].ModeRead()
	got := c.main.tell([]pigruntime.Prompt{{Text: "c"}})[0].Mode
	assert.Equal(t, c.dirsText(1, 2, true), got)

	second[0].ModeRead()
	assert.Empty(t, c.main.tell([]pigruntime.Prompt{{Text: "d"}})[0].Mode)
}

func TestReadingNeverLowersTheDirectoriesTold(t *testing.T) {
	c := &Controller{authority: policy.NewAuthority()}
	c.cfg.Workspace = "/ws"
	c.main = &runner{c: c}
	c.dirs = []addedDir{{dir: tool.Dir{Real: "/one"}}}
	first := c.main.tell([]pigruntime.Prompt{{Text: "a"}})
	c.dirs = append(c.dirs, addedDir{dir: tool.Dir{Real: "/two"}})
	second := c.main.tell([]pigruntime.Prompt{{Text: "b"}})

	second[0].ModeRead()
	first[0].ModeRead()

	assert.Equal(t, 2, c.main.dirsTold)
}

func TestReadOfAReplacedSessionTellsNothing(t *testing.T) {
	c := &Controller{authority: policy.NewAuthority()}
	c.cfg.Workspace = "/ws"
	c.main = &runner{c: c, sess: &pigruntime.Session{}}
	c.dirs = []addedDir{{dir: tool.Dir{Real: "/one"}}}
	sent := c.main.tell([]pigruntime.Prompt{{Text: "a"}})

	c.main.sess = &pigruntime.Session{}
	sent[0].ModeRead()

	assert.Empty(t, c.main.told)
	assert.Zero(t, c.main.dirsTold)
}

func TestRecallLeavesOutDirectoriesNotRead(t *testing.T) {
	c := &Controller{authority: policy.NewAuthority()}
	c.cfg.Workspace = "/ws"
	c.main = &runner{c: c}
	c.dirs = []addedDir{{dir: tool.Dir{Real: "/one"}}}
	handed := c.main.tell([]pigruntime.Prompt{{Text: "a"}})
	line := modeLine(policy.Plan, "", false)
	assert.Equal(t, line, c.recall(), "handed over, not read")

	handed[0].ModeRead()
	assert.Equal(t, line+"\n"+c.dirsText(0, 1, true), c.recall())
}

func TestSubagentRunnerIsToldTheDirectoriesButNotTheMode(t *testing.T) {
	c := &Controller{authority: policy.NewAuthority()}
	c.cfg.Workspace = "/ws"
	c.main = &runner{c: c}
	c.dirs = []addedDir{{dir: tool.Dir{Real: "/other"}, files: []instruction.File{{Path: "/other/AGENTS.md", Text: "use tabs"}}}}
	child := &runner{c: c}

	first := child.tell([]pigruntime.Prompt{{Text: "a"}})
	assert.Equal(t, "The workspace also includes /other. Give a path there as an absolute path; relative paths and Bash still start from /ws.\nInstructions from /other/AGENTS.md:\nuse tabs", first[0].Mode)
	first[0].ModeRead()
	assert.Empty(t, child.tell([]pigruntime.Prompt{{Text: "b"}})[0].Mode)
	assert.Empty(t, child.told)
}

func TestRecallCarriesTheAddedDirectories(t *testing.T) {
	d := newDirHarness(t, Config{}, true)
	dir := otherDir(t)
	decide(t, d.c, dir, trust.Trusted)
	require.NoError(t, d.c.AddDir(dir))
	assert.Equal(t, d.planLine(), d.c.recall(), "a directory the session has not been told arrives with the next prompt")

	d.userTexts(t, "hi")
	assert.Equal(t, d.planLine()+"\n"+told(d.sb.Workspace, dir), d.c.recall())
}

func TestCompactionRecallsTheAddedDirectories(t *testing.T) {
	d := newDirHarness(t, Config{}, false)
	d.showModeLine()
	require.NoError(t, d.c.Open(t.Context(), OpenRequest{}))
	dir := otherDir(t)
	decide(t, d.c, dir, trust.Trusted)
	require.NoError(t, d.c.AddDir(dir))
	old, kept := strings.Repeat("a", 240_000), strings.Repeat("b", 240_000)
	requests := make(chan pigruntimetest.Request, 1)
	d.faux.SetResponses([]ai.FauxResponseStep{
		pigruntimetest.Text("first"), pigruntimetest.Text("second"), pigruntimetest.Text("the summary"), pigruntimetest.Capture(t, requests, "third"),
	})

	d.turn(t, old)
	d.turn(t, kept)
	d.turn(t, "next")

	texts := receive(t, requests).UserTexts
	require.NotEmpty(t, texts)
	assert.Contains(t, texts[0], "the summary")
	texts = texts[1:]
	for i, text := range texts {
		if text == kept {
			texts[i] = "kept prompt"
		}
	}
	assert.Equal(t, []string{"kept prompt", d.planLine() + "\n" + told(d.sb.Workspace, dir), "next"}, texts)
}

func TestCompactionDoesNotRecallADirectoryAddedDuringTheTurn(t *testing.T) {
	d := newDirHarness(t, Config{}, true)
	dir := canonicalTemp(t)
	decide(t, d.c, dir, trust.Untrusted)
	old, kept := strings.Repeat("a", 240_000), strings.Repeat("b", 240_000)
	addsDuringTheTurn := ai.FauxFactoryStep(func(ai.TranscriptContext, ai.StreamOptions, *ai.FauxProviderState, *ai.Model) (ai.AssistantMessage, error) {
		assert.NoError(t, d.c.AddDir(dir))
		return ai.FauxAssistantMessage(ai.FauxContentString("second"), ai.FauxAssistantMessageOptions{}), nil
	})
	requests := make(chan pigruntimetest.Request, 1)
	d.faux.SetResponses([]ai.FauxResponseStep{
		pigruntimetest.Text("first"), addsDuringTheTurn, pigruntimetest.Text("the summary"), pigruntimetest.Capture(t, requests, "third"),
	})

	d.turn(t, old)
	d.turn(t, kept)
	d.turn(t, "next")

	text := inJSON(t, "The workspace also includes "+dir+". Give a path there as an absolute path; relative paths and Bash still start from "+d.sb.Workspace+".")
	raw := receive(t, requests).Raw
	assert.Contains(t, raw, "the summary")
	assert.Equal(t, 1, strings.Count(raw, text), "told with the next prompt, not also recalled")
}
