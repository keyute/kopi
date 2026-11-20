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
// one page per question and, with several, a review page after them.
type questionDialog struct {
	token  uint64
	qs     []tool.Question
	pages  []*questionPage
	review *dialog
	km     *keyMap
	// tab is the shown page; len(qs) is the review page.
	tab int
	// scrollAt is the tab and option the shown dialog's scroll was last reset
	// for; a change of either resets it.
	scrollAt [2]int
	// previews are rendered previews by tab and option, for previewWidth and
	// previewTheme; sizes are by tab the widest line and the most lines among
	// them.
	previews     map[[2]int][]string
	sizes        map[int][2]int
	previewWidth int
	previewTheme theme
	// reviewWidth and reviewTheme are what review.body was made for; a key
	// clears the body, as it may change an answer.
	reviewWidth int
	reviewTheme theme
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
		q.review = &dialog{}
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

// keys is the key hints row.
func (q *questionDialog) keys() string {
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

// scrollHint is "<page up>/<page down> to scroll", empty unless both are bound.
func scrollHint(km *keyMap) string {
	if up, down := km.pageUp.Keys(), km.pageDown.Keys(); len(up) > 0 && len(down) > 0 {
		return up[0] + "/" + down[0] + " to scroll"
	}
	return ""
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

// dialog is the shown page with its tab strip, key hints and preview or
// review body for width.
func (q *questionDialog) dialog(width int, th theme) *dialog {
	d := q.current()
	if at := [2]int{q.tab, d.sel}; at != q.scrollAt {
		d.scroll.off, q.scrollAt = 0, at
	}
	d.head = nil
	if strip := q.strip(width, th); strip != "" {
		d.head = []string{strip, ""}
	}
	d.hint = scrollHint(q.km)
	if q.tab == len(q.qs) {
		q.fillReview(width, th)
	} else {
		q.fillPage(width, th)
	}
	d.foot = q.keys()
	return d
}

// fillPage sets the shown question's title and preview pane for width: the
// options go beside the preview when it fits.
func (q *questionDialog) fillPage(width int, th theme) {
	qu, p := q.qs[q.tab], q.pages[q.tab]
	p.settle()
	d := p.d
	d.title, d.note, d.body, d.pane = qu.Question, "", nil, nil
	preview := !qu.MultiSelect && slices.ContainsFunc(qu.Options, func(o tool.QuestionOption) bool { return o.Preview != "" })
	beside := preview && width >= besideWidth
	listWidth := width
	if beside {
		q.label(p, qu, width, th)
		listWidth = min(naturalWidth(d), (width-previewGap)/2)
	}
	q.label(p, qu, listWidth, th)
	switch {
	case beside:
		textWidth := width - listWidth - previewGap - 4
		d.pane = &pane{lines: q.previewLines(d.sel, textWidth, th), list: listWidth}
		// Every option's preview is measured so the box keeps its width as focus moves.
		if len(d.pane.lines) > 0 {
			size, ok := q.sizes[q.tab]
			if !ok {
				for i := range qu.Options {
					ol := q.previewLines(i, textWidth, th)
					size[1] = max(size[1], len(ol))
					for _, l := range ol {
						size[0] = max(size[0], ansi.StringWidth(l))
					}
				}
				q.sizes[q.tab] = size
			}
			d.pane.width, d.pane.most = size[0], size[1]
		}
	case preview:
		d.pane = &pane{lines: q.previewLines(d.sel, width-6, th)}
	}
	if p.noting >= 0 {
		d.hint = ""
	}
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
		q.previews, q.sizes, q.previewWidth, q.previewTheme = map[[2]int][]string{}, map[int][2]int{}, width, th
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

// fillReview sets the review body: every question with its answer.
func (q *questionDialog) fillReview(width int, th theme) {
	d := q.review
	if d.body != nil && q.reviewWidth == width && q.reviewTheme == th {
		return
	}
	q.reviewWidth, q.reviewTheme = width, th
	d.body = nil
	none, muted := lipgloss.NewStyle(), fg(th.muted)
	for i, qu := range q.qs {
		d.body = append(d.body, hang("", none, fmt.Sprintf("%d. %s", i+1, qu.Question), none.Bold(true), width-2)...)
		if !q.answered(i) {
			d.body = append(d.body, hang("", fg(th.warning), "↳ Answer required", fg(th.warning), width-2)...)
			continue
		}
		d.body = append(d.body, hang("", muted, "↳ "+q.pages[i].summary(qu), muted, width-2)...)
	}
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

// key answers when the questions are, and dismiss dismisses them. Keys it
// has no use for are swallowed. The dialog stays until its QuestionEnded
// arrives.
func (q *questionDialog) key(m *model, msg tea.KeyPressMsg) (tea.Cmd, bool) {
	if q.review != nil {
		q.review.body = nil
	}
	if q.tab == len(q.qs) {
		return q.reviewKey(m, msg), true
	}
	if q.pages[q.tab].settle(); q.pages[q.tab].noting >= 0 {
		return q.noteKey(m, msg), true
	}
	return q.pageKey(m, msg), true
}

// seen ends the dialog with its QuestionEnded.
func (q *questionDialog) seen(_ *model, msg tea.Msg) bool {
	ev, ended := msg.(event.QuestionEnded)
	return !ended || ev.Token != q.token
}

func (q *questionDialog) dismiss(m *model) { m.c.AnswerQuestion(q.token, nil) }

func (q *questionDialog) reviewKey(m *model, msg tea.KeyPressMsg) tea.Cmd {
	km := &m.keys
	switch {
	case key.Matches(msg, km.dismiss):
		q.dismiss(m)
	case key.Matches(msg, km.submit):
		if i := q.firstUnanswered(); i >= 0 {
			cmd := q.goTo(i)
			q.current().warn = unansweredNote
			return cmd
		}
		m.c.AnswerQuestion(q.token, q.answers())
	case plainKey(msg, tea.KeyLeft):
		return q.goTo(len(q.qs) - 1)
	}
	return nil
}

func (q *questionDialog) noteKey(m *model, msg tea.KeyPressMsg) tea.Cmd {
	p, km := q.pages[q.tab], &m.keys
	switch {
	case key.Matches(msg, km.submit) || key.Matches(msg, km.complete):
		p.closeNote(true)
	case key.Matches(msg, km.dismiss):
		p.closeNote(false)
	default:
		return p.note.update(msg)
	}
	return nil
}

func (q *questionDialog) pageKey(m *model, msg tea.KeyPressMsg) tea.Cmd {
	km := &m.keys
	p, qu := q.pages[q.tab], q.qs[q.tab]
	d, n := p.d, len(qu.Options)
	several := len(q.qs) > 1
	if key.Matches(msg, km.dismiss) {
		q.dismiss(m)
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
			d.warn = chooseNotice
			return nil
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
		q.current().warn = chooseNotice
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
