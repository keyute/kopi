package tui

import (
	"path/filepath"
	"slices"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/event"
)

// working reports whether the session is doing anything on its own: the main
// turn, a Work the Controller still runs, a subagent or a workflow. Shells do
// not count; one may be a server that never ends.
func (m *model) working() bool {
	return m.views[app.Main].busy || m.status.Busy || m.status.Agents+m.status.Workflows > 0
}

// programStatus is the session-wide state for the terminal, whichever view is
// shown. The first match wins: blocked on the human, working, a finished or
// failed run not yet seen, idle.
func (m *model) programStatus() tea.ProgramStatus {
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

// terminalLabel is the session's name, else the workspace folder, else kopi,
// on one line.
func (m *model) terminalLabel() string {
	label := m.title
	if label == "" {
		label = filepath.Base(m.status.Workspace)
	}
	label = oneLine(label)
	if label == "" || label == "." || label == string(filepath.Separator) {
		return "kopi"
	}
	return label
}

func (m *model) terminalTitle(state tea.ProgramState) string {
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
	return glyph + " " + m.terminalLabel()
}

// attend follows the terminal's focus, keeps unseen, and returns the
// notification when the state changed from was into one that needs the human
// while the terminal is away. It runs after update, with was taken before it.
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
	case m.ended != nil && (was.State == tea.ProgramStateWorking || m.mainEnded) && m.programStatus().State != tea.ProgramStateBlocked:
		switch *m.ended {
		case event.Completed:
			m.unseen = tea.ProgramStateDone
		case event.Failed:
			m.unseen = tea.ProgramStateError
		}
	}
	m.mainEnded = false
	now := m.programStatus()
	if !m.away || now == was {
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
	return m.notify(text + m.terminalLabel())
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
