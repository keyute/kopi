package tui

import (
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/policy"
)

// contextWarnPercent is the context use from which the percentage warns.
const contextWarnPercent = 90

// statusLine sits two in from both edges: model id, context use and branch
// on the left, the mode on the right. Parts that do not fit are dropped from
// the right end of the left side; the mode always shows.
func statusLine(s app.Status, width int, th theme) string {
	c := th.warning
	if s.Mode == policy.Execute {
		c = th.success
	}
	right := fg(c).Render(s.Mode.String()) + "  "

	parts := []string{fg(th.accent).Render(modelID(s.Model))}
	if s.ContextWindow > 0 {
		pct := s.ContextTokens * 100 / s.ContextWindow
		pc := th.dim
		if pct >= contextWarnPercent {
			pc = th.warning
		}
		parts = append(parts, fg(pc).Render(strconv.Itoa(pct)+"%"))
	}
	if s.Branch != "" {
		parts = append(parts, fg(th.accent).Render(s.Branch))
	}
	sep := fg(th.dim).Render(" · ")
	var left string
	for n := len(parts); n >= 0; n-- {
		left = ""
		if n > 0 {
			left = "  " + strings.Join(parts[:n], sep)
		}
		if lipgloss.Width(left)+1+lipgloss.Width(right) <= width {
			break
		}
	}
	gap := max(1, width-lipgloss.Width(left)-lipgloss.Width(right))
	return left + strings.Repeat(" ", gap) + right
}

// modelID is the part of a "provider/id" model reference after the first
// "/", or the whole reference without one.
func modelID(ref string) string {
	if _, id, ok := strings.Cut(ref, "/"); ok {
		return id
	}
	return ref
}
