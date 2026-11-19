package tui

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/keyute/kopi/internal/app"
)

const (
	// maxAgentRows is the most entry rows drawn; the rest scroll behind the
	// "more" rows.
	maxAgentRows = 5
	// descriptionWidth is the columns of a subagent's description the agent
	// segment shows.
	descriptionWidth = 36
)

// entry is one row of the agent rows: what it opens and what its segments show.
type entry struct {
	app.AgentStatus
	// busy is whether a turn runs in the session, as its view saw it.
	busy bool
	// since is when the session's running turn started, zero when none runs.
	since time.Time
	// activity is the title of the tool call running in the session, "" when none.
	activity string
}

// agentSegment renders one part of an agent row; false draws nothing.
type agentSegment func(entry) (string, bool)

// agentSegments is keyed by the config's agent segment names.
var agentSegments = map[string]agentSegment{
	"agent": func(e entry) (string, bool) {
		if e.ID == app.Main {
			return "main", true
		}
		desc := strings.Join(strings.Fields(e.Description), " ")
		if desc == "" {
			return e.Agent, true
		}
		return e.Agent + " › " + cutWords(desc, descriptionWidth), true
	},
	"tokens": func(e entry) (string, bool) {
		return compactCount(e.ContextTokens) + " tokens", e.ContextTokens > 0
	},
	"model": func(e entry) (string, bool) { return modelID(e.Model), e.Model != "" },
	"elapsed": func(e entry) (string, bool) {
		return formatDuration(time.Since(e.since)), !e.since.IsZero()
	},
	"state": func(e entry) (string, bool) {
		if e.ID == app.Main {
			return "running", e.busy
		}
		return string(e.State), e.State != ""
	},
	"activity": func(e entry) (string, bool) { return e.activity, e.activity != "" },
	"id":       func(e entry) (string, bool) { return e.ID, e.ID != app.Main },
}

// agentFocus is whether the agent rows have the keys and the session id of the
// entry the cursor is on. at is the cursor's last index, which finds the
// nearest entry when its own has gone.
type agentFocus struct {
	on bool
	id string
	at int
}

// cutWords cuts text to width columns, at a word boundary where there is one,
// ending in "…" when it cut.
func cutWords(text string, width int) string {
	if ansi.StringWidth(text) <= width {
		return text
	}
	cut := ansi.Truncate(text, width-1, "")
	if !strings.HasPrefix(text[len(cut):], " ") {
		if i := strings.LastIndexByte(cut, ' '); i > 0 {
			cut = cut[:i]
		}
	}
	return strings.TrimRight(cut, " ") + "…"
}

// compactCount is n with one decimal and a lowercase unit: "850", "22.1k", "1.2m".
func compactCount(n int) string {
	switch {
	case n >= 999_950:
		return fmt.Sprintf("%.1fm", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return strconv.Itoa(n)
}

// entries is the agent rows' entries: main, then each subagent that is busy or
// shown. Without a subagent entry there are none.
func (m *model) entries() []entry {
	var es []entry
	for _, a := range m.agents {
		e := entry{AgentStatus: a}
		if v := m.views[a.ID]; v != nil {
			if e.busy = v.busy; e.busy {
				e.since = v.turnStart
			}
			e.activity, _ = v.tr.Running()
		}
		if a.ID != app.Main && !e.busy && a.ID != m.shown {
			continue
		}
		es = append(es, e)
	}
	if len(es) < 2 {
		return nil
	}
	return es
}

// elapsedTicks reports whether an entry shows a clock, which needs the screen
// redrawn on every tick.
func (m *model) elapsedTicks() bool {
	if !slices.Contains(m.agentLine.Left, "elapsed") && !slices.Contains(m.agentLine.Right, "elapsed") {
		return false
	}
	return slices.ContainsFunc(m.entries(), func(e entry) bool { return !e.since.IsZero() })
}

// tickWanted reports whether anything on screen changes with the spinner's
// ticks.
func (m *model) tickWanted() bool {
	_, working := m.workingText()
	return working || m.elapsedTicks()
}

// agentRows draws the entries, a window of at most maxAgentRows of them. The
// window shrinks to limit lines, more rows included, and no rows draw when not
// even one entry fits.
func (m *model) agentRows(limit int) []string {
	es := m.entries()
	if len(es) == 0 {
		return nil
	}
	at := slices.IndexFunc(es, func(e entry) bool { return e.ID == m.shown })
	if m.focus.on {
		at = slices.IndexFunc(es, func(e entry) bool { return e.ID == m.focus.id })
	}
	for size := min(maxAgentRows, len(es)); size > 0; size-- {
		from := min(max(0, at-size/2), len(es)-size)
		to := from + size
		lines := size
		if from > 0 {
			lines++
		}
		if to < len(es) {
			lines++
		}
		if lines > limit {
			continue
		}
		var rows []string
		if from > 0 {
			rows = append(rows, fg(m.th.dim).Render(fmt.Sprintf("  ↑ %d more", from)))
		}
		for _, e := range es[from:to] {
			rows = append(rows, m.agentRow(e))
		}
		if to < len(es) {
			rows = append(rows, fg(m.th.dim).Render(fmt.Sprintf("  ↓ %d more", len(es)-to)))
		}
		return rows
	}
	return nil
}

// agentRow sits two in from the left edge. Left segments that do not fit are
// dropped from their right end, then the row is truncated.
func (m *model) agentRow(e entry) string {
	glyph := "○"
	switch {
	case m.focus.on && e.ID == m.focus.id:
		glyph = fg(m.th.accent).Render("❭")
	case e.ID == m.shown:
		glyph = "●"
	}
	render := func(names []string) []string {
		var parts []string
		for _, n := range names {
			if text, ok := agentSegments[n](e); ok {
				parts = append(parts, text)
			}
		}
		return parts
	}
	sep := m.agentLine.Separator
	var right string
	if parts := render(m.agentLine.Right); len(parts) > 0 {
		right = strings.Join(parts, sep) + "  "
	}
	left := "  " + glyph + " "
	parts := render(m.agentLine.Left)
	for n := len(parts); n >= 0; n-- {
		left = "  " + glyph + " " + strings.Join(parts[:n], sep)
		if lipgloss.Width(left)+1+lipgloss.Width(right) <= m.width {
			break
		}
	}
	gap := max(1, m.width-lipgloss.Width(left)-lipgloss.Width(right))
	if right == "" {
		gap = 0
	}
	return ansi.Truncate(left+strings.Repeat(" ", gap)+right, m.width, "")
}

// syncFocus drops the focus while a dialog is open or no rows show, and moves a
// cursor whose entry has gone to the nearest remaining one.
func (m *model) syncFocus() {
	if !m.focus.on {
		return
	}
	es := m.entries()
	if len(es) == 0 || m.dialog != nil || m.tasks != nil {
		m.focus = agentFocus{}
		return
	}
	i := slices.IndexFunc(es, func(e entry) bool { return e.ID == m.focus.id })
	if i < 0 {
		i = min(m.focus.at, len(es)-1)
	}
	m.focus.id, m.focus.at = es[i].ID, i
}

// focusRows gives the rows the keys with the cursor on the first entry, false
// when no rows show.
func (m *model) focusRows() bool {
	es := m.entries()
	if len(es) == 0 {
		return false
	}
	m.focus = agentFocus{on: true, id: es[0].ID}
	return true
}

// agentKey handles a key while the rows have focus; false leaves the rows and
// lets the key be handled normally.
func (m *model) agentKey(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	es := m.entries()
	at := slices.IndexFunc(es, func(e entry) bool { return e.ID == m.focus.id })
	if at < 0 {
		m.focus = agentFocus{}
		return nil, false
	}
	km := &m.keys
	switch {
	case key.Matches(msg, km.menuDown):
		m.focus.at = min(at+1, len(es)-1)
		m.focus.id = es[m.focus.at].ID
		return nil, true
	case key.Matches(msg, km.menuUp):
		if at == 0 {
			m.focus = agentFocus{}
			return nil, true
		}
		m.focus.at = at - 1
		m.focus.id = es[at-1].ID
		return nil, true
	case key.Matches(msg, km.submit):
		m.focus = agentFocus{}
		return m.show(es[at].ID), true
	case key.Matches(msg, km.stopAgent):
		if e := es[at]; e.ID != app.Main && e.busy && e.ID != m.shown {
			m.c.Interrupt(e.ID)
			return nil, true
		}
	case key.Matches(msg, km.menuClose), key.Matches(msg, km.interrupt):
		m.focus = agentFocus{}
		return nil, true
	}
	m.focus = agentFocus{}
	return nil, false
}
