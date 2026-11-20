package tui

import (
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"

	"github.com/keyute/kopi/internal/app"
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
// pending approval wins, then the shown view's running turn, then, in the
// main view only, the Controller's activity, then idle waiting on background
// work. The waiting clock runs underneath the others.
func (m *model) workingText() (string, bool) {
	v, front := m.view(), m.front()
	switch {
	case front == promptApproval || front == promptQuestion || front == promptPlan:
		return "Waiting for you…", true
	case v.busy:
		return v.verb.ing + "… " + formatDuration(time.Since(v.turnStart)), true
	case m.shown != app.Main:
		return "", false
	case m.activity != "":
		return m.activity, true
	case m.waiting():
		return "Waiting on " + m.waitingOn() + "… " + formatDuration(time.Since(m.waitStart)), true
	}
	return "", false
}

// waiting reports whether the root is idle while background work runs.
func (m *model) waiting() bool { return !m.views[app.Main].busy && m.status.Agents+m.status.Shells > 0 }

// waitingOn is the running work as "2 agents, 1 shell".
func (m *model) waitingOn() string { return taskCounts(m.status.Agents, m.status.Shells) }

// taskCounts joins the nonzero counts as "2 agents, 1 shell", "" when both are zero.
func taskCounts(agents, shells int) string {
	var parts []string
	if agents > 0 {
		parts = append(parts, countText(agents, "agent"))
	}
	if shells > 0 {
		parts = append(parts, countText(shells, "shell"))
	}
	return strings.Join(parts, ", ")
}

// countText is n and noun, pluralised: "1 shell", "2 shells".
func countText(n int, noun string) string {
	s := strconv.Itoa(n) + " " + noun
	if n > 1 {
		s += "s"
	}
	return s
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

// workingHint names the background key while the main turn's foreground
// command can be moved to the background, "" otherwise.
func (m *model) workingHint() string {
	ks := m.keys.background.Keys()
	if m.shown != app.Main || !m.view().busy || m.front() != promptNone || !m.status.Foreground || len(ks) == 0 {
		return ""
	}
	return " · " + ks[0] + " to background"
}
