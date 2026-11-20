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
	// head is rows under the top rule that outlast the title: the question tabs.
	head  []string
	title string
	body  []string // rendered at width-2; rows indents them
	note  string
	// hint joins the foot, else the note, while the body or pane is cut.
	hint    string
	options []option
	sel     int
	// tail starts the body at its end and keeps it there until scrolled off it.
	tail bool
	// pane is the selected option's preview.
	pane *pane
	// foot is the key hints above the bottom rule.
	foot string
	// warn is drawn in place of foot; the next key clears it.
	warn string
	// areas is where the last rows call drew each field, y counting from its
	// first row.
	areas []fieldArea
	// optionStarts includes the row after the last option.
	optionStarts []int
	// scroll is the offset rows windows the body or pane from.
	scroll scroller
}

// pane is a framed preview: beside the options when list is set, else under
// them. Without lines only the column for the options is kept.
type pane struct {
	lines []string
	// width is the widest line of any option's preview and most the most lines
	// any has, so the frame keeps its width as the selection moves.
	width, most int
	list        int
}

// rows draws d between two rules within maxRows, never more. Below the rules:
// the head, the title, the body windowed from scroll, the note, a blank row,
// the numbered options with the cursor on sel, the pane, a blank row and the
// foot. A field shows the rows maxRows leaves after every row but the body's
// and pane's, at most limit; with none left, the selected option's field still
// shows one row and a field below an unselected label shows none, so it never
// pushes the selected row out. The body or pane takes what is left, at most
// limit, and a hint joins the foot, else the note, while it is cut.
//
// When even the fixed rows do not fit, the body and pane go first, then fields
// shrink to those rules. If the options still fit they all stay, and the rows
// left go to the head, title, note, foot and blanks in that order. If not, the
// rows go to the selected option's first row, the head, the rest of the
// selected option, an "n of m" row, the title, note, foot and blanks, and then
// to the options around the selected one.
func (d *dialog) rows(width, maxRows, limit int, th theme) []string {
	d.areas, d.optionStarts = nil, nil
	rule := fg(th.frame).Render(strings.Repeat("─", width))
	switch {
	case maxRows <= 0:
		return nil
	case maxRows <= 2:
		return slices.Repeat([]string{rule}, maxRows)
	}
	none := lipgloss.NewStyle()
	s := &shape{width: width, col: width, rule: rule}
	if d.pane != nil && d.pane.list > 0 {
		s.col = d.pane.list
	}
	if d.title != "" {
		s.title = hang("  ", none, d.title, none.Bold(true), width)
	}
	s.opts = d.blocks(s.col, th)
	scroll, note, foot := d.scroll, d.note, d.foot
	rows, cut := d.fit(s, note, foot, maxRows, limit, th)
	if !cut || d.hint == "" {
		return rows
	}
	bare, areas, starts, scrolled := rows, d.areas, d.optionStarts, d.scroll
	d.scroll = scroll
	if foot != "" {
		foot = strings.Join([]string{d.hint, foot}, " · ")
	} else {
		note = strings.Join(slices.DeleteFunc([]string{note, d.hint}, isEmpty), " · ")
	}
	rows, cut = d.fit(s, note, foot, maxRows, limit, th)
	if !cut {
		// The hint cost the body or pane its last row, so it would advertise
		// nothing.
		rows, d.areas, d.optionStarts, d.scroll = bare, areas, starts, scrolled
	}
	return rows
}

// shape is a dialog's title and options rendered for one width, before a
// height is applied; note and foot are the rows of the pass being fitted.
type shape struct {
	width, col int
	rule       string
	title      []string
	note, foot []string
	// gap is 1 when a blank row separates the options from the foot.
	gap  int
	opts []optionBlock
}

// optionBlock is an option's rows without its field's, which go in at slot.at.
type optionBlock struct {
	rows []string
	slot fieldSlot
}

// blocks renders the options for col cells.
func (d *dialog) blocks(col int, th theme) []optionBlock {
	none := lipgloss.NewStyle()
	opts := make([]optionBlock, len(d.options))
	for i, o := range d.options {
		prefix, st := "  ", none
		if i == d.sel {
			prefix, st = cursorMark, fg(th.accent)
		}
		if o.chosen {
			st = st.Bold(true)
		}
		or := &opts[i]
		if o.field != nil && o.label == "" {
			if len(d.options) == 1 {
				or.slot = fieldSlot{f: o.field, head: fg(th.accent).Render(prefix), indent: lipgloss.Width(prefix)}
				continue
			}
			head := fmt.Sprintf("%d. %s", i+1, o.mark)
			or.slot = fieldSlot{f: o.field, head: fg(th.accent).Render(prefix) + st.Render(head), indent: lipgloss.Width(prefix + head)}
			if o.chosen && o.mark == "" && i != d.sel {
				or.slot.tail = " ✔"
			}
			continue
		}
		label := fmt.Sprintf("%d. %s", i+1, o.label)
		beside := false
		if o.field != nil && o.fieldPrefix == "" {
			head := label + "  "
			indent := lipgloss.Width(prefix + head)
			if beside = fitsBeside(o.field.value(), col-indent); beside {
				or.slot = fieldSlot{f: o.field, head: fg(th.accent).Render(prefix) + st.Render(head), indent: indent}
			}
		}
		if !beside {
			or.rows = hang(prefix, fg(th.accent), label, st, col)
		}
		for _, r := range o.detail {
			or.rows = append(or.rows, "  "+r)
		}
		switch {
		case o.field == nil || beside:
		case o.fieldPrefix != "":
			or.slot = fieldSlot{f: o.field, head: "  " + fg(th.muted).Render(o.fieldPrefix), indent: 2 + lipgloss.Width(o.fieldPrefix), at: len(or.rows)}
		default:
			or.slot = fieldSlot{f: o.field, indent: 2, selected: i == d.sel, at: len(or.rows)}
		}
	}
	return opts
}

// fit draws d within maxRows with the given note and foot, reporting whether
// the body or pane is cut.
func (d *dialog) fit(s *shape, note, foot string, maxRows, limit int, th theme) ([]string, bool) {
	none := lipgloss.NewStyle()
	d.areas, d.optionStarts = nil, nil
	s.note, s.foot, s.gap = nil, nil, 0
	if note != "" {
		s.note = hang("  ", none, note, fg(th.dim), s.width)
	}
	switch {
	case d.warn != "":
		s.foot = hang("  ", none, d.warn, fg(th.warning), s.width)
	case foot != "":
		s.foot = hang("  ", none, foot, fg(th.dim), s.width)
	}
	// Without options the blank row after the body already separates the foot.
	if len(s.foot) > 0 && len(s.opts) > 0 {
		s.gap = 1
	}
	above := 1 + len(d.head) + len(s.title) + len(s.note) + 1
	fixed := above + s.gap + len(s.foot) + 1
	smallest, plain := fixed, 0
	for _, o := range s.opts {
		plain += len(o.rows)
		if o.slot.f != nil && (o.slot.head != "" || o.slot.selected) {
			smallest++
		}
	}
	if smallest+plain > maxRows {
		return d.squeeze(s, maxRows, th), false
	}

	all, areas, starts := d.expand(s, min(limit, maxRows-fixed-plain), th)
	if fixed+len(all) > maxRows {
		all, areas, starts = d.expand(s, 0, th)
	}
	rest := maxRows - fixed - len(all)
	var body []string
	cut, windowed := false, false
	if len(d.body) > 0 {
		// The view follows the end until it is scrolled off it.
		if d.tail && d.scroll.below == 0 && d.scroll.off >= d.scroll.last {
			d.scroll.off = len(d.body)
		}
		for _, r := range d.scroll.window(d.body, min(limit, rest), th) {
			body = append(body, "  "+r)
		}
		cut, windowed = d.scroll.cut(), true
	}
	first := above + len(body)
	n := len(all)
	var under []string
	if p := d.pane; p != nil && len(p.lines) > 0 {
		beside := p.list > 0
		room := min(limit, maxRows-first-n-s.gap-len(s.foot)-1-3)
		if beside {
			room = min(limit, maxRows-first-len(s.foot)-4)
		}
		if room >= 1 {
			cut, windowed = len(p.lines) > room, true
			switch {
			case !beside:
				under = append([]string{""}, d.previewBox(p.lines, s.width-2, room, 2, th)...)
			default:
				cw := p.width
				if p.most > room {
					cw = max(cw, ansi.StringWidth(elided(p.most, "earlier")))
				}
				space := s.width - p.list - previewGap
				for i, b := range d.previewBox(p.lines, min(max(cw+4, minPreviewBox), space), room, previewGap, th) {
					if i == len(all) {
						all = append(all, "")
					}
					all[i] += strings.Repeat(" ", max(0, p.list-ansi.StringWidth(all[i]))) + b
				}
			}
		}
	}
	if !windowed {
		// A stale offset or cut would take page keys the transcript should get.
		d.scroll = scroller{}
	}
	rows := make([]string, 0, first+len(all)+len(under)+s.gap+len(s.foot)+1)
	rows = append(append(append(append(rows, s.rule), d.head...), s.title...), body...)
	rows = append(append(rows, s.note...), "")
	rows = append(append(rows, all...), under...)
	if s.gap == 1 {
		rows = append(rows, "")
	}
	rows = append(append(rows, s.foot...), s.rule)
	for _, a := range areas {
		a.y += first
		d.areas = append(d.areas, a)
	}
	for _, at := range starts {
		d.optionStarts = append(d.optionStarts, first+at)
	}
	return rows, cut
}

// expand is the options' rows with each field's, a field at most room rows;
// with none, only a field beside its label or selected shows, at one row. It
// also gives the field areas and where each option starts, counted from the
// first option row, the last start being the end of the options.
func (d *dialog) expand(s *shape, room int, th theme) (all []string, areas []fieldArea, starts []int) {
	for _, o := range s.opts {
		starts = append(starts, len(all))
		sl := o.slot
		if sl.f == nil || (room < 1 && sl.head == "" && !sl.selected) {
			all = append(all, o.rows...)
			continue
		}
		fr := sl.rows(s.col, max(1, room), th)
		areas = append(areas, fieldArea{c: sl.f, x: sl.indent, y: len(all) + sl.at, h: len(fr)})
		all = append(append(append(all, o.rows[:sl.at]...), fr...), o.rows[sl.at:]...)
	}
	return all, areas, append(starts, len(all))
}

// previewBox is lines in a frame bw wide, a column of padding inside each
// side, with at most room rows of content, the markers of what is cut inside
// it; every row is indented by indent.
func (d *dialog) previewBox(lines []string, bw, room, indent int, th theme) []string {
	inner := bw - 2
	text := inner - 2
	frame, pad := fg(th.frame), strings.Repeat(" ", indent)
	bar := strings.Repeat("─", inner)
	out := []string{pad + frame.Render("┌"+bar+"┐")}
	for _, l := range d.scroll.window(lines, room, th) {
		l = ansi.Truncate(l, text, "")
		out = append(out, pad+frame.Render("│")+" "+l+strings.Repeat(" ", text-ansi.StringWidth(l)+1)+frame.Render("│"))
	}
	return append(out, pad+frame.Render("└"+bar+"┘"))
}

// squeeze draws d within maxRows of 3 or more when its fixed rows do not fit.
func (d *dialog) squeeze(s *shape, maxRows int, th theme) []string {
	d.scroll.window(nil, 0, th)
	all, areas, starts := d.expand(s, 0, th)
	room := maxRows - 2
	take := func(n int) int {
		n = min(n, room)
		room -= n
		return n
	}
	overflow := len(all) > room
	var sel, span, kept, pos int
	w := len(all)
	if overflow {
		sel, span = starts[d.sel], starts[d.sel+1]-starts[d.sel]
		w = take(1)
	} else {
		room -= len(all)
	}
	kh := take(len(d.head))
	if overflow {
		w += take(span - 1)
		kept = w
		if len(s.opts) > 1 {
			pos = take(1)
		}
	}
	kt, kn := take(len(s.title)), take(len(s.note))
	// The foot is drawn whole or left out, so key hints are never cut mid-sentence.
	kf := 0
	if len(s.foot) <= room {
		kf = take(len(s.foot))
	}
	blank, gap := take(1), take(s.gap)
	start := 0
	if overflow {
		w += room
		start = around(sel, kept, w, len(all))
	}

	rows := append([]string{s.rule}, d.head[:kh]...)
	rows = append(append(rows, s.title[:kt]...), s.note[:kn]...)
	if blank == 1 {
		rows = append(rows, "")
	}
	first := len(rows)
	rows = append(rows, all[start:start+w]...)
	if pos == 1 {
		rows = append(rows, positionRow(d.sel, len(d.options), s.width, th))
	}
	if gap == 1 {
		rows = append(rows, "")
	}
	rows = append(append(rows, s.foot[:kf]...), s.rule)

	for _, a := range areas {
		if top, bottom := max(a.y, start), min(a.y+a.h, start+w); top < bottom {
			a.y, a.h = top-start+first, bottom-top
			d.areas = append(d.areas, a)
		}
	}
	for _, at := range starts[:len(starts)-1] {
		if at >= start && at < start+w {
			at += first - start
		} else {
			at = -1
		}
		d.optionStarts = append(d.optionStarts, at)
	}
	d.optionStarts = append(d.optionStarts, first+w)
	return rows
}

// around is where a window of room rows out of total starts so that the keep
// rows from row at stay in it, centred on at as far as that allows.
func around(at, keep, room, total int) int {
	return min(max(at-room/2, at+keep-room, 0), min(at, total-room))
}

// positionRow is the dim row naming item sel, counted from 0, of total, cut to
// width.
func positionRow(sel, total, width int, th theme) string {
	return fg(th.dim).Render(ansi.Truncate(fmt.Sprintf("  %d of %d", sel+1, total), width, ""))
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
	// at is the row of its option's rows the field's rows go in at.
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
func (d *dialog) field() *composer {
	if len(d.options) == 0 {
		return nil
	}
	return d.options[d.sel].field
}

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
	// gave a cut body and below the rows it left out under it; last is the
	// offset the last window left.
	off, visible, below, last int
}

// cut reports whether the last window left rows out.
func (s *scroller) cut() bool { return s.off > 0 || s.below > 0 }

// page moves the offset n pages, a page being the rows of content the last
// window gave.
func (s *scroller) page(n int) { s.off += n * s.visible }

// window is lines within room rows from the offset; the last row counts the
// lines left out below and, once scrolled, the first those above.
func (s *scroller) window(lines []string, room int, th theme) []string {
	defer func() { s.last = s.off }()
	s.below = 0
	if room < 1 {
		s.off, s.visible = 0, 0
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
