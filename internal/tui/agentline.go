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
	"github.com/keyute/kopi/internal/config"
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

// agentLayout holds the parsed entries on each side of an agent row and the
// separator between them.
type agentLayout struct {
	left, right []config.Format
	separator   string
}

func newAgentLayout(line config.AgentLine) agentLayout {
	return agentLayout{left: parseFormats(line.Left), right: parseFormats(line.Right), separator: line.Separator}
}

// agentTitle is a subagent's or workflow's name and, cut to a width, what it was
// started for.
func agentTitle(agent, description string) string {
	name, desc := oneLine(agent), strings.Join(strings.Fields(oneLine(description)), " ")
	if desc == "" {
		return name
	}
	return name + " › " + cutWords(desc, descriptionWidth)
}

// agentSegment renders one part of an agent row; false draws nothing.
type agentSegment func(entry, theme) (string, bool)

// agentSegments is keyed by the config's agent segment names.
var agentSegments = map[string]agentSegment{
	"agent": func(e entry, _ theme) (string, bool) {
		if e.ID == app.Main {
			return "main", true
		}
		return agentTitle(e.Agent, e.Description), true
	},
	"tokens": func(e entry, _ theme) (string, bool) { return tokensText(e.ContextTokens), e.ContextTokens > 0 },
	"model":  func(e entry, _ theme) (string, bool) { return modelID(e.Model), e.Model != "" },
	"elapsed": func(e entry, _ theme) (string, bool) {
		return formatDuration(time.Since(e.since)), !e.since.IsZero()
	},
	"state": func(e entry, _ theme) (string, bool) {
		if e.ID == app.Main {
			return "running", e.busy
		}
		return string(e.State), e.State != ""
	},
	"activity": func(e entry, _ theme) (string, bool) { return oneLine(e.activity), e.activity != "" },
	"id":       func(e entry, _ theme) (string, bool) { return e.ID, e.ID != app.Main },
	"effort":   func(e entry, _ theme) (string, bool) { return e.Effort, e.Effort != "" },
	"context": func(e entry, th theme) (string, bool) {
		text, warn, ok := contextText(e.ContextTokens, e.ContextWindow)
		if warn {
			text = fg(th.warning).Render(text)
		}
		return text, ok
	},
	"tools": func(e entry, _ theme) (string, bool) { t := toolsText(e.ToolCalls); return t, t != "" },
	"cost":  func(e entry, _ theme) (string, bool) { t := costText(e.Cost); return t, t != "" },
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

// tokensText is the context size as "22.1k tokens".
func tokensText(n int) string { return compactCount(n) + " tokens" }

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

// entries is the agent rows' entries: main, then each subagent that is busy,
// shown or holding handed-back input. Without a subagent entry there are none.
func (m *model) entries() []entry {
	var es []entry
	for _, a := range m.agents {
		e := entry{AgentStatus: a}
		held := false
		if v := m.views[a.ID]; v != nil {
			if e.busy = v.busy; e.busy {
				e.since = v.turnStart
			}
			if m.usesActivity {
				e.activity, _ = v.tr.Running()
			}
			held = len(v.unsent) > 0
		}
		if a.ID != app.Main && !e.busy && !held && a.ID != m.shown {
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
	if !m.usesElapsed {
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
// window moves only when the cursor, or without one the shown entry, would
// leave it, so rows keep their place as the cursor moves. It shrinks to limit
// lines, more rows included, and no rows draw when not even one entry fits.
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
		from := min(max(m.agentFrom, 0), len(es)-size)
		if at >= 0 {
			from = min(max(from, at-size+1), at)
		}
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
		m.agentFrom = from
		return rows
	}
	return nil
}

// agentRow sits two in from the left edge. Left segments that do not fit are
// dropped from their right end, then the row is truncated.
func (m *model) agentRow(e entry) string {
	glyph := "○"
	if e.ID == m.shown {
		glyph = "●"
	}
	cursor := m.focus.on && e.ID == m.focus.id
	gutter := "  "
	if cursor {
		gutter = cursorMark
	}
	// paint colours plain text in the accent on the cursor row; a part that
	// carries its own colour keeps it.
	paint := func(s string) string {
		if !cursor || s == "" || strings.Contains(s, "\x1b") {
			return s
		}
		return fg(m.th.accent).Render(s)
	}
	lookup := func(name string) (string, bool) {
		text, ok := agentSegments[name](e, m.th)
		return paint(text), ok
	}
	render := func(fs []config.Format) []string { return renderEntries(fs, lookup, paint) }
	sep := paint(m.agentLine.separator)
	var right string
	if parts := render(m.agentLine.right); len(parts) > 0 {
		right = strings.Join(parts, sep) + "  "
	}
	var left string
	parts := render(m.agentLine.left)
	for n := len(parts); n >= 0; n-- {
		left = paint(gutter+glyph+" ") + strings.Join(parts[:n], sep)
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
	if m.geo.agents == 0 || len(es) == 0 || m.front() != nil {
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
	if m.geo.agents == 0 || len(es) == 0 {
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
		m.focus.at = at
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
