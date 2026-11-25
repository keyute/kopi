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
	// planMD draws a plan at its row's body width; created on first use.
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
		o := renderRow(b.Call, f, width, th)
		if b.Call.Plan != "" && (b.Call.State == transcript.Running || f.expanded[0]) {
			if w := max(1, o.bodyWidth()); c.planMD == nil || c.planMD.width != w {
				c.planMD = newMarkdown(w, th)
			}
			o.body(plain(c.planMD.lines(b.Key(), b.Call.Plan)))
		}
		return o.rows
	case transcript.Group:
		return renderGroup(b, f, width, th)
	case transcript.Notice:
		return plain(hang("κ ", fg(th.muted), b.Text, fg(th.muted), width))
	case transcript.Error:
		return plain(hang("κ ", fg(th.error), b.Text, fg(th.error), width))
	case transcript.Turn:
		return renderTurn(b, f.verb, width, th)
	case transcript.Task:
		return renderTask(b, len(f.expanded) > 0 && f.expanded[0], width, th)
	case transcript.Command:
		return commandRows(b.UserCommand, commandOptions{
			Lines:    transcriptCommandLines,
			Expanded: len(f.expanded) > 0 && f.expanded[0],
			Hit:      hit{kind: hitOutput, id: b.Key()},
		}, width, th)
	}
	return nil
}

// step is how far a line's text starts past the text of the line it is under;
// only outline places a line.
const step = 2

// outline draws a row top down. col is the column where the text of its last
// title or ↳ line starts, which is what a body under it is placed by.
type outline struct {
	width, col int
	rows       []line
}

func (o *outline) title(glyph string, gs lipgloss.Style, text string, ts lipgloss.Style, h hit) {
	o.rows = append(o.rows, withHit(h, hang(glyph, gs, text, ts, o.width))...)
	o.col = lipgloss.Width(glyph)
}

func (o *outline) under(text string, style lipgloss.Style, h hit) {
	o.rows = append(o.rows, withHit(h, under(text, style, o.width))...)
	o.col = step + lipgloss.Width("↳ ")
}

// bodyWidth is the width for drawing rows that body places.
func (o *outline) bodyWidth() int { return o.width - o.col - step }

// body appends rows drawn flush left, step past the text above.
func (o *outline) body(rows []line) {
	pad := strings.Repeat(" ", o.col+step)
	for _, r := range rows {
		o.rows = append(o.rows, line{text: pad + r.text, hit: r.hit})
	}
}

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

func renderRow(c transcript.Call, f blockFolds, width int, th theme) *outline {
	bullet, lineStyle := callTone(c.State).styles(th)
	title := fg(th.text)
	o := &outline{width: width}
	if c.State == transcript.Running {
		o.title("• ", bullet, c.Title, title, hit{})
		return o
	}
	h := commandHit(c)
	var text string
	var head, tail []string
	var omitted int
	switch c.State {
	case transcript.OK:
		h = openHit(c)
	case transcript.Failed:
		text = bodyText(c.Body)
		head, tail, omitted = capBody(text)
		if omitted > 0 {
			h = hit{kind: hitOutput, id: c.ID}
		}
	}
	o.title("• ", bullet, c.Title, title, h)
	summary := c.Summary
	if c.State == transcript.Unfinished {
		summary = "interrupted"
	}
	if summary != "" {
		o.under(summary, lineStyle, hit{})
	}
	w := o.bodyWidth()
	switch c.State {
	case transcript.OK:
		if f.expanded[0] {
			o.body(opened(c, w, th))
		}
	case transcript.Failed:
		if f.expanded[0] {
			o.body(commandLines(c, w, th))
		}
		switch {
		case text == "":
		case omitted == 0 || f.expanded[0]:
			o.body(bodyLines(text, fg(th.muted), w))
		default:
			o.body(bodyLines(strings.Join(head, "\n"), fg(th.muted), w))
			o.body(bodyLines(fmt.Sprintf("… %d more lines", omitted), fg(th.dim), w))
			o.body(bodyLines(strings.Join(tail, "\n"), fg(th.muted), w))
		}
	default:
		if f.expanded[0] {
			o.body(commandLines(c, w, th))
		}
		if c.State == transcript.Rejected {
			o.body(bodyLines(bodyText(c.Body), fg(th.muted), w))
		}
	}
	return o
}

// renderGroup draws a live group, or a sealed one closed behind ▸ or open
// behind ▾; live and open groups list their members.
func renderGroup(g transcript.Group, f blockFolds, width int, th theme) []line {
	o := &outline{width: width}
	text := fg(th.text)
	switch {
	case !g.Sealed:
		o.title("• ", fg(th.success), g.Sentence(), text, hit{})
	case f.open:
		o.title("▾ ", text, g.Sentence(), text, hit{kind: hitHandle, id: g.Key()})
	default:
		o.title("▸ ", fg(th.dim), g.Sentence(), fg(th.dim), hit{kind: hitHandle, id: g.Key()})
		return o.rows
	}
	for i, c := range g.Calls {
		label := c.Title
		if c.Summary != "" {
			label += " · " + c.Summary
		}
		o.under(label, fg(th.dim), openHit(c))
		if f.expanded[i] {
			o.body(opened(c, o.bodyWidth(), th))
		}
	}
	return o.rows
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

// under is the line below a row: its arrow sits step in, and wrapped text
// hangs under the text.
func under(text string, style lipgloss.Style, width int) []string {
	return hang(strings.Repeat(" ", step)+"↳ ", style, text, style, width)
}

// opened is what a successful call opens to, drawn flush left: its command when
// the title cut it, then a file change's lines, else its body in full.
func opened(c transcript.Call, width int, th theme) []line {
	out := commandLines(c, width, th)
	if len(c.Preview) == 0 {
		return append(out, bodyLines(bodyText(c.Body), fg(th.muted), width)...)
	}
	return append(out, plain(previewRows(c.Preview, width, th))...)
}

// commandLines is the whole command after a "$", nil when the title shows it
// all. It is not passed through bodyText, which would drop what the command
// itself holds.
func commandLines(c transcript.Call, width int, th theme) []line {
	if c.Command == "" {
		return nil
	}
	return plain(hang("$ ", fg(th.dim), c.Command, fg(th.text), width))
}

// bodyLines draws text flush left.
func bodyLines(text string, style lipgloss.Style, width int) []line {
	if text == "" {
		return nil
	}
	return plain(hang("", style, text, style, width))
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

func renderTurn(t transcript.Turn, v verb, width int, th theme) []line {
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
	o := &outline{width: width}
	o.title("κ ", c, text, c, hit{})
	if t.Outcome.Message != "" {
		o.body(bodyLines(t.Outcome.Message, fg(th.error), o.bodyWidth()))
	}
	return o.rows
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
	o := &outline{width: width}
	o.title("• ", bullet, head+command+tail, fg(th.text), h)
	var parts []string
	if t.Agents > 0 {
		parts = append(parts, countText(t.Agents, "agent")+failedSuffix(t.Failed))
	}
	if t.Reason != "" {
		parts = append(parts, t.Reason)
	}
	if len(parts) > 0 {
		o.under(strings.Join(parts, " · "), lineStyle, hit{})
	}
	if expanded {
		o.body(bodyLines(result, fg(th.muted), o.bodyWidth()))
	}
	return o.rows
}

// transcriptCommandLines is how many output lines a command in the transcript
// shows before it is opened.
const transcriptCommandLines = 10

// commandOptions says how commandRows draws a user command, which the
// transcript and the steering section above the composer both show.
type commandOptions struct {
	// Lines is how many output lines to show while not Expanded.
	Lines    int
	Expanded bool
	Unread   bool
	// Elapsed is the time a running command has run so far.
	Elapsed time.Duration
	// BackgroundKey and StopKey name the keys of the running hints; empty omits one.
	BackgroundKey, StopKey string
	// Hit is what a click on the hint row does; it applies only when there is output.
	Hit hit
}

// commandRows is a user command: its shaded box, a blank row, the hint row
// for its state and its output rows.
func commandRows(c event.UserCommand, o commandOptions, width int, th theme) []line {
	ps := commandShade(th).Foreground(th.error)
	out := &outline{width: width}
	out.rows = append(plain(shadedBox("! ", ps, c.Command, nil, width, th)), line{})

	output := bodyText(c.Output)
	h := hit{}
	if output != "" {
		h = o.Hit
	}
	text, style, exit := commandHint(c, o, th)
	first := len(out.rows)
	out.under(text, style, h)
	if exit != nil {
		// The exit text leads the hint, so it sits on the first row unless the width cuts it.
		out.rows[first].text = lipgloss.StyleRanges(out.rows[first].text, lipgloss.NewRange(out.col, out.col+lipgloss.Width(c.Exit), *exit))
	}

	if output == "" {
		return out.rows
	}
	ls := strings.Split(output, "\n")
	if !o.Expanded && len(ls) > o.Lines {
		omitted := len(ls) - o.Lines
		out.body(bodyLines(fmt.Sprintf("… %d earlier lines", omitted), fg(th.dim), out.bodyWidth()))
		ls = ls[omitted:]
	}
	out.body(bodyLines(strings.Join(ls, "\n"), fg(th.muted), out.bodyWidth()))
	return out.rows
}

// commandHint is the hint row's text and style; exit is the style of the exit
// word when it is coloured apart from the rest.
func commandHint(c event.UserCommand, o commandOptions, th theme) (text string, style lipgloss.Style, exit *lipgloss.Style) {
	// since is " · 12s" or "" under a second.
	since := func(sep string, d time.Duration) string {
		if d < time.Second {
			return ""
		}
		return sep + formatDuration(d)
	}
	unread := ""
	if o.Unread {
		unread = " · unread"
	}
	_, running := toneRunning.styles(th)
	switch c.State {
	case event.CommandQueued:
		return "queued", running, nil
	case event.CommandRunning:
		text := "running" + since(" ", o.Elapsed)
		if o.BackgroundKey != "" {
			text += " · " + o.BackgroundKey + " to background"
		}
		if o.StopKey != "" {
			text += " · " + o.StopKey + " to stop"
		}
		return text, running, nil
	case event.CommandExited:
		text := c.Exit + since(" · ", c.Elapsed) + unread
		if c.Failed {
			_, failed := toneFailed.styles(th)
			return text, failed, nil
		}
		word, okLine := toneOK.styles(th)
		return text, okLine, &word
	case event.CommandStopped:
		_, stopped := toneRefused.styles(th)
		return "stopped" + since(" after ", c.Elapsed) + unread, stopped, nil
	}
	return "moved to background" + since(" after ", c.Elapsed), running, nil
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

// shadedBox is a shaded box: a blank shaded row, prefix and the wrapped text
// with continuation lines two in, a blank shaded row, all full width. ps styles
// the prefix and carries the shade, which the text takes with the theme's text
// colour. spans,
// when not nil, gives the runs of the cleaned text to highlight.
func shadedBox(prefix string, ps lipgloss.Style, text string, spans func(string) []app.Span, width int, th theme) []string {
	st := ps.Foreground(th.text)
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

// commandShade is shade for a command the user ran.
func commandShade(th theme) lipgloss.Style {
	return lipgloss.NewStyle().Background(th.userCommandBg).Foreground(th.text)
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
