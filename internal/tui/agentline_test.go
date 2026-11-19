package tui

import (
	"fmt"
	"maps"
	"slices"
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

func TestAgentSegmentsMatchConfig(t *testing.T) {
	assert.ElementsMatch(t, config.AgentSegments, slices.Collect(maps.Keys(agentSegments)))
}

// explorer is a busy subagent with a context in use.
func explorer(id string) app.AgentStatus {
	return app.AgentStatus{
		ID: id, Agent: "explorer", Description: "Find the config loader",
		Model: "mock/gpt-5.6", ContextTokens: 22_100, State: "running",
	}
}

var mainAgent = app.AgentStatus{ID: app.Main, Model: "mock/model-1", ContextTokens: 850}

// agentLineModel lists main and agents under line.
func agentLineModel(t *testing.T, width int, line config.AgentLine, agents ...app.AgentStatus) (*model, *fakeController) {
	t.Helper()
	opts := testOptions()
	opts.AgentLine = line
	m, c := newTestModelWith(t, width, 20, opts)
	c.agents = append([]app.AgentStatus{mainAgent}, agents...)
	for _, a := range agents {
		m.viewOf(a.ID).busy = a.State == tool.TaskRunning
	}
	send(m, event.TasksChanged{})
	return m, c
}

// lastRows is the screen's last n rows.
func lastRows(m *model, n int) []string {
	s := screen(m)
	return s[len(s)-n:]
}

func lineOf(left []string, right ...string) config.AgentLine {
	return config.AgentLine{Left: left, Right: right, Separator: " · "}
}

func TestAgentRowsAreAbsentWithoutASubagent(t *testing.T) {
	m, _ := agentLineModel(t, 60, config.DefaultAgentLine())
	assert.Equal(t, 20-chromeRows-m.composer.height(), m.transcriptHeight())
	assert.Equal(t, statusText(m), screen(m)[len(screen(m))-1])
	assert.NotContains(t, strings.Join(screen(m), "\n"), "main ·")
}

func TestAgentRowsFollowTheStatusLine(t *testing.T) {
	m, _ := agentLineModel(t, 70, config.DefaultAgentLine(), explorer("t1"))

	assert.Equal(t, 20-chromeRows-m.composer.height()-2, m.transcriptHeight())
	assert.Equal(t, []string{
		"  ● main · 850 tokens · model-1",
		"  ○ explorer › Find the config loader · 22.1k tokens · gpt-5.6",
	}, lastRows(m, 2))
	assert.Equal(t, statusText(m), screen(m)[len(screen(m))-3])
}

func TestAgentRowsMarkTheShownView(t *testing.T) {
	m, _ := agentLineModel(t, 60, lineOf([]string{"agent"}), explorer("t1"))

	display(m, "t1")
	assert.Equal(t, []string{"  ○ main", "  ● explorer › Find the config loader"}, lastRows(m, 2))
}

func TestAgentRowsDropAnEndedSubagentUnlessShown(t *testing.T) {
	m, c := agentLineModel(t, 60, lineOf([]string{"agent"}), explorer("t1"))
	ended := explorer("t1")
	ended.State = "completed"
	c.agents[1] = ended
	m.views["t1"].busy = false
	send(m, event.TasksChanged{})
	assert.Equal(t, statusText(m), screen(m)[len(screen(m))-1], "no rows")

	display(m, "t1")
	assert.Equal(t, []string{"  ○ main", "  ● explorer › Find the config loader"}, lastRows(m, 2))

	display(m, app.Main)
	assert.Equal(t, statusText(m), screen(m)[len(screen(m))-1])
}

func TestAgentSegment(t *testing.T) {
	for _, tc := range []struct {
		name string
		e    entry
		want string
		ok   bool
	}{
		{"main", entry{AgentStatus: app.AgentStatus{ID: app.Main}}, "main", true},
		{"described", entry{AgentStatus: app.AgentStatus{ID: "t1", Agent: "explorer", Description: "Find it"}}, "explorer › Find it", true},
		{"undescribed", entry{AgentStatus: app.AgentStatus{ID: "t1", Agent: "explorer"}}, "explorer", true},
		{"multiline description", entry{AgentStatus: app.AgentStatus{ID: "t1", Agent: "x", Description: "a\n  b"}}, "x › a b", true},
		{"exactly 36", entry{AgentStatus: app.AgentStatus{ID: "t1", Agent: "x", Description: strings.Repeat("a", 36)}}, "x › " + strings.Repeat("a", 36), true},
		{"cut at a word", entry{AgentStatus: app.AgentStatus{ID: "t1", Agent: "x", Description: "Find every place the configuration loader is called"}}, "x › Find every place the configuration…", true},
		{"cut on a space", entry{AgentStatus: app.AgentStatus{ID: "t1", Agent: "x", Description: strings.Repeat("a", 35) + " bbb"}}, "x › " + strings.Repeat("a", 35) + "…", true},
		{"cut without a word boundary", entry{AgentStatus: app.AgentStatus{ID: "t1", Agent: "x", Description: strings.Repeat("a", 40)}}, "x › " + strings.Repeat("a", 35) + "…", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := agentSegments["agent"](tc.e)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.ok, ok)
		})
	}
}

func TestTokensSegment(t *testing.T) {
	for tokens, want := range map[int]string{850: "850 tokens", 999: "999 tokens", 1000: "1.0k tokens", 22_100: "22.1k tokens", 999_949: "999.9k tokens", 999_950: "1.0m tokens", 1_200_000: "1.2m tokens"} {
		got, ok := agentSegments["tokens"](entry{AgentStatus: app.AgentStatus{ContextTokens: tokens}})
		assert.True(t, ok)
		assert.Equal(t, want, got)
	}
	_, ok := agentSegments["tokens"](entry{})
	assert.False(t, ok, "unknown")
}

func TestModelSegmentMatchesTheStatusLine(t *testing.T) {
	got, ok := agentSegments["model"](entry{AgentStatus: app.AgentStatus{Model: "mock/gpt-5.6"}})
	assert.True(t, ok)
	assert.Equal(t, modelID("mock/gpt-5.6"), got)
	_, ok = agentSegments["model"](entry{})
	assert.False(t, ok)
}

func TestStateAndIDSegments(t *testing.T) {
	state := agentSegments["state"]
	got, ok := state(entry{AgentStatus: app.AgentStatus{ID: "t1", State: "failed"}})
	assert.True(t, ok)
	assert.Equal(t, "failed", got)
	_, ok = state(entry{AgentStatus: app.AgentStatus{ID: "t1"}})
	assert.False(t, ok)
	got, ok = state(entry{AgentStatus: app.AgentStatus{ID: app.Main}, busy: true})
	assert.True(t, ok)
	assert.Equal(t, "running", got)
	_, ok = state(entry{AgentStatus: app.AgentStatus{ID: app.Main}})
	assert.False(t, ok, "main idle")

	got, ok = agentSegments["id"](entry{AgentStatus: app.AgentStatus{ID: "t1"}})
	assert.True(t, ok)
	assert.Equal(t, "t1", got)
	_, ok = agentSegments["id"](entry{AgentStatus: app.AgentStatus{ID: app.Main}})
	assert.False(t, ok, "main has no id")
}

func TestElapsedAndActivitySegmentsFollowTheirSession(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := agentLineModel(t, 60, lineOf([]string{"state", "elapsed", "activity", "id"}), explorer("t1"))
		assert.Equal(t, []string{"  ●", "  ○ running · t1"}, lastRows(m, 2))

		send(m, sub("t1", event.TurnStart{Prompt: "dig"}), sub("t1", event.ToolStart{CallID: "a", Title: "Read a.go"}))
		time.Sleep(72 * time.Second)
		m.arrange()
		assert.Equal(t, []string{"  ●", "  ○ running · 1m 12s · Read a.go · t1"}, lastRows(m, 2))

		send(m, sub("t1", event.ToolEnd{CallID: "a"}), event.TurnStart{Prompt: "go"})
		time.Sleep(3 * time.Second)
		m.arrange()
		assert.Equal(t, []string{"  ● running · 3s", "  ○ running · 1m 15s · t1"}, lastRows(m, 2))
	})
}

func TestMainRowShowsRunningFromTurnStartToTurnEnd(t *testing.T) {
	m, _ := agentLineModel(t, 60, lineOf([]string{"agent", "state"}), explorer("t1"))
	assert.Equal(t, "  ● main", lastRows(m, 2)[0])

	send(m, event.TurnStart{Prompt: "go"})
	assert.Equal(t, "  ● main · running", lastRows(m, 2)[0])

	send(m, event.TurnEnd{})
	assert.Equal(t, "  ● main", lastRows(m, 2)[0])
}

func TestAgentRowsShrinkToTheHeightThatIsLeft(t *testing.T) {
	var agents []app.AgentStatus
	for _, id := range []string{"t1", "t2", "t3", "t4", "t5", "t6", "t7"} {
		agents = append(agents, explorer(id))
	}
	// short is a screen with room for free agent row lines under the working row.
	short := func(free int) *model {
		opts := testOptions()
		opts.AgentLine = lineOf([]string{"id"})
		probe, _ := newTestModelWith(t, 40, 20, opts)
		height := chromeRows + probe.composer.height() + busyRows + free
		m, c := newTestModelWith(t, 40, height, opts)
		c.agents = append([]app.AgentStatus{mainAgent}, agents...)
		for _, a := range agents {
			m.viewOf(a.ID).busy = true
		}
		send(m, event.TasksChanged{}, event.TurnStart{Prompt: "go"})
		require.Len(t, screen(m), height)
		return m
	}

	m := short(4)
	assert.Equal(t, []string{"  ●", "  ○ t1", "  ○ t2", "  ↓ 5 more"}, lastRows(m, 4))
	assert.Contains(t, screen(m)[len(screen(m))-5], "plan", "the status line")

	display(m, "t6")
	assert.Len(t, screen(m), m.height)
	assert.Equal(t, []string{"  ↑ 5 more", "  ○ t5", "  ● t6", "  ○ t7"}, lastRows(m, 4))

	m = short(0)
	assert.Zero(t, m.geo.agents)
	assert.Contains(t, screen(m)[len(screen(m))-1], "plan", "the status line")

	m = short(1)
	assert.Zero(t, m.geo.agents, "one line holds no entry with its more row")

	m = short(2)
	assert.Equal(t, []string{"  ●", "  ↓ 7 more"}, lastRows(m, 2))

	m = short(20)
	assert.Equal(t, maxAgentRows+1, m.geo.agents)
}

func TestAgentRowsTakeSpaceOnlyFromTheTranscript(t *testing.T) {
	var agents []app.AgentStatus
	for _, id := range []string{"t1", "t2", "t3", "t4", "t5", "t6", "t7"} {
		agents = append(agents, explorer(id))
	}
	build := func(height int, agents []app.AgentStatus, events ...event.Event) *model {
		opts := testOptions()
		opts.AgentLine = lineOf([]string{"id"})
		m, c := newTestModelWith(t, 40, height, opts)
		c.agents = append([]app.AgentStatus{mainAgent}, agents...)
		for _, a := range agents {
			m.viewOf(a.ID).busy = true
		}
		send(m, append([]event.Event{event.TasksChanged{}}, events...)...)
		require.Len(t, screen(m), height)
		return m
	}
	approval := event.ApprovalRequested{Token: 1, CallID: "c1", Title: "Write a.go"}
	queued := event.QueueChanged{Pending: []string{"first", "second"}}
	for _, height := range []int{12, 16, 20} {
		for name, events := range map[string][]event.Event{"dialog": {approval}, "queued rows": {event.TurnStart{Prompt: "go"}, queued}} {
			t.Run(fmt.Sprintf("%s at %d", name, height), func(t *testing.T) {
				without := build(height, nil, events...)
				with := build(height, agents, events...)

				assert.Equal(t, without.prompt, with.prompt)
				assert.Equal(t, without.pending, with.pending)
				assert.Equal(t, without.geo.transcript, with.geo.transcript+with.geo.agents, "the rows used only what the transcript had")
				assert.LessOrEqual(t, with.geo.agents, without.geo.transcript)
			})
		}
	}
}

func TestAgentRowsWindowFollowsTheShownRow(t *testing.T) {
	var agents []app.AgentStatus
	for _, id := range []string{"t1", "t2", "t3", "t4", "t5", "t6", "t7"} {
		agents = append(agents, explorer(id))
	}
	m, _ := agentLineModel(t, 40, lineOf([]string{"id"}), agents...)

	assert.Equal(t, []string{"  ●", "  ○ t1", "  ○ t2", "  ○ t3", "  ○ t4", "  ↓ 3 more"}, lastRows(m, 6))

	display(m, "t7")
	assert.Equal(t, []string{"  ↑ 3 more", "  ○ t3", "  ○ t4", "  ○ t5", "  ○ t6", "  ● t7"}, lastRows(m, 6))
}

func TestAgentRowsWindowFollowsTheCursor(t *testing.T) {
	var agents []app.AgentStatus
	for _, id := range []string{"t1", "t2", "t3", "t4", "t5", "t6", "t7"} {
		agents = append(agents, explorer(id))
	}
	m, _ := agentLineModel(t, 40, lineOf([]string{"id"}), agents...)
	press(m, down)
	for range 4 {
		press(m, down)
	}

	assert.Equal(t, "t4", m.focus.id)
	assert.Equal(t, []string{"  ↑ 2 more", "  ○ t2", "  ○ t3", "  ❭ t4", "  ○ t5", "  ○ t6", "  ↓ 1 more"}, lastRows(m, 7))
}

func TestAgentRowFitsTheWidth(t *testing.T) {
	for _, tc := range []struct {
		name  string
		width int
		line  config.AgentLine
		want  []string
	}{
		{"drops left segments from the right end", 40, config.DefaultAgentLine(), []string{"  ● main · 850 tokens · model-1", "  ○ explorer › Find the config loader"}},
		{"keeps what fits", 60, config.DefaultAgentLine(), []string{"  ● main · 850 tokens · model-1", "  ○ explorer › Find the config loader · 22.1k tokens"}},
		{"aligns the right segments", 60, lineOf([]string{"agent"}, "model"), []string{"  ● main" + strings.Repeat(" ", 43) + "model-1", "  ○ explorer › Find the config loader" + strings.Repeat(" ", 14) + "gpt-5.6"}},
		{"drops left segments for the right ones", 28, lineOf([]string{"agent", "tokens"}, "model"), []string{"  ● main" + strings.Repeat(" ", 11) + "model-1", "  ○" + strings.Repeat(" ", 16) + "gpt-5.6"}},
		{"truncates to the width", 10, lineOf([]string{"agent"}, "model"), []string{"  ●  model", "  ○  gpt-5"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := agentLineModel(t, tc.width, tc.line, explorer("t1"))
			got := lastRows(m, 2)
			assert.Equal(t, tc.want, got)
			for _, r := range got {
				assert.LessOrEqual(t, lipgloss.Width(r), tc.width)
			}
		})
	}
}

// focused has the rows focused with the cursor on main.
func focused(t *testing.T, agents ...app.AgentStatus) (*model, *fakeController) {
	t.Helper()
	m, c := agentLineModel(t, 60, lineOf([]string{"agent"}), agents...)
	press(m, down)
	require.True(t, m.focus.on)
	return m, c
}

var keyX = tea.KeyPressMsg{Code: 'x', Text: "x"}

func TestDownAtTheBottomEntersTheRowsAtMain(t *testing.T) {
	m, _ := focused(t, explorer("t1"))
	assert.Equal(t, app.Main, m.focus.id)
	assert.Equal(t, []string{"  ❭ main", "  ○ explorer › Find the config loader"}, lastRows(m, 2))
}

func TestDownDoesNotEnterWhileItCanStillMove(t *testing.T) {
	m, _ := agentLineModel(t, 60, lineOf([]string{"agent"}), explorer("t1"))
	typeText(m, "a\nb")
	press(m, up)
	press(m, down)
	assert.False(t, m.focus.on, "it moved down a line")
	press(m, down)
	assert.True(t, m.focus.on)
}

func TestDownDoesNotEnterWhileANewerHistoryEntryExists(t *testing.T) {
	m, _ := agentLineModel(t, 60, lineOf([]string{"agent"}), explorer("t1"))
	m.composer.setHistory([]string{"old"})
	press(m, up)
	require.Equal(t, "old", m.composer.value())
	press(m, down)
	assert.False(t, m.focus.on, "it went to the newer entry")
	assert.Empty(t, m.composer.value())
	press(m, down)
	assert.True(t, m.focus.on)
}

func TestDownDoesNotEnterWithoutRows(t *testing.T) {
	m, _ := agentLineModel(t, 60, config.DefaultAgentLine())
	press(m, down)
	assert.False(t, m.focus.on)
}

func TestFocusedKeysMoveAndClamp(t *testing.T) {
	m, _ := focused(t, explorer("t1"))

	press(m, down)
	assert.Equal(t, "t1", m.focus.id)
	press(m, down)
	assert.Equal(t, "t1", m.focus.id, "stops at the last entry")
	press(m, up)
	assert.Equal(t, app.Main, m.focus.id)
	assert.True(t, m.focus.on)
	press(m, up)
	assert.False(t, m.focus.on, "up on the first entry leaves")
}

func TestFocusedSubmitShowsTheEntry(t *testing.T) {
	m, c := focused(t, explorer("t1"))
	press(m, down)

	press(m, enter)

	assert.Equal(t, "t1", m.shown)
	assert.False(t, m.focus.on)
	assert.Empty(t, c.submitted)
	assert.Equal(t, []string{"  ○ main", "  ● explorer › Find the config loader"}, lastRows(m, 2))
}

func TestFocusedStopInterruptsABusyRowThatIsNotShown(t *testing.T) {
	m, c := focused(t, explorer("t1"))
	press(m, down)

	press(m, keyX)

	assert.Equal(t, []string{"interrupt"}, c.calls)
	assert.Equal(t, []string{"t1"}, c.sessions)
	assert.True(t, m.focus.on)
	assert.Equal(t, "t1", m.focus.id)
}

func TestFocusedStopOnMainAndOnTheShownRowTypes(t *testing.T) {
	m, c := focused(t, explorer("t1"))
	press(m, keyX)
	assert.Equal(t, "x", m.composer.value())
	assert.False(t, m.focus.on)
	assert.Empty(t, c.calls)

	m, c = focused(t, explorer("t1"))
	display(m, "t1")
	press(m, down)
	require.Equal(t, "t1", m.focus.id)
	press(m, keyX)
	assert.Equal(t, "x", m.composer.value())
	assert.False(t, m.focus.on)
	assert.Empty(t, c.calls)
}

func TestFocusedEscapeLeavesWithoutInterrupting(t *testing.T) {
	m, c := focused(t, explorer("t1"))
	press(m, escape)
	assert.False(t, m.focus.on)
	assert.Empty(t, c.calls)
}

func TestFocusedOtherKeyLeavesAndTypes(t *testing.T) {
	m, _ := focused(t, explorer("t1"))
	press(m, tea.KeyPressMsg{Code: 'h', Text: "h"})
	assert.False(t, m.focus.on)
	assert.Equal(t, "h", m.composer.value())
}

func TestFocusClearsWhenADialogOpens(t *testing.T) {
	m, _ := focused(t, explorer("t1"))
	send(m, event.ApprovalRequested{Token: 1, CallID: "c1", Title: "Write a.go"})
	assert.False(t, m.focus.on)
}

func TestFocusClearsOnSessionChanged(t *testing.T) {
	m, _ := focused(t, explorer("t1"))
	send(m, event.SessionChanged{SessionID: "s2"})
	assert.False(t, m.focus.on)
}

func TestFocusClearsWhenTheRowsVanish(t *testing.T) {
	m, c := focused(t, explorer("t1"))
	c.agents = c.agents[:1]
	send(m, event.TasksChanged{})
	assert.False(t, m.focus.on)
}

func TestFocusMovesToTheNearestEntryWhenItsOwnVanishes(t *testing.T) {
	m, _ := focused(t, explorer("t1"), explorer("t2"), explorer("t3"))
	press(m, down)
	press(m, down)
	require.Equal(t, "t2", m.focus.id)

	m.views["t2"].busy = false
	send(m, event.TasksChanged{})

	assert.True(t, m.focus.on)
	assert.Equal(t, "t3", m.focus.id)
	assert.Equal(t, 2, m.focus.at)
}

func TestElapsedKeepsTheTickRunningInAnIdleView(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := agentLineModel(t, 60, lineOf([]string{"agent", "elapsed"}), explorer("t1"))
		send(m, sub("t1", event.TurnStart{Prompt: "dig"}))

		_, cmd := m.Update(m.spinner.Tick())
		assert.NotNil(t, cmd, "a busy entry shows a clock")

		send(m, sub("t1", event.TurnEnd{}))
		_, cmd = m.Update(m.spinner.Tick())
		assert.Nil(t, cmd)
	})
}

func TestWithoutElapsedAnIdleViewDoesNotTick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := agentLineModel(t, 60, lineOf([]string{"agent"}), explorer("t1"))
		send(m, sub("t1", event.TurnStart{Prompt: "dig"}))

		_, cmd := m.Update(m.spinner.Tick())
		assert.Nil(t, cmd)
	})
}

func TestElapsedStartsTheTickWhenAClockAppears(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := agentLineModel(t, 60, lineOf([]string{"agent", "elapsed"}), explorer("t1"))

		cmd := m.applyBatch(eventsMsg{sub("t1", event.TurnStart{Prompt: "dig"})})

		assert.NotNil(t, cmd)
		assert.True(t, m.tickWanted())
	})
}
