package tui

import (
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
	// optional reports that q's command also runs without an argument.
	optional bool
	// arg is q's whole argument, trimmed: q.Text stops at the cursor, but Enter
	// sends or replaces all of it.
	arg string
	// picked reports that Up, Down or Tab chose the row since the token's text
	// changed; while pending they are held instead.
	picked bool
	// seq numbers the latest load; an older candidatesMsg is dropped.
	seq int
	// typed reports that q's command is asked again for every change of the
	// argument and returns candidates already filtered: items are shown as
	// given.
	typed bool
	// pending reports that a typed load for q.Text has not arrived: the rows
	// shown are for older text, so none is lit.
	pending bool
	// held is the Up, Down and Tab presses made while pending, run against the
	// reply's rows when it arrives.
	held []tea.KeyPressMsg
	// carry is the held keys a replayed key left unreplayed by changing the
	// draft: followMenu hands them to the load that change asks.
	carry []tea.KeyPressMsg
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
	v := m.composer.value()
	q, ok := app.QueryAt(v, m.composer.ta.Line(), m.composer.ta.Column())
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
	prev, prevArg := mn.q, mn.arg
	mn.q, mn.arg = q, ""
	if q.Kind == app.KindArgument {
		l := []rune(strings.Split(v, "\n")[q.Line])
		mn.arg = strings.TrimSpace(string(l[q.Start:q.End]))
	}
	var cmd tea.Cmd
	if !mn.open || q.Kind != prev.Kind || q.Command != prev.Command {
		mn.items, mn.sel, mn.picked, mn.optional, mn.typed, mn.pending, mn.held, mn.carry = nil, 0, false, false, false, false, nil, nil
		mn.seq++
		seq, c := mn.seq, m.c
		switch q.Kind {
		case app.KindCommand:
			for _, cm := range c.Commands() {
				mn.items = append(mn.items, app.Candidate{Value: cm.Name, Description: cm.Summary})
			}
		case app.KindArgument:
			cms := c.Commands()
			if i := slices.IndexFunc(cms, func(cm app.Command) bool { return cm.Name == q.Command }); i >= 0 {
				mn.optional, mn.typed = cms[i].Optional, cms[i].Typed
			}
			cmd = m.ask()
		case app.KindReference:
			cmd = func() tea.Msg {
				cs, err := c.Files()
				return candidatesMsg{seq, cs, err}
			}
		}
	} else if q.Text != prev.Text || mn.arg != prevArg {
		mn.sel, mn.picked, mn.held = 0, false, nil
		// The rows stay until the reply, so the menu does not blink between keys.
		if mn.typed && q.Text != prev.Text {
			mn.seq++
			cmd = m.ask()
			mn.held = mn.carry
		}
		mn.carry = nil
	}
	mn.open = true
	m.rematch()
	return cmd
}

// ask loads the candidates of the argument's command for the text typed so
// far, as load number seq.
func (m *model) ask() tea.Cmd {
	mn := &m.menu
	mn.pending = mn.typed
	seq, c, q := mn.seq, m.c, mn.q
	return func() tea.Msg {
		cs, err := c.Arguments(q.Command, q.Text)
		return candidatesMsg{seq, cs, err}
	}
}

func (m *model) rematch() {
	mn := &m.menu
	if mn.typed {
		mn.shown = mn.items
	} else {
		mn.shown = app.Match(mn.q, mn.items)
	}
	mn.sel = min(mn.sel, max(0, len(mn.shown)-1))
}

// loaded takes a reply. For a typed menu its rows start unpicked, then the
// keys held while it was pending run against them.
func (m *model) loaded(msg candidatesMsg) tea.Cmd {
	mn := &m.menu
	if msg.seq != mn.seq || !mn.open {
		return nil
	}
	held := mn.held
	mn.held = nil
	if mn.typed {
		mn.pending = false
		mn.sel, mn.picked = 0, false
	}
	if msg.err != nil {
		slog.Warn("failed to load completions", "err", msg.err)
		if mn.typed {
			mn.items = nil
			m.rematch()
		}
		return m.replay(held)
	}
	mn.items = msg.cs
	if m.holding() {
		return nil
	}
	m.rematch()
	if len(mn.shown) == 0 {
		return m.replay(held)
	}
	var cmds []tea.Cmd
	for i, k := range held {
		draft := m.composer.value()
		cmd, _ := m.menuKey(k)
		cmds = append(cmds, cmd)
		// The rows no longer belong to a draft a key changed: the keys left wait
		// for the reply to the new draft, as if pressed after the change.
		if m.composer.value() != draft {
			mn.carry = held[i+1:]
			break
		}
	}
	return tea.Batch(cmds...)
}

// replay runs keys held for a reply with no rows as the composer takes them
// with no menu, so an Up held for history recall is not lost.
func (m *model) replay(keys []tea.KeyPressMsg) tea.Cmd {
	var cmds []tea.Cmd
	for _, k := range keys {
		cmds = append(cmds, m.key(k))
	}
	return tea.Batch(cmds...)
}

// want is the rows the menu would take with room for all of them: its
// candidates up to menuMax, plus a position row when some are left out.
func (mn *menu) want() int {
	if !mn.open || len(mn.shown) == 0 {
		return 0
	}
	h := min(len(mn.shown), menuMax)
	if len(mn.shown) > h {
		h++
	}
	return h
}

// lit reports whether the selected row is the one Enter takes. It is not when
// the draft already runs as typed: an optional argument left out, or an
// argument typed in full, nor while the rows are for older text. Up, Down or
// Tab lights it.
func (mn *menu) lit() bool {
	if mn.pending {
		return false
	}
	if mn.picked || mn.q.Kind != app.KindArgument {
		return true
	}
	if mn.arg == "" {
		return !mn.optional
	}
	return !slices.ContainsFunc(mn.items, func(cd app.Candidate) bool { return cd.Value == mn.arg || cd.Value == mn.arg+"/" })
}

func (m *model) menuHeight() int { return m.geo.menu }

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
	start := around(mn.sel, 1, n, len(mn.shown))
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
		if start+i == mn.sel && mn.lit() {
			marker, ls, ds = cursorMark, fg(th.accent), fg(th.accent)
			marker = ls.Render(marker)
		}
		rows = append(rows, ansi.Truncate(marker+ls.Render(label)+ds.Render(desc), m.width, ""))
	}
	if len(mn.shown) > n && n < h {
		rows = append(rows, positionRow(mn.sel, len(mn.shown), m.width, th))
	}
	return rows
}

// oneLine is s with control characters removed and line breaks as spaces.
func oneLine(s string) string {
	return strings.Join(wrap(s, beyond), " ")
}

// navKey reports whether k picks a row of the menu.
func (m *model) navKey(k tea.KeyPressMsg) bool {
	km := &m.keys
	return key.Matches(k, km.menuUp) || key.Matches(k, km.menuDown) || key.Matches(k, km.complete)
}

// menuKey handles k while the menu shows, reporting whether it did. Up, Down
// and Tab while a reply is pending are only held for it.
func (m *model) menuKey(k tea.KeyPressMsg) (tea.Cmd, bool) {
	km, mn := &m.keys, &m.menu
	if mn.pending && m.navKey(k) {
		mn.held = append(mn.held, k)
		return nil, true
	}
	if len(mn.shown) == 0 {
		return nil, false
	}
	switch {
	case key.Matches(k, km.menuUp):
		mn.sel = (mn.sel - 1 + len(mn.shown)) % len(mn.shown)
		mn.picked = true
	case key.Matches(k, km.menuDown):
		if mn.lit() {
			mn.sel = (mn.sel + 1) % len(mn.shown)
		}
		mn.picked = true
	case key.Matches(k, km.complete):
		if !mn.lit() {
			mn.picked = true
			return nil, true
		}
		return m.accept(false), true
	case key.Matches(k, km.submit):
		if !mn.lit() {
			return nil, false
		}
		return m.accept(true), true
	case key.Matches(k, km.menuClose):
		mn.dismissed = mn.q
		mn.dismissed.Text = ""
		mn.open = false
		mn.held = nil
	default:
		return nil, false
	}
	return nil, true
}

// accept puts the selected candidate in place of the token; with run it then
// submits, except for a file reference, a candidate ending in "/" that is not
// already the argument as typed, or a command that needs an argument or a
// skill, which are only ever filled in.
func (m *model) accept(run bool) tea.Cmd {
	q, cd := m.menu.q, m.menu.shown[m.menu.sel]
	var ins string
	switch q.Kind {
	case app.KindCommand:
		if slices.ContainsFunc(m.c.Commands(), func(c app.Command) bool { return c.Name == cd.Value && ((c.Argument && !c.Optional) || c.Skill) }) {
			run = false
		}
		ins = "/" + cd.Value
		if !run {
			ins += " "
		}
	case app.KindArgument:
		ins = cd.Value
		if strings.HasSuffix(cd.Value, "/") && cd.Value != m.menu.arg {
			run = false
		}
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
