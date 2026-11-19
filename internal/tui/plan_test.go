package tui

import (
	"os/exec"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/config"
	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/tool"
)

var ctrlG = tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl}

const planPath = "/home/k/plans/s1.md"

func planRequest(token uint64, plan string) event.PlanRequested {
	return event.PlanRequested{Token: token, Path: planPath, Plan: plan}
}

func openPlan(t *testing.T) (*model, *fakeController) {
	t.Helper()
	m, c := newTestModel(t, 70, 30)
	send(m, planRequest(7, "# Heading\n\nstep one"))
	require.NotNil(t, m.plan)
	return m, c
}

func TestPlanDialogShowsTitleRowsAndHints(t *testing.T) {
	m, _ := newTestModel(t, 100, 30)
	send(m, planRequest(7, "# Heading\n\nstep one"))
	rows := screen(m)
	rule := strings.Repeat("─", 100)

	assert.Equal(t, []string{
		rule,
		"  Approve this plan?",
		"  " + planPath + " · ctrl+g to edit · pgup/pgdown to scroll · esc to dismiss",
		"",
		"❭ 1. Yes",
		"  2. No (esc)  What should change?",
		rule,
	}, rows[len(rows)-8:len(rows)-1], "directly above the footer")
	assert.Contains(t, strings.Join(rows, "\n"), "Heading", "the plan is in the transcript")
}

func TestPlanDialogHintsLeaveOutUnboundKeys(t *testing.T) {
	opts := testOptions()
	delete(opts.Keys, "edit-plan")
	delete(opts.Keys, "page-up")
	m, _ := newTestModelWith(t, 70, 30, opts)
	send(m, planRequest(7, "x"))
	assert.Contains(t, screen(m), "  "+planPath+" · esc to dismiss")
}

func TestPlanYesApproves(t *testing.T) {
	m, c := openPlan(t)
	press(m, enter)
	assert.Equal(t, []planAnswer{{7, tool.PlanAnswer{Approved: true}}}, c.planAnswers)
	assert.NotNil(t, m.plan, "the dialog stays until PlanEnded")
}

func TestPlanNoSendsTheFeedbackOrDismissesWhenBlank(t *testing.T) {
	m, c := openPlan(t)
	press(m, digit('2'))
	typeKeys(m, "split it")
	press(m, enter)
	assert.Equal(t, []planAnswer{{7, tool.PlanAnswer{Feedback: "split it"}}}, c.planAnswers)

	m, c = openPlan(t)
	press(m, digit('2'))
	typeKeys(m, "  ")
	press(m, enter)
	assert.Equal(t, []planAnswer{{7, tool.PlanAnswer{}}}, c.planAnswers)
}

func TestPlanEscDismisses(t *testing.T) {
	m, c := openPlan(t)
	press(m, escape)
	assert.Equal(t, []planAnswer{{7, tool.PlanAnswer{}}}, c.planAnswers)
}

func TestPlanCapturesKeys(t *testing.T) {
	m, c := openPlan(t)
	press(m, shiftTab)
	press(m, digit('x'))
	pasteText(m, "pasted")

	assert.Zero(t, c.toggles)
	assert.Empty(t, m.composer.value())
	assert.Empty(t, c.planAnswers)
}

func TestPlanPageKeysScrollTheTranscript(t *testing.T) {
	m, _ := newTestModel(t, 40, 14)
	send(m, event.TextDelta{Text: strings.Repeat("line\n\n", 30)}, planRequest(7, "plan"))
	require.True(t, m.follow)

	press(m, tea.KeyPressMsg{Code: tea.KeyPgUp})
	assert.False(t, m.follow)
	press(m, pgDown)
	assert.True(t, m.follow)
}

func TestPlanRepeatKeepsTheDialogAndTheTypedFeedback(t *testing.T) {
	m, c := openPlan(t)
	press(m, digit('2'))
	typeKeys(m, "tighter")

	send(m, planRequest(7, "# Heading\n\nstep one, edited"))
	assert.Contains(t, strings.Join(body(m), "\n"), "step one, edited")
	press(m, enter)
	assert.Equal(t, []planAnswer{{7, tool.PlanAnswer{Feedback: "tighter"}}}, c.planAnswers)
}

func TestPlanClosesOnEndTurnAndSessionChange(t *testing.T) {
	for name, ev := range map[string]event.Event{
		"PlanEnded":      event.PlanEnded{Token: 7},
		"TurnEnd":        event.TurnEnd{},
		"SessionChanged": event.SessionChanged{},
	} {
		t.Run(name, func(t *testing.T) {
			m, _ := openPlan(t)
			send(m, ev)
			assert.Nil(t, m.plan)
			assert.Nil(t, m.dialog)
			assert.NotContains(t, strings.Join(screen(m), "\n"), "Approve this plan?")
		})
	}
}

func TestPlanEndedForAnotherTokenKeepsTheDialog(t *testing.T) {
	m, _ := openPlan(t)
	send(m, event.PlanEnded{Token: 8})
	assert.NotNil(t, m.plan)
}

func TestPlanBlockStaysAfterTheDialogCloses(t *testing.T) {
	m, _ := openPlan(t)
	send(m, event.PlanEnded{Token: 7})
	assert.Contains(t, strings.Join(body(m), "\n"), "step one")
}

func TestPlanReturnsAfterAChildApprovalOverIt(t *testing.T) {
	m, c := openPlan(t)
	send(m, subagentAsk(2, "explore"))
	text := strings.Join(screen(m), "\n")
	assert.Contains(t, text, "explore t2 › Edit explore.go")
	assert.NotContains(t, text, "Approve this plan?")

	press(m, enter)
	assert.Len(t, c.answers, 1)
	assert.Empty(t, c.planAnswers, "keys go to the approval")

	send(m, event.ApprovalEnded{Token: 2})
	assert.Nil(t, m.approval)
	assert.Contains(t, strings.Join(screen(m), "\n"), "Approve this plan?")
	press(m, enter)
	assert.Equal(t, []planAnswer{{7, tool.PlanAnswer{Approved: true}}}, c.planAnswers)
}

func TestPlanWaitsForAChildApprovalAlreadyShown(t *testing.T) {
	m, c := newTestModel(t, 70, 30)
	send(m, subagentAsk(2, "explore"), planRequest(7, "x"))
	assert.NotContains(t, strings.Join(screen(m), "\n"), "Approve this plan?")

	send(m, event.ApprovalEnded{Token: 2})
	assert.Contains(t, strings.Join(screen(m), "\n"), "Approve this plan?")
	assert.Empty(t, c.planAnswers)
}

type execCall struct {
	cmd  *exec.Cmd
	done tea.ExecCallback
}

// recordExec replaces the model's exec function with one that records its
// calls, so no editor runs.
func recordExec(m *model) *[]execCall {
	var calls []execCall
	m.execProcess = func(cmd *exec.Cmd, done tea.ExecCallback) tea.Cmd {
		calls = append(calls, execCall{cmd, done})
		return nil
	}
	return &calls
}

func TestPlanEditKeyRunsTheEditorThenRefreshesThePlan(t *testing.T) {
	m, c := openPlan(t)
	m.getenv = func(string) string { return "vim -u NONE" }
	calls := recordExec(m)

	press(m, ctrlG)
	require.Len(t, *calls, 1)
	assert.Equal(t, []string{"sh", "-c", `vim -u NONE "$1"`, "sh", planPath}, (*calls)[0].cmd.Args)
	assert.Empty(t, c.refreshes, "not before the editor exits")

	m.Update((*calls)[0].done(nil))
	assert.Equal(t, []uint64{7}, c.refreshes)
}

func TestPlanEditKeyWithoutAnEditorShowsTheNotice(t *testing.T) {
	m, c := openPlan(t)
	m.getenv = func(string) string { return "" }
	calls := recordExec(m)

	press(m, ctrlG)
	assert.Empty(t, *calls)
	assert.Empty(t, c.refreshes)
	assert.Contains(t, body(m), "κ "+noEditorNotice)
}

func TestPlanEditorFailureIsShownAndTheEditedFileStillRead(t *testing.T) {
	m, c := openPlan(t)
	m.getenv = func(string) string { return "vim" }
	calls := recordExec(m)

	press(m, ctrlG)
	m.Update((*calls)[0].done(assert.AnError))
	assert.Contains(t, strings.Join(body(m), "\n"), "failed to run the editor: "+assert.AnError.Error())
	assert.Equal(t, []uint64{7}, c.refreshes)
}

func TestPlanDialogUsesAReboundEditKey(t *testing.T) {
	opts := testOptions()
	opts.Keys["edit-plan"] = []string{"ctrl+e"}
	m, _ := newTestModelWith(t, 70, 30, opts)
	send(m, planRequest(7, "x"))
	m.getenv = func(string) string { return "vim" }
	calls := recordExec(m)

	press(m, ctrlG)
	assert.Empty(t, *calls)
	press(m, tea.KeyPressMsg{Code: 'e', Mod: tea.ModCtrl})
	assert.Len(t, *calls, 1)
	assert.Contains(t, strings.Join(screen(m), "\n"), "ctrl+e to edit")
}

func TestPlanDefaultsBindEditPlan(t *testing.T) {
	km := newKeyMap(config.DefaultKeys())
	assert.Equal(t, []string{"ctrl+g"}, km.editPlan.Keys())
}
