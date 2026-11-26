package tui

import (
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
		time.Sleep(hintDelay)
		m.arrange()
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

func TestWorkingRowShowsActivityOnlyAfterTheDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestModel(t, 40, 20)
		idle := m.transcriptHeight()
		working := func() string { s := screen(m); return s[len(s)-6] }

		_, cmd := m.Update(eventsMsg{event.Activity{Text: "Connecting to MCP servers…"}})
		require.NotNil(t, cmd)
		assert.Equal(t, idle, m.transcriptHeight())
		assert.NotContains(t, screen(m), "⠋ Connecting to MCP servers…")
		_, cmd = m.Update(m.spinner.Tick())
		require.Nil(t, cmd, "nothing ticks while the row is held back")

		time.Sleep(hintDelay)
		m.arrange()
		assert.Equal(t, idle-busyRows, m.transcriptHeight())
		assert.Equal(t, "⠋ Connecting to MCP servers…", working())
		_, cmd = m.Update(hintMsg{})
		assert.NotNil(t, cmd, "the wake starts the spinner")
	})
}

// hintWakes runs the commands cmd batches, after the bridge has an event to
// return, and reports how long after now each hintMsg arrived.
func hintWakes(t *testing.T, m *model, cmd tea.Cmd) []time.Duration {
	t.Helper()
	m.bridge.push(event.Notice{Text: "release the bridge wait"})
	// A lone command comes back unbatched, and is the bridge wait.
	batch, _ := cmd().(tea.BatchMsg)
	var waits []time.Duration
	start := time.Now()
	for _, c := range batch {
		if _, ok := c().(hintMsg); ok {
			waits = append(waits, time.Since(start))
		}
	}
	return waits
}

func TestWorkingRowWakesWhenTheActivityIsDue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestModel(t, 40, 20)
		_, cmd := m.Update(eventsMsg{event.Activity{Text: "Connecting to MCP servers…"}})
		assert.Equal(t, []time.Duration{hintDelay}, hintWakes(t, m, cmd))

		_, cmd = m.Update(eventsMsg{event.Activity{Text: "Logging in to X…"}})
		assert.Empty(t, hintWakes(t, m, cmd), "a change of text keeps the start and the wake")
	})
}

func TestWorkingRowWakesForAnActivityClearedAndSetInOneBatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestModel(t, 40, 20)
		m.Update(eventsMsg{event.Activity{Text: "Connecting to MCP servers…"}})
		time.Sleep(time.Second)

		_, cmd := m.Update(eventsMsg{event.Activity{}, event.Activity{Text: "Logging in to X…"}})
		assert.Equal(t, []time.Duration{hintDelay}, hintWakes(t, m, cmd), "the new start gets its own wake")
	})
}

func TestWorkingRowNeverDrawsAnActivityClearedBeforeTheDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestModel(t, 40, 20)
		idle := m.transcriptHeight()

		send(m, event.Activity{Text: "Connecting to MCP servers…"})
		time.Sleep(hintDelay - time.Millisecond)
		send(m, event.Activity{})
		time.Sleep(hintDelay)
		m.Update(hintMsg{})

		assert.Equal(t, idle, m.transcriptHeight())
		assert.NotContains(t, screen(m), "⠋ Connecting to MCP servers…")
	})
}

func TestWorkingRowWaitsOnBackgroundWork(t *testing.T) {
	for _, tc := range []struct {
		agents, workflows, shells int
		want                      string
	}{
		{1, 0, 0, "1 agent still running"},
		{2, 0, 0, "2 agents still running"},
		{0, 0, 1, "1 shell still running"},
		{0, 0, 3, "3 shells still running"},
		{0, 1, 0, "1 workflow still running"},
		{1, 0, 1, "1 agent, 1 shell still running"},
		{2, 0, 1, "2 agents, 1 shell still running"},
		{2, 1, 1, "2 agents, 1 workflow, 1 shell still running"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, c := newTestModel(t, 60, 20)
				c.status.Agents, c.status.Workflows, c.status.Shells = tc.agents, tc.workflows, tc.shells
				send(m, event.TasksChanged{})
				s := screen(m)
				assert.Equal(t, "⠋ "+tc.want, s[len(s)-6])
			})
		})
	}
}

func TestWorkingRowWaitingEndsWithTheTasks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := newTestModel(t, 60, 20)
		idle := m.transcriptHeight()
		working := func() string { s := screen(m); return s[len(s)-6] }

		c.status.Agents = 1
		send(m, event.TasksChanged{})
		assert.Equal(t, idle-busyRows, m.transcriptHeight())
		c.status.Agents, c.status.Shells = 2, 1
		send(m, event.TasksChanged{})
		assert.Equal(t, "⠋ 2 agents, 1 shell still running", working())

		send(m, event.TurnStart{Prompt: "go"})
		assert.Equal(t, "⠋ Working… 0s", working(), "the turn's text wins while busy")
		c.status = app.Status{Agents: 1}
		send(m, event.TurnEnd{})
		assert.Equal(t, "⠋ 1 agent still running", working())

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

		send(m, event.Activity{Text: "Connecting to MCP servers…"})
		time.Sleep(hintDelay)
		m.arrange()
		assert.Equal(t, "⠋ Connecting to MCP servers…", working())

		send(m, event.Activity{})
		assert.Equal(t, "⠋ 1 agent still running", working())
	})
}

func TestWorkingRowApprovalWinsWhileIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := newTestModel(t, 60, 30)
		c.status.Agents = 1
		send(m, event.TasksChanged{})

		send(m, event.ApprovalRequested{Token: 1, Title: "Edit a.go", Agent: "explore", TaskID: "t1"})
		assert.Contains(t, screen(m), "⠋ Waiting for you…")
		assert.NotContains(t, strings.Join(screen(m), "\n"), "still running")

		send(m, event.ApprovalEnded{Token: 1})
		assert.Contains(t, screen(m), "⠋ 1 agent still running")
	})
}

func TestWorkingRowNamesTheBackgroundKey(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := newTestModel(t, 60, 20)
		working := func() string { s := screen(m); return s[len(s)-6] }
		c.status.ForegroundSince = time.Now().Add(-hintDelay)
		send(m, event.TasksChanged{})
		assert.NotContains(t, screen(m), "⠋ Working… 0s · ctrl+b to background", "not while idle")

		send(m, event.TurnStart{Prompt: "go"})
		assert.Equal(t, "⠋ Working… 0s · ctrl+b to background", working())
		th := m.th
		assert.Contains(t, m.below[len(m.below)-6].text, fg(th.dim).Render(" · ctrl+b to background"))

		c.status.ForegroundSince = time.Time{}
		send(m, event.TasksChanged{})
		assert.Equal(t, "⠋ Working… 0s", working(), "only while a foreground command runs")

		c.status.ForegroundSince = time.Now().Add(-hintDelay)
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

func TestWorkingRowNamesTheBackgroundKeyOnlyAfterTheDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := newTestModel(t, 60, 20)
		working := func() string { s := screen(m); return s[len(s)-6] }
		c.status.ForegroundSince = time.Now()
		send(m, event.TasksChanged{}, event.TurnStart{Prompt: "go"})
		assert.Equal(t, "⠋ Working… 0s", working())

		time.Sleep(hintDelay - time.Millisecond)
		m.arrange()
		assert.NotContains(t, working(), "to background")

		time.Sleep(time.Millisecond)
		m.arrange()
		assert.Equal(t, "⠋ Working… 2s · ctrl+b to background", working())
	})
}

func TestWorkingRowHidesBackgroundKeyWhileTasksOpen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := tasksModel(t)
		c.status.ForegroundSince = time.Now().Add(-hintDelay)
		c.foreground = true
		send(m, event.TasksChanged{}, event.TurnStart{Prompt: "go"})
		assert.Contains(t, screen(m), "⠋ Working… 0s · ctrl+b to background")

		submitText(m, "/tasks")

		require.NotNil(t, tasksOf(m))
		assert.Contains(t, screen(m), "⠋ Working… 0s")
		assert.NotContains(t, screen(m), "⠋ Working… 0s · ctrl+b to background")
		press(m, ctrlB)
		assert.Zero(t, c.backgrounds, "the dialog swallows the background key")

		press(m, escape)

		require.Nil(t, tasksOf(m))
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
			c.status.ForegroundSince = time.Now().Add(-hintDelay)
			send(m, event.TasksChanged{}, event.TurnStart{Prompt: "go"})
			s := screen(m)
			assert.Equal(t, want, s[len(s)-6])
		}
	})
}

func TestWorkingRowWaitsDuringAQuestion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := newTestModel(t, 60, 30)
		c.status.ForegroundSince = time.Now().Add(-hintDelay)
		ask := event.QuestionRequested{Token: 1, Questions: []tool.Question{choice("db", false, "A", "B")}}
		send(m, event.TasksChanged{}, event.TurnStart{}, ask)
		assert.Contains(t, screen(m), "⠋ Waiting for you…", "without the background hint")
		assert.NotContains(t, screen(m), "⠋ Working… 0s")

		send(m, event.QuestionEnded{Token: 1})
		assert.Contains(t, screen(m), "⠋ Working… 0s · ctrl+b to background")
	})
}

func TestWorkingRowWaitsDuringALogin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := newTestModel(t, 60, 30)
		c.status.ForegroundSince = time.Now().Add(-hintDelay)
		send(m, event.TasksChanged{}, event.TurnStart{}, event.LoginRequested{Token: 1, Message: "Code"})
		assert.Contains(t, screen(m), "⠋ Working… 0s")
		assert.NotContains(t, screen(m), "⠋ Working… 0s · ctrl+b to background")

		send(m, event.LoginEnded{Token: 1})
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
		assert.NotContains(t, screen(m), "⠋ 1 agent still running", "waiting shows in main only")

		display(m, app.Main)
		assert.Equal(t, "⠋ 1 agent still running", working())
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
		c.status.ForegroundSince = time.Now().Add(-hintDelay)
		send(m, event.TasksChanged{}, sub("t1", event.TurnStart{Prompt: "dig"}), event.Activity{Text: "Reading a.go"})
		time.Sleep(hintDelay)
		m.arrange()

		assert.Contains(t, screen(m), "⠋ Reading a.go")
		display(m, "t1")
		assert.Contains(t, screen(m), "⠋ Working… 2s")
		assert.NotContains(t, screen(m), "Reading a.go")

		display(m, app.Main)
		send(m, event.TurnStart{Prompt: "go"})
		assert.Contains(t, screen(m), "⠋ Working… 0s · ctrl+b to background")
		display(m, "t1")
		assert.Contains(t, screen(m), "⠋ Working… 2s")
		assert.NotContains(t, screen(m), "to background")
	})
}
