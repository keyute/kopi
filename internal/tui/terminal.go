package tui

import (
	"path/filepath"
	"strings"
	"unicode"

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
	switch m.front() {
	case promptApproval, promptPlan:
		s.State, s.Kind = tea.ProgramStateBlocked, tea.ProgramStatusKindPermission
	case promptQuestion:
		s.State, s.Kind = tea.ProgramStateBlocked, tea.ProgramStatusKindQuestion
	case promptLogin:
		s.State, s.Kind = tea.ProgramStateBlocked, tea.ProgramStatusKindAuth
	case promptTasks, promptNone:
		switch {
		case m.working():
			s.State = tea.ProgramStateWorking
		case m.unseen != "":
			s.State = m.unseen
		}
	}
	return s
}

// terminalLabel is the session's name, else the workspace folder, else kopi,
// without control characters.
func (m *model) terminalLabel() string {
	label := m.title
	if label == "" {
		label = filepath.Base(m.status.Workspace)
	}
	label = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, label)
	if label == "" || label == "." || label == string(filepath.Separator) {
		return "kopi"
	}
	return label
}

// terminalTitle is the state's glyph and the label.
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
	working := m.working()
	switch {
	case !m.away || working:
		m.unseen = ""
	case was.State == tea.ProgramStateWorking && m.programStatus().State != tea.ProgramStateBlocked:
		switch m.ended {
		case event.Completed:
			m.unseen = tea.ProgramStateDone
		case event.Failed:
			m.unseen = tea.ProgramStateError
		}
	}
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

// notify writes text through the configured channel.
func (m *model) notify(text string) tea.Cmd {
	switch m.terminal.Notifications {
	case "off":
		return nil
	case "bell":
		return m.raw("\a")
	case "osc9":
		return m.raw(ansi.Notify(text))
	}
	switch m.getenv("TERM_PROGRAM") {
	case "ghostty", "iTerm.app", "WarpTerminal", "WezTerm":
		return m.raw(ansi.Notify(text))
	}
	switch m.getenv("TERM") {
	case "xterm-kitty", "xterm-ghostty":
		return m.raw(ansi.Notify(text))
	}
	// ansi.BEL is an integer constant, which tea.Raw would print as 7.
	return m.raw("\a")
}
