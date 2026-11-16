package tui

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/keyute/kopi/internal/app"
)

const (
	menuMax = 5
	// The label column is the widest label plus a gap, within these bounds.
	menuLabelMin, menuLabelMax, menuGap = 12, 32, 2
	// menuDescMin is the narrowest description worth showing.
	menuDescMin = 10
)

// menu is the completion menu for the token at the cursor.
type menu struct {
	q     app.Query
	open  bool
	items []app.Candidate
	shown []app.Candidate
	sel   int
	// seq numbers the latest load; an older candidatesMsg is dropped.
	seq int
	// dismissed is the token, Text cleared, that the menu was closed on; it
	// stays closed until the token changes. Zero means none.
	dismissed app.Query
}

// candidatesMsg is the result of load seq.
type candidatesMsg struct {
	seq int
	cs  []app.Candidate
	err error
}

// followMenu opens, refilters or closes the menu for the token at the cursor.
func (m *model) followMenu() tea.Cmd {
	mn := &m.menu
	q, ok := app.QueryAt(m.composer.value(), m.composer.ta.Line(), m.composer.ta.Column())
	if !ok || m.composer.browsing() {
		*mn = menu{seq: mn.seq}
		return nil
	}
	token := q
	token.Text = ""
	if token == mn.dismissed {
		mn.open = false
		return nil
	}
	mn.dismissed = app.Query{}
	prev := mn.q
	mn.q = q
	var cmd tea.Cmd
	if !mn.open || q.Kind != prev.Kind || q.Command != prev.Command {
		mn.items, mn.sel = nil, 0
		mn.seq++
		seq, c := mn.seq, m.c
		switch q.Kind {
		case app.KindCommand:
			for _, cm := range c.Commands() {
				mn.items = append(mn.items, app.Candidate{Value: cm.Name, Description: cm.Summary})
			}
		case app.KindArgument:
			cmd = func() tea.Msg {
				cs, err := c.Arguments(q.Command)
				return candidatesMsg{seq, cs, err}
			}
		case app.KindReference:
			cmd = func() tea.Msg {
				cs, err := c.Files()
				return candidatesMsg{seq, cs, err}
			}
		}
	} else if q.Text != prev.Text {
		mn.sel = 0
	}
	mn.open = true
	m.rematch()
	return cmd
}

func (m *model) rematch() {
	mn := &m.menu
	mn.shown = app.Match(mn.q, mn.items)
	mn.sel = min(mn.sel, max(0, len(mn.shown)-1))
}

func (m *model) loaded(msg candidatesMsg) {
	if msg.seq != m.menu.seq || !m.menu.open {
		return
	}
	if msg.err != nil {
		slog.Warn("failed to load completions", "err", msg.err)
		return
	}
	m.menu.items = msg.cs
	m.rematch()
}

// menuHeight is the rows the menu covers directly above the composer: none
// unless it is open with a candidate to show and at least one row is above the
// composer.
func (m *model) menuHeight() int {
	mn := &m.menu
	if !mn.open || len(mn.shown) == 0 {
		return 0
	}
	h := min(len(mn.shown), menuMax)
	if len(mn.shown) > h {
		h++
	}
	// The composer's shaded top row is at composerTop()-1.
	return max(0, min(h, m.composerTop()-1))
}

// menuRows draws the menu: a window of candidates around the selection, then,
// when some are left out, a row giving the selection's position.
func (m *model) menuRows() []string {
	h := m.menuHeight()
	if h == 0 {
		return nil
	}
	mn, th := &m.menu, m.th
	n := min(h, len(mn.shown), menuMax)
	if n == h && n < len(mn.shown) && n > 1 {
		// Clipped: keep the last row for the position.
		n--
	}
	start := min(max(0, mn.sel-n/2), len(mn.shown)-n)
	window := mn.shown[start : start+n]

	labels := make([]string, n)
	labelW, described := 0, false
	for i, cd := range window {
		labels[i] = oneLine(cd.Shown())
		if mn.q.Kind == app.KindCommand {
			labels[i] = "/" + labels[i]
		}
		labelW = max(labelW, ansi.StringWidth(labels[i]))
		described = described || cd.Description != ""
	}
	width := m.width - 2
	col := width
	if described {
		col = min(max(labelW+menuGap, menuLabelMin), menuLabelMax)
		if width-col < menuDescMin {
			described, col = false, width
		}
	}

	rows := make([]string, 0, h)
	for i, cd := range window {
		label := labels[i]
		if described {
			label = ansi.Truncate(label, col-menuGap, "…")
			label += strings.Repeat(" ", col-ansi.StringWidth(label))
		} else {
			label = ansi.Truncate(label, col, "…")
		}
		desc := ""
		if described {
			desc = ansi.Truncate(oneLine(cd.Description), width-col, "…")
		}
		marker, ls, ds := "  ", fg(th.text), fg(th.muted)
		if start+i == mn.sel {
			marker, ls, ds = "❭ ", fg(th.accent), fg(th.accent)
			marker = ls.Render(marker)
		}
		rows = append(rows, ansi.Truncate(marker+ls.Render(label)+ds.Render(desc), m.width, ""))
	}
	if len(mn.shown) > n && n < h {
		rows = append(rows, fg(th.dim).Render(ansi.Truncate(fmt.Sprintf("  %d of %d", mn.sel+1, len(mn.shown)), m.width, "")))
	}
	return rows
}

// oneLine is s with control characters removed and line breaks as spaces.
func oneLine(s string) string {
	return strings.Join(wrap(s, beyond), " ")
}

// menuKey handles k while the menu shows, reporting whether it did.
func (m *model) menuKey(k tea.KeyPressMsg) (tea.Cmd, bool) {
	km, mn := &m.keys, &m.menu
	switch {
	case key.Matches(k, km.menuUp):
		mn.sel = (mn.sel - 1 + len(mn.shown)) % len(mn.shown)
	case key.Matches(k, km.menuDown):
		mn.sel = (mn.sel + 1) % len(mn.shown)
	case key.Matches(k, km.complete):
		return m.accept(false), true
	case key.Matches(k, km.submit):
		return m.accept(true), true
	case key.Matches(k, km.menuClose):
		mn.dismissed = mn.q
		mn.dismissed.Text = ""
		mn.open = false
	default:
		return nil, false
	}
	return nil, true
}

// accept puts the selected candidate in place of the token; with run it then
// submits, except for a file reference or a command that takes an argument,
// which are only ever filled in.
func (m *model) accept(run bool) tea.Cmd {
	q, cd := m.menu.q, m.menu.shown[m.menu.sel]
	var ins string
	switch q.Kind {
	case app.KindCommand:
		if slices.ContainsFunc(m.c.Commands(), func(c app.Command) bool { return c.Name == cd.Value && c.Argument }) {
			run = false
		}
		ins = "/" + cd.Value
		if !run {
			ins += " "
		}
	case app.KindArgument:
		ins = cd.Value
	case app.KindReference:
		run = false
		ins = app.Reference(cd.Value)
		// A directory stays open so its entries list next.
		if !strings.HasSuffix(cd.Value, "/") {
			ins += " "
		}
	}
	cmd := m.composer.replace(q.Start, q.End, ins)
	if !run {
		return cmd
	}
	return tea.Batch(cmd, m.submit())
}
