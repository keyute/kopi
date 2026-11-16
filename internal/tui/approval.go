package tui

import (
	"fmt"

	"charm.land/lipgloss/v2"

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
	no := "No"
	if ks := km.interrupt.Keys(); len(ks) > 0 {
		no += " (" + ks[0] + ")"
	}
	return append(opts, approvalOption{no, policy.Reject})
}

// approvalRows draws the prompt in place of the composer: the title, the
// preview capped at maxPreview rows, the rule that asked, a blank row, the
// options with the cursor on choice, and a blank row. maxRows caps the whole
// prompt; past it the top is cut so the options stay visible.
func approvalRows(a *event.ApprovalRequested, choice int, km *keyMap, width, maxPreview, maxRows int, th theme) []string {
	none := lipgloss.NewStyle()
	head := hang("", none, a.Title, none.Bold(true), width)
	var rule []string
	if a.Rule != "" {
		rule = hang("", none, "asked by "+a.Rule, fg(th.dim), width)
	}
	tail := []string{""}
	for i, o := range approvalOptions(a, km) {
		prefix, st := "  ", none
		if i == choice {
			prefix, st = "❯ ", fg(th.accent)
		}
		tail = append(tail, hang(prefix, fg(th.accent), fmt.Sprintf("%d. %s", i+1, o.label), st, width)...)
	}
	tail = append(tail, "")

	limit := min(maxPreview, maxRows-len(head)-len(rule)-len(tail))
	rows := head
	rows = append(rows, previewRows(a.Preview, limit, width, th)...)
	rows = append(rows, rule...)
	rows = append(rows, tail...)
	return rows[max(0, len(rows)-maxRows):]
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
		rs := previewLine(l, width, th)
		last := i == len(p)-1
		if len(rows)+len(rs) < limit || (last && len(rows)+len(rs) == limit) {
			rows = append(rows, rs...)
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

func previewLine(l tool.PreviewLine, width int, th theme) []string {
	switch l.Kind {
	case tool.Removed:
		return hang("- ", fg(th.error), l.Text, fg(th.error), width)
	case tool.Added:
		return hang("+ ", fg(th.success), l.Text, fg(th.success), width)
	}
	return hang("  ", fg(th.dim), l.Text, fg(th.dim), width)
}
