package tui

import (
	"log/slog"
	"time"

	tea "charm.land/bubbletea/v2"
)

const (
	wheelStep = 3
	// autoScrollEvery is how often a drag held at a transcript edge scrolls
	// one line.
	autoScrollEvery = 50 * time.Millisecond
)

// autoScrollMsg is a tick of a drag held at a transcript edge.
type autoScrollMsg struct{}

// region is where a drag began; it keeps the drag until release.
type region int

const (
	regionNone region = iota
	regionTranscript
	// regionRows is the rows below the transcript, selected as drawn.
	regionRows
	// regionField is an editable text area, which keeps its own selection.
	regionField
)

// drag is the left press being held.
type drag struct {
	region region
	field  *composer
	// at is the press, which a single click acts on at release.
	at    tea.Mouse
	press point
	count int
	// unit is what the press selected; a drag extends from it.
	unit  selection
	moved bool
	// x is the pointer's last column; edge is -1 or 1 while it is held at the
	// transcript's top or bottom row, else 0.
	x, edge int
}

// clearSelection also discards the transcript or lower-row drag's
// coordinates. A pending auto-scroll tick will stop when it finds no
// transcript drag.
func (m *model) clearSelection() {
	m.sel, m.selIn = selection{}, regionNone
	if m.drag.region == regionTranscript || m.drag.region == regionRows {
		m.drag = drag{}
	}
}

// deselect clears every selection, the text areas' included, so only one is
// ever live.
func (m *model) deselect() {
	m.clearSelection()
	m.composer.ta.ClearSelection()
	if m.dialog != nil {
		for _, o := range m.dialog.options {
			if o.field != nil {
				o.field.ta.ClearSelection()
			}
		}
	}
}

// space is the lines a selection in r indexes, and shift, which takes a line
// index to its screen row.
func (m *model) space(r region) (lines []line, shift int) {
	if r == regionRows {
		return m.below, m.geo.transcript
	}
	return m.view().lines, -m.top()
}

// area is where field c is drawn, if it is on screen.
func (m *model) area(c *composer) (fieldArea, bool) {
	for _, a := range m.geo.fields {
		if a.c == c {
			return a, true
		}
	}
	return fieldArea{}, false
}

func (m *model) mouse(msg tea.MouseMsg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.MouseClickMsg:
		return m.press(msg.Mouse())
	case tea.MouseMotionMsg:
		return m.motion(msg.Mouse())
	case tea.MouseReleaseMsg:
		return m.release()
	case tea.MouseWheelMsg:
		if m.question != nil && m.approval == nil && msg.Y >= m.geo.composerTop-1 && msg.Y < m.geo.composerTop-1+len(m.prompt) {
			switch msg.Button {
			case tea.MouseWheelUp:
				m.question.scroll -= wheelStep
			case tea.MouseWheelDown:
				m.question.scroll += wheelStep
			}
			return nil
		}
		top := m.top()
		switch msg.Button {
		case tea.MouseWheelUp:
			m.scroll(-wheelStep)
		case tea.MouseWheelDown:
			m.scroll(wheelStep)
		}
		if m.top() != top && m.drag.region == regionTranscript && m.drag.count == 1 && !m.drag.moved {
			m.drag = drag{}
		}
	}
	return nil
}

// press starts a drag in a field, the transcript or the rows below it; a
// field takes the press first. A double click selects the word under the
// pointer and a triple click its line; in a field that is the logical line
// across soft wraps.
func (m *model) press(mo tea.Mouse) tea.Cmd {
	m.deselect()
	m.drag = drag{}
	if mo.Button != tea.MouseLeft {
		return nil
	}
	n := m.clicks.press(time.Now(), mo.X, mo.Y)
	for _, a := range m.geo.fields {
		if mo.Y >= a.y && mo.Y < a.y+a.h && mo.X >= a.x {
			if a.c == &m.composer {
				m.focus = agentFocus{}
			}
			var cmd tea.Cmd
			if m.dialog != nil {
				cmd = m.dialog.pick(a.c)
			}
			m.drag = drag{region: regionField, field: a.c}
			a.c.press(mo.X-a.x, mo.Y-a.y, n)
			return cmd
		}
	}
	r := regionTranscript
	if mo.Y >= m.geo.transcript {
		r = regionRows
	}
	lines, shift := m.space(r)
	if mo.Y < 0 || (r == regionRows && mo.Y-shift >= len(lines)) {
		return nil
	}
	p := point{line: mo.Y - shift, col: mo.X}
	unit := unitAt(lines, p, n)
	m.sel, m.selIn = unit, r
	m.drag = drag{region: r, at: mo, press: p, count: n, unit: unit, x: mo.X}
	return nil
}

// holding reports a drag held in the composer; the menu holds still until the
// button is released.
func (m *model) holding() bool {
	return m.drag.region == regionField && m.drag.field == &m.composer
}

// motion extends the drag in the region it began in. In the transcript or the
// lower rows the pointer is clamped to them; holding it on the transcript's
// top or bottom row scrolls.
func (m *model) motion(mo tea.Mouse) tea.Cmd {
	switch m.drag.region {
	case regionTranscript:
		h := m.transcriptHeight()
		m.drag.x = max(0, mo.X)
		m.extend(min(max(0, mo.Y), h-1))
		switch {
		case mo.Y <= 0:
			m.drag.edge = -1
		case mo.Y >= h-1:
			m.drag.edge = 1
		default:
			m.drag.edge = 0
		}
		if m.drag.edge != 0 && !m.ticking {
			return m.armAutoScroll()
		}
	case regionRows:
		m.drag.x = max(0, mo.X)
		m.extend(min(max(m.geo.transcript, mo.Y), m.geo.transcript+len(m.below)-1))
	case regionField:
		a, ok := m.area(m.drag.field)
		if !ok {
			m.drag = drag{}
			return nil
		}
		a.c.drag(mo.X-a.x, mo.Y-a.y)
	}
	return nil
}

// extend stretches the selection to the drag's column on screen row y, by
// the unit the press selected: characters, words or lines.
func (m *model) extend(y int) {
	lines, shift := m.space(m.selIn)
	p := point{line: y - shift, col: m.drag.x}
	m.sel = stretch(m.drag.unit, unitAt(lines, p, m.drag.count))
	if p != m.drag.press {
		m.drag.moved = true
	}
}

func (m *model) armAutoScroll() tea.Cmd {
	m.ticking = true
	return tea.Tick(autoScrollEvery, func(time.Time) tea.Msg { return autoScrollMsg{} })
}

// autoScroll scrolls one line toward the edge the drag is held at and moves
// the selection's head onto the line revealed. The chain stops once the drag
// ends, leaves the edge or cannot scroll further.
func (m *model) autoScroll() tea.Cmd {
	m.ticking = false
	if m.drag.region != regionTranscript || m.drag.edge == 0 {
		return nil
	}
	top := m.top()
	m.scroll(m.drag.edge)
	if m.top() == top {
		return nil
	}
	y := 0
	if m.drag.edge > 0 {
		y = m.transcriptHeight() - 1
	}
	m.extend(y)
	return m.armAutoScroll()
}

// release ends the drag. A single click that never moved toggles the fold
// under it in the transcript and only clears below it; any other drag copies
// what it selected, a field's only while the field is on screen.
func (m *model) release() tea.Cmd {
	d := m.drag
	m.drag = drag{}
	switch d.region {
	case regionTranscript:
		if d.count == 1 && !d.moved {
			m.clearSelection()
			m.click(d.at)
			return nil
		}
		if m.sel.empty() {
			return nil
		}
		return m.copy(m.sel.text(m.view().lines))
	case regionRows:
		if d.count == 1 && !d.moved {
			m.clearSelection()
			return nil
		}
		if m.sel.empty() {
			return nil
		}
		return m.copy(m.sel.text(m.below))
	case regionField:
		if _, ok := m.area(d.field); !ok {
			return nil
		}
		if text, ok := d.field.release(); ok {
			return m.copy(text)
		}
	}
	return nil
}

// copy puts text on the clipboard both through the terminal (OSC 52, which
// reaches the local machine over SSH and tmux) and through the system
// clipboard, for terminals that ignore OSC 52.
func (m *model) copy(text string) tea.Cmd {
	if text == "" {
		return nil
	}
	clip := m.clip
	write := func() tea.Msg {
		if err := clip.WriteText(text); err != nil {
			slog.Warn("failed to copy to the system clipboard", "err", err)
		}
		return nil
	}
	return tea.Batch(tea.Batch(tea.SetClipboard(text), write), m.flash("Copied"))
}

// click toggles the fold a left click in the transcript lands on. Clicks
// are inert while everything is shown.
func (m *model) click(mo tea.Mouse) {
	if mo.Button != tea.MouseLeft || mo.Y < 0 || mo.Y >= m.transcriptHeight() {
		return
	}
	v := m.view()
	i := m.top() + mo.Y
	if i >= len(v.lines) {
		return
	}
	h := v.lines[i].hit
	if h.kind == hitNone || v.folds.all {
		return
	}
	m.holdTop(func() { v.folds.click(h) })
}
