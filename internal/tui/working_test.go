package tui

import (
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

func TestIdleLayout(t *testing.T) {
	m, _ := newTestModel(t, 40, 20)
	typeText(m, "hi")
	s := screen(m)

	assert.Len(t, s, 20)
	assert.Equal(t, 20-1-chromeRows, m.transcriptHeight())
	h := m.transcriptHeight()
	assert.Equal(t, []string{"", "", "❯ hi", ""}, s[h:h+4], "blank, shaded, composer, shaded")
	assert.Equal(t, h+2, m.composerTop())
	assert.Contains(t, s[len(s)-1], "plan")
}

func TestWorkingRowShowsWhileTurnRuns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestModel(t, 40, 20)
		idle := m.transcriptHeight()
		working := func() string { s := screen(m); return s[len(s)-6] }

		send(m, event.TurnStart{Prompt: "go"})
		assert.Equal(t, "⠋ Working… 0s", working())
		assert.Equal(t, idle-busyRows, m.transcriptHeight())
		assert.Len(t, screen(m), 20)
		assert.Empty(t, screen(m)[len(screen(m))-7], "a blank row sits above the working row")
		assert.Empty(t, screen(m)[len(screen(m))-5], "and another below it")
		assert.Equal(t, "❯", screen(m)[m.composerTop()])

		time.Sleep(72 * time.Second)
		_, cmd := m.Update(m.spinner.Tick())
		assert.NotNil(t, cmd, "the spinner keeps ticking while busy")
		assert.Equal(t, "⠙ Working… 1m 12s", working())

		send(m, event.TurnEnd{Elapsed: 72 * time.Second})
		assert.Equal(t, idle, m.transcriptHeight())
		assert.Len(t, screen(m), 20)
		assert.NotContains(t, screen(m), "⠙ Working… 1m 12s")
		assert.Equal(t, "❯", screen(m)[m.composerTop()])
		_, cmd = m.Update(m.spinner.Tick())
		require.Nil(t, cmd, "ticking stops once idle")
	})
}

func TestWorkingRowShowsActivity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestModel(t, 40, 20)
		idle := m.transcriptHeight()
		working := func() string { s := screen(m); return s[len(s)-6] }

		send(m, event.Activity{Text: "Connecting to MCP servers…"})
		assert.Equal(t, "⠋ Connecting to MCP servers…", working())
		assert.Equal(t, idle-busyRows, m.transcriptHeight())
		_, cmd := m.Update(m.spinner.Tick())
		assert.NotNil(t, cmd, "the spinner ticks during the activity")

		send(m, event.TurnStart{Prompt: "go"})
		assert.Equal(t, "⠙ Working… 0s", working(), "a running turn's text wins")
		send(m, event.TurnEnd{})
		assert.Equal(t, "⠙ Connecting to MCP servers…", working())

		send(m, event.Activity{})
		assert.Equal(t, idle, m.transcriptHeight())
		_, cmd = m.Update(m.spinner.Tick())
		require.Nil(t, cmd, "ticking stops once the activity ends")
	})
}

func TestWorkingRowWaitsOnBackgroundWork(t *testing.T) {
	for _, tc := range []struct {
		agents, shells int
		want           string
	}{
		{1, 0, "Waiting on 1 agent… 0s"},
		{2, 0, "Waiting on 2 agents… 0s"},
		{0, 1, "Waiting on 1 shell… 0s"},
		{0, 3, "Waiting on 3 shells… 0s"},
		{1, 1, "Waiting on 1 agent, 1 shell… 0s"},
		{2, 1, "Waiting on 2 agents, 1 shell… 0s"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, c := newTestModel(t, 60, 20)
				c.status.Agents, c.status.Shells = tc.agents, tc.shells
				send(m, event.TasksChanged{})
				s := screen(m)
				assert.Equal(t, "⠋ "+tc.want, s[len(s)-6])
			})
		})
	}
}

func TestWorkingRowWaitingCountsFromWhenItBegan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := newTestModel(t, 60, 20)
		idle := m.transcriptHeight()
		working := func() string { s := screen(m); return s[len(s)-6] }

		c.status.Agents = 1
		send(m, event.TasksChanged{})
		assert.Equal(t, idle-busyRows, m.transcriptHeight())
		time.Sleep(30 * time.Second)
		c.status.Agents, c.status.Shells = 2, 1
		send(m, event.TasksChanged{})
		assert.Equal(t, "⠋ Waiting on 2 agents, 1 shell… 30s", working(), "a count change keeps the clock")

		send(m, event.TurnStart{Prompt: "go"})
		assert.Equal(t, "⠋ Working… 0s", working(), "the turn's text wins while busy")
		time.Sleep(10 * time.Second)
		c.status = app.Status{Agents: 1}
		send(m, event.TurnEnd{})
		assert.Equal(t, "⠋ Waiting on 1 agent… 0s", working(), "the clock restarts after the turn")

		c.status = app.Status{}
		send(m, event.TasksChanged{})
		assert.Equal(t, idle, m.transcriptHeight())
		_, cmd := m.Update(m.spinner.Tick())
		require.Nil(t, cmd, "ticking stops with the last task")
	})
}

func TestWorkingRowActivityWinsOverWaiting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := newTestModel(t, 60, 20)
		working := func() string { s := screen(m); return s[len(s)-6] }
		c.status.Agents = 1
		send(m, event.TasksChanged{})
		time.Sleep(20 * time.Second)

		send(m, event.Activity{Text: "Connecting to MCP servers…"})
		assert.Equal(t, "⠋ Connecting to MCP servers…", working())

		send(m, event.Activity{})
		assert.Equal(t, "⠋ Waiting on 1 agent… 20s", working(), "the clock kept running")
	})
}

func TestWorkingRowApprovalWinsWhileIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := newTestModel(t, 60, 30)
		c.status.Agents = 1
		send(m, event.TasksChanged{})

		send(m, event.ApprovalRequested{Token: 1, Title: "Edit a.go", Agent: "explore", TaskID: "t1"})
		assert.Contains(t, screen(m), "⠋ Waiting for you…")
		assert.NotContains(t, strings.Join(screen(m), "\n"), "Waiting on")

		send(m, event.ApprovalEnded{Token: 1})
		assert.Contains(t, screen(m), "⠋ Waiting on 1 agent… 0s")
	})
}

func TestWorkingRowNamesTheBackgroundKey(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := newTestModel(t, 60, 20)
		working := func() string { s := screen(m); return s[len(s)-6] }
		c.status.Foreground = true
		send(m, event.TasksChanged{})
		assert.NotContains(t, screen(m), "⠋ Working… 0s · ctrl+b to background", "not while idle")

		send(m, event.TurnStart{Prompt: "go"})
		assert.Equal(t, "⠋ Working… 0s · ctrl+b to background", working())
		th := m.th
		assert.Contains(t, m.below[len(m.below)-6].text, fg(th.dim).Render(" · ctrl+b to background"))

		c.status.Foreground = false
		send(m, event.TasksChanged{})
		assert.Equal(t, "⠋ Working… 0s", working(), "only while a foreground command runs")

		c.status.Foreground = true
		ask := editApproval()
		send(m, event.TasksChanged{}, ask)
		assert.Contains(t, screen(m), "⠋ Waiting for you…", "not while an approval waits")
		send(m, event.ApprovalEnded{Token: ask.Token})
		assert.Equal(t, "⠋ Working… 0s · ctrl+b to background", working())

		send(m, event.PlanRequested{Token: 9, Plan: "# plan"})
		assert.Contains(t, screen(m), "⠋ Waiting for you…", "not while a plan waits")
		assert.NotContains(t, screen(m), "to background")
		send(m, event.PlanEnded{Token: 9})
		assert.Equal(t, "⠋ Working… 0s · ctrl+b to background", working())
	})
}

func TestWorkingRowHidesBackgroundKeyWhileTasksOpen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := tasksModel(t)
		c.status.Foreground = true
		c.foreground = true
		send(m, event.TasksChanged{}, event.TurnStart{Prompt: "go"})
		assert.Contains(t, screen(m), "⠋ Working… 0s · ctrl+b to background")

		submitText(m, "/tasks")

		require.NotNil(t, m.tasks)
		assert.Contains(t, screen(m), "⠋ Working… 0s")
		assert.NotContains(t, screen(m), "⠋ Working… 0s · ctrl+b to background")
		press(m, ctrlB)
		assert.Zero(t, c.backgrounds, "the dialog swallows the background key")

		press(m, escape)

		require.Nil(t, m.tasks)
		assert.Contains(t, screen(m), "⠋ Working… 0s · ctrl+b to background")
		press(m, ctrlB)
		assert.Equal(t, 1, c.backgrounds, "the background key works again after closing")
	})
}

func TestWorkingRowUsesTheConfiguredBackgroundKey(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for keys, want := range map[string]string{"ctrl+g": "⠋ Working… 0s · ctrl+g to background", "": "⠋ Working… 0s"} {
			opts := testOptions()
			opts.Keys["background"] = nil
			if keys != "" {
				opts.Keys["background"] = []string{keys}
			}
			m, c := newTestModelWith(t, 60, 20, opts)
			c.status.Foreground = true
			send(m, event.TasksChanged{}, event.TurnStart{Prompt: "go"})
			s := screen(m)
			assert.Equal(t, want, s[len(s)-6])
		}
	})
}

func TestWorkingRowWaitsDuringAQuestion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := newTestModel(t, 60, 30)
		c.status.Foreground = true
		ask := event.QuestionRequested{Token: 1, Questions: []tool.Question{choice("db", false, "A", "B")}}
		send(m, event.TasksChanged{}, event.TurnStart{}, ask)
		assert.Contains(t, screen(m), "⠋ Waiting for you…", "without the background hint")
		assert.NotContains(t, screen(m), "⠋ Working… 0s")

		send(m, event.QuestionEnded{Token: 1})
		assert.Contains(t, screen(m), "⠋ Working… 0s · ctrl+b to background")
	})
}

func TestWorkingRowFollowsTheView(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := agentsModel(t)
		send(m, sub("t1", event.TurnStart{Prompt: "dig"}))
		working := func() string { s := screen(m); return s[len(s)-6-m.geo.agents] }
		time.Sleep(5 * time.Second)
		assert.NotContains(t, screen(m), "⠋ Working… 5s", "main is idle")

		cmd := display(m, "t1")
		assert.NotNil(t, cmd, "the spinner ticks for the new view")
		assert.Equal(t, "⠋ Working… 5s", working())

		send(m, sub("t1", event.TurnEnd{}))
		c.status.Agents = 1
		send(m, event.TasksChanged{})
		assert.NotContains(t, screen(m), "⠋ Waiting on 1 agent… 0s", "waiting shows in main only")

		display(m, app.Main)
		assert.Equal(t, "⠋ Waiting on 1 agent… 0s", working())
	})
}

func TestWorkingRowKeepsMainsTurnAcrossSessionChanged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestModel(t, 60, 20)
		send(m, event.TurnStart{Prompt: "go"})

		send(m, event.SessionChanged{SessionID: "s2"})

		assert.True(t, m.views[app.Main].busy)
		assert.Contains(t, screen(m), "⠋ Working… 0s")

		send(m, event.TurnEnd{})
		assert.False(t, m.views[app.Main].busy)
		assert.NotContains(t, screen(m), "⠋ Working… 0s")
	})
}

func TestWorkingRowInASubagentViewHidesMainsActivityAndBackgroundHint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := agentsModel(t)
		c.status.Foreground = true
		send(m, event.TasksChanged{}, sub("t1", event.TurnStart{Prompt: "dig"}), event.Activity{Text: "Reading a.go"})

		assert.Contains(t, screen(m), "⠋ Reading a.go")
		display(m, "t1")
		assert.Contains(t, screen(m), "⠋ Working… 0s")
		assert.NotContains(t, screen(m), "Reading a.go")

		display(m, app.Main)
		send(m, event.TurnStart{Prompt: "go"})
		assert.Contains(t, screen(m), "⠋ Working… 0s · ctrl+b to background")
		display(m, "t1")
		assert.Contains(t, screen(m), "⠋ Working… 0s")
		assert.NotContains(t, screen(m), "to background")
	})
}
