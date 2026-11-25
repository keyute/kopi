package tui

import (
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/tool"
)

func workflowOf(m *model) *workflowDialog { return promptOf[*workflowDialog](m) }

func TestProgressBar(t *testing.T) {
	th := mocha()
	cells := func(parts ...string) string { return strings.Join(parts, "") }
	for _, tc := range []struct {
		name string
		s    app.WorkflowStatus
		want string
	}{
		{"no agents", app.WorkflowStatus{}, fg(th.dim).Render(strings.Repeat("▱", 16))},
		{
			"mixed",
			app.WorkflowStatus{Done: 5, Failed: 1, Running: 1, Queued: 2},
			cells(fg(th.success).Render(strings.Repeat("▰", 8)), fg(th.error).Render("▰▰"), fg(th.accent).Render("▰▰"), fg(th.dim).Render(strings.Repeat("▱", 4))),
		},
		{
			"more calls than cells",
			app.WorkflowStatus{Done: 20, Running: 12},
			cells(fg(th.success).Render(strings.Repeat("▰", 10)), fg(th.accent).Render(strings.Repeat("▰", 6))),
		},
		{"all done", app.WorkflowStatus{Done: 3}, fg(th.success).Render(strings.Repeat("▰", 16))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, progressBar(tc.s, th))
		})
	}
}

func TestWorkflowCounts(t *testing.T) {
	assert.Equal(t, "0/0", workflowCounts(app.WorkflowStatus{}))
	assert.Equal(t, "5/9", workflowCounts(app.WorkflowStatus{Done: 5, Running: 1, Queued: 3}))
	assert.Equal(t, "5/9 · 1 failed", workflowCounts(app.WorkflowStatus{Done: 5, Failed: 1, Running: 1, Queued: 2}))
}

// agentOf is workflow call n, with a session id when it has started.
func agentOf(n int, id, agent, label, phase string, state tool.TaskStatus) app.WorkflowAgent {
	return app.WorkflowAgent{
		AgentStatus: app.AgentStatus{ID: id, Agent: agent, Description: label, State: state},
		N:           n, Phase: phase,
	}
}

// workflowFixture is a run w1 started 130s ago with a finished explorer, a
// running reviewer, a failed one and a queued one, as the Controller would
// describe it.
func workflowFixture(now time.Time) (app.WorkflowStatus, []app.WorkflowAgent) {
	done := agentOf(1, "s1", "explorer", "list route files", "Survey", tool.TaskCompleted)
	done.Started, done.Ended, done.ContextTokens = now.Add(-100*time.Second), now.Add(-60*time.Second), 12_300
	running := agentOf(2, "s2", "reviewer", "users.ts", "Review", tool.TaskRunning)
	running.Started, running.ContextTokens, running.ContextWindow, running.Model = now.Add(-48*time.Second), 18_200, 100_000, "mock/rev-1"
	failed := agentOf(3, "s3", "reviewer", "pay.ts", "Review", tool.TaskFailed)
	failed.Started, failed.Ended, failed.ContextTokens, failed.Reason = now.Add(-100*time.Second), now.Add(-37*time.Second), 9_000, "boom"
	queued := agentOf(4, "", "reviewer", "auth.ts", "Review", app.WorkflowQueued)
	status := app.WorkflowStatus{
		ID: "w1", Description: "audit route handlers", State: tool.TaskRunning, Started: now.Add(-130 * time.Second),
		Done: 1, Failed: 1, Running: 1, Queued: 1, Phase: "Review",
	}
	return status, []app.WorkflowAgent{done, running, failed, queued}
}

// workflowModel has run w1 running, listed in the rows and known to the dialog.
func workflowModel(t *testing.T, width int) (*model, *fakeController) {
	t.Helper()
	m, c := newTestModel(t, width, 30)
	status, agents := workflowFixture(time.Now())
	c.workflows = []app.WorkflowStatus{status}
	c.runs = map[string]fakeRun{"w1": {status: status, agents: agents, history: map[int][]event.Event{
		1: {event.TurnStart{Prompt: "list them"}},
		3: {event.TurnStart{Prompt: "check pay.ts"}},
	}}}
	c.agents = []app.AgentStatus{mainAgent}
	send(m, event.TasksChanged{})
	return m, c
}

// openFromRow opens the dialog the way a human does: down into the rows, Enter.
func openFromRow(t *testing.T, m *model) *workflowDialog {
	t.Helper()
	press(m, down)
	press(m, enter)
	w := workflowOf(m)
	require.NotNil(t, w)
	return w
}

// workflowBox is the screen from the dialog's top rule to its bottom one.
func workflowBox(t *testing.T, m *model) []string {
	t.Helper()
	s := screen(m)
	rule := strings.Repeat("─", m.width)
	top := -1
	for i, r := range s {
		if r != rule {
			continue
		}
		if top < 0 {
			top = i
			continue
		}
		return s[top : i+1]
	}
	require.Fail(t, "no dialog on screen")
	return nil
}

// setRun replaces run w1 as the Controller reports it and announces the change.
func setRun(m *model, c *fakeController, change func(*fakeRun)) {
	run := c.runs["w1"]
	change(&run)
	c.runs["w1"] = run
	send(m, event.TasksChanged{})
}

func TestWorkflowRowShrinksTheDescriptionThenDropsTheBar(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const full = "  ○ workflow › audit route handlers  ▰▰▰▰▰▰▰▰▰▰▰▰▱▱▱▱  5/9 · 1 failed · Review · 2m 10s"
		m, c := workflowModel(t, 120)
		c.workflows[0].Done, c.workflows[0].Queued = 5, 2
		send(m, event.TasksChanged{})
		assert.Equal(t, full, lastRows(m, 1)[0])

		noBar := []rune("  ○ workflow › audit route…  5/9 · 1 failed · Review · 2m 10s")
		for width, want := range map[int]string{
			87: full,
			80: "  ○ workflow › audit route …  ▰▰▰▰▰▰▰▰▰▰▰▰▱▱▱▱  5/9 · 1 failed · Review · 2m 10s",
			79: "  ○ workflow › audit route…  ▰▰▰▰▰▰▰▰▰▰▰▰▱▱▱▱  5/9 · 1 failed · Review · 2m 10s",
			78: "  ○ workflow › audit route handlers  5/9 · 1 failed · Review · 2m 10s",
			45: string(noBar[:45]),
		} {
			m.Update(tea.WindowSizeMsg{Width: width, Height: 30})
			assert.Equal(t, want, lastRows(m, 1)[0], "width %d", width)
		}
	})
}

func TestWorkflowDialogGroupsAgentsByPhase(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := workflowModel(t, 52)
		// A call with no phase lists first, before any heading.
		setRun(m, c, func(r *fakeRun) {
			r.agents = append(r.agents, agentOf(5, "s5", "", "", "", tool.TaskCompleted))
			r.status.Done++
		})
		openFromRow(t, m)
		rule := strings.Repeat("─", 52)

		assert.Equal(t, []string{
			rule,
			"  Workflow › audit route handlers",
			"  ▰▰▰▰▰▰▰▰▰▰▰▰▱▱▱▱  2/5 · 1 failed · 2m 10s",
			"",
			"❭ • agent #5",
			"  Survey · 1/1",
			"    • explorer › list route files        40s · 12.3k",
			"  Review · 0/3",
			"    • reviewer › users.ts                48s · 18.2k",
			"    • reviewer › pay.ts        failed · 1m 3s · 9.0k",
			"    • reviewer › auth.ts                      queued",
			"",
			"  enter to view · esc to close",
			rule,
		}, workflowBox(t, m))
	})
}

func TestWorkflowDialogBulletsFollowTheState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := workflowModel(t, 52)
		w := openFromRow(t, m)
		th := m.th
		for i, want := range []string{
			fg(th.success).Render("•"), fg(th.muted).Render("•"), fg(th.error).Render("•"), fg(th.dim).Render("•"),
		} {
			assert.Contains(t, w.d.options[i].drawn[len(w.d.options[i].drawn)-1], "    "+want+" ", "option %d", i)
		}
	})
}

func TestWorkflowDialogHeaderNamesAnEndedRunAndStopsCounting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := workflowModel(t, 52)
		openFromRow(t, m)
		end := time.Now().Add(-10 * time.Second)

		setRun(m, c, func(r *fakeRun) {
			r.status.State, r.status.Ended = tool.TaskFailed, end
		})

		box := workflowBox(t, m)
		assert.Equal(t, "  ▰▰▰▰▰▰▰▰▰▰▰▰▱▱▱▱  1/4 · 1 failed · failed · 2m 0s", box[2])
		time.Sleep(time.Minute)
		m.arrange()
		assert.Equal(t, box[2], workflowBox(t, m)[2], "the end fixes the time")
	})
}

func TestWorkflowDialogHeaderColoursHowTheRunEnded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := workflowModel(t, 52)
		w := openFromRow(t, m)
		th := m.th
		for state, colour := range map[tool.TaskStatus]lipgloss.Style{tool.TaskStopped: fg(th.warning), tool.TaskFailed: fg(th.error)} {
			setRun(m, c, func(r *fakeRun) { r.status.State, r.status.Ended = state, time.Now() })
			workflowBox(t, m)
			assert.Contains(t, w.head[1], colour.Render(string(state)))
		}
	})
}

func TestWorkflowDialogRunningTimesAdvanceEachSecond(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := workflowModel(t, 52)
		openFromRow(t, m)
		assert.Contains(t, workflowBox(t, m)[7], "48s · 18.2k")

		time.Sleep(3 * time.Second)
		m.arrange()

		box := workflowBox(t, m)
		assert.Contains(t, box[7], "51s · 18.2k")
		assert.Contains(t, box[2], "2m 13s")
		assert.Contains(t, box[5], "40s · 12.3k", "an ended agent's time is fixed")
	})
}

func TestWorkflowDialogEnterWatchesARunningAgent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := workflowModel(t, 60)
		send(m, sub("s2", event.TurnStart{Prompt: "review users.ts"}))
		w := openFromRow(t, m)
		press(m, down)

		press(m, enter)

		assert.True(t, w.watching)
		assert.Equal(t, "s2", m.shown)
		assert.Contains(t, body(m), "❯ review users.ts")
		assert.Equal(t, []string{"  watching reviewer › users.ts", "", "  esc to go back"}, workflowBoxFoot(t, m))
	})
}

func TestWorkflowDialogWatchNoteKeepsOneRow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := workflowModel(t, 30)
		send(m, sub("s2", event.TurnStart{Prompt: "review"}))
		setRun(m, c, func(r *fakeRun) { r.agents[1].Description = strings.Repeat("long ", 20) })
		openFromRow(t, m)
		press(m, down)

		press(m, enter)

		foot := workflowBoxFoot(t, m)
		require.Len(t, foot, 3)
		assert.Contains(t, foot[0], "watching")
		assert.Contains(t, foot[0], "…")
	})
}

// workflowBoxFoot is the note, the blank row and the key hint of the watch bar.
func workflowBoxFoot(t *testing.T, m *model) []string {
	t.Helper()
	box := workflowBox(t, m)
	return box[1 : len(box)-1]
}

func TestWorkflowDialogEnterRestoresAnEndedAgent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := workflowModel(t, 60)
		w := openFromRow(t, m)
		press(m, down)
		press(m, down)

		press(m, enter)

		assert.True(t, w.watching)
		assert.Equal(t, "s3", m.shown)
		assert.Contains(t, body(m), "❯ check pay.ts")
		assert.True(t, m.views["s3"].closed)
	})
}

func TestWorkflowDialogEnterOnAnAgentWithoutASessionFlashesWhy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := workflowModel(t, 60)
		setRun(m, c, func(r *fakeRun) {
			r.agents = append(r.agents, agentOf(5, "", "reviewer", "x.ts", "Review", tool.TaskFailed))
			r.agents[4].Reason = "no slot"
		})
		w := openFromRow(t, m)
		press(m, down)
		press(m, down)
		press(m, down)

		press(m, enter)
		assert.Contains(t, strings.Join(strings.Fields(strings.Join(workflowBox(t, m), "\n")), " "), "not started yet", "the notice wraps in the hint row")
		press(m, down)
		press(m, enter)
		assert.Contains(t, strings.Join(workflowBox(t, m), "\n"), "no slot")
		assert.False(t, w.watching)
		assert.Equal(t, app.Main, m.shown)
	})
}

func TestWorkflowDialogEnterOnACallStoppedWhileQueuedSaysSo(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := workflowModel(t, 60)
		setRun(m, c, func(r *fakeRun) {
			r.agents = append(r.agents, agentOf(5, "", "reviewer", "x.ts", "Review", tool.TaskStopped))
		})
		openFromRow(t, m)
		for range 4 {
			press(m, down)
		}

		press(m, enter)

		assert.Contains(t, strings.Join(workflowBox(t, m), "\n"), "stopped before it started")
	})
}

func TestWorkflowDialogStaysInTheListWhenTheHistoryCannotBeRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := workflowModel(t, 60)
		setRun(m, c, func(r *fakeRun) { r.historyErr = fmt.Errorf("failed to read: %w", tool.ErrUnknownTask) })
		w := openFromRow(t, m)

		press(m, enter)

		assert.False(t, w.watching)
		assert.Equal(t, app.Main, m.shown)
		assert.Contains(t, body(m)[len(body(m))-1], "failed to read")
	})
}

func TestWorkflowDialogDismissWhileWatchingReturnsToTheListOnTheSameAgent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := workflowModel(t, 60)
		send(m, sub("s2", event.TurnStart{Prompt: "review"}))
		w := openFromRow(t, m)
		press(m, down)
		press(m, enter)
		require.Equal(t, "s2", m.shown)

		press(m, escape)

		assert.False(t, w.watching)
		assert.Equal(t, app.Main, m.shown)
		assert.Equal(t, 2, m.workflowSel[w.id])
		assert.Contains(t, strings.Join(workflowBox(t, m), "\n"), "❭   • reviewer › users.ts")
		assert.Contains(t, m.views, "s2", "a running agent's view stays")

		press(m, down)
		press(m, enter)
		require.Equal(t, "s3", m.shown)
		press(m, escape)
		assert.NotContains(t, m.views, "s3", "a closed view goes when it is left")
		press(m, escape)
		assert.Nil(t, workflowOf(m))
	})
}

func TestWorkflowDialogWatchingReturnsToTheViewShownBefore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := workflowModel(t, 60)
		c.agents = append(c.agents, app.AgentStatus{ID: "t1", Agent: "explore"})
		send(m, event.TasksChanged{}, sub("t1", event.TurnStart{Prompt: "dig"}), sub("s2", event.TurnStart{Prompt: "review"}))
		display(m, "t1")
		press(m, down)
		press(m, down)
		press(m, down)
		press(m, enter)
		require.NotNil(t, workflowOf(m))
		press(m, down)

		press(m, enter)
		assert.Equal(t, "s2", m.shown)
		press(m, escape)

		assert.Equal(t, "t1", m.shown)
	})
}

func TestWorkflowDialogWatchingSwallowsKeysAndKeepsTheFolds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := workflowModel(t, 60)
		send(m, sub("s2", event.TurnStart{Prompt: "review"}), sub("s2", start("a")), sub("s2", ok("a", "3 lines")),
			sub("s2", start("b")), sub("s2", ok("b", "4 lines")), sub("s2", event.TextDelta{Text: "Done."}))
		openFromRow(t, m)
		press(m, down)
		press(m, enter)
		before := strings.Join(body(m), "\n")

		for _, k := range []tea.KeyPressMsg{digit('1'), keyX, enter, ctrlEnter, up, down, ctrlD, ctrlB, shiftTab, {Code: 'a', Text: "a"}} {
			press(m, k)
		}
		m.Update(tea.PasteMsg{Content: "pasted"})

		assert.Empty(t, m.composer.value())
		assert.Empty(t, c.calls, "nothing reaches the agent")
		assert.Empty(t, c.submitted)
		assert.Empty(t, c.sentNow)
		assert.Zero(t, c.interrupts)
		assert.Zero(t, c.takes)
		assert.Zero(t, c.toggles)
		assert.Empty(t, c.agentStops)
		assert.Equal(t, "s2", m.shown)
		assert.Equal(t, before, strings.Join(body(m), "\n"))

		press(m, ctrlO)
		assert.NotEqual(t, before, strings.Join(body(m), "\n"), "expand-all opens the folds")
	})
}

func TestWorkflowDialogStopKeyStopsOnlyAQueuedOrRunningAgent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := workflowModel(t, 60)
		openFromRow(t, m)
		hint := func() string { box := workflowBox(t, m); return box[len(box)-2] }

		press(m, keyX)
		assert.Empty(t, c.agentStops, "a completed agent is not stopped")
		assert.NotContains(t, hint(), "stop agent")

		press(m, down)
		assert.Contains(t, hint(), "x to stop agent")
		press(m, keyX)
		press(m, down)
		press(m, keyX)
		assert.Equal(t, []string{"w1#2"}, c.agentStops, "a failed agent is not stopped")
		press(m, down)
		press(m, keyX)
		assert.Equal(t, []string{"w1#2", "w1#4"}, c.agentStops)
		assert.Zero(t, c.interrupts)
		assert.Empty(t, c.stops)
	})
}

func TestWorkflowDialogReopensOnTheAgentLeft(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := workflowModel(t, 60)
		openFromRow(t, m)
		press(m, down)
		press(m, down)
		press(m, escape)
		require.Nil(t, workflowOf(m))

		w := openFromRow(t, m)

		assert.Equal(t, 3, m.workflowSel[w.id])
		assert.Equal(t, 2, w.d.sel)
	})
}

func TestWorkflowDialogTasksChangedReadsAgainAndKeepsTheSelectedAgent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := workflowModel(t, 60)
		w := openFromRow(t, m)
		press(m, down)
		press(m, down)
		reads := c.workflowReads
		m.arrange()
		assert.Equal(t, reads, c.workflowReads, "a frame reads nothing")

		// A new call with no phase lists first, so every index moves.
		setRun(m, c, func(r *fakeRun) {
			r.agents = append(r.agents, agentOf(5, "s5", "reviewer", "new", "", tool.TaskRunning))
			r.agents[2].State = tool.TaskCompleted
		})
		assert.Equal(t, reads+1, c.workflowReads, "one read for the change")
		m.arrange()
		assert.Equal(t, reads+1, c.workflowReads, "and none for the frames after it")
		assert.Equal(t, 3, m.workflowSel[w.id])
		assert.Equal(t, 3, w.d.sel)
		assert.Contains(t, strings.Join(workflowBox(t, m), "\n"), "❭   • reviewer › pay.ts")
	})
}

func TestWorkflowDialogToolEndAndTurnEndRefreshIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, ev := range []event.Event{sub("s2", ok("a", "x")), sub("s2", event.TurnEnd{})} {
			m, c := workflowModel(t, 60)
			openFromRow(t, m)
			reads := c.workflowReads

			send(m, ev)
			m.arrange()

			assert.Equal(t, reads+1, c.workflowReads, "%T", ev)
		}
	})
}

func TestSessionChangeClosesTheWorkflowDialogAndWatching(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := workflowModel(t, 60)
		openFromRow(t, m)
		press(m, down)
		press(m, enter)
		require.Equal(t, "s2", m.shown)
		c.workflows = nil

		send(m, event.SessionChanged{SessionID: "s2"})

		assert.Nil(t, workflowOf(m))
		assert.Equal(t, app.Main, m.shown)
		assert.Empty(t, m.workflowSel)
		assert.Empty(t, m.workflows)
	})
}

func TestLeavingAWatchedAgentWhoseSessionClosedDeletesItsView(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := workflowModel(t, 60)
		send(m, sub("s2", event.TurnStart{Prompt: "review"}))
		openFromRow(t, m)
		press(m, down)
		press(m, enter)
		require.Equal(t, "s2", m.shown)

		send(m, event.SessionClosed{ID: "s2"})
		require.True(t, m.views["s2"].closed)

		press(m, escape)

		assert.Equal(t, app.Main, m.shown)
		assert.NotContains(t, m.views, "s2")
	})
}

func TestAPlanLeavesWatchingButKeepsTheListOpen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := workflowModel(t, 60)
		w := openFromRow(t, m)
		press(m, down)
		press(m, down)
		press(m, enter)
		require.Equal(t, "s3", m.shown)

		send(m, planRequest(1, "# Plan"))

		assert.Equal(t, app.Main, m.shown)
		assert.NotContains(t, m.views, "s3")
		assert.False(t, w.watching)
		assert.Equal(t, promptPlan, frontKind(m))

		send(m, event.PlanEnded{Token: 1})

		assert.Equal(t, promptWorkflow, frontKind(m))
		assert.Equal(t, 3, m.workflowSel[w.id])
	})
}

func TestApprovalCoversTheWorkflowDialogAndItReturns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := workflowModel(t, 60)
		openFromRow(t, m)

		send(m, editApproval())

		assert.NotNil(t, workflowOf(m))
		assert.Equal(t, promptApproval, frontKind(m))
		assert.NotContains(t, screen(m), "  Workflow › audit route handlers")

		send(m, event.ApprovalEnded{Token: 1})

		assert.Equal(t, promptWorkflow, frontKind(m))
		assert.Contains(t, screen(m), "  Workflow › audit route handlers")
	})
}

func TestWorkflowDialogSecondFrameAllocationsDoNotGrowWithTheAgents(t *testing.T) {
	if raceEnabled {
		t.Skip("sync.Pool randomly drops items under -race, so allocation counts vary")
	}
	allocs := func(n int) float64 {
		m, c := newTestModel(t, 60, 30)
		now := time.Now()
		agents := make([]app.WorkflowAgent, n)
		for i := range agents {
			agents[i] = agentOf(i+1, fmt.Sprintf("s%d", i), "reviewer", fmt.Sprintf("file %d", i), "", tool.TaskCompleted)
			agents[i].Started, agents[i].Ended = now.Add(-time.Minute), now
		}
		status := app.WorkflowStatus{ID: "w1", Description: "audit", State: tool.TaskCompleted, Started: now.Add(-time.Minute), Ended: now, Done: n}
		c.runs = map[string]fakeRun{"w1": {status: status, agents: agents}}
		require.True(t, m.openWorkflow("w1"))
		w := workflowOf(m)
		w.dialog(40, mocha()).rows(40, 20, 5, mocha())
		return testing.AllocsPerRun(10, func() { w.dialog(40, mocha()).rows(40, 20, 5, mocha()) })
	}
	assert.LessOrEqual(t, allocs(1000), allocs(50)+2)
}
