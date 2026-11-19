package tui

import (
	"fmt"
	"reflect"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/tool"
	"github.com/keyute/kopi/internal/transcript"
)

// line is one rendered row of the transcript.
type line struct {
	text string
	hit  hit
}

type hitKind int

const (
	hitNone hitKind = iota
	// hitHandle toggles the sealed group whose key is id.
	hitHandle
	// hitOutput toggles the output of the call whose ID is id.
	hitOutput
)

// hit names what a left click on a line acts on; the zero value is nothing.
type hit struct {
	kind hitKind
	id   string
}

// renderCache holds each block's lines by key; a block is re-rendered only
// when its value or its fold state changed. A new width or theme drops every
// entry.
type renderCache struct {
	width   int
	th      theme
	entries map[string]cached
	md      *markdown
}

type cached struct {
	block transcript.Block
	folds blockFolds
	lines []line
}

func (c *renderCache) reset() { c.entries = nil }

func (c *renderCache) lines(b transcript.Block, f blockFolds, width int, th theme) []line {
	if c.entries == nil || width != c.width || th != c.th {
		c.entries, c.width, c.th = map[string]cached{}, width, th
		c.md = newMarkdown(width, th)
	}
	if e, ok := c.entries[b.Key()]; ok && reflect.DeepEqual(e.block, b) && reflect.DeepEqual(e.folds, f) {
		return e.lines
	}
	ls := c.render(b, f)
	c.entries[b.Key()] = cached{block: b, folds: f, lines: ls}
	return ls
}

func (c *renderCache) render(b transcript.Block, f blockFolds) []line {
	width, th := c.width, c.th
	switch b := b.(type) {
	case transcript.User:
		return plain(shadedBox("❯ ", shade(th), b.Text, width, th))
	case transcript.Assistant:
		return plain(c.md.lines(b.Key(), b.Text))
	case transcript.Plan:
		return plain(append([]string{fg(th.muted).Render("Plan")}, c.md.lines(b.Key(), b.Text)...))
	case transcript.Row:
		return renderRow(b.Call, f, width, th)
	case transcript.Group:
		return renderGroup(b, f, width, th)
	case transcript.Notice:
		return plain(hang("κ ", fg(th.muted), b.Text, fg(th.muted), width))
	case transcript.Error:
		return plain(hang("κ ", fg(th.error), b.Text, fg(th.error), width))
	case transcript.Turn:
		return plain(renderTurn(b, f.verb, width, th))
	case transcript.Task:
		return plain(renderTask(b, width, th))
	}
	return nil
}

func renderRow(c transcript.Call, f blockFolds, width int, th theme) []line {
	none := lipgloss.NewStyle()
	switch c.State {
	case transcript.OK:
		out := withHit(outputHit(c), hang("• ", fg(th.success), c.Title, none, width))
		if c.Summary != "" {
			out = append(out, plain(hang("  ↳ ", fg(th.dim), c.Summary, fg(th.dim), width))...)
		}
		if f.expanded[0] {
			out = append(out, outputLines(c, width, th)...)
		}
		return out
	case transcript.Failed:
		out := hang("• ", fg(th.error), c.Title, fg(th.error), width)
		if output := strings.TrimRight(c.Output, "\n"); output != "" {
			out = append(out, hang("  ", fg(th.error), output, fg(th.error), width)...)
		}
		return plain(out)
	case transcript.Unfinished:
		out := hang("• ", fg(th.warning), c.Title, none, width)
		return plain(append(out, hang("  ↳ ", fg(th.dim), "interrupted", fg(th.dim), width)...))
	}
	return plain(hang("• ", fg(th.muted), c.Title, none, width))
}

// renderGroup draws a live group, or a sealed one closed behind ▸ or open
// behind ▾; live and open groups list their members.
func renderGroup(g transcript.Group, f blockFolds, width int, th theme) []line {
	var out []line
	switch {
	case !g.Sealed:
		out = plain(hang("• ", fg(th.success), g.Sentence(), lipgloss.NewStyle(), width))
	case f.open:
		out = withHit(hit{kind: hitHandle, id: g.Key()}, hang("▾ ", lipgloss.NewStyle(), g.Sentence(), lipgloss.NewStyle(), width))
	default:
		return withHit(hit{kind: hitHandle, id: g.Key()}, hang("▸ ", fg(th.dim), g.Sentence(), fg(th.dim), width))
	}
	for i, c := range g.Calls {
		label := c.Title
		if c.Summary != "" {
			label += " · " + c.Summary
		}
		out = append(out, withHit(outputHit(c), hang("  ↳ ", fg(th.dim), label, fg(th.dim), width))...)
		if f.expanded[i] {
			out = append(out, outputLines(c, width, th)...)
		}
	}
	return out
}

// outputHit makes a call's line toggle its output; a call without output
// has nothing to toggle.
func outputHit(c transcript.Call) hit {
	if strings.TrimSpace(c.Output) == "" {
		return hit{}
	}
	return hit{kind: hitOutput, id: c.ID}
}

// outputLines is a call's output in full, four in.
func outputLines(c transcript.Call, width int, th theme) []line {
	output := strings.TrimRight(c.Output, "\n")
	if strings.TrimSpace(output) == "" {
		return nil
	}
	return plain(hang("    ", fg(th.muted), output, fg(th.muted), width))
}

func renderTurn(t transcript.Turn, v verb, width int, th theme) []string {
	var text string
	var c lipgloss.Style
	switch t.Outcome.Status {
	case event.Completed:
		if t.Elapsed == 0 {
			return nil
		}
		text, c = v.ed+" for "+formatDuration(t.Elapsed), fg(th.dim)
	case event.Interrupted:
		text, c = "Interrupted", fg(th.warning)
	case event.Failed:
		text, c = "Failed", fg(th.error)
	}
	if t.Outcome.Status != event.Completed && t.Elapsed > 0 {
		text += " after " + formatDuration(t.Elapsed)
	}
	out := hang("κ ", c, text, c, width)
	if t.Outcome.Message != "" {
		out = append(out, hang("  ", fg(th.error), t.Outcome.Message, fg(th.error), width)...)
	}
	return out
}

// renderTask is one row naming a background task's end, its command's (an
// agent's: description's) first line cut to fit.
func renderTask(t transcript.Task, width int, th theme) []string {
	word, ps, ts := "finished", fg(th.success), lipgloss.NewStyle()
	if t.Status != string(tool.TaskCompleted) {
		word, ps, ts = t.Status, fg(th.error), fg(th.error)
	}
	head := "task " + oneLine(t.ID) + " " + word + " · "
	if t.Agent != "" {
		head = oneLine(t.Agent) + " " + word + " · "
	}
	tail := " · " + formatDuration(t.Elapsed)
	command, _, _ := strings.Cut(t.Command, "\n")
	command = cut(oneLine(command), width-2-lipgloss.Width(head+tail))
	return hang("• ", ps, head+command+tail, ts, width)
}

// formatDuration renders d as "41s", "1m 12s" or "1h 2m".
func formatDuration(d time.Duration) string {
	h, m, s := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm %ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

// hang wraps text after prefix, aligning continuation lines under the text.
func hang(prefix string, ps lipgloss.Style, text string, ts lipgloss.Style, width int) []string {
	pw := lipgloss.Width(prefix)
	indent := strings.Repeat(" ", pw)
	segs := wrap(text, width-pw)
	out := make([]string, len(segs))
	for i, s := range segs {
		p := indent
		if i == 0 {
			p = ps.Render(prefix)
		}
		out[i] = p + ts.Render(s)
	}
	return out
}

// shadedBox is the user box: a blank shaded row, prefix and the wrapped text
// with continuation lines two in, a blank shaded row, all full width.
func shadedBox(prefix string, ps lipgloss.Style, text string, width int, th theme) []string {
	st := shade(th)
	blank := st.Render(strings.Repeat(" ", width))
	out := []string{blank}
	for i, s := range wrap(text, width-2) {
		p := st.Render("  ")
		if i == 0 {
			p = ps.Render(prefix)
		}
		pad := strings.Repeat(" ", max(0, width-2-lipgloss.Width(s)))
		out = append(out, p+st.Render(s+pad))
	}
	return append(out, blank)
}

func shade(th theme) lipgloss.Style {
	return lipgloss.NewStyle().Background(th.userMessageBg).Foreground(th.text)
}

// wrap breaks text into lines no wider than width. Tabs become spaces because
// they have no fixed width.
func wrap(text string, width int) []string {
	// Strip removes escape sequences but retains executable controls.
	text = strings.Map(func(r rune) rune {
		if (r < 0x20 && r != '\n' && r != '\t') || (r >= 0x7f && r <= 0x9f) {
			return -1
		}
		return r
	}, ansi.Strip(text))
	text = strings.ReplaceAll(text, "\t", "    ")
	return strings.Split(lipgloss.Wrap(text, max(1, width), ""), "\n")
}

func plain(ss []string) []line { return withHit(hit{}, ss) }

func withHit(h hit, ss []string) []line {
	out := make([]line, len(ss))
	for i, s := range ss {
		out[i] = line{text: s, hit: h}
	}
	return out
}
