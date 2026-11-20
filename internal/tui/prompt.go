package tui

// promptKind is a kind of prompt.
type promptKind int

const (
	promptNone promptKind = iota
	promptApproval
	promptQuestion
	promptPlan
	promptLogin
	promptTasks
)

// front is the prompt that is drawn and takes input: the open one of the
// highest kind, in the order approval, question, plan, login, /tasks; among
// approvals, the earliest to arrive.
func (m *model) front() promptKind {
	switch {
	case m.approval != nil:
		return promptApproval
	case m.question != nil:
		return promptQuestion
	case m.plan != nil:
		return promptPlan
	case m.login != nil:
		return promptLogin
	case m.tasks != nil:
		return promptTasks
	}
	return promptNone
}

// frontDialog is the front prompt's dialog, nil for /tasks and when no prompt
// is open.
func (m *model) frontDialog() *dialog {
	switch m.front() {
	case promptApproval:
		return m.approvalDialog
	case promptQuestion:
		return m.question.current()
	case promptPlan:
		return m.plan.d
	case promptLogin:
		return m.loginDialog
	}
	return nil
}

// scrollFront scrolls the front prompt's cut body by n rows, or n pages when
// pages is set, and reports whether it took the input. A question always takes
// it; an approval and an open /tasks task only while their body is cut.
func (m *model) scrollFront(n int, pages bool) bool {
	var s *scroller
	switch m.front() {
	case promptApproval:
		if s = &m.approvalPreview.scroller; !s.cut() {
			return false
		}
	case promptQuestion:
		s = &m.question.scroll
	case promptTasks:
		if m.tasks.open == "" || !m.tasks.scroll.cut() {
			return false
		}
		// The next render leaves the end, and follows it again on reaching it.
		s, m.tasks.scrolled = &m.tasks.scroll, true
	default:
		return false
	}
	if pages {
		n *= s.visible
	}
	s.off += n
	return true
}

// pageFront scrolls the front prompt by a page in direction dir, else the
// transcript.
func (m *model) pageFront(dir int) {
	if !m.scrollFront(dir, true) {
		m.scroll(dir * m.transcriptHeight())
	}
}
