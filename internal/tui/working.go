package tui

import (
	"time"

	"charm.land/bubbles/v2/spinner"
)

// newSpinner uses MiniDot, whose frames are one column wide, so the text
// after it starts two in.
func newSpinner(th theme) spinner.Model {
	return spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(fg(th.accent)))
}

// workingRow is the row above the composer while a turn runs.
func workingRow(sp *spinner.Model, text string, th theme) string {
	return sp.View() + " " + fg(th.muted).Render(text)
}

// workingText is the working row's text, false when it is not shown. A
// running turn's text wins over the Controller's activity.
func (m *model) workingText() (string, bool) {
	switch {
	case m.busy && m.approval != nil:
		return "Waiting for you…", true
	case m.busy:
		return m.verb.ing + "… " + formatDuration(time.Since(m.turnStart)), true
	}
	return m.activity, m.activity != ""
}
