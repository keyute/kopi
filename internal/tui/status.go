package tui

import (
	"fmt"
	"slices"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/policy"
)

// contextWarnPercent is the context use from which the percentage warns.
const contextWarnPercent = 90

// statusInput is what a segment draws from: the session status and the
// current transient hint, empty when none shows.
type statusInput struct {
	app.Status
	notice string
}

// segment renders one part of the status line; false draws nothing.
type segment func(statusInput, theme) (string, bool)

// segments is keyed by the config's status segment names.
var segments = map[string]segment{
	"model": func(s statusInput, th theme) (string, bool) {
		return fg(th.accent).Render(modelID(s.Model)), true
	},
	"context": func(s statusInput, th theme) (string, bool) {
		if s.ContextWindow <= 0 {
			return "", false
		}
		pct := float64(s.ContextTokens) * 100 / float64(s.ContextWindow)
		c := th.text
		if pct >= contextWarnPercent {
			c = th.warning
		}
		return fg(c).Render(fmt.Sprintf("%.1f%%", pct)), true
	},
	"branch": func(s statusInput, th theme) (string, bool) {
		return fg(th.accent).Render(s.Branch), s.Branch != ""
	},
	"mode": func(s statusInput, th theme) (string, bool) {
		c := th.warning
		if s.Mode == policy.Execute {
			c = th.success
		}
		return fg(c).Render(s.Mode.String()), true
	},
	"notice": func(s statusInput, th theme) (string, bool) {
		return fg(th.dim).Render(s.notice), s.notice != ""
	},
	"sandbox": func(s statusInput, th theme) (string, bool) {
		if s.Sandboxed {
			return fg(th.dim).Render("sandboxed"), s.Shell
		}
		return fg(th.warning).Render("unsandboxed"), s.Shell
	},
	"tasks": func(s statusInput, th theme) (string, bool) {
		text := fmt.Sprintf("%d shells", s.Tasks)
		if s.Tasks == 1 {
			text = "1 shell"
		}
		return fg(th.dim).Render(text), s.Tasks > 0
	},
}

// statusLayout names the segments on each side of the status line.
type statusLayout struct {
	left, right []string
}

func renderSegments(names []string, in statusInput, th theme) []string {
	var parts []string
	for _, n := range names {
		if seg, ok := segments[n]; ok {
			if text, ok := seg(in, th); ok {
				parts = append(parts, text)
			}
		}
	}
	return parts
}

// statusLine sits two in from both edges. Left segments that do not fit
// are dropped from its right end. At narrow widths the right side is
// truncated, preserving the configured mode when it fits.
func statusLine(s app.Status, layout statusLayout, notice string, width int, th theme) string {
	if width <= 0 {
		return ""
	}
	in := statusInput{Status: s, notice: notice}
	sep := fg(th.dim).Render(" · ")
	right := strings.Join(renderSegments(layout.right, in, th), sep) + "  "

	parts := renderSegments(layout.left, in, th)
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
	mode, _ := segments["mode"](in, th)
	configuredMode := slices.Contains(layout.left, "mode") || slices.Contains(layout.right, "mode")
	if lipgloss.Width(right)+1 > width || (configuredMode && !strings.Contains(left+right, mode)) {
		left = ""
		right = ansi.Truncate(strings.TrimRight(right, " "), width, "")
		if configuredMode && lipgloss.Width(mode) <= width {
			names := slices.DeleteFunc(slices.Clone(layout.right), func(name string) bool { return name == "mode" })
			other := strings.Join(renderSegments(names, in, th), sep)
			budget := width - lipgloss.Width(mode) - lipgloss.Width(sep)
			right = mode
			if budget > 0 && other != "" {
				right = ansi.Truncate(other, budget, "") + sep + mode
			}
		}
	}
	gap := max(0, width-lipgloss.Width(left)-lipgloss.Width(right))
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
