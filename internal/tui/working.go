package tui

import (
	"strconv"
	"strings"
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
// pending approval wins, then a running turn, then the Controller's
// activity, then idle waiting on background work. The waiting clock runs
// underneath the others.
func (m *model) workingText() (string, bool) {
	switch {
	case m.approval != nil:
		return "Waiting for you…", true
	case m.busy:
		return m.verb.ing + "… " + formatDuration(time.Since(m.turnStart)), true
	case m.activity != "":
		return m.activity, true
	case m.waiting():
		return "Waiting on " + m.waitingOn() + "… " + formatDuration(time.Since(m.waitStart)), true
	}
	return "", false
}

// waiting reports whether the root is idle while background work runs.
func (m *model) waiting() bool { return !m.busy && m.status.Agents+m.status.Shells > 0 }

// waitingOn is the running work as "2 agents, 1 shell".
func (m *model) waitingOn() string { return taskCounts(m.status.Agents, m.status.Shells) }

// taskCounts joins the nonzero counts as "2 agents, 1 shell", "" when both are zero.
func taskCounts(agents, shells int) string {
	var parts []string
	for _, p := range []struct {
		n    int
		noun string
	}{{agents, "agent"}, {shells, "shell"}} {
		if p.n == 0 {
			continue
		}
		s := strconv.Itoa(p.n) + " " + p.noun
		if p.n > 1 {
			s += "s"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}

// syncWaiting starts the waiting clock when the waiting state begins and
// clears it when it ends.
func (m *model) syncWaiting() {
	switch {
	case !m.waiting():
		m.waitStart = time.Time{}
	case m.waitStart.IsZero():
		m.waitStart = time.Now()
	}
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
