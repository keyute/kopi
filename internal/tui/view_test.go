package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/event"
)

// sub wraps ev as session id's.
func sub(id string, ev event.Event) event.Event { return event.Session{ID: id, Event: ev} }

// agentsModel lists the main session and a subagent t1, whose view is not
// shown yet.
func agentsModel(t *testing.T) (*model, *fakeController) {
	t.Helper()
	m, c := newTestModel(t, 40, 20)
	c.status.ContextTokens, c.status.ContextWindow = 10, 100
	c.agents = []app.AgentStatus{
		{ID: app.Main, Model: "mock/model-1", ContextTokens: 10, ContextWindow: 100},
		{ID: "t1", Agent: "explore", Model: "mock/sub-1", ContextTokens: 50, ContextWindow: 200},
	}
	send(m, event.TasksChanged{})
	return m, c
}

// display shows session id as Update would, so the rows below the transcript
// are laid out for it.
func display(m *model, id string) tea.Cmd {
	cmd := m.show(id)
	m.arrange()
	return cmd
}

func TestSubagentEventsDrawInItsViewOnly(t *testing.T) {
	m, _ := agentsModel(t)

	send(m, sub("t1", event.TurnStart{Prompt: "dig"}), sub("t1", event.TextDelta{Text: "found"}))
	assert.Empty(t, body(m), "main's view is untouched")

	display(m, "t1")
	assert.Equal(t, []string{"", "", "❯ dig", "", "", "found"}, body(m))

	send(m, event.TurnStart{Prompt: "main"}, event.TextDelta{Text: "hello"})
	assert.Equal(t, []string{"", "", "❯ dig", "", "", "found"}, body(m), "main's events do not draw in the subagent's view")

	display(m, app.Main)
	assert.Equal(t, []string{"", "", "❯ main", "", "", "hello"}, body(m))
}

func TestShowIgnoresAnUnknownSession(t *testing.T) {
	m, _ := agentsModel(t)

	assert.Nil(t, display(m, "nope"))

	assert.Equal(t, app.Main, m.shown)
	assert.NotContains(t, m.views, "nope")
}

func TestSwitchingViewsKeepsEachScrollAndFoldsAndClearsSelection(t *testing.T) {
	m, _ := agentsModel(t)
	send(m, event.TurnStart{Prompt: "go"}, event.TextDelta{Text: linesOf(40)})
	press(m, tea.KeyPressMsg{Code: tea.KeyPgUp})
	mainOffset := m.view().offset
	require.False(t, m.view().follow)
	main := screen(m)

	display(m, "t1")
	send(m, sub("t1", event.TextDelta{Text: "dig"}))
	assert.True(t, m.view().follow, "a view of its own starts at the bottom")
	press(m, tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	require.True(t, m.view().folds.all)
	m.sel, m.selIn = selection{anchor: point{1, 0}, head: point{1, 3}}, regionTranscript

	display(m, app.Main)

	assert.Equal(t, regionNone, m.selIn)
	assert.False(t, m.view().folds.all)
	assert.False(t, m.view().follow)
	assert.Equal(t, mainOffset, m.view().offset)
	assert.Equal(t, main, screen(m))

	display(m, "t1")
	assert.True(t, m.view().folds.all)
}

func TestSessionChangedDropsSubagentViewsAndShowsMain(t *testing.T) {
	m, _ := agentsModel(t)
	send(m, sub("t1", event.TurnStart{Prompt: "dig"}))
	display(m, "t1")

	send(m, event.SessionChanged{SessionID: "s2"})

	assert.Equal(t, app.Main, m.shown)
	assert.Len(t, m.views, 1)
	assert.Empty(t, body(m))
}

func TestSessionChangedRestoresMainsUnsentInputWhileASubagentIsShown(t *testing.T) {
	m, _ := agentsModel(t)
	send(m, event.TurnStart{Prompt: "go"})
	display(m, "t1")
	send(m, event.TurnEnd{Unsent: []event.Input{{Text: "unsent"}}})
	require.Empty(t, m.composer.value())

	send(m, event.SessionChanged{SessionID: "s2"})

	assert.Equal(t, app.Main, m.shown)
	assert.Equal(t, "unsent", m.composer.value())
}

func TestSessionChangedRestoresAHiddenSubagentsHeldInput(t *testing.T) {
	m, c := agentsModel(t)
	c.agents = append(c.agents, app.AgentStatus{ID: "t2", Agent: "explore"})
	send(m, event.TasksChanged{})
	send(m, sub("t2", event.TurnStart{Prompt: "dig"}), sub("t2", event.TurnEnd{Unsent: []event.Input{{Text: "held"}}}))
	send(m, event.TurnStart{Prompt: "go"})
	display(m, "t1")
	send(m, event.TurnEnd{Unsent: []event.Input{{Text: "unsent"}}})
	require.Empty(t, m.composer.value())

	send(m, event.SessionChanged{SessionID: "s2"})

	assert.Equal(t, "unsent\n\nheld", m.composer.value())
}

func TestInputActsOnTheShownView(t *testing.T) {
	m, c := agentsModel(t)
	send(m, sub("t1", event.TurnStart{Prompt: "dig"}), sub("t1", event.QueueChanged{Pending: []string{"q"}}))
	display(m, "t1")

	submitText(m, "hi")
	typeText(m, "now")
	press(m, ctrlEnter)
	c.queued = []event.Input{{Text: "q"}}
	press(m, up)
	press(m, escape)
	press(m, ctrlC)
	assert.Equal(t, []string{"t1", "t1", "t1", "t1", "t1"}, c.sessions)

	display(m, app.Main)
	press(m, escape)
	assert.Equal(t, app.Main, c.sessions[len(c.sessions)-1])
}

func TestBackgroundKeyIsInertInASubagentView(t *testing.T) {
	m, c := agentsModel(t)
	c.foreground = true
	display(m, "t1")

	press(m, ctrlB)

	assert.Zero(t, c.backgrounds)
}

func TestStatusLineFollowsTheShownView(t *testing.T) {
	m, _ := agentsModel(t)
	require.Contains(t, statusText(m), "model-1")
	require.Contains(t, statusText(m), "10.0%")

	display(m, "t1")
	assert.Contains(t, statusText(m), "sub-1")
	assert.Contains(t, statusText(m), "25.0%")
	assert.NotContains(t, statusText(m), "model-1")

	display(m, app.Main)
	assert.Contains(t, statusText(m), "model-1")
	assert.Contains(t, statusText(m), "10.0%")
}

func TestSubagentQueueShowsInItsViewOnly(t *testing.T) {
	m, _ := agentsModel(t)
	send(m, sub("t1", event.TurnStart{Prompt: "dig"}), sub("t1", event.QueueChanged{Pending: []string{"more"}}))
	assert.NotContains(t, screen(m), "❯ more")

	display(m, "t1")
	assert.Contains(t, screen(m), "❯ more")

	display(m, app.Main)
	assert.NotContains(t, screen(m), "❯ more")
}

func TestUnsentComesBackWhenItsViewIsShown(t *testing.T) {
	m, _ := agentsModel(t)
	send(m, sub("t1", event.TurnStart{Prompt: "dig"}))

	send(m, sub("t1", event.TurnEnd{Unsent: []event.Input{{Text: "held"}}}))
	assert.Empty(t, m.composer.value(), "a hidden view's input waits")
	display(m, "t1")
	assert.Equal(t, "held", m.composer.value())

	send(m, sub("t1", event.TurnStart{Prompt: "again"}), sub("t1", event.TurnEnd{Unsent: []event.Input{{Text: "now"}}}))
	assert.Equal(t, "now\n\nheld", m.composer.value(), "the shown view's input returns at once")
}

func TestAStoppedHiddenSubagentKeepsItsRowWhileItHoldsUnsentInput(t *testing.T) {
	m, c := agentsModel(t)
	c.agents[1].State = "completed"
	send(m, sub("t1", event.TurnStart{Prompt: "dig"}), sub("t1", event.TurnEnd{Unsent: []event.Input{{Text: "held"}}}), event.TasksChanged{})
	require.NotEmpty(t, m.entries(), "the row stays so the view can be shown")

	display(m, "t1")
	display(m, app.Main)

	assert.Empty(t, m.entries(), "the row goes once its input came back")
}

func TestSubagentTurnEndDropsOnlyItsOwnApproval(t *testing.T) {
	m, _ := agentsModel(t)
	send(m,
		event.ApprovalRequested{Token: 1, Agent: "explore", TaskID: "t1"},
		event.ApprovalRequested{Token: 2, Agent: "explore", TaskID: "t2"},
		event.ApprovalRequested{Token: 3},
	)

	send(m, sub("t1", event.TurnEnd{}))

	require.NotNil(t, m.approval)
	assert.Equal(t, uint64(2), m.approval.Token)
	require.Len(t, m.approvalQueue, 1)
	assert.Equal(t, uint64(3), m.approvalQueue[0].Token)

	send(m, event.TurnEnd{})
	assert.Equal(t, uint64(2), m.approval.Token)
	assert.Empty(t, m.approvalQueue)
}
