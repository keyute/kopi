package tui

import (
	"fmt"
	"os/exec"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/keyute/kopi/internal/event"
)

const noEditorNotice = "Set VISUAL or EDITOR to open the plan file in an editor."

// editorDoneMsg is the editor's exit; token is the plan to read again, 0 for
// none.
type editorDoneMsg struct {
	token uint64
	err   error
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

// openEditor runs the editor on path; its exit delivers an editorDoneMsg
// carrying token.
func (m *model) openEditor(path string, token uint64) tea.Cmd {
	cmd := editorCommand(m.getenv, path)
	if cmd == nil {
		m.tr.Apply(event.Notice{Text: noEditorNotice})
		m.relayout()
		return nil
	}
	return m.execProcess(cmd, func(err error) tea.Msg { return editorDoneMsg{token, err} })
}

// editorDone shows a failed run, then has the Controller read the plan again.
func (m *model) editorDone(msg editorDoneMsg) {
	if msg.err != nil {
		m.tr.Apply(event.Error{Err: fmt.Errorf("failed to run the editor: %w", msg.err)})
		m.relayout()
	}
	if msg.token != 0 {
		m.c.RefreshPlan(msg.token)
	}
}
