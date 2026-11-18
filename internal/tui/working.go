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

// workingRow is the row above the composer while a turn runs; hint follows
// text, dimmer.
func workingRow(sp *spinner.Model, text, hint string, th theme) string {
	row := sp.View() + " " + fg(th.muted).Render(text)
	if hint != "" {
		row += fg(th.dim).Render(hint)
	}
	return row
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

// workingHint names the background key while the turn's foreground command
// can be moved to the background, "" otherwise.
func (m *model) workingHint() string {
	ks := m.keys.background.Keys()
	if !m.busy || m.approval != nil || m.tasks != nil || !m.status.Foreground || len(ks) == 0 {
		return ""
	}
	return " · " + ks[0] + " to background"
}
