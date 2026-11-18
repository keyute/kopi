package tui

import (
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"

	"github.com/keyute/kopi/internal/event"
)

// secretMark is drawn in place of each character of a secret.
const secretMark = "•"

// newLoginDialog is the prompt for ev: its message as the title and its
// provider as the note. Options become one numbered choice each; otherwise
// the one option is a text field, masked when ev asks for a secret.
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
	d.options = []option{{label: "Answer", field: &f}}
	return d
}

// loginRows draws d in place of the composer, within maxRows. On overflow,
// both rules surround a window of the interior containing the selected label.
func loginRows(d *dialog, width, maxFieldRows, maxRows int, th theme) []string {
	if maxRows <= 0 {
		return nil
	}
	rows := d.rows(width, maxRows, max(1, maxFieldRows), th)
	if len(rows) <= maxRows {
		return rows
	}
	if maxRows < 3 {
		d.areas = nil
		if maxRows == 2 {
			return []string{rows[0], rows[len(rows)-1]}
		}
		return rows[:maxRows]
	}
	room := maxRows - 2
	start := min(d.optionStarts[d.sel], len(rows)-1-room)
	end := start + room
	var areas []fieldArea
	for _, fa := range d.areas {
		top, bottom := max(fa.y, start), min(fa.y+fa.h, end)
		if top < bottom {
			fa.y, fa.h = top-start+1, bottom-top
			areas = append(areas, fa)
		}
	}
	d.areas = areas
	return append(append([]string{rows[0]}, rows[start:end]...), rows[len(rows)-1])
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
