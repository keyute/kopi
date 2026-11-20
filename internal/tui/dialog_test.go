package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/config"
)

func plainRows(rows []string) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = strings.TrimRight(ansi.Strip(r), " ")
	}
	return out
}

// fieldDialog has a plain option, then an option with a field, then labels
// more plain options.
func fieldDialog(more ...string) *dialog {
	km := newKeyMap(config.DefaultKeys())
	f := newField(mocha(), km.newline, "What should change?")
	d := &dialog{title: "Pick", options: []option{{label: "Yes"}, {label: "No", field: &f}}}
	for _, l := range more {
		d.options = append(d.options, option{label: l})
	}
	return d
}

// optionRows are d's option rows at width.
func optionRows(d *dialog, width int) []string {
	rows := d.rows(width, 100, 5, mocha())
	return rows[3 : len(rows)-1]
}

func TestDialogRows(t *testing.T) {
	th := mocha()
	rule20 := strings.Repeat("─", 20)
	d := dialog{title: "Pick", body: []string{"- a"}, note: "why", options: []option{{label: "Yes"}, {label: "No"}}, sel: 1}
	rows := d.rows(20, 100, 1, th)
	assert.Equal(t, []string{rule20, "  Pick", "  - a", "  why", "", "  1. Yes", "❭ 2. No", rule20}, plainRows(rows))
	assert.Equal(t, fg(th.frame).Render(rule20), rows[0])

	d.note = ""
	assert.Equal(t, []string{rule20, "  Pick", "  - a", "", "  1. Yes", "❭ 2. No", rule20}, plainRows(d.rows(20, 100, 1, th)))

	d.options = []option{{label: "a long option that has to wrap"}}
	d.sel = 0
	for _, r := range d.rows(20, 100, 1, th) {
		assert.LessOrEqual(t, ansi.StringWidth(r), 20, r)
	}
}

func TestDialogKey(t *testing.T) {
	km := newKeyMap(config.DefaultKeys())
	for _, tt := range []struct {
		name      string
		sel       int
		draft     string
		msg       tea.KeyPressMsg
		want      int
		wantPick  bool
		wantDraft string
	}{
		{name: "up at the top", sel: 0, msg: up, want: 0},
		{name: "up", sel: 1, msg: up, want: 0},
		{name: "down", sel: 0, msg: down, want: 1},
		{name: "down at the bottom", sel: 2, msg: down, want: 2},
		{name: "enter", sel: 2, msg: enter, want: 2, wantPick: true},
		{name: "plain digit answers", sel: 0, msg: digit('3'), want: 2, wantPick: true},
		{name: "field digit focuses", sel: 0, msg: digit('2'), want: 1},
		{name: "zero", sel: 2, msg: digit('0'), want: 2},
		{name: "digit past n", sel: 2, msg: digit('4'), want: 2},
		{name: "other key", sel: 2, msg: digit('x'), want: 2},
		{name: "digits are text in the field", sel: 1, msg: digit('3'), want: 1, wantDraft: "3"},
		{name: "enter in the field picks", sel: 1, draft: "why", msg: enter, want: 1, wantPick: true, wantDraft: "why"},
		{name: "up leaves from the first row", sel: 1, draft: "why", msg: up, want: 0, wantDraft: "why"},
		{name: "up stays below the first row", sel: 1, draft: "a\nb", msg: up, want: 1, wantDraft: "a\nb"},
		{name: "down leaves from the last row", sel: 1, draft: "why", msg: down, want: 2, wantDraft: "why"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := fieldDialog("Later")
			d.move(tt.sel)
			d.paste(tt.draft)
			picked, _ := d.key(tt.msg, &km)
			assert.Equal(t, tt.want, d.sel)
			assert.Equal(t, tt.wantPick, picked)
			assert.Equal(t, tt.wantDraft, d.options[1].field.value())
			assert.Equal(t, tt.want == 1, d.options[1].field.ta.Focused())
		})
	}
}

func TestDialogDownFromFieldNeedsAnOptionBelow(t *testing.T) {
	km := newKeyMap(config.DefaultKeys())
	d := fieldDialog()
	d.move(1)
	d.paste("a")
	d.key(down, &km)
	assert.Equal(t, 1, d.sel)
	assert.Equal(t, "a", d.options[1].field.value())
}

func TestDialogUpReturnsFromTheFirstVisualRow(t *testing.T) {
	km := newKeyMap(config.DefaultKeys())
	d := fieldDialog()
	d.move(1)
	d.paste("a\nb")
	optionRows(d, 30)
	d.key(up, &km)
	assert.Equal(t, 1, d.sel, "the first press moves to the first row")
	d.key(up, &km)
	assert.Equal(t, 0, d.sel)
	assert.Equal(t, "a\nb", d.options[1].field.value(), "the draft is kept")
}

func TestDialogFieldLayout(t *testing.T) {
	const width = 30
	// "❭ 2. No  " takes 9 cells.
	inline := width - 9
	km := newKeyMap(config.DefaultKeys())
	d := fieldDialog()
	assert.Equal(t, []string{"❭ 1. Yes", "  2. No  What should change?"}, plainRows(optionRows(d, width)), "the placeholder sits beside the label")

	d.move(1)
	fits := strings.Repeat("x", inline-1)
	d.paste(fits)
	assert.Equal(t, []string{"  1. Yes", "❭ 2. No  " + fits}, plainRows(optionRows(d, width)))

	d.key(digit('y'), &km)
	assert.Equal(t, []string{"  1. Yes", "❭ 2. No", "  " + fits + "y"}, plainRows(optionRows(d, width)), "a draft that fills the row moves below")

	d.key(tea.KeyPressMsg{Code: tea.KeyBackspace}, &km)
	assert.Equal(t, []string{"  1. Yes", "❭ 2. No  " + fits}, plainRows(optionRows(d, width)), "shortening returns it beside the label")

	d.key(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift}, &km)
	d.key(digit('z'), &km)
	assert.Equal(t, []string{"  1. Yes", "❭ 2. No", "  " + fits, "  z"}, plainRows(optionRows(d, width)), "a newline moves it below")
}

func TestDialogRowsRecordFieldAreas(t *testing.T) {
	d := fieldDialog()
	d.rows(30, 100, 5, mocha())
	f := d.options[1].field
	// "  2. No  " takes 9 cells.
	assert.Equal(t, []fieldArea{{c: f, x: 9, y: 4, h: 1}}, d.areas, "beside its label")

	d.move(1)
	d.paste("a\nb")
	rows := plainRows(d.rows(30, 100, 5, mocha()))
	require.Equal(t, "❭ 2. No", rows[4])
	assert.Equal(t, []fieldArea{{c: f, x: 2, y: 5, h: 2}}, d.areas, "below its label")
}

func TestDialogRowsRecordOptionBoundaries(t *testing.T) {
	d := fieldDialog("Later")
	d.rows(30, 100, 5, mocha())
	assert.Equal(t, []int{3, 4, 5, 6}, d.optionStarts)

	d.move(1)
	d.paste("a\nb")
	d.rows(30, 100, 5, mocha())
	assert.Equal(t, []int{3, 4, 7, 8}, d.optionStarts)
}

func TestDialogFieldNarrowStacks(t *testing.T) {
	d := fieldDialog()
	d.move(1)
	assert.Equal(t, []string{"  1. Yes", "❭ 2. No", "  What"}, plainRows(optionRows(d, 10)))
}

func TestDialogFieldRowsFitTheWidth(t *testing.T) {
	for _, draft := range []string{"", "short", strings.Repeat("word ", 30), "a\nb\nc", strings.Repeat("界", 40)} {
		for _, width := range []int{8, 12, 20, 30, 60} {
			for sel := range 2 {
				d := fieldDialog()
				d.move(1)
				d.paste(draft)
				d.move(sel)
				for _, r := range d.rows(width, 100, 5, mocha()) {
					assert.LessOrEqual(t, ansi.StringWidth(r), width, "%q at %d: %q", draft, width, r)
				}
			}
		}
	}
}

func TestDialogFieldDrawsNoCursorWhenBlurred(t *testing.T) {
	d := fieldDialog()
	d.move(1)
	d.paste("ab")
	_, focused, ok := strings.Cut(optionRows(d, 30)[1], "No  ")
	require.True(t, ok)
	assert.NotEqual(t, ansi.Strip(focused), focused, "a focused field draws its cursor")
	d.move(0)
	_, blurred, ok := strings.Cut(optionRows(d, 30)[1], "No  ")
	require.True(t, ok)
	assert.Equal(t, ansi.Strip(blurred), blurred, "a blurred field draws plain text")
	assert.Equal(t, "ab", strings.TrimRight(blurred, " "))
}

func TestDialogPaste(t *testing.T) {
	d := fieldDialog()
	assert.Nil(t, d.paste("text"))
	assert.Empty(t, d.options[1].field.value(), "nothing is pasted without a field")
	assert.Empty(t, d.text())

	d.move(1)
	big := strings.Repeat("line\n", 20)
	d.paste(big)
	assert.Equal(t, "[paste #1 +20 lines]", d.options[1].field.value())
	assert.Equal(t, big, d.text())
	require.Len(t, d.options[1].field.blocks, 1)
}

func TestDialogDetailSitsUnderTheLabelAndBeforeABelowField(t *testing.T) {
	d := fieldDialog("Later")
	d.options[0].detail = []string{"says yes"}
	d.options[1].detail = []string{"says no", "twice"}
	assert.Equal(t, []string{"❭ 1. Yes", "  says yes", "  2. No  What should change?", "  says no", "  twice", "  3. Later"}, plainRows(optionRows(d, 40)), "a field beside its label stays on the label row")
	assert.Equal(t, []int{3, 5, 8, 9}, d.optionStarts)

	d.move(1)
	d.paste("a\nb")
	assert.Equal(t, []string{"  1. Yes", "  says yes", "❭ 2. No", "  says no", "  twice", "  a", "  b", "  3. Later"}, plainRows(optionRows(d, 40)), "a field below the label follows the detail")
	assert.Equal(t, []int{3, 5, 10, 11}, d.optionStarts)
	require.Len(t, d.areas, 1)
	assert.Equal(t, 8, d.areas[0].y)
}

func TestDialogLabellessFieldIsNumberedUnlessSole(t *testing.T) {
	km := newKeyMap(config.DefaultKeys())
	f := newField(mocha(), km.newline, "Type something.")
	sole := &dialog{title: "Pick", options: []option{{field: &f}}}
	assert.Equal(t, []string{"❭ Type something."}, plainRows(optionRows(sole, 30)), "a sole option is alone and unnumbered")

	g := newField(mocha(), km.newline, "Type something.")
	d := &dialog{title: "Pick", options: []option{{label: "Yes"}, {field: &g, mark: "[ ] "}}}
	assert.Equal(t, []string{"❭ 1. Yes", "  2. [ ] Type something."}, plainRows(optionRows(d, 30)))
	d.move(1)
	g.paste("a\nb")
	assert.Equal(t, []string{"  1. Yes", "❭ 2. [ ] a", "         b"}, plainRows(optionRows(d, 30)), "rows hang under the field's text")
	require.Len(t, d.areas, 1)
	assert.Equal(t, 9, d.areas[0].x)
}

func TestDialogChosenBoldsTheLabel(t *testing.T) {
	th := mocha()
	d := &dialog{title: "Pick", options: []option{{label: "Yes", chosen: true}, {label: "No"}}}
	rows := optionRows(d, 30)
	assert.Equal(t, fg(th.accent).Render("❭ ")+fg(th.accent).Bold(true).Render("1. Yes"), rows[0])
	d.move(1)
	rows = optionRows(d, 30)
	assert.Equal(t, fg(th.accent).Render("  ")+lipgloss.NewStyle().Bold(true).Render("1. Yes"), rows[0])
}

func TestDialogChosenLabellessFieldEndsWithATickUnlessSelected(t *testing.T) {
	km := newKeyMap(config.DefaultKeys())
	f := newField(mocha(), km.newline, "Type something.")
	f.ta.SetValue("mine")
	d := &dialog{title: "Pick", options: []option{{label: "Yes"}, {field: &f, chosen: true}}}
	assert.Equal(t, []string{"❭ 1. Yes", "  2. mine ✔"}, plainRows(optionRows(d, 30)))
	d.move(1)
	assert.Equal(t, []string{"  1. Yes", "❭ 2. mine"}, plainRows(optionRows(d, 30)))
}

func TestDialogFieldPrefixDrawsTheFieldBelowTheDetail(t *testing.T) {
	km := newKeyMap(config.DefaultKeys())
	f := newField(mocha(), km.newline, "Add a note")
	d := &dialog{title: "Pick", options: []option{{label: "Yes", detail: []string{"says yes"}, field: &f, fieldPrefix: "↳ note: "}, {label: "No"}}}
	assert.Equal(t, []string{"❭ 1. Yes", "  says yes", "  ↳ note: Add a note", "  2. No"}, plainRows(optionRows(d, 40)))
	require.Len(t, d.areas, 1)
	assert.Equal(t, fieldArea{c: &f, x: 10, y: 5, h: 1}, d.areas[0])
	assert.Equal(t, []int{3, 6, 7}, d.optionStarts)

	f.ta.SetValue("one\ntwo")
	assert.Equal(t, []string{"❭ 1. Yes", "  says yes", "  ↳ note: one", "          two", "  2. No"}, plainRows(optionRows(d, 40)))
}

func TestScrollerWindow(t *testing.T) {
	var lines []string
	for i := range 10 {
		lines = append(lines, fmt.Sprintf("l%d", i+1))
	}
	th := mocha()
	for _, tt := range []struct {
		name        string
		off, room   int
		want        []string
		below       int
		cut         bool
		wantOff     int
		wantVisible int
	}{
		{name: "fits", room: 10, want: lines, wantVisible: 10},
		{name: "fits from a stale offset", off: 4, room: 12, want: lines, wantVisible: 10},
		{name: "no room", room: 0},
		{name: "top", room: 4, want: []string{"l1", "l2", "l3", "… 7 more lines"}, below: 7, cut: true, wantVisible: 2},
		{name: "middle", off: 4, room: 4, want: []string{"… 4 earlier lines", "l5", "l6", "… 4 more lines"}, below: 4, cut: true, wantOff: 4, wantVisible: 2},
		{name: "end", off: 100, room: 4, want: []string{"… 7 earlier lines", "l8", "l9", "l10"}, wantOff: 7, cut: true, wantVisible: 2},
		{name: "last row left out", off: 6, room: 4, want: []string{"… 6 earlier lines", "l7", "l8", "… 2 more lines"}, below: 2, wantOff: 6, cut: true, wantVisible: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := scroller{off: tt.off, below: 99}
			got := plainRows(s.window(lines, tt.room, th))
			if tt.want == nil {
				assert.Empty(t, got)
			} else {
				assert.Equal(t, tt.want, got)
			}
			assert.Equal(t, tt.below, s.below)
			assert.Equal(t, tt.wantOff, s.off)
			assert.Equal(t, tt.wantVisible, s.visible)
			assert.Equal(t, tt.cut || tt.below > 0, s.cut())
		})
	}
}

func TestScrollerWindowSingularMarkers(t *testing.T) {
	s := scroller{off: 1}
	assert.Equal(t, []string{"… 1 earlier line", "b", "… 2 more lines"}, plainRows(s.window([]string{"a", "b", "c", "d"}, 3, mocha())))
}

func scrollDialog(n int) *dialog {
	d := &dialog{title: "Pick", note: "why", options: []option{{label: "Yes"}}}
	for i := range n {
		d.body = append(d.body, fmt.Sprintf("l%d", i+1))
	}
	return d
}

func TestDialogScrollRowsJoinsTheHintToTheNoteWhenCut(t *testing.T) {
	d := scrollDialog(10)
	body := d.body
	rows := plainRows(d.scrollRows(40, 100, 1, 4, "pgup/pgdn to scroll", mocha()))
	assert.Contains(t, rows, "  why · pgup/pgdn to scroll")
	assert.Contains(t, rows, "  … 7 more lines")
	assert.Equal(t, body, d.body)
	assert.Equal(t, "why", d.note)

	d = scrollDialog(2)
	assert.NotContains(t, strings.Join(plainRows(d.scrollRows(40, 100, 1, 4, "pgup/pgdn to scroll", mocha())), "\n"), "pgup")

	d = scrollDialog(10)
	d.note = ""
	assert.Contains(t, plainRows(d.scrollRows(40, 100, 1, 4, "pgup/pgdn to scroll", mocha())), "  pgup/pgdn to scroll")
	assert.Empty(t, d.note)
}

func TestDialogScrollRowsWithNoBodyIsRows(t *testing.T) {
	d := scrollDialog(0)
	assert.Equal(t, d.rows(40, 100, 1, mocha()), d.scrollRows(40, 100, 1, 4, "pgup/pgdn to scroll", mocha()))
}

func TestDialogScrollRowsReachesTheEndWhenTheBudgetBinds(t *testing.T) {
	d := scrollDialog(10)
	d.note = "" // the hint takes a row of its own
	d.scroll.off = 100
	rows := plainRows(d.scrollRows(40, 8, 1, 100, "pgup/pgdn to scroll", mocha()))
	assert.LessOrEqual(t, len(rows), 8)
	assert.Contains(t, rows, "  l10")
	assert.Equal(t, 9, d.scroll.off)
}
