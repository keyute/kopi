package tui

import (
	"fmt"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/keyute/kopi/internal/app"
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
	// planMD draws a plan bodyIndent columns in; created on first use.
	planMD *markdown
}

type cached struct {
	block transcript.Block
	folds blockFolds
	lines []line
}

func (c *renderCache) lines(b transcript.Block, f blockFolds, spans func(string) []app.Span, width int, th theme) []line {
	if c.entries == nil || width != c.width || th != c.th {
		c.entries, c.width, c.th = map[string]cached{}, width, th
		c.md, c.planMD = newMarkdown(width, th), nil
	}
	if e, ok := c.entries[b.Key()]; ok && reflect.DeepEqual(e.block, b) && reflect.DeepEqual(e.folds, f) {
		return e.lines
	}
	ls := c.render(b, f, spans)
	c.entries[b.Key()] = cached{block: b, folds: f, lines: ls}
	return ls
}

func (c *renderCache) render(b transcript.Block, f blockFolds, spans func(string) []app.Span) []line {
	width, th := c.width, c.th
	switch b := b.(type) {
	case transcript.User:
		return plain(shadedBox("❯ ", shade(th), b.Text, spans, width, th))
	case transcript.Assistant:
		return plain(c.md.lines(b.Key(), b.Text))
	case transcript.Row:
		out := renderRow(b.Call, f, width, th)
		if b.Call.Plan != "" && (b.Call.State == transcript.Running || f.expanded[0]) {
			if c.planMD == nil {
				c.planMD = newMarkdown(max(1, width-bodyIndent), th)
			}
			for _, s := range c.planMD.lines(b.Key(), b.Call.Plan) {
				out = append(out, line{text: bodyPad(0) + s})
			}
		}
		return out
	case transcript.Group:
		return renderGroup(b, f, width, th)
	case transcript.Notice:
		return plain(hang("κ ", fg(th.muted), b.Text, fg(th.muted), width))
	case transcript.Error:
		return plain(hang("κ ", fg(th.error), b.Text, fg(th.error), width))
	case transcript.Turn:
		return plain(renderTurn(b, f.verb, width, th))
	case transcript.Task:
		return renderTask(b, len(f.expanded) > 0 && f.expanded[0], width, th)
	}
	return nil
}

// memberIndent is how far a member's ↳ sits in from the start of the group's
// row, where its sentence text starts too, and bodyIndent how far an opened body
// sits in from the start of its own row; a member's body is at their sum.
const (
	memberIndent = 2
	bodyIndent   = 6
)

// tone is how a row's outcome reads in colour: a neutral, or a signal.
type tone int

const (
	toneRunning tone = iota
	toneOK
	// toneRefused is a person or a rule saying no, or cutting the row short.
	toneRefused
	toneFailed
)

// styles are the colours of the row's bullet and of its line under the title.
func (t tone) styles(th theme) (bullet, under lipgloss.Style) {
	switch t {
	case toneRunning:
		return fg(th.muted), fg(th.dim)
	case toneOK:
		return fg(th.success), fg(th.dim)
	case toneRefused:
		return fg(th.warning), fg(th.warning)
	}
	return fg(th.error), fg(th.error)
}

func callTone(s transcript.State) tone {
	switch s {
	case transcript.OK:
		return toneOK
	case transcript.Denied, transcript.Rejected, transcript.Unfinished:
		return toneRefused
	case transcript.Failed:
		return toneFailed
	}
	return toneRunning
}

func taskTone(s tool.TaskStatus) tone {
	switch s {
	case tool.TaskCompleted:
		return toneOK
	case tool.TaskStopped:
		return toneRefused
	case tool.TaskRunning:
		return toneRunning
	}
	return toneFailed
}

func renderRow(c transcript.Call, f blockFolds, width int, th theme) []line {
	bullet, lineStyle := callTone(c.State).styles(th)
	title := fg(th.text)
	if c.State == transcript.Running {
		return plain(hang("• ", bullet, c.Title, title, width))
	}
	h := commandHit(c)
	var rest []line
	switch c.State {
	case transcript.OK:
		h = openHit(c)
		if f.expanded[0] {
			rest = opened(c, 0, width, th)
		}
	case transcript.Failed:
		text := bodyText(c.Body)
		head, tail, omitted := capBody(text)
		if omitted > 0 {
			h = hit{kind: hitOutput, id: c.ID}
		}
		if f.expanded[0] {
			rest = commandLines(c, 0, width, th)
		}
		switch {
		case text == "":
		case omitted == 0 || f.expanded[0]:
			rest = append(rest, bodyLines(text, 0, width, th)...)
		default:
			rest = append(rest, bodyLines(strings.Join(head, "\n"), 0, width, th)...)
			rest = append(rest, plain(hang(bodyPad(0), fg(th.dim), fmt.Sprintf("… %d more lines", omitted), fg(th.dim), width))...)
			rest = append(rest, bodyLines(strings.Join(tail, "\n"), 0, width, th)...)
		}
	default:
		if f.expanded[0] {
			rest = commandLines(c, 0, width, th)
		}
		if c.State == transcript.Rejected {
			rest = append(rest, bodyLines(bodyText(c.Body), 0, width, th)...)
		}
	}
	out := withHit(h, hang("• ", bullet, c.Title, title, width))
	summary := c.Summary
	if c.State == transcript.Unfinished {
		summary = "interrupted"
	}
	if summary != "" {
		out = append(out, plain(under(summary, lineStyle, width))...)
	}
	return append(out, rest...)
}

// renderGroup draws a live group, or a sealed one closed behind ▸ or open
// behind ▾; live and open groups list their members.
func renderGroup(g transcript.Group, f blockFolds, width int, th theme) []line {
	var out []line
	text := fg(th.text)
	switch {
	case !g.Sealed:
		out = plain(hang("• ", fg(th.success), g.Sentence(), text, width))
	case f.open:
		out = withHit(hit{kind: hitHandle, id: g.Key()}, hang("▾ ", text, g.Sentence(), text, width))
	default:
		return withHit(hit{kind: hitHandle, id: g.Key()}, hang("▸ ", fg(th.dim), g.Sentence(), fg(th.dim), width))
	}
	for i, c := range g.Calls {
		label := c.Title
		if c.Summary != "" {
			label += " · " + c.Summary
		}
		out = append(out, withHit(openHit(c), under(label, fg(th.dim), width))...)
		if f.expanded[i] {
			out = append(out, opened(c, memberIndent, width, th)...)
		}
	}
	return out
}

// openHit makes a successful call's line toggle what it opens to; a call that
// opens to nothing has nothing to toggle.
func openHit(c transcript.Call) hit {
	if len(c.Preview) == 0 && bodyText(c.Body) == "" {
		return commandHit(c)
	}
	return hit{kind: hitOutput, id: c.ID}
}

// commandHit makes a refused or interrupted call's line toggle its command, or
// its plan; a call with neither has nothing to toggle.
func commandHit(c transcript.Call) hit {
	if c.Command == "" && c.Plan == "" {
		return hit{}
	}
	return hit{kind: hitOutput, id: c.ID}
}

// bodyPad is the columns before an opened body of a row that starts in columns in.
func bodyPad(in int) string { return strings.Repeat(" ", in+bodyIndent) }

// under is the line below a row: its arrow sits memberIndent in, and wrapped
// text hangs under the text.
func under(text string, style lipgloss.Style, width int) []string {
	return hang(strings.Repeat(" ", memberIndent)+"↳ ", style, text, style, width)
}

// opened is what a successful call opens to: its command when the title cut
// it, then a file change's lines, else its body in full.
func opened(c transcript.Call, in, width int, th theme) []line {
	out := commandLines(c, in, width, th)
	if len(c.Preview) == 0 {
		return append(out, bodyLines(bodyText(c.Body), in, width, th)...)
	}
	pad := bodyPad(in)
	for _, s := range previewRows(c.Preview, width-in-bodyIndent, th) {
		out = append(out, line{text: pad + s})
	}
	return out
}

// commandLines is the whole command after a "$", nil when the title shows it
// all. It is not passed through bodyText, which would drop what the command
// itself holds.
func commandLines(c transcript.Call, in, width int, th theme) []line {
	if c.Command == "" {
		return nil
	}
	return plain(hang(bodyPad(in)+"$ ", fg(th.dim), c.Command, fg(th.text), width))
}

// bodyLines draws text as the body of a row that starts in columns in.
func bodyLines(text string, in, width int, th theme) []line {
	if text == "" {
		return nil
	}
	return plain(hang(bodyPad(in), fg(th.muted), text, fg(th.muted), width))
}

// bodyText prepares a command's output for drawing: a carriage return redraws
// its line, so a line keeps only what follows its last one.
func bodyText(body string) string {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	for i, l := range lines {
		l = strings.TrimRight(l, "\r")
		lines[i] = l[strings.LastIndex(l, "\r")+1:]
	}
	text := strings.TrimRight(strings.Join(lines, "\n"), "\n")
	if strings.TrimSpace(text) == "" {
		return ""
	}
	return text
}

// capBody cuts a failure body longer than five non-empty lines to its first
// two and last two, and counts the non-empty lines between; omitted is 0 when
// the body is not cut.
func capBody(text string) (head, tail []string, omitted int) {
	var ne []string
	for l := range strings.SplitSeq(text, "\n") {
		if strings.TrimSpace(l) != "" {
			ne = append(ne, l)
		}
	}
	if len(ne) <= 5 {
		return nil, nil, 0
	}
	return ne[:2], ne[len(ne)-2:], len(ne) - 4
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
// agent's: description's) first line cut to fit. A workflow adds its agent
// count and a failed run's reason; a task with a result opens to it.
func renderTask(t transcript.Task, expanded bool, width int, th theme) []line {
	bullet, lineStyle := taskTone(tool.TaskStatus(t.Status)).styles(th)
	word := "finished"
	if t.Status != string(tool.TaskCompleted) {
		word = t.Status
	}
	head := "shell " + word + " · "
	if t.Agent != "" {
		head = oneLine(t.Agent) + " " + word + " · "
	}
	tail := " · " + formatDuration(t.Elapsed)
	command, _, _ := strings.Cut(t.Command, "\n")
	command = cut(oneLine(command), width-2-lipgloss.Width(head+tail))
	result := bodyText(t.Result)
	h := hit{}
	if result != "" {
		h = hit{kind: hitOutput, id: t.Key()}
	}
	out := withHit(h, hang("• ", bullet, head+command+tail, fg(th.text), width))
	var parts []string
	if t.Agents > 0 {
		parts = append(parts, countText(t.Agents, "agent")+failedSuffix(t.Failed))
	}
	if t.Reason != "" {
		parts = append(parts, t.Reason)
	}
	if len(parts) > 0 {
		out = append(out, plain(under(strings.Join(parts, " · "), lineStyle, width))...)
	}
	if expanded {
		out = append(out, bodyLines(result, 0, width, th)...)
	}
	return out
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
// with continuation lines two in, a blank shaded row, all full width. spans,
// when not nil, gives the runs of the cleaned text to highlight.
func shadedBox(prefix string, ps lipgloss.Style, text string, spans func(string) []app.Span, width int, th theme) []string {
	st := shade(th)
	blank := st.Render(strings.Repeat(" ", width))
	out := []string{blank}
	text = clean(text)
	var runs []app.Span
	if spans != nil {
		runs = spans(text)
	}
	first := true
	for n, l := range strings.Split(text, "\n") {
		// Wrap only drops whitespace at a break, so the rows are in-order substrings of l.
		cursor := 0
		for _, s := range strings.Split(lipgloss.Wrap(l, max(1, width-2), ""), "\n") {
			p := st.Render("  ")
			if first {
				p = ps.Render(prefix)
				first = false
			}
			pad := strings.Repeat(" ", max(0, width-2-lipgloss.Width(s)))
			row := p + st.Render(s+pad)
			if at := strings.Index(l[cursor:], s); at >= 0 {
				start := utf8.RuneCountInString(l[:cursor+at])
				cursor += at + len(s)
				row = styleRuns(row, runs, n, start, s, th)
			}
			out = append(out, row)
		}
	}
	return append(out, blank)
}

func shade(th theme) lipgloss.Style {
	return lipgloss.NewStyle().Background(th.userMessageBg).Foreground(th.text)
}

// wrap breaks text into lines no wider than width. Tabs become spaces because
// they have no fixed width.
func wrap(text string, width int) []string {
	return strings.Split(lipgloss.Wrap(clean(text), max(1, width), ""), "\n")
}

// clean strips escape sequences and control characters from text and turns
// tabs into spaces.
func clean(text string) string {
	// Strip removes escape sequences but retains executable controls.
	text = strings.Map(func(r rune) rune {
		if (r < 0x20 && r != '\n' && r != '\t') || (r >= 0x7f && r <= 0x9f) {
			return -1
		}
		return r
	}, ansi.Strip(text))
	return strings.ReplaceAll(text, "\t", "    ")
}

func plain(ss []string) []line { return withHit(hit{}, ss) }

func withHit(h hit, ss []string) []line {
	out := make([]line, len(ss))
	for i, s := range ss {
		out[i] = line{text: s, hit: h}
	}
	return out
}
