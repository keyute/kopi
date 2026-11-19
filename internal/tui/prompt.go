package tui

// promptKind is a kind of prompt; after promptNone the kinds are in the order they take the front.
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
