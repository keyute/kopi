package tui

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
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
	if maxRows <= 0 {
		return nil
	}
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
