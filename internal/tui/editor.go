package tui

import (
	"fmt"
	"os/exec"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/keyute/kopi/internal/event"
)

const noEditorNotice = "Set VISUAL or EDITOR to open the plan file in an editor."

// editorDoneMsg is the editor's exit.
type editorDoneMsg struct {
	err error
}

// editorCommand opens path in the first non-blank of $VISUAL and $EDITOR, run
// by sh as git does so a value with arguments works; nil when neither is set.
func editorCommand(getenv func(string) string, path string) *exec.Cmd {
	for _, name := range []string{"VISUAL", "EDITOR"} {
		if editor := getenv(name); strings.TrimSpace(editor) != "" {
			return exec.Command("sh", "-c", editor+` "$1"`, "sh", path)
		}
	}
	return nil
}

// openEditor runs the editor on path; its exit delivers an editorDoneMsg. While
// it runs, editing is set so SIGINT meant for the editor is not taken as a cancel.
func (m *model) openEditor(path string) tea.Cmd {
	cmd := editorCommand(m.getenv, path)
	if cmd == nil {
		m.view().tr.Apply(event.Notice{Text: noEditorNotice})
		m.relayout()
		return nil
	}
	m.editing.Store(true)
	return tea.Sequence(m.raw(ansi.ResetModeLightDark), m.execProcess(cmd, func(err error) tea.Msg {
		m.editing.Store(false)
		return editorDoneMsg{err}
	}))
}

// editorDone shows a failed run, then tells the prompts, whichever editor run
// it was.
func (m *model) editorDone(msg editorDoneMsg) {
	if msg.err != nil {
		m.view().tr.Apply(event.Error{Err: fmt.Errorf("failed to run the editor: %w", msg.err)})
		m.relayout()
	}
	m.sweep(msg)
}
