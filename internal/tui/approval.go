package tui

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"

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

// newApprovalDialog is the prompt for a: the rule that asked as its note and
// approvalOptions as its options, the reject option taking a reason.
func newApprovalDialog(a *event.ApprovalRequested, km *keyMap, th theme) *dialog {
	d := &dialog{title: a.Title}
	if a.Agent != "" {
		d.title = a.Agent + " " + a.TaskID + " › " + a.Title
	}
	switch {
	case a.Rule != "":
		d.note = "asked by " + a.Rule
	case a.Review != nil && a.Review.Verdict == policy.VerdictDeny:
		d.note = "the classifier would deny this"
		if a.Review.Reason != "" {
			d.note += ": " + a.Review.Reason
		}
	case a.Review != nil:
		d.note = "the classifier could not decide"
		if a.Review.Reason != "" {
			d.note += ": " + a.Review.Reason
		}
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

// approvalRows draws d, a's prompt, in place of the composer: the title, the
// preview capped at maxPreview rows, the rule that asked, a blank row, and
// the options. The reason field grows to maxPreview rows within what maxRows
// leaves after the other rows, and the preview fits into what is left after
// it. Past maxRows the top is cut so the options stay visible, along with
// any field area the cut reaches.
func approvalRows(d *dialog, a *event.ApprovalRequested, width, maxPreview, maxRows int, th theme) []string {
	if maxRows <= 0 {
		return nil
	}
	fieldCap := max(1, maxPreview)
	// Measure the rest without the last render's preview.
	d.body = nil
	d.body = previewRows(a.Preview, min(maxPreview, maxRows-len(d.rows(width, maxRows, fieldCap, th))), width-2, th)
	rows := d.rows(width, maxRows, fieldCap, th)
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

// previewRows draws whole preview lines within limit rows; when they do not
// all fit, the last row counts the lines left out. Lines past the cut are
// never rendered, so a large preview costs only what shows.
func previewRows(p []tool.PreviewLine, limit, width int, th theme) []string {
	if limit < 1 {
		return nil
	}
	var rows []string
	for i, l := range p {
		budget := limit - len(rows)
		if i != len(p)-1 {
			budget--
		}
		if previewFits(l.Text, width-2, budget) {
			rows = append(rows, previewLine(l, width, th)...)
			continue
		}
		more := fmt.Sprintf("… %d more lines", len(p)-i)
		if len(p)-i == 1 {
			more = "… 1 more line"
		}
		return append(rows, hang("  ", fg(th.dim), more, fg(th.dim), width)[0])
	}
	return rows
}

// previewFits probes geometrically growing prefixes before wrapping the whole
// line. Overflow is irreversible at a complete, sanitized grapheme boundary;
// leave the last cluster out because the unseen suffix may still extend it.
// Invisible text and discarded whitespace may require reading the whole input.
func previewFits(text string, width, budget int) bool {
	if budget < 1 {
		return false
	}
	for end := 256; end < len(text); {
		cut := end
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		prefix := strings.Map(func(r rune) rune {
			if (r < 0x20 && r != '\n' && r != '\t') || (r >= 0x7f && r <= 0x9f) {
				return -1
			}
			return r
		}, ansi.Strip(text[:cut]))
		last := 0
		for pos := 0; pos < len(prefix); {
			last = pos
			cluster, _ := ansi.FirstGraphemeCluster(prefix[pos:], ansi.GraphemeWidth)
			pos += len(cluster)
		}
		if len(wrap(prefix[:last], width)) > budget {
			return false
		}
		if end > len(text)/2 {
			break
		}
		end *= 2
	}
	return len(wrap(text, width)) <= budget
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
