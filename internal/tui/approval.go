package tui

import (
	"slices"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/tool"
)

type approvalOption struct {
	label  string
	answer policy.Answer
}

// approvalOptions are the prompt's choices in order; option i is digit i+1.
func approvalOptions(a *event.ApprovalRequested) []approvalOption {
	opts := []approvalOption{{"Yes", policy.Allow}}
	if a.Edits {
		opts = append(opts, approvalOption{"Yes, allow all edits this session", policy.AllowEdits})
	}
	return append(opts, approvalOption{"No", policy.Reject})
}

// newFeedbackField is the text field of a No row.
func newFeedbackField(km *keyMap, th theme) *composer {
	f := newField(th, km.newline, "What should change?")
	return &f
}

// approvalNote is why the prompt asks: the rule that asked, or the classifier's
// review.
func approvalNote(a *event.ApprovalRequested) string {
	switch {
	case a.Rule != "":
		return "asked by " + a.Rule
	case a.Review != nil:
		note := "the classifier could not decide"
		if a.Review.Verdict == policy.VerdictDeny {
			note = "the classifier would deny this"
		}
		if a.Review.Reason != "" {
			note += ": " + a.Review.Reason
		}
		return note
	}
	return ""
}

// approvalDialog is the prompt for a call awaiting an answer.
type approvalDialog struct {
	req *event.ApprovalRequested
	d   *dialog
	km  *keyMap
	// preview is the call's preview rows.
	preview approvalPreview
}

// newApprovalDialog is the prompt for a: approvalOptions as its options, the
// reject option taking a reason.
func newApprovalDialog(a *event.ApprovalRequested, km *keyMap, th theme) *approvalDialog {
	d := &dialog{title: a.Title, note: approvalNote(a)}
	if a.Agent != "" {
		d.title = a.Agent + " " + a.TaskID + " › " + a.Title
	}
	d.hints = []string{binding(km.submit, "choose"), binding(km.dismiss, "reject")}
	for _, o := range approvalOptions(a) {
		opt := option{label: o.label}
		if o.answer == policy.Reject {
			opt.field = newFeedbackField(km, th)
		}
		d.options = append(d.options, opt)
	}
	return &approvalDialog{req: a, d: d, km: km}
}

// dialog has the preview as its body, windowed from d's scroll.
func (a *approvalDialog) dialog(width int, th theme) *dialog {
	a.d.body = a.preview.lines(a.req.Preview, width-2, th)
	return a.d
}

// key answers when an option is picked; dismiss rejects without a reason.
// Keys it has no use for are swallowed. The prompt stays until its
// ApprovalEnded arrives.
func (a *approvalDialog) key(m *model, msg tea.KeyPressMsg) (tea.Cmd, bool) {
	if key.Matches(msg, m.keys.dismiss) {
		m.c.Answer(a.req.Token, policy.Decision{})
		return nil, true
	}
	picked, cmd := a.d.key(msg, &m.keys)
	if picked {
		opts := approvalOptions(a.req)
		m.c.Answer(a.req.Token, policy.Decision{Answer: opts[a.d.sel].answer, Reason: a.d.text()})
	}
	return cmd, true
}

// seen ends the prompt with its ApprovalEnded.
func (a *approvalDialog) seen(_ *model, msg tea.Msg) bool {
	ev, ended := msg.(event.ApprovalEnded)
	return !ended || ev.Token != a.req.Token
}

// approvalPreview is the shown approval's preview rendered by previewRows for
// width and th, kept across renders.
type approvalPreview struct {
	rows  []string
	width int
	th    theme
}

func (p *approvalPreview) lines(preview []tool.PreviewLine, width int, th theme) []string {
	if p.rows == nil || p.width != width || p.th != th {
		p.rows, p.width, p.th = previewRows(preview, width, th), width, th
	}
	return p.rows
}

// previewRows draws a preview flush left in the muted colour; when any line is
// added or removed, each line takes a sign column instead: "+ " and "- " in
// the success and error colours, two spaces and dim for context.
func previewRows(preview []tool.PreviewLine, width int, th theme) []string {
	signed := slices.ContainsFunc(preview, func(l tool.PreviewLine) bool { return l.Kind != tool.Context })
	rows := make([]string, 0, len(preview))
	for _, l := range preview {
		switch {
		case !signed:
			for _, s := range wrap(l.Text, width) {
				rows = append(rows, fg(th.muted).Render(s))
			}
		case l.Kind == tool.Removed:
			rows = append(rows, hang("- ", fg(th.error), l.Text, fg(th.error), width)...)
		case l.Kind == tool.Added:
			rows = append(rows, hang("+ ", fg(th.success), l.Text, fg(th.success), width)...)
		default:
			rows = append(rows, hang("  ", fg(th.dim), l.Text, fg(th.dim), width)...)
		}
	}
	return rows
}
