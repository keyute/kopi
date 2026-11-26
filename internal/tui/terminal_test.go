package tui

import (
	"reflect"
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

// deliveredRecordRaw captures output only when the raw command runs.
func deliveredRecordRaw(m *model) *[]any {
	var got []any
	m.raw = func(v any) tea.Cmd {
		return func() tea.Msg {
			got = append(got, v)
			return tea.Raw(v)()
		}
	}
	return &got
}

// messagesOf runs cmd and returns what it yields, flattening a batch.
func messagesOf(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		return []tea.Msg{msg}
	}
	var msgs []tea.Msg
	for _, c := range batch {
		msgs = append(msgs, messagesOf(c)...)
	}
	return msgs
}

// recordSequence exposes Bubble Tea's unexported sequenceMsg for delivery in order.
func recordSequence(t *testing.T, cmd tea.Cmd) []tea.Cmd {
	t.Helper()
	require.NotNil(t, cmd)
	msg := reflect.ValueOf(cmd())
	target := reflect.TypeFor[[]tea.Cmd]()
	require.True(t, msg.Type().ConvertibleTo(target))
	return msg.Convert(target).Interface().([]tea.Cmd)
}

func deliverRecords(t *testing.T, m *model, cmd tea.Cmd) {
	t.Helper()
	for _, next := range recordSequence(t, cmd) {
		_, follow := m.Update(next())
		if follow != nil {
			deliverRecords(t, m, follow)
		}
	}
}

func sendRecords(t *testing.T, m *model, evs ...event.Event) {
	t.Helper()
	_, cmd := m.Update(eventsMsg(evs))
	if !m.recordsWriting {
		return
	}
	batch, ok := cmd().(tea.BatchMsg)
	require.True(t, ok)
	require.Len(t, batch, 2, "bridge wait and record sequence")
	deliverRecords(t, m, batch[1])
	require.False(t, m.recordsWriting)
}

func state(m *model) tea.ProgramState { return m.programStatus(m.attention()).State }

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
		{"workflow dialog is not blocked", func(t *testing.T) *model {
			m, _ := workflowModel(t, 60)
			openFromRow(t, m)
			assert.Equal(t, promptWorkflow, frontKind(m))
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
		{"approval over a finished turn", func(t *testing.T) *model {
			m, _ := newTestModel(t, 60, 20)
			send(m, turnStart, turnEnd(event.Completed))
			require.Equal(t, tea.ProgramStateDone, state(m))
			send(m, editApproval())
			return m
		}, blocked(tea.ProgramStatusKindPermission)},
		{"approval closed over a finished turn", func(t *testing.T) *model {
			m, _ := newTestModel(t, 60, 20)
			send(m, turnStart, turnEnd(event.Completed))
			send(m, editApproval(), event.ApprovalEnded{Token: 1})
			return m
		}, tea.ProgramStatus{App: "kopi", State: tea.ProgramStateDone}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, func() tea.ProgramStatus { m := tc.set(t); return m.programStatus(m.attention()) }())
		})
	}
}

func TestProgramStatusIgnoresTheShownView(t *testing.T) {
	m, c := subagentLineModel(t, 60, config.DefaultSubagentLine(), explorer("t1"))
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
	assert.Equal(t, "◐ Fix flaky test · kopi.osc", title())
	send(m, editApproval())
	assert.Equal(t, "✋ Fix flaky test · kopi.osc", title())
	send(m, event.ApprovalEnded{Token: 1}, turnEnd(event.Interrupted))

	send(m, event.SessionChanged{Name: "Other"})
	assert.Equal(t, "κ Other · kopi.osc", title())
	send(m, event.SessionChanged{})
	assert.Equal(t, "κ kopi.osc", title(), "a session with no name shows the folder")

	m.status.Workspace = ""
	assert.Equal(t, "κ kopi", title())
	c.status.Workspace = ""
	send(m, event.SessionChanged{Name: "Other"})
	assert.Equal(t, "κ Other", title(), "no folder, no separator")

	c.status.Workspace = "/"
	send(m, event.SessionChanged{Name: "Other"})
	assert.Equal(t, "κ Other", title(), "the root has no folder name")
	send(m, event.SessionChanged{})
	assert.Equal(t, "κ kopi", title())
}

func TestWindowTitleDropsTheGlyphOnceTheTerminalReadsStatus(t *testing.T) {
	m, _ := newTestModel(t, 60, 20)
	m.status.Workspace = "/work/kopi.osc"
	title := func() string { return m.View().WindowTitle }

	send(m, turnStart)
	assert.Equal(t, "◐ kopi.osc", title())
	m.Update(tea.ProgramStatusSupportMsg{})
	assert.Equal(t, "κ kopi.osc", title())
	send(m, editApproval())
	assert.Equal(t, "κ kopi.osc", title())
}

func TestChildRecordsFollowRunningAgentsAndWorkflows(t *testing.T) {
	m, c := newTestModel(t, 60, 20)
	raw := deliveredRecordRaw(m)
	report := func(id, title string) string {
		return ansi.SetProgramStatus(ansi.ProgramStatus{State: ansi.ProgramStateWorking, ID: id, App: "kopi", Title: title})
	}
	main := app.AgentStatus{ID: app.Main}
	researcher := app.AgentStatus{ID: "t1", Agent: "researcher", Description: "peer survey", State: tool.TaskRunning}
	workflow := app.WorkflowStatus{ID: "t2", Description: "review changes", State: tool.TaskRunning, Done: 1, Running: 2, Queued: 1}

	c.agents, c.workflows = []app.AgentStatus{main, researcher}, []app.WorkflowStatus{workflow}
	sendRecords(t, m, event.TasksChanged{})
	assert.Equal(t, []any{report("t1", "researcher › peer survey") + report("t2", "workflow › review changes")}, *raw)

	*raw = nil
	sendRecords(t, m, event.TasksChanged{})
	assert.Empty(t, *raw, "nothing changed")

	researcher.State = tool.TaskCompleted
	c.agents = []app.AgentStatus{main, researcher}
	sendRecords(t, m, event.TasksChanged{})
	assert.Equal(t, []any{ansi.ClearProgramStatusID("t1")}, *raw)

	*raw = nil
	c.agents, c.workflows = nil, nil
	sendRecords(t, m, event.SessionChanged{})
	assert.Equal(t, []any{ansi.ClearProgramStatusID("t2")}, *raw)
}

func TestChildRecordsSyncInOneWriteInIDOrder(t *testing.T) {
	m, c := newTestModel(t, 60, 20)
	raw := deliveredRecordRaw(m)
	report := func(id, title string) string {
		return ansi.SetProgramStatus(ansi.ProgramStatus{State: ansi.ProgramStateWorking, ID: id, App: "kopi", Title: title})
	}
	running := func(id, description string) app.AgentStatus {
		return app.AgentStatus{ID: id, Agent: "researcher", Description: description, State: tool.TaskRunning}
	}
	c.agents = []app.AgentStatus{running("t1", "one"), running("t3", "three")}
	sendRecords(t, m, event.TasksChanged{})

	*raw = nil
	c.agents = []app.AgentStatus{running("t2", "two"), running("t3", "three again")}
	sendRecords(t, m, event.TasksChanged{})
	assert.Equal(t, []any{ansi.ClearProgramStatusID("t1") + report("t2", "researcher › two") + report("t3", "researcher › three again")}, *raw)
}

func TestChildRecordTitleIsTheAgentRowsText(t *testing.T) {
	m, c := newTestModel(t, 60, 20)
	raw := deliveredRecordRaw(m)
	r := app.AgentStatus{ID: "t1", Agent: "researcher", Description: "a long description of what this agent was started to look into", State: tool.TaskRunning}
	c.agents = []app.AgentStatus{r}
	sendRecords(t, m, event.TasksChanged{})

	text, ok := subagentSegments["agent"](entry{AgentStatus: r}, theme{})
	require.True(t, ok)
	assert.Equal(t, []any{ansi.SetProgramStatus(ansi.ProgramStatus{State: ansi.ProgramStateWorking, ID: "t1", App: "kopi", Title: text})}, *raw)
}

func TestChildRecordsReconcileLatestAfterHeldWrite(t *testing.T) {
	m, c := newTestModel(t, 60, 20)
	raw := deliveredRecordRaw(m)
	write := m.raw
	started, release := make(chan struct{}), make(chan struct{})
	m.raw = func(v any) tea.Cmd {
		cmd := write(v)
		if started == nil {
			return cmd
		}
		ready := started
		started = nil
		return func() tea.Msg {
			close(ready)
			<-release
			return cmd()
		}
	}
	ready := started
	c.agents = []app.AgentStatus{{ID: "t1", Agent: "researcher", Description: "survey", State: tool.TaskRunning}}
	c.workflows = []app.WorkflowStatus{{ID: "t2", Description: "original", State: tool.TaskRunning}}
	_, cmd := m.Update(eventsMsg{event.TasksChanged{}})
	batch, ok := cmd().(tea.BatchMsg)
	require.True(t, ok)
	require.Len(t, batch, 2)
	sequence := recordSequence(t, batch[1])
	require.Len(t, sequence, 2, "raw write then acknowledgement")
	written := make(chan tea.Msg)
	go func() { written <- sequence[0]() }()
	<-ready
	require.True(t, m.recordsWriting)
	require.Empty(t, m.records, "no delivered snapshot before acknowledgement")

	c.agents = nil
	c.workflows = []app.WorkflowStatus{{ID: "t2", Description: "intermediate", State: tool.TaskRunning}}
	m.bridge.push(event.TasksChanged{})
	_, cmd = m.Update(m.bridge.wait()())
	require.NotNil(t, cmd, "bridge remains armed while a write is held")
	require.Empty(t, *raw)
	c.workflows = []app.WorkflowStatus{{ID: "t2", Description: "latest", State: tool.TaskRunning}}
	m.bridge.push(event.TasksChanged{})
	_, cmd = m.Update(cmd())
	require.NotNil(t, cmd)
	require.Equal(t, "latest", m.workflows[0].Description)
	require.Empty(t, *raw, "newer updates cannot start another record write")

	close(release)
	_, follow := m.Update(<-written)
	require.Nil(t, follow, "RawMsg is processed before acknowledgement")
	require.Empty(t, m.records)
	_, follow = m.Update(sequence[1]())
	require.Equal(t, map[string]string{"t1": "researcher › survey", "t2": "workflow › original"}, m.records)
	require.True(t, m.recordsWriting, "acknowledgement starts reconciliation")
	deliverRecords(t, m, follow)

	report := func(id, title string) string {
		return ansi.SetProgramStatus(ansi.ProgramStatus{State: ansi.ProgramStateWorking, ID: id, App: "kopi", Title: title})
	}
	assert.Equal(t, []any{
		report("t1", "researcher › survey") + report("t2", "workflow › original"),
		ansi.ClearProgramStatusID("t1") + report("t2", "workflow › latest"),
	}, *raw)
	assert.Equal(t, map[string]string{"t2": "workflow › latest"}, m.records)
	assert.False(t, m.recordsWriting)
	assert.Nil(t, m.syncRecords(), "no later stale working record remains queued")
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

func TestTerminalProgressBar(t *testing.T) {
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

func TestTerminalProgressBarKeepsAlive(t *testing.T) {
	opts := testOptions()
	opts.Terminal.Progress = true
	m, c := newTestModelWith(t, 60, 20, opts)
	value := func() int { return m.View().ProgressBar.Value }
	tick := func() tea.Cmd {
		_, cmd := m.Update(barMsg{})
		return cmd
	}

	send(m, turnStart)
	require.True(t, m.barBeating, "the loop starts with the bar")
	before := value()
	assert.NotNil(t, tick())
	assert.NotEqual(t, before, value())
	assert.True(t, m.barBeating)

	send(m, turnEnd(event.Interrupted))
	assert.Nil(t, tick(), "idle: not rescheduled")
	assert.False(t, m.barBeating)

	c.status.Agents = 1
	send(m, event.TasksChanged{})
	assert.True(t, m.barBeating, "a subagent alone starts it")
	c.status.Agents = 0
	send(m, event.TasksChanged{})
	send(m, turnStart)
	assert.True(t, m.barBeating, "working again before the tick fires keeps the one loop")
	assert.Nil(t, m.beat(), "a tick is pending: no second loop")
	assert.NotNil(t, tick())

	m, _ = newTestModel(t, 60, 20)
	send(m, turnStart)
	assert.False(t, m.barBeating, "progress off: no loop")
}

func TestUnseenFollowsHowTheSessionSettledWhileAway(t *testing.T) {
	for _, tc := range []struct {
		status event.Status
		want   tea.ProgramState
		title  string
	}{
		{event.Completed, tea.ProgramStateDone, "✓ kopi"},
		{event.Failed, tea.ProgramStateError, "✗ kopi"},
		{event.Interrupted, tea.ProgramStateIdle, "κ kopi"},
	} {
		t.Run(tc.status.String(), func(t *testing.T) {
			m, _ := awayModel(t)
			send(m, turnStart)
			send(m, turnEnd(tc.status))
			assert.Equal(t, tc.want, state(m))
			assert.Equal(t, tc.title, m.View().WindowTitle)
		})
	}
}

func TestFinishedTurnWhilePresentIsReportedWithoutAGlyph(t *testing.T) {
	for _, tc := range []struct {
		status event.Status
		want   tea.ProgramState
	}{
		{event.Completed, tea.ProgramStateDone},
		{event.Failed, tea.ProgramStateError},
	} {
		t.Run(tc.status.String(), func(t *testing.T) {
			m, _ := newTestModel(t, 60, 20)
			send(m, turnStart)
			send(m, turnEnd(tc.status))
			assert.Equal(t, tc.want, state(m))
			assert.Empty(t, m.unseen)
			assert.Equal(t, "κ kopi", m.View().WindowTitle)
		})
	}
}

func TestRecordIsIdleAfterAnInterruptedTurnAndASessionChange(t *testing.T) {
	m, _ := newTestModel(t, 60, 20)
	send(m, turnStart)
	send(m, turnEnd(event.Completed))
	require.Equal(t, tea.ProgramStateDone, state(m))
	send(m, turnStart)
	send(m, turnEnd(event.Interrupted))
	assert.Equal(t, tea.ProgramStateIdle, state(m))

	send(m, turnStart)
	send(m, turnEnd(event.Failed))
	require.Equal(t, tea.ProgramStateError, state(m))
	send(m, event.SessionChanged{})
	assert.Equal(t, tea.ProgramStateIdle, state(m))
}

func TestUnseenClearsOnFocusAndOnANewTurn(t *testing.T) {
	m, _ := awayModel(t)
	send(m, turnStart)
	send(m, turnEnd(event.Completed))
	require.Equal(t, tea.ProgramStateDone, state(m))
	require.Equal(t, "✓ kopi", m.View().WindowTitle)
	m.Update(tea.FocusMsg{})
	assert.Equal(t, tea.ProgramStateDone, state(m), "the record holds at rest")
	assert.Equal(t, "κ kopi", m.View().WindowTitle)

	m.Update(tea.BlurMsg{})
	send(m, turnStart)
	send(m, turnEnd(event.Failed))
	require.Equal(t, tea.ProgramStateError, state(m))
	require.Equal(t, "✗ kopi", m.View().WindowTitle)
	send(m, turnStart)
	assert.Equal(t, tea.ProgramStateWorking, state(m))
	assert.Equal(t, "◐ kopi", m.View().WindowTitle)
	send(m, turnEnd(event.Interrupted))
	assert.Equal(t, tea.ProgramStateIdle, state(m))
	assert.Equal(t, "κ kopi", m.View().WindowTitle)
}

func TestNotifySilentWhenADialogClosesOntoARestingRecord(t *testing.T) {
	m, raw := awayModel(t)
	send(m, turnStart)
	send(m, turnEnd(event.Completed))
	m.Update(tea.FocusMsg{})
	m.Update(tea.BlurMsg{})
	require.Empty(t, m.unseen)
	require.Equal(t, tea.ProgramStateDone, state(m))
	*raw = nil

	send(m, event.LoginRequested{Token: 1, Message: "API key"})
	send(m, event.LoginEnded{Token: 1})
	assert.Equal(t, tea.ProgramStateDone, state(m))
	assert.Empty(t, *raw)
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
		{"auto tmux inherited ghostty", "auto", map[string]string{"TMUX": "/tmux,1,0", "TERM_PROGRAM": "ghostty"}, "\a"},
		{"auto tmux inherited iTerm", "auto", map[string]string{"TMUX": "/tmux,1,0", "TERM_PROGRAM": "iTerm.app"}, "\a"},
		{"auto tmux inherited warp", "auto", map[string]string{"TMUX": "/tmux,1,0", "TERM_PROGRAM": "WarpTerminal"}, "\a"},
		{"auto tmux inherited wezterm", "auto", map[string]string{"TMUX": "/tmux,1,0", "TERM_PROGRAM": "WezTerm"}, "\a"},
		{"auto tmux inherited kitty", "auto", map[string]string{"TMUX": "/tmux,1,0", "TERM": "xterm-kitty"}, "\a"},
		{"auto tmux inherited xterm-ghostty", "auto", map[string]string{"TMUX": "/tmux,1,0", "TERM": "xterm-ghostty"}, "\a"},
		{"tmux explicit osc9", "osc9", map[string]string{"TMUX": "/tmux,1,0"}, ansi.Notify(text)},
		{"tmux explicit bell", "bell", map[string]string{"TMUX": "/tmux,1,0", "TERM_PROGRAM": "ghostty"}, "\a"},
		{"tmux explicit off", "off", map[string]string{"TMUX": "/tmux,1,0", "TERM_PROGRAM": "ghostty"}, nil},
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

func TestNotifyOnceWhenTheFrontPromptChangesKind(t *testing.T) {
	m, raw := awayModel(t)
	m.getenv = env(map[string]string{"TERM_PROGRAM": "ghostty"})
	send(m, event.QuestionRequested{Token: 1, Questions: []tool.Question{choice("H", false, "a", "b")}})
	require.Equal(t, promptQuestion, m.front().kind)
	send(m, editApproval())
	require.Equal(t, promptApproval, m.front().kind)
	assert.Equal(t, []any{ansi.Notify("Waiting for you: kopi")}, *raw)
}

func TestNotifyForAPlan(t *testing.T) {
	m, raw := awayModel(t)
	m.getenv = env(map[string]string{"TERM_PROGRAM": "ghostty"})
	send(m, planRequest(7, "# Heading\n\nstep one"))
	require.NotNil(t, planOf(m))
	assert.Equal(t, []any{ansi.Notify("Waiting for you: kopi")}, *raw)
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

func TestConsecutiveBatchedTurnOutcomes(t *testing.T) {
	for _, tc := range []struct {
		outcome event.Status
		want    tea.ProgramState
		text    string
	}{
		{event.Completed, tea.ProgramStateDone, "Finished: kopi"},
		{event.Failed, tea.ProgramStateError, "Failed: kopi"},
	} {
		for _, interrupted := range []bool{false, true} {
			name := tc.outcome.String() + map[bool]string{false: "/consecutive", true: "/interrupted"}[interrupted]
			t.Run(name, func(t *testing.T) {
				m, raw := awayModel(t)
				m.getenv = env(map[string]string{"TERM_PROGRAM": "ghostty"})
				send(m, turnStart, turnEnd(tc.outcome))
				require.Equal(t, tc.want, state(m))
				require.Equal(t, []any{ansi.Notify(tc.text)}, *raw)

				if interrupted {
					send(m, turnStart, turnEnd(event.Interrupted))
					assert.Equal(t, tea.ProgramStateIdle, state(m))
					assert.Len(t, *raw, 1)
				}

				send(m, turnStart, turnEnd(tc.outcome))
				assert.Equal(t, tc.want, state(m))
				assert.Equal(t, []any{ansi.Notify(tc.text), ansi.Notify(tc.text)}, *raw)
				send(m, event.Notice{Text: "later"})
				m.Update(tea.WindowSizeMsg{Width: 70, Height: 20})
				assert.Len(t, *raw, 2, "unrelated updates must not notify")
			})
		}
	}
}

func TestSubagentBatchedTurnDoesNotClearUnseen(t *testing.T) {
	for _, outcome := range []event.Status{event.Completed, event.Failed} {
		t.Run(outcome.String(), func(t *testing.T) {
			m, raw := awayModel(t)
			send(m, turnStart, turnEnd(outcome))
			want := state(m)
			require.Len(t, *raw, 1)
			send(m, event.Session{ID: "t1", Event: turnStart}, event.Session{ID: "t1", Event: turnEnd(event.Interrupted)})
			assert.Equal(t, want, state(m))
			assert.Len(t, *raw, 1)
		})
	}
}

func TestSessionChangeForgetsThePreviousOutcome(t *testing.T) {
	m, raw := awayModel(t)
	c := m.c.(*fakeController)
	c.status.Agents = 1
	send(m, turnStart)
	send(m, turnEnd(event.Failed))
	send(m, event.SessionChanged{})

	c.status.Agents = 0
	send(m, event.TasksChanged{})
	assert.Equal(t, tea.ProgramStateIdle, state(m))
	assert.Empty(t, *raw)
}

func TestSessionChangeClearsUnseenOutcome(t *testing.T) {
	for _, outcome := range []event.Status{event.Completed, event.Failed} {
		for _, resumed := range []bool{false, true} {
			t.Run(outcome.String()+map[bool]string{false: "/new", true: "/resumed"}[resumed], func(t *testing.T) {
				m, raw := awayModel(t)
				m.getenv = env(map[string]string{"TERM_PROGRAM": "ghostty"})
				send(m, turnStart, turnEnd(outcome))
				require.Len(t, *raw, 1)
				changed := event.SessionChanged{Resumed: resumed}
				if resumed {
					changed.History = []event.Event{turnStart, turnEnd(outcome)}
				}
				send(m, changed)
				assert.Equal(t, tea.ProgramStateIdle, state(m))
				send(m, event.Notice{Text: "later"})
				assert.Equal(t, tea.ProgramStateIdle, state(m))
				assert.Len(t, *raw, 1, "switching must not notify")
			})
		}
	}
}

func TestSessionChangeBatchedWithTurnEnd(t *testing.T) {
	for _, outcome := range []event.Status{event.Completed, event.Failed} {
		t.Run(outcome.String(), func(t *testing.T) {
			m, raw := awayModel(t)
			send(m, turnStart)
			send(m, turnEnd(outcome), event.SessionChanged{})
			assert.Equal(t, tea.ProgramStateIdle, state(m))
			assert.False(t, m.mainEnded)
			assert.Empty(t, *raw)
		})
	}
}

func TestSessionChangeKeepsRunningTurnAndItsCompletion(t *testing.T) {
	for _, batched := range []bool{false, true} {
		t.Run(map[bool]string{false: "separate", true: "batched"}[batched], func(t *testing.T) {
			m, raw := awayModel(t)
			m.getenv = env(map[string]string{"TERM_PROGRAM": "ghostty"})
			send(m, turnStart)
			if batched {
				send(m, event.SessionChanged{}, turnEnd(event.Completed))
			} else {
				send(m, event.SessionChanged{})
				assert.Equal(t, tea.ProgramStateWorking, state(m))
				assert.Empty(t, *raw)
				send(m, turnEnd(event.Completed))
			}
			assert.Equal(t, tea.ProgramStateDone, state(m))
			assert.Equal(t, []any{ansi.Notify("Finished: kopi")}, *raw)
		})
	}
}

func TestUnseenNoOutcomeWhenOnlyAgentsStop(t *testing.T) {
	m, raw := awayModel(t)
	c := m.c.(*fakeController)
	c.status.Agents = 1
	send(m, event.TasksChanged{})
	require.Equal(t, tea.ProgramStateWorking, state(m))
	c.status.Agents = 0
	send(m, event.TasksChanged{})
	assert.Equal(t, tea.ProgramStateIdle, state(m))
	assert.Empty(t, *raw)
}
