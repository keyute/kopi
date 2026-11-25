package tui

import (
	"slices"

	tea "charm.land/bubbletea/v2"

	"github.com/keyute/kopi/internal/event"
)

// promptKind is a kind of prompt, in rank order: a prompt of an earlier kind
// is in front of one of a later kind.
type promptKind int

const (
	promptApproval promptKind = iota
	promptQuestion
	promptPlan
	promptLogin
	promptDir
	promptWorkflow
	promptTasks
	promptUsage
)

// waits reports that a turn is blocked on the answer.
func (k promptKind) waits() bool { return k <= promptPlan }

// programKind is the program status kind of a prompt that blocks on the human,
// "" for one that does not.
func (k promptKind) programKind() tea.ProgramStatusKind {
	switch k {
	case promptApproval, promptPlan:
		return tea.ProgramStatusKindPermission
	case promptQuestion:
		return tea.ProgramStatusKindQuestion
	case promptLogin:
		return tea.ProgramStatusKindAuth
	}
	return ""
}

// prompt is a dialog drawn in the composer's slot; the one in front takes the
// keys.
type prompt interface {
	// dialog is what is drawn now, its rows that depend on width made for it.
	// A repeat call at the same width and theme redraws from what
	// was made, though some prompts rebuild their labels or options.
	dialog(width int, th theme) *dialog
	// key applies msg; false closes the prompt.
	key(m *model, msg tea.KeyPressMsg) (tea.Cmd, bool)
	// seen applies an event, or the editor's exit; false closes the prompt.
	seen(m *model, msg tea.Msg) bool
}

// requester is a prompt that can own the request an event repeats.
type requester interface {
	// owns reports that ev repeats the request the prompt was opened for.
	owns(ev event.Event) bool
}

// held is an open prompt.
type held struct {
	prompt
	kind promptKind
}

// open adds p behind the prompts of its kind and of earlier kinds, so the front
// is the open prompt of the earliest kind, the first to arrive among them. A
// covered prompt stays open and comes back to the front when the ones before it
// close.
func (m *model) open(kind promptKind, p prompt) {
	at := slices.IndexFunc(m.prompts, func(h held) bool { return h.kind > kind })
	if at < 0 {
		at = len(m.prompts)
	}
	m.prompts = slices.Insert(m.prompts, at, held{p, kind})
}

// front is the prompt that is drawn and takes input, nil when none is open.
func (m *model) front() *held {
	if len(m.prompts) == 0 {
		return nil
	}
	return &m.prompts[0]
}

// owned reports that an open prompt already holds the request ev repeats.
func (m *model) owned(ev event.Event) bool {
	return slices.ContainsFunc(m.prompts, func(h held) bool {
		r, ok := h.prompt.(requester)
		return ok && r.owns(ev)
	})
}

// frontDialog is the front prompt's dialog, nil when no prompt is open.
func (m *model) frontDialog() *dialog {
	if h := m.front(); h != nil {
		return h.dialog(m.width, m.th)
	}
	return nil
}

// sweep gives every open prompt an event, or the editor's exit, and closes
// those that report it ends them.
func (m *model) sweep(msg tea.Msg) {
	m.prompts = slices.DeleteFunc(m.prompts, func(h held) bool { return !h.seen(m, msg) })
}

// scrollFront scrolls the front prompt's dialog d, nil when none is open, by n
// rows, or n pages when pages is set, and reports whether it took the input:
// only a cut body takes it.
func (m *model) scrollFront(d *dialog, n int, pages bool) bool {
	if d == nil || !d.scroll.cut() {
		return false
	}
	if pages {
		d.scroll.page(n)
	} else {
		d.scroll.off += n
	}
	return true
}

// pageFront scrolls the front prompt by a page in direction dir, else the
// transcript.
func (m *model) pageFront(dir int) {
	m.focus = agentFocus{} // as any key the agent rows have no use for
	d := m.frontDialog()
	if d != nil {
		d.warn = ""
	}
	if !m.scrollFront(d, dir, true) {
		m.scroll(dir * m.transcriptHeight())
	}
}
