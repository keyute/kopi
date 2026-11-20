package tui

import (
	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/policy"
	"github.com/keyute/kopi/internal/tool"
)

type approvalOption struct {
	label  string
	answer policy.Answer
}

// approvalOptions are the prompt's choices in order; option i is digit i+1.
func approvalOptions(a *event.ApprovalRequested, km *keyMap) []approvalOption {
	opts := []approvalOption{{"Yes", policy.Allow}}
	if a.Edits {
		opts = append(opts, approvalOption{"Yes, allow all edits this session", policy.AllowEdits})
	}
	return append(opts, approvalOption{rejectLabel(km), policy.Reject})
}

// rejectLabel is the No row's label, naming the key that picks it without a
// reason.
func rejectLabel(km *keyMap) string {
	no := "No"
	if ks := km.interrupt.Keys(); len(ks) > 0 {
		no += " (" + ks[0] + ")"
	}
	return no
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

// newApprovalDialog is the prompt for a: approvalOptions as its options, the
// reject option taking a reason.
func newApprovalDialog(a *event.ApprovalRequested, km *keyMap, th theme) *dialog {
	d := &dialog{title: a.Title, note: approvalNote(a)}
	if a.Agent != "" {
		d.title = a.Agent + " " + a.TaskID + " › " + a.Title
	}
	for _, o := range approvalOptions(a, km) {
		opt := option{label: o.label}
		if o.answer == policy.Reject {
			opt.field = newFeedbackField(km, th)
		}
		d.options = append(d.options, opt)
	}
	return d
}

// approvalPreview is the shown approval's preview rendered by previewLine for
// width and th, kept across renders.
type approvalPreview struct {
	rows  []string
	width int
	th    theme
}

func (p *approvalPreview) lines(preview []tool.PreviewLine, width int, th theme) []string {
	if p.rows == nil || p.width != width || p.th != th {
		p.rows, p.width, p.th = make([]string, 0, len(preview)), width, th
		for _, l := range preview {
			p.rows = append(p.rows, previewLine(l, width, th)...)
		}
	}
	return p.rows
}

// approvalRows draws d, a's prompt, in place of the composer: the title, the
// preview windowed at maxPreview rows from d's scroll, the rule that asked and
// the scroll hint when the preview is cut, a blank row, and the options. The
// reason field grows to maxPreview rows within what maxRows leaves after the
// other rows, and the preview fits into what is left after it. Past maxRows the
// top is cut so the options stay visible, along with any field area the cut
// reaches.
func approvalRows(d *dialog, p *approvalPreview, a *event.ApprovalRequested, km *keyMap, width, maxPreview, maxRows int, th theme) []string {
	if maxRows <= 0 {
		return nil
	}
	fieldCap := max(1, maxPreview)
	d.body = p.lines(a.Preview, width-2, th)
	rows := d.scrollRows(width, maxRows, fieldCap, maxPreview, scrollHint(km), th)
	cut := max(0, len(rows)-maxRows)
	var areas []fieldArea
	for _, fa := range d.areas {
		if fa.y >= cut {
			fa.y -= cut
			areas = append(areas, fa)
		}
	}
	d.areas = areas
	return rows[cut:]
}

func previewLine(l tool.PreviewLine, width int, th theme) []string {
	switch l.Kind {
	case tool.Removed:
		return hang("- ", fg(th.error), l.Text, fg(th.error), width)
	case tool.Added:
		return hang("+ ", fg(th.success), l.Text, fg(th.success), width)
	}
	return hang("  ", fg(th.dim), l.Text, fg(th.dim), width)
}
