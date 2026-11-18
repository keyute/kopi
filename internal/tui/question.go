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
	besideWidth = 80
	// previewGap is the columns between the options and a preview beside them.
	previewGap = 3
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
		custom, note := newField(th, km.newline, "Type your answer"), newField(th, km.newline, "Add a note")
		p := &questionPage{d: &dialog{options: make([]option, n+1)}, chosen: -1, picked: make([]bool, n), notes: make([]string, n), noting: -1, note: &note}
		p.d.options[n].field = &custom
		q.pages = append(q.pages, p)
	}
	if len(q.qs) > 1 {
		q.review = &dialog{title: "Review your answers", options: []option{{label: "Submit answers"}}}
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

func (q *questionDialog) hint() string {
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
	return strings.Join(slices.DeleteFunc(parts, isEmpty), " · ")
}

func isEmpty(s string) bool { return s == "" }

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
	styles[q.tab] = fg(th.accent).Bold(true)
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
	head := 1
	if strip != "" {
		head = 2
	}
	var rows []string
	if q.tab == len(q.qs) {
		rows = q.reviewRows(width, maxPreview, maxRows-head+1, th)
	} else {
		rows = q.pageRows(width, maxPreview, maxRows-head+1, th)
	}
	if strip != "" {
		rows = slices.Insert(rows, 1, strip)
		for i := range d.areas {
			d.areas[i].y++
		}
		for i := range d.optionStarts {
			d.optionStarts[i]++
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

func (q *questionDialog) pageRows(width, maxPreview, maxRows int, th theme) []string {
	qu, p := q.qs[q.tab], q.pages[q.tab]
	p.settle()
	d := p.d
	d.title, d.note, d.body = qu.Question, q.hint(), nil
	if q.notice != "" {
		d.body = hang("", lipgloss.NewStyle(), q.notice, fg(th.warning), width-2)
	}
	preview := !qu.MultiSelect && slices.ContainsFunc(qu.Options, func(o tool.QuestionOption) bool { return o.Preview != "" })
	beside := preview && width >= besideWidth
	listWidth := width
	if beside {
		listWidth = (width - previewGap) / 2
	}
	q.label(p, qu, listWidth, th)
	fieldCap := max(1, maxPreview)
	if !beside {
		rows := d.rows(width, maxRows, fieldCap, th)
		if !preview {
			return rows
		}
		last := len(rows) - 1
		room := min(maxPreview, maxRows-len(rows)-1)
		pv := q.window(q.previewLines(d.sel, width-4, th), room, th)
		if len(pv) == 0 {
			return rows
		}
		out := slices.Clone(rows[:last])
		out = append(out, "")
		for _, r := range pv {
			out = append(out, "  "+r)
		}
		return append(out, rows[last])
	}

	// The title and hint are drawn across the whole width, the options and
	// their fields in the left column.
	head := d.rows(width, maxRows, fieldCap, th)
	head = head[:d.optionStarts[0]]
	list := d.rows(listWidth, maxRows, fieldCap, th)
	opts := list[d.optionStarts[0] : len(list)-1]
	delta := len(head) - d.optionStarts[0]
	for i := range d.areas {
		d.areas[i].y += delta
	}
	for i := range d.optionStarts {
		d.optionStarts[i] += delta
	}
	pw := width - listWidth - previewGap
	pv := q.window(q.previewLines(d.sel, pw, th), min(maxPreview, maxRows-len(head)-1), th)
	rows := slices.Clone(head)
	for i := range max(len(opts), len(pv)) {
		var left, right string
		if i < len(opts) {
			left = opts[i]
		}
		if i < len(pv) {
			right = strings.Repeat(" ", listWidth-ansi.StringWidth(left)+previewGap) + pv[i]
		}
		rows = append(rows, left+right)
	}
	return append(rows, head[0])
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
	set := func(i int, label string, on bool, description string) {
		switch {
		case qu.MultiSelect && on:
			label = "[✔] " + label
		case qu.MultiSelect:
			label = "[ ] " + label
		case on:
			label += " ✔"
		}
		o := &p.d.options[i]
		o.label, o.detail = label, nil
		if description != "" {
			o.detail = hang("", muted, description, muted, width-2)
		}
		if i < len(p.notes) && p.notes[i] != "" {
			o.detail = append(o.detail, hang("", muted, "↳ note: "+p.notes[i], muted, width-2)...)
		}
	}
	for i, o := range qu.Options {
		set(i, o.Label, p.picked[i] || p.chosen == i, o.Description)
	}
	n := len(qu.Options)
	set(n, "Other", p.hasCustom() && (qu.MultiSelect || p.chosen == n), "")
}

func (q *questionDialog) reviewRows(width, maxPreview, maxRows int, th theme) []string {
	d := q.review
	d.note, d.body = q.hint(), nil
	var body []string
	none, muted := lipgloss.NewStyle(), fg(th.muted)
	for i, qu := range q.qs {
		body = append(body, hang("", none, fmt.Sprintf("%d. %s", i+1, qu.Question), none, width-2)...)
		if !q.answered(i) {
			body = append(body, hang("", fg(th.warning), "↳ not answered", fg(th.warning), width-2)...)
			continue
		}
		body = append(body, hang("", muted, "↳ "+q.pages[i].summary(qu), muted, width-2)...)
	}
	room := min(maxPreview, maxRows-len(d.rows(width, maxRows, 1, th)))
	d.body = q.window(body, room, th)
	return d.rows(width, maxRows, 1, th)
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
