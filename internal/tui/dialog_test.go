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

func TestScrollerWindowWithoutRoomResetsTheOffset(t *testing.T) {
	s := scroller{off: 3}
	assert.Nil(t, s.window([]string{"a", "b", "c", "d", "e"}, 0, mocha()))
	assert.False(t, s.cut())
}

func scrollDialog(n int) *dialog {
	d := &dialog{title: "Pick", note: "why", options: []option{{label: "Yes"}}}
	for i := range n {
		d.body = append(d.body, fmt.Sprintf("l%d", i+1))
	}
	return d
}

func TestDialogScrollRowsJoinsTheHintToTheNoteWhenCut(t *testing.T) {
	const hint = "pgup/pgdn to scroll"
	d := scrollDialog(10)
	d.hint = hint
	body := d.body
	rows := plainRows(d.rows(40, 100, 4, mocha()))
	assert.Contains(t, rows, "  why · pgup/pgdn to scroll")
	assert.Contains(t, rows, "  … 7 more lines")
	assert.Equal(t, body, d.body)
	assert.Equal(t, "why", d.note)

	d = scrollDialog(2)
	d.hint = hint
	assert.NotContains(t, strings.Join(plainRows(d.rows(40, 100, 4, mocha())), "\n"), "pgup")

	d = scrollDialog(10)
	d.hint = hint
	d.note = ""
	assert.Contains(t, plainRows(d.rows(40, 100, 4, mocha())), "  pgup/pgdn to scroll")
	assert.Empty(t, d.note)
}

func TestDialogScrollRowsWithNoBodyIsRows(t *testing.T) {
	d := scrollDialog(0)
	want := d.rows(40, 100, 4, mocha())
	d.hint = "pgup/pgdn to scroll"
	assert.Equal(t, want, d.rows(40, 100, 4, mocha()))
}

func TestDialogScrollRowsKeepTheMarkerWithoutTheHintWhenTheHintWouldStarveTheBody(t *testing.T) {
	d := scrollDialog(10)
	d.note = ""
	d.hint = "pgup/pgdn to scroll"
	rows := plainRows(d.rows(40, 6, 4, mocha()))
	assert.Equal(t, []string{"  Pick", "  … 10 more lines"}, rows[1:3])
	assert.NotContains(t, strings.Join(rows, "\n"), "pgup")
	assert.True(t, d.scroll.cut())
}

func TestDialogRowsResetTheScrollWhenNothingIsWindowed(t *testing.T) {
	lines := make([]string, 30)
	d := &dialog{title: "Pick", options: []option{{label: "Yes"}}, pane: &pane{lines: lines}}
	d.rows(40, 100, 6, mocha())
	require.True(t, d.scroll.cut())

	d.rows(40, 8, 6, mocha())
	assert.Equal(t, scroller{}, d.scroll, "the pane has no room")

	d.rows(40, 100, 6, mocha())
	require.True(t, d.scroll.cut())
	d.pane.lines = nil
	d.rows(40, 100, 6, mocha())
	assert.Equal(t, scroller{}, d.scroll, "the pane has no lines")
}

func TestDialogScrollRowsReachesTheEndWhenTheBudgetBinds(t *testing.T) {
	d := scrollDialog(10)
	d.note = "" // the hint takes a row of its own
	d.hint = "pgup/pgdn to scroll"
	d.scroll.off = 100
	rows := plainRows(d.rows(40, 8, 100, mocha()))
	assert.LessOrEqual(t, len(rows), 8)
	assert.Contains(t, rows, "  l10")
	assert.Equal(t, 9, d.scroll.off)
}

func TestDialogRowsDrawOnlyRulesWhenTheHeightIsTiny(t *testing.T) {
	d := scrollDialog(3)
	rule := strings.Repeat("─", 20)
	assert.Empty(t, d.rows(20, 0, 5, mocha()))
	assert.Equal(t, []string{rule}, plainRows(d.rows(20, 1, 5, mocha())))
	assert.Equal(t, []string{rule, rule}, plainRows(d.rows(20, 2, 5, mocha())))
	assert.Empty(t, d.areas)
}

func TestDialogRowsShrinkTheBodyBeforeTheFields(t *testing.T) {
	d := fieldDialog()
	d.move(1)
	d.paste("a\nb\nc")
	full := len(d.rows(30, 100, 5, mocha()))
	d.body = []string{"l1", "l2", "l3", "l4", "l5", "l6"}

	rows := plainRows(d.rows(30, full, 5, mocha()))
	assert.Len(t, rows, full)
	assert.NotContains(t, rows, "  l1", "the fields keep what they had")
	assert.Equal(t, []string{"  a", "  b", "  c"}, rows[len(rows)-4:len(rows)-1])

	assert.Contains(t, plainRows(d.rows(30, full+3, 5, mocha())), "  l1")

	rows = plainRows(d.rows(30, full-1, 5, mocha()))
	assert.Len(t, rows, full-1)
	assert.Equal(t, []string{"  b", "  c"}, rows[len(rows)-3:len(rows)-1], "then the field gives rows up")
}

func TestDialogRowsKeepEveryOptionThatFitsAndGiveTheRestToTheText(t *testing.T) {
	d := &dialog{head: []string{"tabs"}, title: "Title", note: "note", foot: "foot", options: []option{{label: "A"}, {label: "B"}, {label: "C"}}}
	rule := strings.Repeat("─", 20)
	opts := []string{"❭ 1. A", "  2. B", "  3. C"}
	for height, want := range map[int][]string{
		5:  {rule, opts[0], opts[1], opts[2], rule},
		6:  {rule, "tabs", opts[0], opts[1], opts[2], rule},
		7:  {rule, "tabs", "  Title", opts[0], opts[1], opts[2], rule},
		8:  {rule, "tabs", "  Title", "  note", opts[0], opts[1], opts[2], rule},
		9:  {rule, "tabs", "  Title", "  note", opts[0], opts[1], opts[2], "  foot", rule},
		10: {rule, "tabs", "  Title", "  note", "", opts[0], opts[1], opts[2], "  foot", rule},
		11: {rule, "tabs", "  Title", "  note", "", opts[0], opts[1], opts[2], "", "  foot", rule},
	} {
		assert.Equal(t, want, plainRows(d.rows(20, height, 5, mocha())), "height %d", height)
	}
}

func TestDialogRowsDrawTheFootWholeOrNotAtAll(t *testing.T) {
	d := &dialog{title: "Pick", foot: strings.Repeat("hint ", 4), sel: 3}
	for i := range 8 {
		d.options = append(d.options, option{label: fmt.Sprintf("o%d", i+1)})
	}
	for height := 3; height < 14; height++ {
		rows := strings.Join(plainRows(d.rows(12, height, 5, mocha())), "\n")
		if n := strings.Count(rows, "hint"); n != 0 {
			assert.Equal(t, 4, n, "height %d", height)
		}
	}
}

func TestDialogRowsKeepTheSelectionAndCountWhenTheOptionsDoNotFit(t *testing.T) {
	d := &dialog{title: "Pick", sel: 3}
	for i := range 8 {
		d.options = append(d.options, option{label: fmt.Sprintf("o%d", i+1)})
	}
	rule := strings.Repeat("─", 20)
	for height, want := range map[int][]string{
		3: {rule, "❭ 4. o4", rule},
		4: {rule, "❭ 4. o4", "  4 of 8", rule},
		5: {rule, "  Pick", "❭ 4. o4", "  4 of 8", rule},
		8: {rule, "  Pick", "", "  3. o3", "❭ 4. o4", "  5. o5", "  4 of 8", rule},
	} {
		assert.Equal(t, want, plainRows(d.rows(20, height, 5, mocha())), "height %d", height)
	}
	d.rows(20, 8, 5, mocha())
	assert.Equal(t, []int{-1, -1, 3, 4, 5, -1, -1, -1, 6}, d.optionStarts, "options left out have no row")
}

func TestDialogRowsKeepTheFirstAndLastSelectionWhenTheOptionsDoNotFit(t *testing.T) {
	for _, sel := range []int{0, 7} {
		d := &dialog{title: "Pick", sel: sel}
		for i := range 8 {
			d.options = append(d.options, option{label: fmt.Sprintf("o%d", i+1)})
		}
		for height := 3; height <= 8; height++ {
			rows := plainRows(d.rows(20, height, 5, mocha()))
			require.LessOrEqual(t, len(rows), height, "sel %d height %d", sel, height)
			assert.Contains(t, rows, fmt.Sprintf("❭ %d. o%d", sel+1, sel+1), "sel %d height %d", sel, height)
			if height >= 4 {
				assert.Contains(t, rows, fmt.Sprintf("  %d of 8", sel+1), "sel %d height %d", sel, height)
			}
		}
	}
}

func TestDialogRowsNeverPassTheHeightAndRecordWhatIsDrawn(t *testing.T) {
	th := mocha()
	rule := strings.Repeat("─", 30)
	for name, d := range map[string]*dialog{
		"field beside":  fieldDialog("Later", "More"),
		"field below":   fieldDialog("Later", "More"),
		"body and note": scrollDialog(20),
	} {
		d.title = strings.Repeat("Long title ", 4)
		d.head, d.foot = []string{"tabs"}, "enter to choose"
		if name != "body and note" {
			d.move(1)
		}
		if name == "field below" {
			d.paste("a\nb")
		}
		for height := range 20 {
			rows := plainRows(d.rows(30, height, 5, th))
			require.LessOrEqual(t, len(rows), height, "%s at %d", name, height)
			if height >= 2 {
				assert.Equal(t, rule, rows[0])
				assert.Equal(t, rule, rows[len(rows)-1])
			}
			for _, a := range d.areas {
				require.Positive(t, a.y)
				require.LessOrEqual(t, a.y+a.h, len(rows)-1)
				for _, row := range rows[a.y : a.y+a.h] {
					assert.Contains(t, []string{"What should change?", "a", "b"}, strings.TrimSpace(row[min(a.x, len(row)):]), "%s at %d", name, height)
				}
			}
			for i, at := range d.optionStarts[:max(0, len(d.optionStarts)-1)] {
				if at >= 0 {
					assert.Contains(t, rows[at], fmt.Sprintf("%d. ", i+1), "%s at %d", name, height)
				}
			}
		}
	}
}

func TestDialogRowsWithAPaneNeverPassTheHeight(t *testing.T) {
	th := mocha()
	rule := strings.Repeat("─", 30)
	lines := make([]string, 30)
	for i := range lines {
		lines[i] = fmt.Sprintf("p%d", i+1)
	}
	for _, list := range []int{0, 12} {
		d := fieldDialog("Later", "More")
		d.head, d.foot = []string{"tabs"}, "enter to choose"
		d.pane = &pane{lines: lines, width: 10, most: 30, list: list}
		for height := 2; height < 40; height++ {
			rows := plainRows(d.rows(30, height, 5, th))
			require.LessOrEqual(t, len(rows), height, "list %d at %d", list, height)
			assert.Equal(t, rule, rows[0], "list %d at %d", list, height)
			assert.Equal(t, rule, rows[len(rows)-1], "list %d at %d", list, height)
		}
	}
}

func TestDialogTailFollowsTheEndUntilScrolledOffIt(t *testing.T) {
	th := mocha()
	d := scrollDialog(30)
	d.tail, d.note = true, ""
	rows := func() []string { return plainRows(d.rows(40, 10, 5, th)) }
	assert.Contains(t, rows(), "  l30")

	d.scroll.off -= d.scroll.visible
	assert.NotContains(t, rows(), "  l30")
	d.body = append(d.body, "l31")
	assert.NotContains(t, rows(), "  l31", "scrolled up it stays put")

	d.scroll.off += 100
	rows()
	d.body = append(d.body, "l32")
	assert.Contains(t, rows(), "  l32", "back at the end it follows again")
}

func TestDialogRowsSecondFrameAllocationsDoNotGrowWithTheBody(t *testing.T) {
	if raceEnabled {
		t.Skip("sync.Pool randomly drops items under -race, so allocation counts vary")
	}
	allocs := func(n int) float64 {
		d := scrollDialog(n)
		d.hint = "pgup/pgdn to scroll"
		d.rows(40, 20, 5, mocha())
		return testing.AllocsPerRun(10, func() { d.rows(40, 20, 5, mocha()) })
	}
	assert.LessOrEqual(t, allocs(5000), allocs(50)+2)
}
