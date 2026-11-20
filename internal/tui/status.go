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

// statusInput is what a segment draws from: the session status with the open
// view's effort, tool calls and cost (Effort is empty when the model does not
// reason; Cost is in dollars), the current transient hint (empty when none shows), the home directory shown
// as "~" and the last cleaned output of each command segment.
type statusInput struct {
	app.Status
	Effort    string
	ToolCalls int
	Cost      float64
	// added and removed are the lines changed against HEAD.
	added, removed int
	notice         string
	home           string
	commands       map[string]string
}

// segment renders one part of the status line; false draws nothing.
type segment func(statusInput, theme) (string, bool)

// segments is keyed by the config's status segment names.
var segments = map[string]segment{
	"model": func(s statusInput, th theme) (string, bool) {
		return fg(th.accent).Render(modelID(s.Model)), s.Model != ""
	},
	"effort": func(s statusInput, th theme) (string, bool) {
		return fg(th.text).Render(s.Effort), s.Effort != ""
	},
	"context": func(s statusInput, th theme) (string, bool) {
		text, warn, ok := contextText(s.ContextTokens, s.ContextWindow)
		c := th.text
		if warn {
			c = th.warning
		}
		return fg(c).Render(text), ok
	},
	"tools": func(s statusInput, th theme) (string, bool) {
		text := toolsText(s.ToolCalls)
		return fg(th.text).Render(text), text != ""
	},
	"cost": func(s statusInput, th theme) (string, bool) {
		text := costText(s.Cost)
		return fg(th.text).Render(text), text != ""
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
	"agents": func(s statusInput, th theme) (string, bool) {
		return fg(th.dim).Render(countText(s.Agents, "agent")), s.Agents > 0
	},
	"shells": func(s statusInput, th theme) (string, bool) {
		return fg(th.dim).Render(countText(s.Shells, "shell")), s.Shells > 0
	},
	"workflows": func(s statusInput, th theme) (string, bool) {
		return fg(th.dim).Render(countText(s.Workflows, "workflow")), s.Workflows > 0
	},
	"tokens": func(s statusInput, th theme) (string, bool) {
		return fg(th.text).Render(tokensText(s.ContextTokens)), s.ContextTokens > 0
	},
	"changes": func(s statusInput, th theme) (string, bool) {
		var parts []string
		if s.added > 0 {
			parts = append(parts, fg(th.success).Render(fmt.Sprintf("+%d", s.added)))
		}
		if s.removed > 0 {
			parts = append(parts, fg(th.error).Render(fmt.Sprintf("-%d", s.removed)))
		}
		return strings.Join(parts, " "), len(parts) > 0
	},
}

// contextText is the share of the window the context fills, and whether it is
// at or over contextWarnPercent; ok is false while the usage is unknown.
func contextText(tokens, window int) (text string, warn, ok bool) {
	if window <= 0 {
		return "", false, false
	}
	pct := float64(tokens) * 100 / float64(window)
	return fmt.Sprintf("%.1f%%", pct), pct >= contextWarnPercent, true
}

// toolsText is the tool call count, "" for none.
func toolsText(n int) string {
	switch n {
	case 0:
		return ""
	case 1:
		return "1 tool"
	}
	return fmt.Sprintf("%d tools", n)
}

// costText is the cost in dollars, "" for none.
func costText(cost float64) string {
	if cost <= 0 {
		return ""
	}
	return fmt.Sprintf("$%.2f", cost)
}

// statusLayout holds the parsed entries on each side of the status line, the
// separator between them and the user's segments.
type statusLayout struct {
	left, right []config.Format
	separator   string
	segments    map[string]config.StatusSegment
	// mode is the entry that names the mode segment, the right side's first
	// else the left side's first; nil when none does.
	mode config.Format
}

func namesMode(f config.Format) bool { return slices.Contains(f.Segments(), "mode") }

func newStatusLayout(line config.StatusLine) statusLayout {
	l := statusLayout{
		left: parseFormats(line.Left), right: parseFormats(line.Right),
		separator: line.Separator, segments: line.Segments,
	}
	for _, side := range [][]config.Format{l.right, l.left} {
		if i := slices.IndexFunc(side, namesMode); i >= 0 {
			l.mode = side[i]
			break
		}
	}
	return l
}

// parseFormats parses entries the config has already validated.
func parseFormats(entries []string) []config.Format {
	fs := make([]config.Format, len(entries))
	for i, e := range entries {
		fs[i], _ = config.ParseFormat(e)
	}
	return fs
}

// renderParts draws f: lookup gives a segment's text, literal styles literal
// text. A group draws only when a segment inside it does.
func renderParts(f config.Format, lookup func(string) (string, bool), literal func(string) string) (string, bool) {
	var b strings.Builder
	shown := false
	for _, p := range f {
		switch p.Kind {
		case config.Literal:
			b.WriteString(literal(p.Text))
		case config.Segment:
			if text, ok := lookup(p.Text); ok {
				b.WriteString(text)
				shown = true
			}
		case config.Group:
			if text, ok := renderParts(p.Parts, lookup, literal); ok {
				b.WriteString(text)
				shown = true
			}
		}
	}
	return b.String(), shown
}

// renderEntries draws each entry, leaving out those that draw nothing.
func renderEntries(fs []config.Format, lookup func(string) (string, bool), literal func(string) string) []string {
	var parts []string
	for _, f := range fs {
		if text, _ := renderParts(f, lookup, literal); text != "" {
			parts = append(parts, text)
		}
	}
	return parts
}

func (l statusLayout) lookup(in statusInput, th theme) func(string) (string, bool) {
	return func(name string) (string, bool) {
		if seg, builtin := segments[name]; builtin {
			return seg(in, th)
		}
		if def, user := l.segments[name]; user {
			return renderUserSegment(name, def, in, th)
		}
		return "", false
	}
}

func (l statusLayout) render(fs []config.Format, in statusInput, th theme) []string {
	return renderEntries(fs, l.lookup(in, th), func(s string) string { return fg(th.text).Render(s) })
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
	trimmed := strings.TrimRight(home, "/\\")
	if trimmed == "" {
		return path
	}
	if rest, ok := strings.CutPrefix(path, trimmed); ok && rest != "" && (rest[0] == '/' || rest[0] == '\\') {
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

// statusLine sits two in from both edges. Left entries that do not fit
// are dropped from its right end. At narrow widths the right side is
// truncated, preserving the entry that names the mode when it fits.
func statusLine(in statusInput, layout statusLayout, width int, th theme) string {
	if width <= 0 {
		return ""
	}
	sep := fg(th.dim).Render(layout.separator)
	right := strings.Join(layout.render(layout.right, in, th), sep) + "  "

	parts := layout.render(layout.left, in, th)
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
	configuredMode := layout.mode != nil
	var mode string
	if configuredMode {
		mode = strings.Join(layout.render([]config.Format{layout.mode}, in, th), "")
	}
	if lipgloss.Width(right)+1 > width || (configuredMode && !strings.Contains(left+right, mode)) {
		left = ""
		right = ansi.Truncate(strings.TrimRight(right, " "), width, "")
		if configuredMode && lipgloss.Width(mode) <= width {
			others := slices.DeleteFunc(slices.Clone(layout.right), namesMode)
			other := strings.Join(layout.render(others, in, th), sep)
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
