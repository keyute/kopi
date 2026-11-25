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
	"github.com/keyute/kopi/internal/config"
	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/tool"
)

// tasksModel has a running t1 started 62s ago and a t2 that completed after
// 5s. Run it inside a synctest bubble so the times hold still.
func tasksModel(t *testing.T) (*model, *fakeController) {
	t.Helper()
	m, c := newTestModel(t, 60, 30)
	now := time.Now()
	c.tasks = []tool.TaskInfo{
		{ID: "t1", Command: "sleep 30\necho done", Started: now.Add(-62 * time.Second), Status: tool.TaskRunning},
		{ID: "t2", Command: "make build", Started: now.Add(-time.Minute), Ended: now.Add(-55 * time.Second), Status: tool.TaskCompleted, Reason: "exit code 0"},
	}
	c.outputs = map[string]string{"t1": "one\ntwo\n", "t2": "built\n"}
	c.submitErr = app.ErrShowTasks
	return m, c
}

// dialogRows is the screen from the dialog's top rule down to its last row.
func dialogRows(t *testing.T, m *model) []string {
	t.Helper()
	s := screen(m)
	rule := strings.Repeat("─", m.width)
	top := -1
	for i, r := range s {
		if r == rule {
			top = i
			break
		}
	}
	require.GreaterOrEqual(t, top, 0, "no dialog on screen")
	return s[top:]
}

// listRow is a /tasks list row as plain text: what the task is and, flush right
// at width, meta.
func listRow(cursor bool, what, meta string, width int) string {
	lead := "  • "
	if cursor {
		lead = "❭ • "
	}
	return lead + what + strings.Repeat(" ", width-lipgloss.Width(lead+what+meta)) + meta
}

func TestTasksWithNoTasksFlashesAndOpensNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := newTestModel(t, 60, 30)
		c.submitErr = app.ErrShowTasks

		assert.NotNil(t, submitText(m, "/tasks"), "the flash expires")

		assert.Nil(t, tasksOf(m))
		assert.Empty(t, m.composer.value())
		assert.Contains(t, statusText(m), "no background tasks")
		assert.True(t, strings.HasPrefix(screen(m)[m.composerTop()], "❯"))
	})
}

func TestTasksOpensTheListAtTheBottom(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := tasksModel(t)
		typeText(m, "/tasks")
		assert.Nil(t, press(m, enter))

		rule := strings.Repeat("─", 60)
		assert.Equal(t, []string{
			rule,
			"  Background tasks",
			"",
			listRow(true, "sleep 30", "1m 2s", 60),
			"  Finished",
			listRow(false, "make build", "5s", 60),
			"",
			"  enter to view · esc to close",
			rule,
		}, dialogRows(t, m))
		assert.Empty(t, m.composer.value(), "the draft is accepted")
		s := screen(m)
		assert.Len(t, s, 30)
		assert.Equal(t, rule, s[len(s)-1], "the dialog is the last rows, the status line is hidden")
		assert.Equal(t, m.height, m.transcriptHeight()+len(m.below))
	})
}

func TestTasksListCutsLongCommands(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := tasksModel(t)
		c.tasks[1].Command = strings.Repeat("x", 100)
		submitText(m, "/tasks")

		row := dialogRows(t, m)[5]
		assert.Equal(t, "  • "+strings.Repeat("x", 51)+"…  5s", row)
		assert.Len(t, []rune(row), 60)
	})
}

func TestTasksListNamesAnAgentTask(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := tasksModel(t)
		c.tasks[1].Agent = "explore"
		c.tasks[1].Command = "look around\nmore"
		submitText(m, "/tasks")

		rows := dialogRows(t, m)
		assert.Equal(t, listRow(true, "sleep 30", "1m 2s", 60), rows[3])
		assert.Equal(t, listRow(false, "explore › look around", "5s", 60), rows[5])
	})
}

func TestTasksOpenAgentWithoutASessionShowsItsOutputAndOptions(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := tasksModel(t)
		c.tasks[0].Agent = "explore"
		c.tasks[0].Command = "look around"
		submitText(m, "/tasks")

		press(m, enter)

		rule := strings.Repeat("─", 60)
		want := []string{rule, "  running · 1m 2s · explore › look around", "    one", "    two", "", "❭ 1. Stop", "  2. Back", "", "  esc to go back", rule}
		assert.Equal(t, want, dialogRows(t, m))
	})
}

func TestTasksOpenAgentWithASessionShowsItsView(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := tasksModel(t)
		c.tasks[0].Agent = "explore"
		c.agents = []app.AgentStatus{{ID: app.Main}, {ID: "t1", Agent: "explore"}}
		send(m, event.TasksChanged{}, event.Session{ID: "t1", Event: event.TurnStart{Prompt: "dig"}})
		submitText(m, "/tasks")
		require.NotNil(t, tasksOf(m))

		press(m, enter)

		assert.Nil(t, tasksOf(m))
		assert.Equal(t, "t1", m.shown)
		assert.Contains(t, body(m), "❯ dig")
	})
}

func TestTasksListStopsTheSelectedRunningAgent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := tasksModel(t)
		c.tasks[1].Agent, c.tasks[1].Status = "explore", tool.TaskRunning
		c.agents = []app.AgentStatus{{ID: app.Main}, {ID: "t2", Agent: "explore"}}
		send(m, event.TasksChanged{}, sub("t2", event.TurnStart{Prompt: "dig"}))
		submitText(m, "/tasks")
		press(m, down)
		assert.Contains(t, strings.Join(screen(m), "\n"), "x to stop agent")

		press(m, keyX)

		assert.Equal(t, []string{"t2"}, c.stops)
		require.NotNil(t, tasksOf(m))
		assert.Empty(t, tasksOf(m).open)
		assert.Equal(t, "t2", tasksOf(m).list[tasksOf(m).d.sel].ID)
		assert.Equal(t, tool.TaskStopped, tasksOf(m).list[tasksOf(m).d.sel].Status)
		assert.NotContains(t, strings.Join(screen(m), "\n"), "to stop agent")
		assert.Equal(t, app.Main, m.shown)
		assert.Zero(t, c.interrupts, "list stopping uses StopTask, not Interrupt")

		press(m, keyX)
		assert.Equal(t, []string{"t2"}, c.stops, "an ended agent is not stopped again")
		press(m, enter)
		assert.Nil(t, tasksOf(m))
		assert.Equal(t, "t2", m.shown, "Enter still opens the stopped agent's view")
	})
}

func TestTasksListStopAgentBinding(t *testing.T) {
	for _, keys := range [][]string{{"s"}, nil} {
		t.Run(fmt.Sprint(keys), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, c := tasksModel(t)
				m.keys.stopAgent.SetKeys(keys...)
				c.tasks[0].Agent = "explore"
				submitText(m, "/tasks")
				press(m, keyX)
				assert.Empty(t, c.stops, "the default binding is replaced")
				if len(keys) > 0 {
					assert.Contains(t, strings.Join(screen(m), "\n"), "s to stop agent")
				} else {
					assert.NotContains(t, strings.Join(screen(m), "\n"), "to stop agent")
				}
				press(m, tea.KeyPressMsg{Code: 's', Text: "s"})
				if len(keys) > 0 {
					assert.Equal(t, []string{"t1"}, c.stops)
				} else {
					assert.Empty(t, c.stops)
				}
				assert.Empty(t, m.composer.value())
			})
		})
	}
}

func TestTasksListStopAgentDoesNothingForACompletedTask(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := tasksModel(t)
		c.tasks[1].Agent = "explore"
		submitText(m, "/tasks")
		press(m, down)
		press(m, keyX)
		assert.Empty(t, c.stops)
		assert.Empty(t, tasksOf(m).open)
		assert.Equal(t, "t2", tasksOf(m).list[tasksOf(m).d.sel].ID)
		assert.NotContains(t, strings.Join(screen(m), "\n"), "to stop agent")
	})
}

func TestTasksListStopAgentLeavesShellControlsUnchanged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := tasksModel(t)
		submitText(m, "/tasks")
		press(m, keyX)
		assert.Empty(t, c.stops)
		assert.Empty(t, tasksOf(m).open)
		assert.NotContains(t, strings.Join(screen(m), "\n"), "to stop agent")
		press(m, enter)
		assert.Equal(t, "t1", tasksOf(m).open)
		press(m, keyX)
		assert.Empty(t, c.stops, "the output view still uses its Stop option")
		press(m, enter)
		assert.Equal(t, []string{"t1"}, c.stops)
	})
}

func TestTasksOpenShowsTheOutputTail(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := tasksModel(t)
		var lines []string
		for i := range 25 {
			lines = append(lines, fmt.Sprintf("line %d", i+1))
		}
		c.outputs["t1"] = strings.Join(lines, "\n") + "\n"
		submitText(m, "/tasks")

		press(m, enter)

		rule := strings.Repeat("─", 60)
		want := []string{rule, "  running · 1m 2s · sleep 30", "  … 16 earlier lines"}
		for i := 17; i <= 25; i++ {
			want = append(want, fmt.Sprintf("    line %d", i))
		}
		want = append(want, "", "❭ 1. Stop", "  2. Back", "", "  "+scrollHint(&m.keys)+" · esc to go back", rule)
		assert.Equal(t, want, dialogRows(t, m))
		assert.Len(t, screen(m), 30)
	})
}

func TestTasksOpenFitsWholeOutput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := tasksModel(t)
		submitText(m, "/tasks")

		press(m, down)
		press(m, enter)

		rule := strings.Repeat("─", 60)
		assert.Equal(t, []string{rule, "  completed · 5s · make build", "    built", "", "❭ 1. Back", "", "  esc to go back", rule}, dialogRows(t, m), "an ended task has no Stop")
	})
}

func TestTasksStopStaysOnTheStoppedTask(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := tasksModel(t)
		submitText(m, "/tasks")
		press(m, enter)

		press(m, enter)

		assert.Equal(t, []string{"t1"}, c.stops)
		rows := dialogRows(t, m)
		assert.Equal(t, "  stopped · 1m 2s · sleep 30", rows[1])
		assert.Equal(t, []string{"❭ 1. Back", "", "  esc to go back", strings.Repeat("─", 60)}, rows[len(rows)-4:])
	})
}

func TestTasksBackAndInterruptNavigate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := tasksModel(t)
		submitText(m, "/tasks")

		press(m, down)
		press(m, enter)
		assert.Equal(t, "  completed · 5s · make build", dialogRows(t, m)[1])
		press(m, enter)
		assert.Equal(t, listRow(true, "make build", "5s", 60), dialogRows(t, m)[5], "Back returns with the task selected")

		press(m, up)
		press(m, enter)
		press(m, down)
		press(m, enter)
		assert.Equal(t, "  Background tasks", dialogRows(t, m)[1], "Back is the second option for a running task")
		assert.Empty(t, c.stops)

		press(m, enter)
		press(m, escape)
		assert.Equal(t, listRow(true, "sleep 30", "1m 2s", 60), dialogRows(t, m)[3], "interrupt goes back to the list")

		press(m, escape)
		assert.Nil(t, tasksOf(m))
		assert.Zero(t, c.interrupts, "interrupt only closes the dialog")
		assert.True(t, strings.HasPrefix(screen(m)[m.composerTop()], "❯"))
	})
}

func TestTasksSwallowsOtherInput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := tasksModel(t)
		c.foreground = true
		submitText(m, "/tasks")

		press(m, digit('x'))
		press(m, ctrlB)
		pasteText(m, "pasted")
		press(m, shiftTab)

		assert.NotNil(t, tasksOf(m))
		assert.Empty(t, m.composer.value())
		assert.Zero(t, c.backgrounds)
		assert.Zero(t, c.toggles)
	})
}

func TestTasksOpensMidTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := tasksModel(t)
		send(m, event.TurnStart{Prompt: "go"})

		submitText(m, "/tasks")

		assert.Equal(t, "  Background tasks", dialogRows(t, m)[1])
		assert.Contains(t, screen(m), "⠋ Working… 0s")
	})
}

func TestApprovalCoversTasksAndTheyReturn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := tasksModel(t)
		submitText(m, "/tasks")

		send(m, editApproval())

		assert.NotNil(t, tasksOf(m))
		assert.Equal(t, promptApproval, frontKind(m))
		assert.NotContains(t, screen(m), "  Background tasks")

		send(m, event.ApprovalEnded{Token: 1})

		assert.Equal(t, promptTasks, frontKind(m))
		assert.Contains(t, screen(m), "  Background tasks")
	})
}

func TestSessionChangeClosesTasks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := tasksModel(t)
		submitText(m, "/tasks")

		send(m, event.SessionChanged{SessionID: "s2"})

		assert.Nil(t, tasksOf(m))
		assert.NotContains(t, screen(m), "  Background tasks")
	})
}

func TestTurnEndLeavesTasksOpen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := tasksModel(t)
		send(m, event.TurnStart{Prompt: "go"})
		submitText(m, "/tasks")

		send(m, event.TurnEnd{})

		assert.Equal(t, "  Background tasks", dialogRows(t, m)[1])
	})
}

func TestTasksChangedRefreshesKeepingTheSelection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := tasksModel(t)
		submitText(m, "/tasks")
		press(m, down)

		c.tasks = append([]tool.TaskInfo{{ID: "t0", Command: "true", Started: time.Now(), Ended: time.Now(), Status: tool.TaskFailed}}, c.tasks...)
		send(m, event.TasksChanged{})

		rows := dialogRows(t, m)
		assert.Equal(t, []string{
			listRow(false, "sleep 30", "1m 2s", 60),
			"  Finished",
			listRow(false, "true", "failed · 0s", 60),
			listRow(true, "make build", "5s", 60),
		}, rows[3:7], "the last to end is first under the heading")
	})
}

func TestTasksListKeepsTheCursorOnATaskThatEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := tasksModel(t)
		submitText(m, "/tasks")

		c.tasks[0].Status, c.tasks[0].Ended = tool.TaskCompleted, time.Now()
		send(m, event.TasksChanged{})

		assert.Equal(t, []string{
			"  Finished",
			listRow(true, "sleep 30", "1m 2s", 60),
			listRow(false, "make build", "5s", 60),
		}, dialogRows(t, m)[3:6])
	})
}

func TestTasksListBulletsFollowTheStatus(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := tasksModel(t)
		c.tasks = append(c.tasks,
			tool.TaskInfo{ID: "t3", Command: "false", Started: time.Now(), Ended: time.Now(), Status: tool.TaskFailed},
			tool.TaskInfo{ID: "t4", Command: "yes", Started: time.Now(), Ended: time.Now(), Status: tool.TaskStopped},
		)
		submitText(m, "/tasks")

		th := m.th
		for i, want := range []string{fg(th.muted).Render("•"), fg(th.error).Render("•"), fg(th.warning).Render("•"), fg(th.success).Render("•")} {
			drawn := tasksOf(m).d.options[i].drawn
			assert.Contains(t, drawn[len(drawn)-1], "  "+want+" ", "option %d", i)
		}
		assert.Equal(t, listRow(false, "yes", "stopped · 0s", 60), dialogRows(t, m)[6])
	})
}

func TestTasksRunningTimesAdvanceEachSecond(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := tasksModel(t)
		submitText(m, "/tasks")
		assert.Equal(t, listRow(true, "sleep 30", "1m 2s", 60), dialogRows(t, m)[3])

		time.Sleep(time.Second)
		m.arrange()
		assert.Equal(t, listRow(true, "sleep 30", "1m 3s", 60), dialogRows(t, m)[3], "in the list")

		press(m, enter)
		time.Sleep(time.Second)
		m.arrange()
		assert.Equal(t, "  running · 1m 4s · sleep 30", dialogRows(t, m)[1], "in an open task's title")
	})
}

func TestTasksChangedWhileCoveredRefreshesTheListBehindThePrompt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := tasksModel(t)
		submitText(m, "/tasks")
		press(m, down)
		send(m, approvalCase.open)

		c.tasks = c.tasks[1:]
		send(m, event.TasksChanged{})
		send(m, approvalCase.end)

		require.NotNil(t, m.front())
		assert.Equal(t, promptTasks, m.front().kind)
		assert.Equal(t, []string{"  Finished", listRow(true, "make build", "5s", 60)}, dialogRows(t, m)[3:5])
	})
}

func TestTasksChangedWhileCoveredClosesTheListWhenNoTasksRemain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := tasksModel(t)
		submitText(m, "/tasks")
		send(m, approvalCase.open)

		c.tasks = nil
		send(m, event.TasksChanged{})
		send(m, approvalCase.end)

		assert.Nil(t, m.front())
	})
}

func TestTasksOverflowKeepsTheSelectedOptionVisible(t *testing.T) {
	for _, size := range []struct{ width, height int }{{60, 30}, {40, 15}, {40, 10}} {
		t.Run(fmt.Sprintf("%dx%d", size.width, size.height), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, c := tasksModel(t)
				c.tasks = nil
				for i := 1; i <= 40; i++ {
					c.tasks = append(c.tasks, tool.TaskInfo{ID: fmt.Sprintf("t%d", i), Command: fmt.Sprintf("job %d", i), Started: time.Now(), Ended: time.Now(), Status: tool.TaskCompleted})
				}
				m.Update(tea.WindowSizeMsg{Width: size.width, Height: size.height})
				submitText(m, "/tasks")

				check := func(number int) []string {
					t.Helper()
					rows := dialogRows(t, m)
					rule := strings.Repeat("─", m.width)
					assert.Equal(t, rule, rows[0])
					assert.Equal(t, "  Background tasks", rows[1])
					assert.Equal(t, rule, rows[len(rows)-1])
					assert.Contains(t, rows, listRow(true, fmt.Sprintf("job %d", number), "0s", m.width))
					s := screen(m)
					assert.Len(t, s, m.height)
					assert.Equal(t, rule, s[len(s)-1])
					return rows
				}
				rows := check(1)
				assert.Equal(t, []string{"  Finished", listRow(true, "job 1", "0s", m.width)}, rows[3:5])
				assert.NotContains(t, rows, listRow(false, "job 40", "0s", m.width))

				for i := 2; i <= 40; i++ {
					press(m, down)
					check(i)
				}
				for i := 39; i >= 1; i-- {
					press(m, up)
					check(i)
				}
				for range 19 {
					press(m, down)
				}
				check(20)
				press(m, enter)
				assert.Equal(t, "t20", tasksOf(m).open, "Enter opens the visible global selection")
				press(m, escape)
				check(20)

				c.tasks = append([]tool.TaskInfo{{ID: "t0", Command: "true", Started: time.Now(), Ended: time.Now(), Status: tool.TaskCompleted}}, c.tasks...)
				send(m, event.TasksChanged{})
				assert.Equal(t, "t20", tasksOf(m).list[tasksOf(m).d.sel].ID)
				assert.Contains(t, dialogRows(t, m), listRow(true, "job 20", "0s", m.width))
				for _, height := range []int{10, 30, 15} {
					m.Update(tea.WindowSizeMsg{Width: size.width, Height: height})
					assert.Equal(t, 20, tasksOf(m).d.sel)
					rows := dialogRows(t, m)
					assert.Contains(t, rows, listRow(true, "job 20", "0s", m.width))
					assert.Equal(t, strings.Repeat("─", m.width), rows[0])
					assert.Equal(t, "  Background tasks", rows[1])
					assert.Equal(t, rows[0], rows[len(rows)-1])
					assert.Len(t, screen(m), height)
					assert.Equal(t, rows[0], screen(m)[height-1])
				}

				press(m, digit('2'))
				assert.Empty(t, tasksOf(m).open, "a digit picks no row")
			})
		})
	}
}

func TestTasksChangedRefreshesTheOpenTask(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := tasksModel(t)
		submitText(m, "/tasks")
		press(m, enter)

		c.tasks[0].Status, c.tasks[0].Ended = tool.TaskFailed, time.Now()
		c.outputs["t1"] = "boom\n"
		send(m, event.TasksChanged{})

		rows := dialogRows(t, m)
		assert.Equal(t, []string{"  failed · 1m 2s · sleep 30", "    boom", "", "❭ 1. Back"}, rows[1:5])
	})
}

func numberedOutput(n int) string {
	var lines []string
	for i := range n {
		lines = append(lines, fmt.Sprintf("line %d", i+1))
	}
	return strings.Join(lines, "\n") + "\n"
}

func TestTasksPageKeysScrollACutOutput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := tasksModel(t)
		c.outputs["t1"] = numberedOutput(25)
		submitText(m, "/tasks")
		press(m, enter)
		foot := "  " + scrollHint(&m.keys) + " · esc to go back"

		press(m, pgUp)
		rows := dialogRows(t, m)
		assert.Equal(t, []string{"  … 8 earlier lines", "    line 9"}, rows[2:4])
		assert.Equal(t, []string{"    line 16", "  … 9 more lines"}, rows[10:12])
		assert.Equal(t, foot, rows[len(rows)-2])

		press(m, pgDown)
		rows = dialogRows(t, m)
		assert.Equal(t, []string{"  … 16 earlier lines", "    line 17"}, rows[2:4])
		assert.Equal(t, "    line 25", rows[11])
		assert.Equal(t, foot, rows[len(rows)-2])
		assert.Zero(t, tasksOf(m).d.scroll.below, "back at the end it follows again")
	})
}

func TestTasksFollowTheOutputUntilScrolledUp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := tasksModel(t)
		c.outputs["t1"] = numberedOutput(25)
		submitText(m, "/tasks")
		press(m, enter)

		c.outputs["t1"] = numberedOutput(30)
		send(m, event.TasksChanged{})
		rows := dialogRows(t, m)
		assert.Equal(t, "  … 21 earlier lines", rows[2])
		assert.Equal(t, "    line 30", rows[11])

		press(m, pgUp)
		c.outputs["t1"] = numberedOutput(40)
		send(m, event.TasksChanged{})
		rows = dialogRows(t, m)
		assert.Equal(t, []string{"  … 13 earlier lines", "    line 14"}, rows[2:4], "scrolled up it stays put")
		assert.Positive(t, tasksOf(m).d.scroll.below)
	})
}

func TestTasksWheelScrollsACutOutput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := tasksModel(t)
		c.outputs["t1"] = numberedOutput(25)
		submitText(m, "/tasks")
		press(m, enter)

		m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp, X: 10, Y: m.geo.composerTop})

		assert.Equal(t, "  … 13 earlier lines", dialogRows(t, m)[2])
	})
}

func TestTasksPageKeysScrollTheTranscriptWhenTheOutputFits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := tasksModel(t)
		notices(m, 40)
		submitText(m, "/tasks")
		press(m, enter)
		require.True(t, m.view().follow)

		press(m, pgUp)

		assert.False(t, m.view().follow)
		assert.NotContains(t, strings.Join(screen(m), "\n"), "to scroll")
		assert.Contains(t, strings.Join(screen(m), "\n"), "    two")
	})
}

func TestTasksKeepTheOffsetAcrossAWidthChange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := tasksModel(t)
		c.outputs["t1"] = numberedOutput(25)
		submitText(m, "/tasks")
		press(m, enter)
		press(m, pgUp)
		require.Positive(t, tasksOf(m).d.scroll.below)

		m.Update(tea.WindowSizeMsg{Width: 20, Height: 30})

		assert.Positive(t, tasksOf(m).d.scroll.below)
		assert.Equal(t, "  … 8 earlier lines", dialogRows(t, m)[2])
	})
}

func TestTasksReopenStartsAtTheEnd(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := tasksModel(t)
		c.outputs["t1"] = numberedOutput(25)
		submitText(m, "/tasks")
		press(m, enter)
		press(m, pgUp)
		require.Positive(t, tasksOf(m).d.scroll.below)

		press(m, digit('2'))
		press(m, enter)

		assert.Zero(t, tasksOf(m).d.scroll.below)
		assert.Equal(t, "  … 16 earlier lines", dialogRows(t, m)[2])
	})
}

func TestTasksPageKeysScrollTheTranscriptFromTheListAfterAScrolledOutput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := tasksModel(t)
		c.outputs["t1"] = numberedOutput(25)
		notices(m, 40)
		submitText(m, "/tasks")
		press(m, enter)
		press(m, pgUp)
		require.Positive(t, tasksOf(m).d.scroll.below)
		require.True(t, m.view().follow)

		press(m, escape)
		require.Empty(t, tasksOf(m).open)
		press(m, pgUp)

		assert.False(t, m.view().follow)
		assert.Zero(t, tasksOf(m).d.scroll.below)
		assert.Zero(t, tasksOf(m).d.scroll.off)
	})
}

func TestTasksSecondFrameAllocationsDoNotGrowWithTheOutput(t *testing.T) {
	if raceEnabled {
		t.Skip("sync.Pool randomly drops items under -race, so allocation counts vary")
	}
	km := newKeyMap(config.DefaultKeys())
	allocs := func(n int) float64 {
		info := tool.TaskInfo{ID: "t1", Command: "make", Status: tool.TaskCompleted, Output: numberedOutput(n)}
		td := &taskDialog{d: &dialog{}, km: &km, open: "t1", info: info}
		td.dialog(60, mocha()).rows(60, 100, 10, mocha())
		return testing.AllocsPerRun(10, func() { td.dialog(60, mocha()).rows(60, 100, 10, mocha()) })
	}
	assert.LessOrEqual(t, allocs(5000), allocs(50)+2)
}

func TestTasksEnterOnAWorkflowOpensItsDialogOverTheList(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := tasksModel(t)
		status, agents := workflowFixture(time.Now())
		c.tasks[0].ID = "w1"
		c.runs = map[string]fakeRun{"w1": {status: status, agents: agents}}
		submitText(m, "/tasks")

		press(m, enter)

		require.NotNil(t, workflowOf(m))
		require.NotNil(t, tasksOf(m))
		assert.Equal(t, promptWorkflow, frontKind(m))
		assert.Equal(t, "  Workflow › audit route handlers", dialogRows(t, m)[1])

		press(m, escape)

		assert.Nil(t, workflowOf(m))
		assert.Equal(t, promptTasks, frontKind(m))
		assert.Equal(t, "  Background tasks", dialogRows(t, m)[1])

		press(m, down)
		press(m, enter)
		assert.Nil(t, workflowOf(m), "a task that is not a workflow opens its output")
		assert.NotEmpty(t, tasksOf(m).open)
	})
}
