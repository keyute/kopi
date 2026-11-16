package tui

import (
	"log/slog"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
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
	regionComposer
)

// drag is the left press being held.
type drag struct {
	region region
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

// clearSelection also discards the transcript drag's coordinates. A pending
// auto-scroll tick will stop when it finds no transcript drag.
func (m *model) clearSelection() {
	m.sel = selection{}
	if m.drag.region == regionTranscript {
		m.drag = drag{}
	}
}

func (m *model) mouse(msg tea.MouseMsg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.MouseClickMsg:
		m.press(msg.Mouse())
	case tea.MouseMotionMsg:
		return m.motion(msg.Mouse())
	case tea.MouseReleaseMsg:
		return m.release()
	case tea.MouseWheelMsg:
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

// press starts a drag in the transcript or the composer. A double click
// selects the word under the pointer and a triple click its line; in the
// composer that is the logical line across soft wraps. A press on the menu,
// which covers the rows above the composer, is ignored.
func (m *model) press(mo tea.Mouse) {
	if menuEnd := m.composerTop() - 1; mo.Y >= menuEnd-m.menuHeight() && mo.Y < menuEnd {
		return
	}
	m.sel, m.drag = selection{}, drag{}
	if mo.Button != tea.MouseLeft {
		return
	}
	n := m.clicks.press(time.Now(), mo.X, mo.Y)
	switch top := m.composerTop(); {
	case mo.Y >= 0 && mo.Y < m.transcriptHeight():
		p := point{line: m.top() + mo.Y, col: mo.X}
		unit := m.unitAt(p, n)
		m.drag = drag{region: regionTranscript, at: mo, press: p, count: n, unit: unit, x: mo.X}
		m.sel = unit
	case mo.Y >= top && mo.Y < top+m.composer.height():
		m.drag = drag{region: regionComposer}
		m.composer.press(mo.X, mo.Y-top, n)
	}
}

// unitAt is what an n-th press at p selects in the transcript: the point
// itself, its word or its line.
func (m *model) unitAt(p point, n int) selection {
	if n == 1 || p.line >= len(m.lines) {
		return selection{anchor: p, head: p}
	}
	text := m.lines[p.line].text
	if n == 2 {
		from, to := wordAt(text, p.col)
		return selection{anchor: point{p.line, from}, head: point{p.line, to}}
	}
	return selection{anchor: point{p.line, 0}, head: point{p.line, ansi.StringWidth(text)}}
}

// motion extends the drag in the region it began in. In the transcript the
// pointer is clamped to the transcript rows, and holding it on the top or
// bottom row scrolls.
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
	case regionComposer:
		m.composer.drag(mo.X, mo.Y-m.composerTop())
	}
	return nil
}

// extend stretches the selection to the drag's column on screen row y, by
// the unit the press selected: characters, words or lines.
func (m *model) extend(y int) {
	p := point{line: m.top() + y, col: m.drag.x}
	m.sel = stretch(m.drag.unit, m.unitAt(p, m.drag.count))
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

// release ends the drag. A single click in the transcript that never moved
// toggles the fold under it; any other transcript drag, and a composer drag,
// copies what it selected.
func (m *model) release() tea.Cmd {
	d := m.drag
	m.drag = drag{}
	switch d.region {
	case regionTranscript:
		if d.count == 1 && !d.moved {
			m.sel = selection{}
			m.click(d.at)
			return nil
		}
		if m.sel.empty() {
			return nil
		}
		return m.copy(m.sel.text(m.lines))
	case regionComposer:
		if text, ok := m.composer.release(); ok {
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
	i := m.top() + mo.Y
	if i >= len(m.lines) {
		return
	}
	h := m.lines[i].hit
	if h.kind == hitNone || m.folds.all {
		return
	}
	m.holdTop(func() { m.folds.click(h) })
}
