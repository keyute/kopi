package tui

import (
	"slices"

	tea "charm.land/bubbletea/v2"
)

// promptKind is a kind of prompt, in rank order: a prompt of an earlier kind
// is in front of one of a later kind.
type promptKind int

const (
	promptNone promptKind = iota
	promptApproval
	promptQuestion
	promptPlan
	promptLogin
	promptTasks
)

// prompt is a dialog drawn in the composer's slot; the one in front takes the
// keys.
type prompt interface {
	// dialog is what is drawn now, its rows that depend on width made for it.
	// A repeat call at the same width and theme renders nothing again.
	dialog(width int, th theme) *dialog
	// key applies msg; false closes the prompt.
	key(m *model, msg tea.KeyPressMsg) (tea.Cmd, bool)
	// seen applies an event of session id, or the editor's exit; false closes
	// the prompt.
	seen(m *model, id string, msg tea.Msg) bool
}

// held is an open prompt.
type held struct {
	prompt
	kind promptKind
	// waits reports that a turn is blocked on the answer.
	waits bool
}

// open adds p behind the prompts of its kind and of earlier kinds, so the front
// is the open prompt of the earliest kind, the first to arrive among them. A
// covered prompt stays open and comes back to the front when the ones before it
// close.
func (m *model) open(kind promptKind, waits bool, p prompt) {
	at := slices.IndexFunc(m.prompts, func(h held) bool { return h.kind > kind })
	if at < 0 {
		at = len(m.prompts)
	}
	m.prompts = slices.Insert(m.prompts, at, held{p, kind, waits})
}

// front is the prompt that is drawn and takes input, nil when none is open.
func (m *model) front() *held {
	if len(m.prompts) == 0 {
		return nil
	}
	return &m.prompts[0]
}

// frontDialog is the front prompt's dialog, nil when no prompt is open.
func (m *model) frontDialog() *dialog {
	if h := m.front(); h != nil {
		return h.dialog(m.width, m.th)
	}
	return nil
}

// sweep gives every open prompt an event of session id, or the editor's exit,
// and closes those that report it ends them.
func (m *model) sweep(id string, msg tea.Msg) {
	m.prompts = slices.DeleteFunc(m.prompts, func(h held) bool { return !h.seen(m, id, msg) })
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
