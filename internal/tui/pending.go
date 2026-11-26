package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/event"
)

// noOutputNotice is what a command's output reads before any byte arrives
// (internal/tool); a running command shows no output rather than the notice.
const noOutputNotice = "(no output)"

// pendingCache keeps the full rendered queue independent of the current
// composer height and the space left for it. A queue holding a running command
// is drawn anew each time: its elapsed time and output tail change.
type pendingCache struct {
	queued  []event.Queued
	session string
	width   int
	th      theme
	// box is every group of the queue with a blank row between them; boxes are
	// the row ranges of its shaded prompt boxes.
	box   []string
	boxes [][2]int
	// opens are the rows where a user command's shaded box opens.
	opens []int
	// note is nil when no prompt is queued.
	note []string

	// tail is the running command's output so far; nil keeps what the queue holds.
	tail func() string
	// backgroundable and stoppable are whether the background key and Esc would
	// act on the running command.
	backgroundable, stoppable bool

	// lines is how many output lines a command shows; total is the rows the
	// queue was fitted to.
	lines, total int
	// tailed is whether a command of the queue has output to show.
	tailed bool
}

// pendingRows draws the queued input above the working row in queue order:
// consecutive prompts share one shaded box with a blank line between messages
// and each user command is a block of its own, a blank row between groups. The
// note naming the keys that act on queued prompts follows under the last
// prompt box, then a blank row. maxContent caps the rows between the first
// shaded row and the last; maxRows caps the entire area.
func pendingRows(queued []event.Queued, session string, spans func(string) []app.Span, km *keyMap, width, maxContent, maxRows int, th theme) []string {
	var c pendingCache
	return c.rows(queued, session, spans, km, width, maxContent, maxRows, th)
}

func (c *pendingCache) rows(queued []event.Queued, session string, spans func(string) []app.Span, km *keyMap, width, maxContent, maxRows int, th theme) []string {
	if len(queued) == 0 {
		c.queued, c.box, c.boxes, c.note = nil, nil, nil, nil
		return nil
	}
	running := hasRunning(queued)
	changed := c.box == nil || width != c.width || th != c.th || session != c.session || !slices.EqualFunc(queued, c.queued, sameQueued)
	if running || changed || c.refit(c.fit(maxContent, maxRows)) {
		c.queued, c.session, c.width, c.th = nil, session, width, th
		if !running {
			c.queued = slices.Clone(queued)
		}
		// The output tails shrink, down to none, before any row of the queue is cut.
		for c.lines = 5; ; c.lines-- {
			c.build(queued, spans, km)
			c.total = c.fit(maxContent, maxRows)
			if len(c.box) <= c.total || c.lines == 0 {
				break
			}
		}
	}
	noteRows := c.noteRows()
	total := c.fit(maxContent, maxRows)
	if total < 3+noteRows {
		return nil
	}
	out := make([]string, 0, total+1)
	if len(c.box) > total {
		out = append(out, c.cut(total, noteRows)...)
	} else {
		out = append(out, c.box...)
	}
	return append(out, "")
}

// refit is whether a change of the room left for the queue changes how many
// output lines its commands show.
func (c *pendingCache) refit(total int) bool {
	return c.tailed && total != c.total && (c.lines < 5 || (c.lines > 0 && len(c.box) > total))
}

func (c *pendingCache) noteRows() int {
	if c.note == nil {
		return 0
	}
	return 1 + len(c.note)
}

// fit is the rows the queue may take, the note included.
func (c *pendingCache) fit(maxContent, maxRows int) int {
	return min(maxContent+2+c.noteRows(), maxRows-1)
}

func (c *pendingCache) build(queued []event.Queued, spans func(string) []app.Span, km *keyMap) {
	c.box, c.boxes, c.opens, c.note, c.tailed = nil, nil, nil, nil, false
	var texts []string
	gap := func() {
		if len(c.box) > 0 {
			c.box = append(c.box, "")
		}
	}
	flush := func() {
		if len(texts) == 0 {
			return
		}
		gap()
		start := len(c.box)
		c.box = append(c.box, shadedBox("❯ ", shade(c.th), strings.Join(texts, "\n\n"), spans, c.width, c.th)...)
		c.boxes = append(c.boxes, [2]int{start, len(c.box)})
		texts = nil
	}
	for _, q := range queued {
		if q.Command == nil {
			texts = append(texts, q.Text)
			continue
		}
		flush()
		gap()
		c.opens = append(c.opens, len(c.box))
		cmd := c.command(*q.Command)
		c.tailed = c.tailed || bodyText(cmd.Output) != ""
		for _, l := range commandRows(cmd, c.options(*q.Command, km), c.width, c.th) {
			c.box = append(c.box, l.text)
		}
	}
	flush()
	if len(c.boxes) == 0 {
		return
	}
	note := "queued"
	if ks := km.sendNow.Keys(); len(ks) > 0 {
		note += " · " + ks[0] + " to send now"
	}
	if ks := km.queueEdit.Keys(); len(ks) > 0 {
		note += " · " + ks[0] + " to edit"
	}
	c.note = under(note, fg(c.th.dim), c.width)
	end := c.boxes[len(c.boxes)-1][1]
	c.box = slices.Insert(c.box, end, append([]string{""}, c.note...)...)
	for i, o := range c.opens {
		if o >= end {
			c.opens[i] += 1 + len(c.note)
		}
	}
}

// command is the command as drawn: a running one shows its live tail.
func (c *pendingCache) command(cmd event.UserCommand) event.UserCommand {
	if cmd.State == event.CommandRunning && c.tail != nil {
		cmd.Output = c.tail()
		if cmd.Output == noOutputNotice {
			cmd.Output = ""
		}
	}
	return cmd
}

func (c *pendingCache) options(cmd event.UserCommand, km *keyMap) commandOptions {
	o := commandOptions{Lines: c.lines}
	switch cmd.State {
	case event.CommandExited, event.CommandStopped:
		o.Unread = true
	case event.CommandRunning:
		o.Elapsed = time.Since(cmd.Started)
		if ks := km.background.Keys(); len(ks) > 0 && c.backgroundable && o.Elapsed >= hintDelay {
			o.BackgroundKey = ks[0]
		}
		if ks := km.interrupt.Keys(); len(ks) > 0 && c.stoppable && o.Elapsed >= hintDelay {
			o.StopKey = ks[0]
		}
	}
	return o
}

// cut is the queue shortened to total rows by a row counting what it hid. A
// cut inside a prompt box keeps the box's shade and closing row. The last prompt
// box and the note under it stay: the rows before them are cut, or the box
// itself when it does not fit.
func (c *pendingCache) cut(total, noteRows int) []string {
	if noteRows > 0 {
		last := c.boxes[len(c.boxes)-1]
		noteEnd := last[1] + noteRows
		// Rows after the note are hidden and counted in a row below it.
		after := len(c.box) - noteEnd
		if keep := total - 2 - noteRows; keep > last[0] && keep < last[1] {
			return append(c.head(len(c.box)-noteRows, total-noteRows), c.box[last[1]:noteEnd]...)
		}
		head := total - (noteEnd - last[0])
		if after > 0 {
			head--
		}
		if head >= 2 || last[0] == 0 && head >= 0 {
			var out []string
			if last[0] > 0 {
				out = c.head(last[0], min(head, last[0]+1))
			}
			out = append(out, c.box[last[0]:noteEnd]...)
			if after > 0 {
				out = append(out, c.more(after))
			}
			return out
		}
		if keep := last[0] + total - 3 - noteRows; keep > last[0] {
			out := append([]string{c.more(last[0])}, c.shadedCut(last[0], keep, last, len(c.box)-noteRows-keep-1)...)
			return append(out, c.box[last[1]:noteEnd]...)
		}
	}
	return c.head(len(c.box), total)
}

// head is the first rows of c.box[:end] fitting n, the last of them a row
// counting the rest.
func (c *pendingCache) head(end, n int) []string {
	for _, b := range c.boxes {
		if keep := n - 2; keep > b[0] && keep < b[1] {
			return c.shadedCut(0, keep, b, end-keep-1)
		}
	}
	kept := n - 1
	// A box's opening shaded row cannot be the last one shown.
	if slices.ContainsFunc(c.boxes, func(b [2]int) bool { return b[0] == kept-1 }) || slices.Contains(c.opens, kept-1) {
		kept--
	}
	return append(slices.Clone(c.box[:kept]), c.more(end-kept))
}

func (c *pendingCache) more(hidden int) string {
	return fg(c.th.dim).Render(fmt.Sprintf("  … %d more lines", hidden))
}

// shadedCut keeps the rows from start up to keep, then a shaded row counting
// hidden and the box's closing row.
func (c *pendingCache) shadedCut(start, keep int, b [2]int, hidden int) []string {
	more := fmt.Sprintf("  … %d more lines", hidden)
	pad := strings.Repeat(" ", max(0, c.width-lipgloss.Width(more)))
	out := slices.Clone(c.box[start:keep])
	return append(out, shade(c.th).Foreground(c.th.dim).Render(more+pad), c.box[b[1]-1])
}

// sameQueued compares a command by value, not by pointer.
func sameQueued(a, b event.Queued) bool {
	if a.Text != b.Text || (a.Command == nil) != (b.Command == nil) {
		return false
	}
	return a.Command == nil || *a.Command == *b.Command
}

// hasRunning reports whether the queue holds a running user command.
func hasRunning(queued []event.Queued) bool {
	return slices.ContainsFunc(queued, func(q event.Queued) bool {
		return q.Command != nil && q.Command.State == event.CommandRunning
	})
}
