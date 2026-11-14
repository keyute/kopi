package tui

import "charm.land/bubbles/v2/spinner"

// newSpinner uses MiniDot, whose frames are one column wide, so the text
// after it starts two in.
func newSpinner(th theme) spinner.Model {
	return spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(fg(th.accent)))
}

// workingRow is the row above the composer while a turn runs; a non-empty
// hint follows the elapsed time.
func workingRow(sp *spinner.Model, elapsed, hint string, th theme) string {
	row := sp.View() + " " + fg(th.muted).Render("Working… "+elapsed)
	if hint != "" {
		row += fg(th.dim).Render(" · " + hint)
	}
	return row
}
