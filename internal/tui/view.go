package tui

import (
	"slices"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/transcript"
)

// view is everything the frontend keeps for one session, the main one or a
// subagent's, whether or not it is the one shown.
type view struct {
	tr    transcript.Transcript
	folds folds
	cache renderCache
	lines []line
	// offset is the first visible transcript line while not following.
	offset int
	follow bool
	busy   bool
	// turnStart is when the running turn's TurnStart was applied.
	turnStart time.Time
	// verb is the running or last turn's.
	verb verb
	// queued is the input queued for the running turn.
	queued []string
	// unsent is the input a turn handed back while the view was hidden, restored
	// into the composer when the view is next shown.
	unsent []event.Input
}

func newView() *view {
	return &view{folds: newFolds(), follow: true, verb: fallbackVerb}
}

// view is the shown session's.
func (m *model) view() *view { return m.views[m.shown] }

// viewOf is session id's view, made on first use.
func (m *model) viewOf(id string) *view {
	v, ok := m.views[id]
	if !ok {
		v = newView()
		m.views[id] = v
	}
	return v
}

// show makes session id's view the one shown and the one human input acts on.
// An id with no view that is not in m.agents is ignored. The command keeps the
// spinner ticking when the view has a working row.
func (m *model) show(id string) tea.Cmd {
	if _, ok := m.views[id]; !ok {
		if _, known := m.agent(id); !known {
			return nil
		}
	}
	wasShown := m.tickWanted()
	v := m.viewOf(id)
	m.shown = id
	m.clearSelection()
	m.relayout()
	m.composer.restore(v.unsent)
	v.unsent = nil
	if m.tickWanted() && !wasShown {
		return m.spinner.Tick
	}
	return nil
}

// agent is session id's status, false when m.agents does not list it.
func (m *model) agent(id string) (app.AgentStatus, bool) {
	i := slices.IndexFunc(m.agents, func(a app.AgentStatus) bool { return a.ID == id })
	if i < 0 {
		return app.AgentStatus{}, false
	}
	return m.agents[i], true
}
