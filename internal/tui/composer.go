package tui

import (
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
)

// maxComposerLines lifts the textarea's content limit, which without it
// falls back to capping logical lines at MaxHeight, the visible height.
const maxComposerLines = 10000

func newComposer(th theme) textarea.Model {
	ta := textarea.New()
	ta.ShowLineNumbers = false
	ta.MaxWidth = 0
	ta.DynamicHeight = true
	ta.MaxContentHeight = maxComposerLines
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "shift+enter", "ctrl+j"))
	ta.SetPromptFunc(2, func(p textarea.PromptInfo) string {
		if p.LineNumber == 0 {
			return "❯ "
		}
		return "  "
	})
	styleComposer(&ta, th)
	ta.Focus()
	return ta
}

func styleComposer(ta *textarea.Model, th theme) {
	shaded := shade(th)
	state := textarea.StyleState{
		Text:        shaded,
		CursorLine:  shaded,
		EndOfBuffer: shaded,
		Prompt:      shaded.Foreground(th.accent),
	}
	ta.SetStyles(textarea.Styles{Focused: state, Blurred: state, Cursor: textarea.CursorStyle{Color: th.text}})
}

// resizeComposer lets the input grow to a third of the terminal height.
func resizeComposer(ta *textarea.Model, width, height int) {
	ta.MaxHeight = max(1, height/3)
	ta.SetWidth(width)
}

// composerView frames the input with a blank shaded row above and below.
func composerView(ta *textarea.Model, width int, th theme) []string {
	blank := shade(th).Render(strings.Repeat(" ", width))
	rows := []string{blank}
	rows = append(rows, strings.Split(ta.View(), "\n")...)
	return append(rows, blank)
}
