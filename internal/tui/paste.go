package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

// imagePastedMsg delivers an image the paste key saved from the clipboard.
type imagePastedMsg struct{ path string }

// pasteFailedMsg reports that the paste key could not paste the clipboard's
// image; the failure is already logged.
type pasteFailedMsg struct{}

var newlines = strings.NewReplacer("\r\n", "\n", "\r", "\n")

// paste inserts pasted content into the composer, whether it came from the
// terminal or from the paste key: a path to an image file as an image block,
// anything else as text.
func (m *model) paste(content string) tea.Cmd {
	content = newlines.Replace(content)
	if path, ok := imagePath(content); ok {
		return m.composer.insertImage(path)
	}
	return m.composer.paste(content)
}
