package tui

import (
	"cmp"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/tool"
)

// working reports whether the session is doing anything on its own: the main
// turn, a Work the Controller still runs, a subagent or a workflow. Shells do
// not count; one may be a server that never ends.
func (m *model) working() bool {
	return m.views[app.Main].busy || m.status.Busy || m.status.Agents+m.status.Workflows > 0
}

// attention is the session-wide state the tab title glyph and the
// notifications follow, whichever view is shown. The first match wins: blocked
// on the human, working, a finished or failed run not yet seen, idle.
func (m *model) attention() tea.ProgramStatus {
	s := tea.ProgramStatus{App: "kopi", State: tea.ProgramStateIdle}
	if front := m.front(); front != nil && front.kind.programKind() != "" {
		s.State, s.Kind = tea.ProgramStateBlocked, front.kind.programKind()
	} else if m.working() {
		s.State = tea.ProgramStateWorking
	} else if m.unseen != "" {
		s.State = m.unseen
	}
	return s
}

// endedState is how the main session's last turn ended as a program state, ""
// when there is none or it was interrupted.
func (m *model) endedState() tea.ProgramState {
	if m.ended == nil {
		return ""
	}
	switch *m.ended {
	case event.Completed:
		return tea.ProgramStateDone
	case event.Failed:
		return tea.ProgramStateError
	}
	return ""
}

// programStatus is the record for the terminal: attention, except that a
// session at rest reports how the main session's last turn ended, seen or not.
func (m *model) programStatus() tea.ProgramStatus {
	s := m.attention()
	if s.State == tea.ProgramStateIdle && m.endedState() != "" {
		s.State = m.endedState()
	}
	return s
}

// terminalNames is the session's name, else the workspace folder, else kopi,
// and the workspace folder's name, each on one line; the folder is "" when the
// workspace has none.
func (m *model) terminalNames() (label, folder string) {
	folder = oneLine(filepath.Base(m.status.Workspace))
	if folder == "." || folder == string(filepath.Separator) {
		folder = ""
	}
	return cmp.Or(oneLine(m.title), folder, "kopi"), folder
}

func (m *model) terminalTitle(state tea.ProgramState) string {
	label, folder := m.terminalNames()
	if oneLine(m.title) != "" && folder != "" {
		label += " · " + folder
	}
	if m.readsStatus {
		return "κ " + label
	}
	glyph := "κ"
	switch state {
	case tea.ProgramStateWorking:
		glyph = "◐"
	case tea.ProgramStateBlocked:
		glyph = "✋"
	case tea.ProgramStateDone:
		glyph = "✓"
	case tea.ProgramStateError:
		glyph = "✗"
	}
	return glyph + " " + label
}

// syncRecords reports a record for each running subagent and workflow that is
// new or retitled, and clears each that ended, in one write sorted by id.
func (m *model) syncRecords() tea.Cmd {
	live := make(map[string]string, len(m.agents)+len(m.workflows))
	for _, a := range m.agents {
		if a.ID != app.Main && a.State == tool.TaskRunning {
			live[a.ID] = agentTitle(a.Agent, a.Description)
		}
	}
	for _, w := range m.workflows {
		live[w.ID] = agentTitle(tool.WorkflowAgent, w.Description)
	}
	ids := slices.Collect(maps.Keys(live))
	for id := range m.records {
		if _, ok := live[id]; !ok {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	var out strings.Builder
	for _, id := range ids {
		title, ok := live[id]
		switch {
		case !ok:
			out.WriteString(ansi.ClearProgramStatusID(id))
		case m.records[id] != title:
			out.WriteString(ansi.SetProgramStatus(ansi.ProgramStatus{State: ansi.ProgramStateWorking, ID: id, App: "kopi", Title: title}))
		}
	}
	m.records = live
	if out.Len() == 0 {
		return nil
	}
	return m.raw(out.String())
}

// attend follows the terminal's focus, keeps unseen, and returns the
// notification when the state changed from was into one that needs the human
// or a fresh main turn finished while away. It runs after update, with was
// taken before it.
func (m *model) attend(msg tea.Msg, was tea.ProgramStatus) tea.Cmd {
	switch msg.(type) {
	case tea.BlurMsg:
		m.away = true
	case tea.FocusMsg:
		m.away = false
	}
	// A turn that starts and ends in one update is never seen working.
	switch {
	case !m.away || m.working():
		m.unseen = ""
	case m.ended != nil && (was.State == tea.ProgramStateWorking || m.mainEnded) && m.attention().State != tea.ProgramStateBlocked:
		if ended := m.endedState(); ended != "" {
			m.unseen = ended
		}
	}
	fresh := m.mainEnded && m.endedState() != ""
	m.mainEnded = false
	now := m.attention()
	fresh = fresh && (now.State == tea.ProgramStateDone || now.State == tea.ProgramStateError)
	if !m.away || (now == was && !fresh) {
		return nil
	}
	var text string
	switch {
	case now.State == tea.ProgramStateBlocked && now.Kind != tea.ProgramStatusKindAuth:
		text = "Waiting for you: "
	case now.State == tea.ProgramStateDone:
		text = "Finished: "
	case now.State == tea.ProgramStateError:
		text = "Failed: "
	default:
		return nil
	}
	label, _ := m.terminalNames()
	return m.notify(text + label)
}

func (m *model) notify(text string) tea.Cmd {
	osc9 := false
	switch m.terminal.Notifications {
	case "off":
		return nil
	case "osc9":
		osc9 = true
	case "auto":
		osc9 = m.getenv("TMUX") == "" && (slices.Contains([]string{"ghostty", "iTerm.app", "WarpTerminal", "WezTerm"}, m.getenv("TERM_PROGRAM")) ||
			slices.Contains([]string{"xterm-kitty", "xterm-ghostty"}, m.getenv("TERM")))
	}
	if osc9 {
		return m.raw(ansi.Notify(text))
	}
	// ansi.BEL is an integer constant, which tea.Raw would print as 7.
	return m.raw("\a")
}
