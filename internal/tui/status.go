package tui

import (
	"fmt"
	"image/color"
	"slices"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/config"
	"github.com/keyute/kopi/internal/policy"
)

// contextWarnPercent is the context use from which the percentage warns.
const contextWarnPercent = 90

// statusInput is what a segment draws from: the session status, the
// current transient hint (empty when none shows), the home directory shown
// as "~" and the last cleaned output of each command segment.
type statusInput struct {
	app.Status
	notice   string
	home     string
	commands map[string]string
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
	"cwd": func(s statusInput, th theme) (string, bool) {
		return fg(th.muted).Render(tildePath(s.Workspace, s.home)), s.Workspace != ""
	},
	"tasks": func(s statusInput, th theme) (string, bool) {
		text := fmt.Sprintf("%d shells", s.Tasks)
		if s.Tasks == 1 {
			text = "1 shell"
		}
		return fg(th.dim).Render(text), s.Tasks > 0
	},
}

// statusLayout names the segments on each side of the status line, the
// separator between them and the user's segments.
type statusLayout struct {
	left, right []string
	separator   string
	segments    map[string]config.StatusSegment
}

func renderSegments(names []string, in statusInput, layout statusLayout, th theme) []string {
	var parts []string
	for _, n := range names {
		var text string
		var ok bool
		if seg, builtin := segments[n]; builtin {
			text, ok = seg(in, th)
		} else if def, user := layout.segments[n]; user {
			text, ok = renderUserSegment(n, def, in, th)
		}
		if ok {
			parts = append(parts, text)
		}
	}
	return parts
}

func renderUserSegment(name string, def config.StatusSegment, in statusInput, th theme) (string, bool) {
	c := th.text
	if def.Color != "" {
		c = segmentColor(def.Color, th)
	}
	if def.Command == "" {
		return fg(c).Render(def.Text), true
	}
	out := in.commands[name]
	if out == "" {
		return "", false
	}
	return fg(c).Render(out), true
}

// segmentColor resolves a role name against th, so a theme switch recolours
// it, and otherwise takes the value as a literal colour.
func segmentColor(value string, th theme) color.Color {
	if slot, ok := th.roles()[value]; ok {
		return *slot
	}
	return lipgloss.Color(value)
}

// tildePath shows the home directory prefix of path as "~" when it ends at
// a path boundary.
func tildePath(path, home string) string {
	if home == "" {
		return path
	}
	if path == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(path, strings.TrimRight(home, "/\\")); ok && rest != "" && (rest[0] == '/' || rest[0] == '\\') {
		return "~" + rest
	}
	return path
}

// cleanStatusOutput keeps the first line of a command's output, its SGR
// sequences and its printable text; every other escape sequence and control
// character is dropped. An SGR sequence is closed with a reset so it cannot
// colour what follows.
func cleanStatusOutput(out string) string {
	line, _, _ := strings.Cut(out, "\n")
	line = strings.TrimRight(line, "\r")
	var pieces []string
	var state byte
	sgr := false
	for len(line) > 0 {
		seq, width, n, next := ansi.DecodeSequence(line, state, nil)
		state = next
		line = line[n:]
		switch {
		case strings.HasPrefix(seq, "\x1b[") && strings.HasSuffix(seq, "m") && isSGR(seq[2:len(seq)-1]):
			pieces = append(pieces, seq)
			sgr = true
		case width > 0 && !strings.HasPrefix(seq, "\x1b"):
			pieces = append(pieces, seq)
		}
	}
	// Trailing blanks and SGR-only tails are dropped; the reset is added back.
	for len(pieces) > 0 {
		last := pieces[len(pieces)-1]
		if strings.HasPrefix(last, "\x1b") {
			pieces = pieces[:len(pieces)-1]
			continue
		}
		last = strings.TrimRight(last, " ")
		if last != "" {
			pieces[len(pieces)-1] = last
			break
		}
		pieces = pieces[:len(pieces)-1]
	}
	text := strings.TrimLeft(strings.Join(pieces, ""), " ")
	if ansi.StringWidth(text) == 0 {
		return ""
	}
	if sgr {
		text += "\x1b[m"
	}
	return text
}

func isSGR(params string) bool {
	return !strings.ContainsFunc(params, func(r rune) bool { return (r < '0' || r > '9') && r != ';' && r != ':' })
}

// statusLine sits two in from both edges. Left segments that do not fit
// are dropped from its right end. At narrow widths the right side is
// truncated, preserving the configured mode when it fits.
func statusLine(in statusInput, layout statusLayout, width int, th theme) string {
	if width <= 0 {
		return ""
	}
	sep := fg(th.dim).Render(layout.separator)
	right := strings.Join(renderSegments(layout.right, in, layout, th), sep) + "  "

	parts := renderSegments(layout.left, in, layout, th)
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
			other := strings.Join(renderSegments(names, in, layout, th), sep)
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
