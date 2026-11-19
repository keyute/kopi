package tui

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"

	"github.com/keyute/kopi/internal/event"
)

// secretMark is drawn in place of each character of a secret.
const secretMark = "•"

// newLoginDialog is the prompt for ev: its message as the title and its
// provider as the note. Options become one numbered choice each; otherwise
// the one option is an unlabelled text field, masked when ev asks for a secret.
func newLoginDialog(ev event.LoginRequested, km *keyMap, th theme) *dialog {
	d := &dialog{title: ev.Message, note: ev.Provider}
	if len(ev.Options) > 0 {
		for _, o := range ev.Options {
			label := o.Label
			if o.Description != "" {
				label += " - " + o.Description
			}
			d.options = append(d.options, option{label: label})
		}
		return d
	}
	f := newField(th, km.newline, ev.Placeholder)
	f.secret = ev.Secret
	f.ta.Focus()
	d.options = []option{{field: &f}}
	return d
}

// loginRows draws d in place of the composer, within maxRows. On overflow,
// both rules surround a window of the interior containing the selected label;
// a dialog of plain options keeps its title and shows a position row.
func loginRows(d *dialog, width, maxFieldRows, maxRows int, th theme) []string {
	if maxRows <= 0 {
		return nil
	}
	rows := d.rows(width, maxRows, max(1, maxFieldRows), th)
	if len(rows) <= maxRows {
		return rows
	}
	if maxRows < 3 {
		return squeeze(d, rows, maxRows)
	}
	if d.options[0].field == nil {
		return pinnedRows(d, rows, width, maxRows, th)
	}
	room := maxRows - 2
	start := min(d.optionStarts[d.sel], len(rows)-1-room)
	return clip(d, rows, 1, start, start+room)
}

// pinnedRows is rows for a dialog of plain options within maxRows (3 or more):
// the rows above the first option stay, then a window of option rows centred on
// the selection, a dim position row as the menu draws one, and the bottom rule.
// Fewer rows above the options stay, and no position row is drawn, when
// maxRows leaves no room for them.
func pinnedRows(d *dialog, rows []string, width, maxRows int, th theme) []string {
	d.areas = nil
	last := len(rows) - 1
	pos := 0
	if maxRows >= 4 {
		pos = 1
	}
	head := rows[:max(1, min(d.optionStarts[0], maxRows-2-pos))]
	room := maxRows - len(head) - 1 - pos
	first := d.optionStarts[0]
	start := min(max(d.optionStarts[d.sel]-room/2, first), max(first, last-room))
	out := append(append([]string{}, head...), rows[start:min(start+room, last)]...)
	if pos == 1 {
		out = append(out, fg(th.dim).Render(ansi.Truncate(fmt.Sprintf("  %d of %d", d.sel+1, len(d.options)), width, "")))
	}
	return append(out, rows[last])
}

// clear empties every field, so an answered or ended prompt keeps no text.
func (d *dialog) clear() {
	for _, o := range d.options {
		if o.field != nil {
			o.field.reset()
		}
	}
}

// secretRow draws the draft of secret field f as one mark per character, the
// last width cells when it is longer, or its dim placeholder when empty.
func secretRow(f *composer, width int, th theme) string {
	if f.empty() {
		return fg(th.dim).Render(ansi.Truncate(f.ta.Placeholder, max(0, width), ""))
	}
	n := utf8.RuneCountInString(strings.ReplaceAll(f.value(), "\n", ""))
	return strings.Repeat(secretMark, min(n, max(0, width)))
}
