package tui

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// cursorMark marks the selected row in the menu and in dialogs.
const cursorMark = "❭ "

// option is a dialog choice; one with a field takes text, and selecting it
// focuses the field. One with no label draws its field in place of the label:
// numbered after mark, or alone and unnumbered when it is the dialog's sole
// option.
type option struct {
	label string
	field *composer
	// chosen draws the label bold; a label-less field with no mark also ends
	// with a ✔ while unselected.
	chosen bool
	// mark is drawn between a label-less field's number and the field.
	mark string
	// fieldPrefix, when set, puts the field below the detail instead of beside
	// the label, after this muted prefix.
	fieldPrefix string
	// detail is rows the caller rendered for width-2, drawn two columns in
	// under the label's rows; a field below the label comes after them.
	detail []string
}

// dialog is a choice drawn in the composer's slot. It is drawn between two
// rules directly above the screen's last row: the status line in the main UI,
// key hints on a screen of its own.
type dialog struct {
	title   string
	body    []string // rendered at width-2; rows indents them
	note    string
	options []option
	sel     int
	// areas is where the last rows call drew each field, y counting from its
	// first row.
	areas []fieldArea
	// optionStarts includes the row after the last option.
	optionStarts []int
}

// rows draws d between two rules: the title, body and note two in, a blank
// row, then the numbered options with the cursor on sel. A field shows the
// rows budget leaves after every row but the body's, at most fieldCap; leaving
// the body out keeps that count the same whatever body is later fitted into
// what remains. With none left, the selected option's field still shows one
// row and a field below an unselected label shows none, so it never pushes the
// selected row out.
func (d *dialog) rows(width, budget, fieldCap int, th theme) []string {
	none := lipgloss.NewStyle()
	rule := fg(th.frame).Render(strings.Repeat("─", width))
	rows := append([]string{rule}, hang("  ", none, d.title, none.Bold(true), width)...)
	for _, r := range d.body {
		rows = append(rows, "  "+r)
	}
	if d.note != "" {
		rows = append(rows, hang("  ", none, d.note, fg(th.dim), width)...)
	}
	rows = append(rows, "")
	var slots []fieldSlot
	d.optionStarts = nil
	for i, o := range d.options {
		d.optionStarts = append(d.optionStarts, len(rows))
		prefix, st := "  ", none
		if i == d.sel {
			prefix, st = cursorMark, fg(th.accent)
		}
		if o.chosen {
			st = st.Bold(true)
		}
		if o.field != nil && o.label == "" {
			if len(d.options) == 1 {
				slots = append(slots, fieldSlot{option: i, at: len(rows), f: o.field, head: fg(th.accent).Render(prefix), indent: lipgloss.Width(prefix)})
				continue
			}
			head := fmt.Sprintf("%d. %s", i+1, o.mark)
			s := fieldSlot{option: i, at: len(rows), f: o.field, head: fg(th.accent).Render(prefix) + st.Render(head), indent: lipgloss.Width(prefix + head)}
			if o.chosen && o.mark == "" && i != d.sel {
				s.tail = " ✔"
			}
			slots = append(slots, s)
			continue
		}
		label := fmt.Sprintf("%d. %s", i+1, o.label)
		if o.field == nil {
			rows = append(rows, hang(prefix, fg(th.accent), label, st, width)...)
			rows = withDetail(rows, o)
			continue
		}
		if o.fieldPrefix != "" {
			rows = append(rows, hang(prefix, fg(th.accent), label, st, width)...)
			rows = withDetail(rows, o)
			slots = append(slots, fieldSlot{option: i, at: len(rows), f: o.field, head: "  " + fg(th.muted).Render(o.fieldPrefix), indent: 2 + lipgloss.Width(o.fieldPrefix)})
			continue
		}
		head := label + "  "
		if indent := lipgloss.Width(prefix + head); fitsBeside(o.field.value(), width-indent) {
			slots = append(slots, fieldSlot{option: i, at: len(rows), f: o.field, head: fg(th.accent).Render(prefix) + st.Render(head), indent: indent})
			rows = withDetail(rows, o)
			continue
		}
		rows = append(rows, hang(prefix, fg(th.accent), label, st, width)...)
		rows = withDetail(rows, o)
		slots = append(slots, fieldSlot{option: i, at: len(rows), f: o.field, indent: 2, selected: i == d.sel})
	}
	d.optionStarts = append(d.optionStarts, len(rows))
	rows = append(rows, rule)
	room := min(fieldCap, budget-(len(rows)-len(d.body)))
	d.areas = nil
	off := 0
	for _, s := range slots {
		if room >= 1 || s.head != "" || s.selected {
			fr := s.rows(width, max(1, room), th)
			rows = slices.Insert(rows, s.at+off, fr...)
			d.areas = append(d.areas, fieldArea{c: s.f, x: s.indent, y: s.at + off, h: len(fr)})
			off += len(fr)
			for i := s.option + 1; i < len(d.optionStarts); i++ {
				d.optionStarts[i] += len(fr)
			}
		}
	}
	return rows
}

func withDetail(rows []string, o option) []string {
	for _, r := range o.detail {
		rows = append(rows, "  "+r)
	}
	return rows
}

// fitsBeside reports whether draft v fits one row of inline cells. The
// textarea wraps a row that fills its width, as it keeps a cell for the
// cursor, so this is decided here rather than by setting a trial width,
// which would move a scrolled view.
func fitsBeside(v string, inline int) bool {
	return !strings.Contains(v, "\n") && lipgloss.Width(v) < inline && inline >= 2
}

// fieldSlot is where a field's rows go among a dialog's rows: beside its
// label, which head draws, or below it when head is empty.
type fieldSlot struct {
	option   int
	at       int
	f        *composer
	head     string
	indent   int
	selected bool
	// tail, when set, draws the field's text bold with tail after it.
	tail string
}

func (s fieldSlot) rows(width, fieldMax int, th theme) []string {
	s.f.fieldStyle(th)
	s.f.ta.MaxHeight = fieldMax
	s.f.ta.SetWidth(width - s.indent)
	if s.f.secret {
		row := secretRow(s.f, width-s.indent, th)
		if s.head != "" {
			return []string{s.head + row}
		}
		return []string{strings.Repeat(" ", s.indent) + row}
	}
	rows := strings.Split(s.f.ta.View(), "\n")
	if s.tail != "" {
		bold := lipgloss.NewStyle().Bold(true)
		for i := range rows {
			rows[i] = bold.Render(strings.TrimRight(ansi.Strip(rows[i]), " "))
		}
		rows[len(rows)-1] += s.tail
	}
	for i := range rows {
		if i == 0 && s.head != "" {
			rows[i] = s.head + rows[i]
		} else {
			rows[i] = strings.Repeat(" ", s.indent) + rows[i]
		}
	}
	return rows
}

// field is the selected option's field, nil when it has none.
func (d *dialog) field() *composer { return d.options[d.sel].field }

// key applies msg and reports whether it picks the selected option. On an
// option with a field, the menu keys leave only from the draft's edge rows and
// digits are text.
func (d *dialog) key(msg tea.KeyPressMsg, km *keyMap) (picked bool, cmd tea.Cmd) {
	if f := d.field(); f != nil {
		switch {
		case key.Matches(msg, km.submit):
			return true, nil
		case key.Matches(msg, km.menuUp) && f.atTop():
			return false, d.move(max(0, d.sel-1))
		case key.Matches(msg, km.menuDown) && f.atBottom() && d.sel < len(d.options)-1:
			return false, d.move(d.sel + 1)
		}
		return false, f.update(msg)
	}
	switch {
	case key.Matches(msg, km.menuUp):
		return false, d.move(max(0, d.sel-1))
	case key.Matches(msg, km.menuDown):
		return false, d.move(min(len(d.options)-1, d.sel+1))
	case key.Matches(msg, km.submit):
		return true, nil
	}
	if n, err := strconv.Atoi(msg.String()); err == nil && n >= 1 && n <= len(d.options) {
		if d.options[n-1].field != nil {
			return false, d.move(n - 1)
		}
		d.sel = n - 1
		return true, nil
	}
	return false, nil
}

func (d *dialog) pick(f *composer) tea.Cmd {
	for i, o := range d.options {
		if o.field == f {
			return d.move(i)
		}
	}
	return nil
}

// move selects option i, moving the focus to its field.
func (d *dialog) move(i int) tea.Cmd {
	if i == d.sel {
		return nil
	}
	if f := d.field(); f != nil {
		f.ta.ClearSelection()
		f.ta.Blur()
	}
	d.sel = i
	if f := d.field(); f != nil {
		return f.ta.Focus()
	}
	return nil
}

// paste goes to the selected option's field only; a blurred textarea would
// ignore the insertion and the paste would collapse the whole draft.
func (d *dialog) paste(text string) tea.Cmd {
	if f := d.field(); f != nil {
		return f.paste(newlines.Replace(text))
	}
	return nil
}

// text is the selected option's draft as sent, "" without a field.
func (d *dialog) text() string {
	if f := d.field(); f != nil {
		text, _ := f.expanded()
		return text
	}
	return ""
}

// scroller is the offset into rows that do not all fit their dialog.
type scroller struct {
	// off is the first row shown, visible the rows of content the last window
	// gave a cut body and below the rows it left out under it.
	off, visible, below int
}

// cut reports whether the last window left rows out.
func (s *scroller) cut() bool { return s.off > 0 || s.below > 0 }

// window is lines within room rows from the offset; the last row counts the
// lines left out below and, once scrolled, the first those above.
func (s *scroller) window(lines []string, room int, th theme) []string {
	s.below = 0
	if room < 1 {
		s.visible = 0
		return nil
	}
	if len(lines) <= room {
		s.off, s.visible = 0, len(lines)
		return lines
	}
	s.visible = max(1, room-2)
	s.off = min(max(0, s.off), max(0, len(lines)-(room-1)))
	content := room
	if s.off > 0 {
		content--
	}
	if s.off+content < len(lines) {
		content--
	}
	content = max(0, content)
	s.below = len(lines) - s.off - content
	var rows []string
	if s.off > 0 {
		rows = append(rows, fg(th.dim).Render(elided(s.off, "earlier")))
	}
	rows = append(rows, lines[s.off:s.off+content]...)
	if s.below > 0 {
		rows = append(rows, fg(th.dim).Render(elided(s.below, "more")))
	}
	return rows[:min(len(rows), room)]
}

func elided(n int, what string) string {
	if n == 1 {
		return "… 1 " + what + " line"
	}
	return fmt.Sprintf("… %d %s lines", n, what)
}
