package tui

import (
	"fmt"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// cursorMark marks the selected row in the menu and in dialogs.
const cursorMark = "❭ "

// dialog is a choice drawn in the composer's slot.
type dialog struct {
	title   string
	body    []string // rendered at width-2; rows indents them
	note    string
	options []string
	sel     int
}

// rows draws d between two rules: the title, body and note two in, a blank
// row, then the numbered options with the cursor on sel.
func (d dialog) rows(width int, th theme) []string {
	none := lipgloss.NewStyle()
	rule := fg(th.frame).Render(strings.Repeat("─", width))
	rows := append([]string{rule}, hang("  ", none, d.title, none.Bold(true), width)...)
	for _, r := range d.body {
		rows = append(rows, "  "+r)
	}
	if d.note != "" {
		rows = append(rows, hang("  ", none, d.note, fg(th.dim), width)...)
	}
	rows = append(rows, "")
	for i, o := range d.options {
		prefix, st := "  ", none
		if i == d.sel {
			prefix, st = cursorMark, fg(th.accent)
		}
		rows = append(rows, hang(prefix, fg(th.accent), fmt.Sprintf("%d. %s", i+1, o), st, width)...)
	}
	return append(rows, rule)
}

// dialogKey is msg's effect on n options with sel selected: the new
// selection, and whether msg picks it.
func dialogKey(msg tea.KeyPressMsg, km *keyMap, sel, n int) (int, bool) {
	switch {
	case key.Matches(msg, km.menuUp):
		return max(0, sel-1), false
	case key.Matches(msg, km.menuDown):
		return min(n-1, sel+1), false
	case key.Matches(msg, km.submit):
		return sel, true
	}
	if d, err := strconv.Atoi(msg.String()); err == nil && d >= 1 && d <= n {
		return d - 1, true
	}
	return sel, false
}
