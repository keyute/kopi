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

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/tool"
)

const (
	chooseNotice   = "Choose an option or enter an answer."
	unansweredNote = "Answer this question before submitting."
	// besideWidth is the narrowest screen that shows a preview beside the options.
	besideWidth = 64
	// previewGap is the columns between the options and a preview beside them.
	previewGap = 6
	// minPreviewBox is the narrowest a preview box is drawn.
	minPreviewBox = 12
	// headerWidth is the widest a header is drawn in the tab strip.
	headerWidth = 16
)

// questionDialog is the AskUserQuestion dialog, drawn in the composer's slot:
// one page per question and, with several, a review page after them. The
// model's dialog is current().
type questionDialog struct {
	token  uint64
	qs     []tool.Question
	pages  []*questionPage
	review *dialog
	km     *keyMap
	// tab is the shown page; len(qs) is the review page.
	tab int
	// notice is a warning row, cleared by the next key.
	notice string
	// scroll is the offset into a cut preview or review body, reset when the
	// tab or the focused option changes; scrollAt is what it was last reset
	// for and visible the rows of content the last render gave a cut body.
	scroll, visible int
	scrollAt        [2]int
	// previews are rendered previews by tab and option, for previewWidth and
	// previewTheme.
	previews     map[[2]int][]string
	previewWidth int
	previewTheme theme
}

// questionPage is one question's draft; it survives switching tabs.
type questionPage struct {
	// d has the question's options, then the custom row.
	d *dialog
	// chosen is a single-select question's answer: -1, an option, or
	// len(options) for the custom row.
	chosen int
	picked []bool
	// notes is per option.
	notes []string
	// noting is the option whose note editor is open, -1 when none; note is
	// that option's field while it is.
	noting int
	note   *composer
}

func newQuestionDialog(ev event.QuestionRequested, km *keyMap, th theme) *questionDialog {
	q := &questionDialog{token: ev.Token, qs: ev.Questions, km: km, scrollAt: [2]int{-1, -1}}
	for _, qu := range q.qs {
		n := len(qu.Options)
		custom, note := newField(th, km.newline, "Type something."), newField(th, km.newline, "Add a note")
		p := &questionPage{d: &dialog{options: make([]option, n+1)}, chosen: -1, picked: make([]bool, n), notes: make([]string, n), noting: -1, note: &note}
		p.d.options[n].field = &custom
		q.pages = append(q.pages, p)
	}
	if len(q.qs) > 1 {
		q.review = &dialog{options: make([]option, 1)}
	}
	return q
}

func (q *questionDialog) current() *dialog {
	if q.tab == len(q.qs) {
		return q.review
	}
	return q.pages[q.tab].d
}

func (p *questionPage) custom() *composer { return p.d.options[len(p.picked)].field }

func (p *questionPage) customText() string {
	text, _ := p.custom().expanded()
	return text
}

func (p *questionPage) hasCustom() bool { return strings.TrimSpace(p.customText()) != "" }

func (p *questionPage) onCustom() bool { return p.d.sel == len(p.picked) }

func (p *questionPage) anyPicked() bool { return slices.Contains(p.picked, true) }

func (q *questionDialog) answered(i int) bool {
	p := q.pages[i]
	if q.qs[i].MultiSelect {
		return p.anyPicked() || p.hasCustom()
	}
	return p.chosen >= 0 && (p.chosen < len(p.picked) || p.hasCustom())
}

func (q *questionDialog) firstUnanswered() int {
	for i := range q.qs {
		if !q.answered(i) {
			return i
		}
	}
	return -1
}

// answers is every question's answer; notes on options that are not chosen are
// left out.
func (q *questionDialog) answers() []tool.Answer {
	out := make([]tool.Answer, len(q.qs))
	for i, p := range q.pages {
		var a tool.Answer
		switch {
		case q.qs[i].MultiSelect:
			for j, on := range p.picked {
				if on {
					a.Picks = append(a.Picks, tool.Pick{Option: j, Note: p.notes[j]})
				}
			}
			if p.hasCustom() {
				a.Custom = p.customText()
			}
		case p.chosen == len(p.picked):
			a.Custom = p.customText()
		default:
			a.Picks = []tool.Pick{{Option: p.chosen, Note: p.notes[p.chosen]}}
		}
		out[i] = a
	}
	return out
}

// settle closes a note editor that a click left behind by moving the focus.
func (p *questionPage) settle() {
	if p.noting >= 0 && p.d.sel != p.noting {
		p.closeNote(true)
	}
}

func (p *questionPage) openNote() tea.Cmd {
	i := p.d.sel
	p.noting = i
	p.note.reset()
	p.note.ta.SetValue(p.notes[i])
	p.d.options[i].field = p.note
	return p.note.ta.Focus()
}

// closeNote closes the editor, keeping what was typed when save is set; a
// blank note is removed.
func (p *questionPage) closeNote(save bool) {
	if save {
		text, _ := p.note.expanded()
		p.notes[p.noting] = strings.TrimSpace(text)
	}
	p.note.ta.ClearSelection()
	p.note.ta.Blur()
	p.d.options[p.noting].field = nil
	p.noting = -1
}

// binding is "<first key> to <verb>", empty when the binding has no keys.
func binding(b key.Binding, verb string) string {
	if ks := b.Keys(); len(ks) > 0 {
		return ks[0] + " to " + verb
	}
	return ""
}

// hint is the key hints row; cut adds scrolling, which the note editor lacks.
func (q *questionDialog) hint(cut bool) string {
	km := q.km
	var parts []string
	switch {
	case q.tab == len(q.qs):
		parts = []string{binding(km.submit, "submit"), "← to go back", binding(km.interrupt, "dismiss")}
	case q.pages[q.tab].noting >= 0:
		return strings.Join(slices.DeleteFunc([]string{binding(km.submit, "save"), binding(km.interrupt, "go back")}, isEmpty), " · ")
	case q.pages[q.tab].onCustom():
		parts = []string{binding(km.submit, "answer"), binding(km.interrupt, "dismiss")}
	case q.qs[q.tab].MultiSelect:
		parts = []string{"space to toggle", binding(km.submit, "continue"), binding(km.complete, "add a note"), binding(km.interrupt, "dismiss")}
	default:
		parts = []string{binding(km.submit, "choose"), binding(km.complete, "add a note"), binding(km.interrupt, "dismiss")}
	}
	if len(q.qs) > 1 && q.tab < len(q.qs) {
		parts = append([]string{"←/→ to switch questions"}, parts...)
	}
	if cut {
		if up, down := km.pageUp.Keys(), km.pageDown.Keys(); len(up) > 0 && len(down) > 0 {
			parts = append([]string{up[0] + "/" + down[0] + " to scroll"}, parts...)
		}
	}
	return strings.Join(slices.DeleteFunc(parts, isEmpty), " · ")
}

func isEmpty(s string) bool { return s == "" }

// footer is the notice, or else the hint, below the options.
func (q *questionDialog) footer(width int, cut bool, th theme) []string {
	none := lipgloss.NewStyle()
	if q.notice != "" {
		return hang("  ", none, q.notice, fg(th.warning), width)
	}
	return hang("  ", none, q.hint(cut), fg(th.dim), width)
}

// strip is the row of tabs, "" for a single question. When it is wider than
// the dialog it starts at the active tab.
func (q *questionDialog) strip(width int, th theme) string {
	if len(q.qs) < 2 {
		return ""
	}
	cells := make([]string, len(q.qs)+1)
	styles := make([]lipgloss.Style, len(cells))
	for i, qu := range q.qs {
		mark, st := "☐", fg(th.muted)
		if q.answered(i) {
			mark, st = "☑", fg(th.success)
		}
		cells[i], styles[i] = " "+mark+" "+cut(qu.Header, headerWidth)+" ", st
	}
	cells[len(q.qs)], styles[len(q.qs)] = " ✔ Submit ", fg(th.muted)
	styles[q.tab] = fg(th.accent).Bold(true).Background(th.userMessageBg)
	start, total := 0, 0
	for _, c := range cells {
		total += lipgloss.Width(c)
	}
	if total > width-2 {
		start = q.tab
	}
	var b strings.Builder
	for i := start; i < len(cells); i++ {
		b.WriteString(styles[i].Render(cells[i]))
	}
	return "  " + ansi.Truncate(b.String(), max(0, width-2), "…")
}

// rows draws the shown page within maxRows, its preview or review body at
// most maxPreview rows. Past maxRows both rules and the tab strip surround a
// window of the interior containing the selected option.
func (q *questionDialog) rows(width, maxPreview, maxRows int, th theme) []string {
	if maxRows <= 0 {
		return nil
	}
	d := q.current()
	if at := [2]int{q.tab, d.sel}; at != q.scrollAt {
		q.scroll, q.scrollAt = 0, at
	}
	strip := q.strip(width, th)
	head, avail := 1, maxRows
	if strip != "" {
		head, avail = 2, maxRows-2
	}
	var rows []string
	if q.tab == len(q.qs) {
		rows = q.reviewRows(width, maxPreview, avail, th)
	} else {
		rows = q.pageRows(width, maxPreview, avail, th)
	}
	if strip != "" {
		rows = slices.Insert(rows, 1, strip, "")
		for i := range d.areas {
			d.areas[i].y += 2
		}
		for i := range d.optionStarts {
			d.optionStarts[i] += 2
		}
	}
	if len(rows) <= maxRows {
		return rows
	}
	if maxRows < head+2 {
		d.areas = nil
		if maxRows >= 2 {
			return []string{rows[0], rows[len(rows)-1]}
		}
		return rows[:maxRows]
	}
	room := maxRows - head - 1
	start := max(head, min(d.optionStarts[d.sel], len(rows)-1-room))
	end := start + room
	var areas []fieldArea
	for _, fa := range d.areas {
		top, bottom := max(fa.y, start), min(fa.y+fa.h, end)
		if top < bottom {
			fa.y, fa.h = top-start+head, bottom-top
			areas = append(areas, fa)
		}
	}
	d.areas = areas
	return append(append(slices.Clone(rows[:head]), rows[start:end]...), rows[len(rows)-1])
}

// window is lines within room rows from the scroll offset; the last row
// counts the lines left out below and, once scrolled, the first those above.
func (q *questionDialog) window(lines []string, room int, th theme) []string {
	if room < 1 {
		q.visible = 0
		return nil
	}
	if len(lines) <= room {
		q.scroll, q.visible = 0, len(lines)
		return lines
	}
	q.visible = max(1, room-2)
	q.scroll = min(max(0, q.scroll), max(0, len(lines)-(room-1)))
	s := q.scroll
	content := room
	if s > 0 {
		content--
	}
	more := s+content < len(lines)
	if more {
		content--
	}
	content = max(0, content)
	var rows []string
	if s > 0 {
		rows = append(rows, fg(th.dim).Render(elided(s, "earlier")))
	}
	rows = append(rows, lines[s:s+content]...)
	if s+content < len(lines) {
		rows = append(rows, fg(th.dim).Render(elided(len(lines)-s-content, "more")))
	}
	return rows[:min(len(rows), room)]
}

func elided(n int, what string) string {
	if n == 1 {
		return "… 1 " + what + " line"
	}
	return fmt.Sprintf("… %d %s lines", n, what)
}

// pageRows is the shown question's page within avail rows, without the strip:
// the title, the options with a preview beside or below them, then the notice
// or the hint.
func (q *questionDialog) pageRows(width, maxPreview, avail int, th theme) []string {
	qu, p := q.qs[q.tab], q.pages[q.tab]
	p.settle()
	d := p.d
	d.title, d.note, d.body = qu.Question, "", nil
	preview := !qu.MultiSelect && slices.ContainsFunc(qu.Options, func(o tool.QuestionOption) bool { return o.Preview != "" })
	beside := preview && width >= besideWidth
	listWidth := width
	if beside {
		q.label(p, qu, width, th)
		listWidth = min(naturalWidth(d), (width-previewGap)/2)
	}
	q.label(p, qu, listWidth, th)
	fieldCap := max(1, maxPreview)
	lines := func(width int) []string {
		if !preview {
			return nil
		}
		return q.previewLines(d.sel, width, th)
	}

	build := func(cut bool) ([]string, bool) {
		var cuts bool
		foot := q.footer(width, cut, th)
		budget := avail - 1 - len(foot)
		if !beside {
			rows := d.rows(width, budget, fieldCap, th)
			last := len(rows) - 1
			out := slices.Clone(rows[:last])
			if lines := lines(width - 6); len(lines) > 0 {
				room := min(maxPreview, budget-len(rows)-3)
				if room >= 1 {
					cuts = len(lines) > room
					out = append(append(out, ""), q.previewBox(lines, width-2, room, 2, th)...)
				}
			}
			out = append(append(out, ""), foot...)
			return append(out, rows[last]), cuts
		}

		// The title is drawn across the whole width, the options and their
		// fields in the left column.
		head := d.rows(width, budget, fieldCap, th)
		head = head[:d.optionStarts[0]]
		list := d.rows(listWidth, budget, fieldCap, th)
		opts := list[d.optionStarts[0] : len(list)-1]
		delta := len(head) - d.optionStarts[0]
		for i := range d.areas {
			d.areas[i].y += delta
		}
		for i := range d.optionStarts {
			d.optionStarts[i] += delta
		}
		var box []string
		space := width - listWidth - previewGap
		if lines := lines(space - 4); len(lines) > 0 {
			room := min(maxPreview, budget-len(head)-3)
			// Every option's preview is measured so the box keeps its width as
			// focus moves.
			cw := 0
			for i := range qu.Options {
				ol := q.previewLines(i, space-4, th)
				for _, l := range ol {
					cw = max(cw, ansi.StringWidth(l))
				}
				if len(ol) > room {
					cw = max(cw, ansi.StringWidth(elided(len(ol), "earlier")))
				}
			}
			bw := min(max(cw+4, minPreviewBox), space)
			if room >= 1 {
				cuts = len(lines) > room
				box = q.previewBox(lines, bw, room, previewGap, th)
			}
		}
		rows := slices.Clone(head)
		for i := range max(len(opts), len(box)) {
			var left string
			if i < len(opts) {
				left = opts[i]
			}
			if i < len(box) {
				left += strings.Repeat(" ", max(0, listWidth-ansi.StringWidth(left))) + box[i]
			}
			rows = append(rows, left)
		}
		rows = append(append(rows, ""), foot...)
		return append(rows, head[0]), cuts
	}
	rows, cuts := build(false)
	if cuts {
		rows, _ = build(true)
	}
	return rows
}

// previewBox is lines in a frame bw wide, a column of padding inside each
// side, with at most room rows of content, the markers of what is cut inside
// it; every row is indented by indent.
func (q *questionDialog) previewBox(lines []string, bw, room, indent int, th theme) []string {
	inner := bw - 2
	text := inner - 2
	frame, pad := fg(th.frame), strings.Repeat(" ", indent)
	bar := strings.Repeat("─", inner)
	out := []string{pad + frame.Render("┌"+bar+"┐")}
	for _, l := range q.window(lines, room, th) {
		l = ansi.Truncate(l, text, "")
		out = append(out, pad+frame.Render("│")+" "+l+strings.Repeat(" ", text-ansi.StringWidth(l)+1)+frame.Render("│"))
	}
	return append(out, pad+frame.Render("└"+bar+"┘"))
}

// naturalWidth is the width d's option rows take unwrapped, as drawn.
func naturalWidth(d *dialog) int {
	w := 0
	for i, o := range d.options {
		head := fmt.Sprintf("%d. %s", i+1, o.label)
		if o.label == "" {
			head = fmt.Sprintf("%d. %s", i+1, o.mark)
		}
		w = max(w, 2+ansi.StringWidth(head))
		for _, r := range o.detail {
			w = max(w, 2+ansi.StringWidth(r))
		}
		if o.field != nil {
			indent := 2
			switch {
			case o.fieldPrefix != "":
				indent += ansi.StringWidth(o.fieldPrefix)
			case o.label == "":
				indent += ansi.StringWidth(head)
			default:
				indent += ansi.StringWidth(head) + 2
			}
			w = max(w, indent+max(ansi.StringWidth(o.field.ta.Placeholder), lipgloss.Width(o.field.value()))+1)
		}
	}
	return w
}

// previewLines is option i's preview rendered at width, none for the custom
// row or an option without one.
func (q *questionDialog) previewLines(i, width int, th theme) []string {
	opts := q.qs[q.tab].Options
	if i >= len(opts) || opts[i].Preview == "" {
		return nil
	}
	if q.previews == nil || q.previewWidth != width || q.previewTheme != th {
		q.previews, q.previewWidth, q.previewTheme = map[[2]int][]string{}, width, th
	}
	at := [2]int{q.tab, i}
	if _, ok := q.previews[at]; !ok {
		q.previews[at] = newMarkdown(width, th).render(opts[i].Preview)
	}
	return q.previews[at]
}

// label sets each option's label and detail rows from the page's draft, for
// a list width.
func (q *questionDialog) label(p *questionPage, qu tool.Question, width int, th theme) {
	muted := fg(th.muted)
	for i, o := range qu.Options {
		on := p.picked[i] || p.chosen == i
		label := o.Label
		switch {
		case qu.MultiSelect && on:
			label = "[✔] " + label
		case qu.MultiSelect:
			label = "[ ] " + label
		case on:
			label += " ✔"
		}
		d := &p.d.options[i]
		d.label, d.chosen, d.detail, d.fieldPrefix = label, on, nil, ""
		if o.Description != "" {
			d.detail = hang("", muted, o.Description, muted, width-2)
		}
		switch {
		case p.noting == i:
			d.fieldPrefix = "↳ note: "
		case p.notes[i] != "":
			d.detail = append(d.detail, hang("", muted, "↳ note: "+p.notes[i], muted, width-2)...)
		}
	}
	c := &p.d.options[len(qu.Options)]
	c.chosen = p.hasCustom() && (qu.MultiSelect || p.chosen == len(qu.Options))
	c.mark = ""
	switch {
	case qu.MultiSelect && c.chosen:
		c.mark = "[✔] "
	case qu.MultiSelect:
		c.mark = "[ ] "
	}
}

// reviewRows is the review page within avail rows, without the strip: every
// question with its answer, then the notice or the hint.
func (q *questionDialog) reviewRows(width, maxPreview, avail int, th theme) []string {
	d := q.review
	var body []string
	none, muted := lipgloss.NewStyle(), fg(th.muted)
	for i, qu := range q.qs {
		body = append(body, hang("", none, fmt.Sprintf("%d. %s", i+1, qu.Question), none.Bold(true), width-2)...)
		if !q.answered(i) {
			body = append(body, hang("", fg(th.warning), "↳ Answer required", fg(th.warning), width-2)...)
			continue
		}
		body = append(body, hang("", muted, "↳ "+q.pages[i].summary(qu), muted, width-2)...)
	}
	// The rows around the body: both rules, a blank row and the footer.
	foot := q.footer(width, false, th)
	room := min(maxPreview, avail-3-len(foot))
	if room >= 1 && len(body) > room {
		foot = q.footer(width, true, th)
		room = min(maxPreview, avail-3-len(foot))
	}
	rows := []string{fg(th.frame).Render(strings.Repeat("─", width))}
	for _, r := range q.window(body, room, th) {
		rows = append(rows, "  "+r)
	}
	d.optionStarts = []int{1, len(rows)}
	rows = append(append(rows, ""), foot...)
	return append(rows, rows[0])
}

// summary is the answer as the review shows it.
func (p *questionPage) summary(qu tool.Question) string {
	var parts []string
	add := func(i int) {
		part := qu.Options[i].Label
		if p.notes[i] != "" {
			part += " — " + p.notes[i]
		}
		parts = append(parts, part)
	}
	switch {
	case qu.MultiSelect:
		for i, on := range p.picked {
			if on {
				add(i)
			}
		}
	case p.chosen < len(p.picked):
		add(p.chosen)
	}
	if p.hasCustom() && (qu.MultiSelect || p.chosen == len(p.picked)) {
		parts = append(parts, strings.TrimSpace(p.customText()))
	}
	return strings.Join(parts, "; ")
}

// plainKey reports whether msg is the unmodified key code.
func plainKey(msg tea.KeyPressMsg, code rune) bool {
	k := msg.Key()
	return k.Code == code && k.Mod == 0
}

// questionKey goes to the dialog: it answers when the questions are, and
// interrupt dismisses them. Keys it has no use for are swallowed. The dialog
// stays until its QuestionEnded arrives.
func (m *model) questionKey(msg tea.KeyPressMsg) tea.Cmd {
	q := m.question
	q.notice = ""
	var cmd tea.Cmd
	if q.tab == len(q.qs) {
		cmd = m.reviewKey(msg)
	} else if q.pages[q.tab].settle(); q.pages[q.tab].noting >= 0 {
		cmd = m.noteKey(msg)
	} else {
		cmd = m.pageKey(msg)
	}
	m.dialog = q.current()
	return cmd
}

func (m *model) dismissQuestion() { m.c.AnswerQuestion(m.question.token, nil) }

func (m *model) reviewKey(msg tea.KeyPressMsg) tea.Cmd {
	q, km := m.question, &m.keys
	switch {
	case key.Matches(msg, km.interrupt):
		m.dismissQuestion()
	case key.Matches(msg, km.submit):
		if i := q.firstUnanswered(); i >= 0 {
			cmd := q.goTo(i)
			q.notice = unansweredNote
			return cmd
		}
		m.c.AnswerQuestion(q.token, q.answers())
	case plainKey(msg, tea.KeyLeft):
		return q.goTo(len(q.qs) - 1)
	case key.Matches(msg, km.pageUp):
		q.scroll -= q.visible
	case key.Matches(msg, km.pageDown):
		q.scroll += q.visible
	}
	return nil
}

func (m *model) noteKey(msg tea.KeyPressMsg) tea.Cmd {
	p, km := m.question.pages[m.question.tab], &m.keys
	switch {
	case key.Matches(msg, km.submit) || key.Matches(msg, km.complete):
		p.closeNote(true)
	case key.Matches(msg, km.interrupt):
		p.closeNote(false)
	case key.Matches(msg, km.paste):
		return readClipboardText(m.clip, p.note)
	default:
		return p.note.update(msg)
	}
	return nil
}

func (m *model) pageKey(msg tea.KeyPressMsg) tea.Cmd {
	q, km := m.question, &m.keys
	p, qu := q.pages[q.tab], q.qs[q.tab]
	d, n := p.d, len(qu.Options)
	several := len(q.qs) > 1
	if key.Matches(msg, km.interrupt) {
		m.dismissQuestion()
		return nil
	}
	if p.onCustom() {
		switch {
		case key.Matches(msg, km.submit):
			switch {
			case p.hasCustom():
				if !qu.MultiSelect {
					p.chosen = n
				}
				return q.advance(m)
			case qu.MultiSelect && p.anyPicked():
				return q.advance(m)
			}
			q.notice = chooseNotice
			return nil
		case key.Matches(msg, km.paste):
			return readClipboardText(m.clip, d.field())
		case several && p.custom().empty() && plainKey(msg, tea.KeyLeft):
			return q.goTo(q.tab - 1)
		case several && p.custom().empty() && plainKey(msg, tea.KeyRight):
			return q.goTo(q.tab + 1)
		}
		_, cmd := d.key(msg, km)
		return cmd
	}
	i, err := strconv.Atoi(msg.String())
	switch {
	case err == nil && i == n+1:
		return d.move(n)
	case err == nil && i >= 1 && i <= n && qu.MultiSelect:
		p.picked[i-1] = !p.picked[i-1]
	case err == nil && i >= 1 && i <= n:
		d.sel, p.chosen = i-1, i-1
		return q.advance(m)
	case key.Matches(msg, km.submit):
		if !qu.MultiSelect {
			p.chosen = d.sel
		}
		return q.advance(m)
	case plainKey(msg, tea.KeySpace) && qu.MultiSelect:
		p.picked[d.sel] = !p.picked[d.sel]
	case key.Matches(msg, km.complete):
		return p.openNote()
	case several && plainKey(msg, tea.KeyLeft):
		return q.goTo(q.tab - 1)
	case several && plainKey(msg, tea.KeyRight):
		return q.goTo(q.tab + 1)
	case key.Matches(msg, km.pageUp):
		q.scroll -= q.visible
	case key.Matches(msg, km.pageDown):
		q.scroll += q.visible
	default:
		_, cmd := d.key(msg, km)
		return cmd
	}
	return nil
}

// advance leaves an answered question: a single question answers at once,
// else the next page opens, review after the last.
func (q *questionDialog) advance(m *model) tea.Cmd {
	if !q.answered(q.tab) {
		q.notice = chooseNotice
		return nil
	}
	if len(q.qs) == 1 {
		m.c.AnswerQuestion(q.token, q.answers())
		return nil
	}
	return q.goTo(q.tab + 1)
}

// goTo shows tab, moving the focus to the field of its selected option.
func (q *questionDialog) goTo(tab int) tea.Cmd {
	tab = min(max(0, tab), len(q.qs))
	if f := q.current().field(); f != nil {
		f.ta.ClearSelection()
		f.ta.Blur()
	}
	q.tab = tab
	if f := q.current().field(); f != nil {
		return f.ta.Focus()
	}
	return nil
}
