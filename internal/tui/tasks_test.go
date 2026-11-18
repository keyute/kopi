package tui

import (
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

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
