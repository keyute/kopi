package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/config"
	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/tool"
)

// recordRaw replaces the model's raw function with one that records what it is
// given, so nothing is written.
func recordRaw(m *model) *[]any {
	var got []any
	m.raw = func(v any) tea.Cmd {
		got = append(got, v)
		return nil
	}
	return &got
}

func state(m *model) tea.ProgramState { return m.programStatus().State }

var (
	turnStart = event.TurnStart{Prompt: "go"}
	turnEnd   = func(s event.Status) event.TurnEnd { return event.TurnEnd{Outcome: event.Outcome{Status: s}} }
)

func awayModel(t *testing.T) (*model, *[]any) {
	t.Helper()
	m, _ := newTestModel(t, 60, 20)
	raw := recordRaw(m)
	m.Update(tea.BlurMsg{})
	return m, raw
}

func TestProgramStatusStates(t *testing.T) {
	blocked := func(kind tea.ProgramStatusKind) tea.ProgramStatus {
		return tea.ProgramStatus{App: "kopi", State: tea.ProgramStateBlocked, Kind: kind}
	}
	for _, tc := range []struct {
		name string
		set  func(t *testing.T) *model
		want tea.ProgramStatus
	}{
		{"idle", func(t *testing.T) *model { m, _ := newTestModel(t, 60, 20); return m }, tea.ProgramStatus{App: "kopi", State: tea.ProgramStateIdle}},
		{"main turn", func(t *testing.T) *model {
			m, _ := newTestModel(t, 60, 20)
			send(m, turnStart)
			return m
		}, tea.ProgramStatus{App: "kopi", State: tea.ProgramStateWorking}},
		{"approval", func(t *testing.T) *model {
			m, _ := newTestModel(t, 60, 20)
			send(m, editApproval())
			return m
		}, blocked(tea.ProgramStatusKindPermission)},
		{"plan", func(t *testing.T) *model { m, _ := openPlan(t); return m }, blocked(tea.ProgramStatusKindPermission)},
		{"question", func(t *testing.T) *model {
			m, _ := openQuestion(t, choice("H", false, "a", "b"))
			return m
		}, blocked(tea.ProgramStatusKindQuestion)},
		{"login", func(t *testing.T) *model {
			m, _ := openLogin(t, event.LoginRequested{Token: 1, Message: "API key"})
			return m
		}, blocked(tea.ProgramStatusKindAuth)},
		{"tasks dialog is not blocked", func(t *testing.T) *model {
			m, _ := tasksModel(t)
			typeText(m, "/tasks")
			press(m, enter)
			require.NotNil(t, m.front())
			assert.Equal(t, promptTasks, m.front().kind)
			return m
		}, tea.ProgramStatus{App: "kopi", State: tea.ProgramStateIdle}},
		{"subagent", func(t *testing.T) *model {
			m, c := newTestModel(t, 60, 20)
			c.status.Agents = 1
			send(m, event.TasksChanged{})
			return m
		}, tea.ProgramStatus{App: "kopi", State: tea.ProgramStateWorking}},
		{"workflow", func(t *testing.T) *model {
			m, c := newTestModel(t, 60, 20)
			c.status.Workflows = 1
			send(m, event.TasksChanged{})
			return m
		}, tea.ProgramStatus{App: "kopi", State: tea.ProgramStateWorking}},
		{"shell alone", func(t *testing.T) *model {
			m, c := newTestModel(t, 60, 20)
			c.status.Shells = 1
			send(m, event.TasksChanged{})
			return m
		}, tea.ProgramStatus{App: "kopi", State: tea.ProgramStateIdle}},
		{"busy after a lone turn end", func(t *testing.T) *model {
			m, c := newTestModel(t, 60, 20)
			send(m, turnStart)
			c.status.Busy = true
			send(m, turnEnd(event.Completed))
			return m
		}, tea.ProgramStatus{App: "kopi", State: tea.ProgramStateWorking}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.set(t).programStatus())
		})
	}
}

func TestProgramStatusIgnoresTheShownView(t *testing.T) {
	m, c := agentLineModel(t, 60, config.DefaultAgentLine(), explorer("t1"))
	c.status.Agents = 1
	send(m, event.TasksChanged{})
	require.Equal(t, tea.ProgramStateWorking, state(m))

	m.show("t1")
	assert.Equal(t, tea.ProgramStateWorking, state(m))
	m.show(app.Main)
	assert.Equal(t, tea.ProgramStateWorking, state(m))
}

func TestViewReportsFocus(t *testing.T) {
	m, _ := newTestModel(t, 60, 20)
	assert.True(t, m.View().ReportFocus)
	assert.True(t, (&model{views: map[string]*view{app.Main: newView()}, terminal: config.DefaultTerminal()}).View().ReportFocus)
}

func TestWindowTitle(t *testing.T) {
	m, c := newTestModel(t, 60, 20)
	c.status.Workspace = "/work/kopi.osc"
	m.status = c.status
	title := func() string { return m.View().WindowTitle }

	assert.Equal(t, "κ kopi.osc", title())
	send(m, turnStart)
	assert.Equal(t, "◐ kopi.osc", title())
	send(m, event.SessionNamed{Name: "Fix flaky test"})
	assert.Equal(t, "◐ Fix flaky test", title())
	send(m, editApproval())
	assert.Equal(t, "✋ Fix flaky test", title())
	send(m, event.ApprovalEnded{Token: 1}, turnEnd(event.Interrupted))

	send(m, event.SessionChanged{Name: "Other"})
	assert.Equal(t, "κ Other", title())
	send(m, event.SessionChanged{})
	assert.Equal(t, "κ kopi.osc", title(), "a session with no name shows the folder")

	m.status.Workspace = ""
	assert.Equal(t, "κ kopi", title())
}

func TestWindowTitleStripsControlCharacters(t *testing.T) {
	m, _ := newTestModel(t, 60, 20)
	send(m, event.SessionNamed{Name: "a\x1b]0;x\x07b\nc"})
	assert.Equal(t, "κ ab c", m.View().WindowTitle)
}

func TestWindowTitleOffLeavesItEmpty(t *testing.T) {
	opts := testOptions()
	opts.Terminal.Title = false
	m, _ := newTestModelWith(t, 60, 20, opts)
	assert.Empty(t, m.View().WindowTitle)
	assert.NotNil(t, m.View().ProgramStatus, "the status report has no key")
}

func TestProgressBar(t *testing.T) {
	m, _ := newTestModel(t, 60, 20)
	send(m, turnStart)
	assert.Nil(t, m.View().ProgressBar, "off by default")

	opts := testOptions()
	opts.Terminal.Progress = true
	m, _ = newTestModelWith(t, 60, 20, opts)
	assert.Nil(t, m.View().ProgressBar, "idle")
	send(m, turnStart)
	require.NotNil(t, m.View().ProgressBar)
	assert.Equal(t, tea.ProgressBarIndeterminate, m.View().ProgressBar.State)
	send(m, editApproval())
	assert.Nil(t, m.View().ProgressBar, "blocked")
}

func TestUnseenFollowsHowTheSessionSettledWhileAway(t *testing.T) {
	for _, tc := range []struct {
		status event.Status
		want   tea.ProgramState
	}{
		{event.Completed, tea.ProgramStateDone},
		{event.Failed, tea.ProgramStateError},
		{event.Interrupted, tea.ProgramStateIdle},
	} {
		t.Run(tc.status.String(), func(t *testing.T) {
			m, _ := awayModel(t)
			send(m, turnStart)
			send(m, turnEnd(tc.status))
			assert.Equal(t, tc.want, state(m))
		})
	}
}

func TestUnseenNeverSetWhenPresent(t *testing.T) {
	m, _ := newTestModel(t, 60, 20)
	send(m, turnStart)
	send(m, turnEnd(event.Completed))
	assert.Equal(t, tea.ProgramStateIdle, state(m))
}

func TestUnseenClearsOnFocusAndOnANewTurn(t *testing.T) {
	m, _ := awayModel(t)
	send(m, turnStart)
	send(m, turnEnd(event.Completed))
	require.Equal(t, tea.ProgramStateDone, state(m))
	m.Update(tea.FocusMsg{})
	assert.Equal(t, tea.ProgramStateIdle, state(m))

	m.Update(tea.BlurMsg{})
	send(m, turnStart)
	send(m, turnEnd(event.Failed))
	require.Equal(t, tea.ProgramStateError, state(m))
	send(m, turnStart)
	assert.Equal(t, tea.ProgramStateWorking, state(m))
	send(m, turnEnd(event.Interrupted))
	assert.Equal(t, tea.ProgramStateIdle, state(m))
}

func TestNotifyChannels(t *testing.T) {
	text := "Waiting for you: kopi"
	for _, tc := range []struct {
		name string
		mode string
		env  map[string]string
		want any
	}{
		{"off", "off", map[string]string{"TERM_PROGRAM": "ghostty"}, nil},
		{"bell", "bell", map[string]string{"TERM_PROGRAM": "ghostty"}, "\a"},
		{"osc9", "osc9", nil, ansi.Notify(text)},
		{"auto ghostty", "auto", map[string]string{"TERM_PROGRAM": "ghostty"}, ansi.Notify(text)},
		{"auto iTerm", "auto", map[string]string{"TERM_PROGRAM": "iTerm.app"}, ansi.Notify(text)},
		{"auto warp", "auto", map[string]string{"TERM_PROGRAM": "WarpTerminal"}, ansi.Notify(text)},
		{"auto wezterm", "auto", map[string]string{"TERM_PROGRAM": "WezTerm"}, ansi.Notify(text)},
		{"auto kitty", "auto", map[string]string{"TERM": "xterm-kitty"}, ansi.Notify(text)},
		{"auto xterm-ghostty", "auto", map[string]string{"TERM": "xterm-ghostty"}, ansi.Notify(text)},
		{"auto tmux", "auto", map[string]string{"TERM_PROGRAM": "tmux", "TERM": "screen-256color"}, "\a"},
		{"auto unknown", "auto", nil, "\a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := testOptions()
			opts.Terminal.Notifications = tc.mode
			m, _ := newTestModelWith(t, 60, 20, opts)
			m.getenv = env(tc.env)
			raw := recordRaw(m)
			m.Update(tea.BlurMsg{})
			m.status.Workspace = "/w/kopi"
			send(m, editApproval())
			if tc.want == nil {
				assert.Empty(t, *raw)
				return
			}
			assert.Equal(t, []any{tc.want}, *raw)
		})
	}
}

func TestNotifyTexts(t *testing.T) {
	m, raw := awayModel(t)
	m.getenv = env(map[string]string{"TERM_PROGRAM": "ghostty"})
	send(m, event.SessionNamed{Name: "Fix it"})

	send(m, turnStart)
	send(m, turnEnd(event.Completed))
	send(m, turnStart)
	send(m, turnEnd(event.Failed))
	send(m, turnStart)
	send(m, event.QuestionRequested{Token: 1, Questions: []tool.Question{choice("H", false, "a", "b")}})
	assert.Equal(t, []any{
		ansi.Notify("Finished: Fix it"),
		ansi.Notify("Failed: Fix it"),
		ansi.Notify("Waiting for you: Fix it"),
	}, *raw)
}

func TestNotifySilentForLogin(t *testing.T) {
	m, raw := awayModel(t)
	send(m, event.LoginRequested{Token: 1, Message: "API key"})
	assert.Empty(t, *raw)
}

func TestNotifySilentWhenPresent(t *testing.T) {
	m, _ := newTestModel(t, 60, 20)
	raw := recordRaw(m)
	send(m, turnStart)
	send(m, editApproval())
	send(m, turnEnd(event.Completed))
	assert.Empty(t, *raw)
}

func TestNotifyOnceAtTheEdge(t *testing.T) {
	m, raw := awayModel(t)
	send(m, editApproval())
	require.Len(t, *raw, 1)
	send(m, event.Notice{Text: "later"})
	m.Update(tea.WindowSizeMsg{Width: 70, Height: 20})
	assert.Len(t, *raw, 1)
}

func TestNotifySilentWhenATurnContinues(t *testing.T) {
	m, raw := awayModel(t)
	send(m, turnStart)
	send(m, turnEnd(event.Completed), turnStart)
	assert.Equal(t, tea.ProgramStateWorking, state(m))
	assert.Empty(t, *raw)
}

func TestTasksDialogOpenDoesNotHideTheFinishedTurn(t *testing.T) {
	m, _ := tasksModel(t)
	raw := recordRaw(m)
	m.Update(tea.BlurMsg{})
	m.getenv = env(map[string]string{"TERM_PROGRAM": "ghostty"})
	typeText(m, "/tasks")
	press(m, enter)
	require.Equal(t, promptTasks, m.front().kind)

	send(m, turnStart)
	send(m, turnEnd(event.Completed))

	assert.Equal(t, tea.ProgramStateDone, state(m))
	assert.Len(t, *raw, 1)
	assert.Contains(t, (*raw)[0], "Finished: ")
}

func TestFinishedWhenTheLastAgentEndsAfterTheTurn(t *testing.T) {
	m, raw := awayModel(t)
	c := m.c.(*fakeController)
	m.getenv = env(map[string]string{"TERM_PROGRAM": "ghostty"})
	c.status.Agents = 1
	send(m, turnStart)
	send(m, turnEnd(event.Completed))
	require.Equal(t, tea.ProgramStateWorking, state(m))
	require.Empty(t, *raw)

	c.status.Agents = 0
	send(m, event.TasksChanged{})
	assert.Equal(t, tea.ProgramStateDone, state(m))
	assert.Equal(t, []any{ansi.Notify("Finished: kopi")}, *raw)
}

func TestSubagentTurnEndDoesNotChangeTheOutcome(t *testing.T) {
	m, raw := awayModel(t)
	c := m.c.(*fakeController)
	m.getenv = env(map[string]string{"TERM_PROGRAM": "ghostty"})
	c.status.Agents = 1
	send(m, turnStart)
	send(m, turnEnd(event.Completed))
	send(m, event.Session{ID: "t1", Event: turnEnd(event.Failed)})

	c.status.Agents = 0
	send(m, event.TasksChanged{})
	assert.Equal(t, tea.ProgramStateDone, state(m))
	assert.Equal(t, []any{ansi.Notify("Finished: kopi")}, *raw)
}

func TestNotifyForATurnThatStartsAndEndsInOneBatch(t *testing.T) {
	m, raw := awayModel(t)
	m.getenv = env(map[string]string{"TERM_PROGRAM": "ghostty"})
	send(m, turnStart, turnEnd(event.Failed))
	assert.Equal(t, tea.ProgramStateError, state(m))
	assert.Equal(t, []any{ansi.Notify("Failed: kopi")}, *raw)
}

func TestSessionChangeForgetsThePreviousOutcome(t *testing.T) {
	m, _ := awayModel(t)
	c := m.c.(*fakeController)
	c.status.Agents = 1
	send(m, turnStart)
	send(m, turnEnd(event.Failed))
	send(m, event.SessionChanged{})

	c.status.Agents = 0
	send(m, event.TasksChanged{})
	assert.Equal(t, tea.ProgramStateDone, state(m))
}
