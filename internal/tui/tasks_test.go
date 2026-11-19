package tui

import (
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/app"
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

// dialogRows is the screen from the dialog's top rule down to the status line,
// which it leaves out.
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
	return s[top : len(s)-1]
}

func TestTasksWithNoTasksFlashesAndOpensNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := newTestModel(t, 60, 30)
		c.submitErr = app.ErrShowTasks

		assert.NotNil(t, submitText(m, "/tasks"), "the flash expires")

		assert.Nil(t, m.tasks)
		assert.Empty(t, m.composer.value())
		assert.Contains(t, statusText(m), "no background tasks")
		assert.True(t, strings.HasPrefix(screen(m)[m.composerTop()], "❯"))
	})
}

func TestTasksOpensTheListAboveTheStatusLine(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := tasksModel(t)
		typeText(m, "/tasks")
		assert.Nil(t, press(m, enter))

		rule := strings.Repeat("─", 60)
		assert.Equal(t, []string{
			rule,
			"  Background tasks",
			"",
			"❭ 1. t1 · running · 1m 2s · sleep 30",
			"  2. t2 · completed · 5s · make build",
			rule,
		}, dialogRows(t, m))
		assert.Empty(t, m.composer.value(), "the draft is accepted")
		s := screen(m)
		assert.Len(t, s, 30)
		assert.Contains(t, s[len(s)-1], "plan", "the status line stays last")
		assert.Equal(t, m.height, m.transcriptHeight()+len(m.below))
	})
}

func TestTasksListCutsLongCommands(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := tasksModel(t)
		c.tasks[1].Command = strings.Repeat("x", 100)
		submitText(m, "/tasks")

		row := dialogRows(t, m)[4]
		assert.Equal(t, "  2. t2 · completed · 5s · "+strings.Repeat("x", 32)+"…", row)
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
		assert.Equal(t, "❭ 1. t1 · running · 1m 2s · sleep 30", rows[3])
		assert.Equal(t, "  2. t2 · completed · 5s · explore › look around", rows[4])
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
		want := []string{rule, "  t1 · running · 1m 2s · explore › look around", "    one", "    two", "", "❭ 1. Stop", "  2. Back", rule}
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
		require.NotNil(t, m.tasks)

		press(m, enter)

		assert.Nil(t, m.tasks)
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
		require.NotNil(t, m.tasks)
		assert.Empty(t, m.tasks.open)
		assert.Equal(t, "t2", m.tasks.list[m.tasks.d.sel].ID)
		assert.Equal(t, tool.TaskStopped, m.tasks.list[m.tasks.d.sel].Status)
		assert.NotContains(t, strings.Join(screen(m), "\n"), "to stop agent")
		assert.Equal(t, app.Main, m.shown)
		assert.Zero(t, c.interrupts, "list stopping uses StopTask, not Interrupt")

		press(m, keyX)
		assert.Equal(t, []string{"t2"}, c.stops, "an ended agent is not stopped again")
		press(m, enter)
		assert.Nil(t, m.tasks)
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
		assert.Empty(t, m.tasks.open)
		assert.Equal(t, "t2", m.tasks.list[m.tasks.d.sel].ID)
		assert.NotContains(t, strings.Join(screen(m), "\n"), "to stop agent")
	})
}

func TestTasksListStopAgentLeavesShellControlsUnchanged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := tasksModel(t)
		submitText(m, "/tasks")
		press(m, keyX)
		assert.Empty(t, c.stops)
		assert.Empty(t, m.tasks.open)
		assert.NotContains(t, strings.Join(screen(m), "\n"), "to stop agent")
		press(m, enter)
		assert.Equal(t, "t1", m.tasks.open)
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
		want := []string{rule, "  t1 · running · 1m 2s · sleep 30", "    … 16 earlier lines"}
		for i := 17; i <= 25; i++ {
			want = append(want, fmt.Sprintf("    line %d", i))
		}
		want = append(want, "", "❭ 1. Stop", "  2. Back", rule)
		assert.Equal(t, want, dialogRows(t, m))
		assert.Len(t, screen(m), 30)
	})
}

func TestTasksOpenFitsWholeOutput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := tasksModel(t)
		submitText(m, "/tasks")

		press(m, digit('2'))

		rule := strings.Repeat("─", 60)
		assert.Equal(t, []string{rule, "  t2 · completed · 5s · make build", "    built", "", "❭ 1. Back", rule}, dialogRows(t, m), "an ended task has no Stop")
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
		assert.Equal(t, "  t1 · stopped · 1m 2s · sleep 30", rows[1])
		assert.Equal(t, []string{"❭ 1. Back", strings.Repeat("─", 60)}, rows[len(rows)-2:])
	})
}

func TestTasksBackAndInterruptNavigate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := tasksModel(t)
		submitText(m, "/tasks")

		press(m, down)
		press(m, enter)
		assert.Equal(t, "  t2 · completed · 5s · make build", dialogRows(t, m)[1])
		press(m, enter)
		assert.Equal(t, "❭ 2. t2 · completed · 5s · make build", dialogRows(t, m)[4], "Back returns with the task selected")

		press(m, digit('1'))
		press(m, down)
		press(m, enter)
		assert.Equal(t, "  Background tasks", dialogRows(t, m)[1], "Back is the second option for a running task")
		assert.Empty(t, c.stops)

		press(m, enter)
		press(m, escape)
		assert.Equal(t, "❭ 1. t1 · running · 1m 2s · sleep 30", dialogRows(t, m)[3], "interrupt goes back to the list")

		press(m, escape)
		assert.Nil(t, m.tasks)
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

		assert.NotNil(t, m.tasks)
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

func TestApprovalAndSessionChangeCloseTasks(t *testing.T) {
	for _, ev := range []event.Event{editApproval(), event.SessionChanged{SessionID: "s2"}} {
		synctest.Test(t, func(t *testing.T) {
			m, _ := tasksModel(t)
			submitText(m, "/tasks")

			send(m, ev)

			assert.Nil(t, m.tasks)
			assert.NotContains(t, screen(m), "  Background tasks")
		})
	}
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
		assert.Equal(t, []string{"  1. t0 · failed · 0s · true", "  2. t1 · running · 1m 2s · sleep 30", "❭ 3. t2 · completed · 5s · make build"}, rows[3:6])
	})
}

func TestTasksOverflowKeepsTheSelectedOptionVisible(t *testing.T) {
	for _, size := range []struct{ width, height int }{{60, 30}, {40, 15}, {40, 10}} {
		t.Run(fmt.Sprintf("%dx%d", size.width, size.height), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, c := tasksModel(t)
				c.tasks = nil
				for i := 1; i <= 40; i++ {
					c.tasks = append(c.tasks, tool.TaskInfo{ID: fmt.Sprintf("t%d", i), Command: "true", Started: time.Now(), Ended: time.Now(), Status: tool.TaskCompleted})
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
					assert.Contains(t, rows, fmt.Sprintf("❭ %d. t%d · completed · 0s · true", number, number))
					s := screen(m)
					assert.Len(t, s, m.height)
					assert.Contains(t, s[len(s)-1], "plan")
					return rows
				}
				rows := check(1)
				assert.Equal(t, "❭ 1. t1 · completed · 0s · true", rows[3])
				assert.NotContains(t, rows, "  40. t40 · completed · 0s · true")

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
				assert.Equal(t, "t20", m.tasks.open, "Enter opens the visible global selection")
				press(m, escape)
				check(20)

				c.tasks = append([]tool.TaskInfo{{ID: "t0", Command: "true", Started: time.Now(), Ended: time.Now(), Status: tool.TaskCompleted}}, c.tasks...)
				send(m, event.TasksChanged{})
				assert.Equal(t, "t20", m.tasks.list[m.tasks.d.sel].ID)
				assert.Contains(t, dialogRows(t, m), "❭ 21. t20 · completed · 0s · true")
				for _, height := range []int{10, 30, 15} {
					m.Update(tea.WindowSizeMsg{Width: size.width, Height: height})
					assert.Equal(t, 20, m.tasks.d.sel)
					rows := dialogRows(t, m)
					assert.Contains(t, rows, "❭ 21. t20 · completed · 0s · true")
					assert.Equal(t, strings.Repeat("─", m.width), rows[0])
					assert.Equal(t, "  Background tasks", rows[1])
					assert.Equal(t, rows[0], rows[len(rows)-1])
					assert.Len(t, screen(m), height)
					assert.Contains(t, screen(m)[height-1], "plan")
				}

				press(m, digit('2'))
				assert.Equal(t, "t1", m.tasks.open, "digits still pick global options, not visible row numbers")
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
		assert.Equal(t, []string{"  t1 · failed · 1m 2s · sleep 30", "    boom", "", "❭ 1. Back"}, rows[1:5])
	})
}
