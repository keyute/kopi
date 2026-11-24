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
	// note is nil when no prompt is queued.
	note []string

	// tail is the running command's output so far; nil keeps what the queue holds.
	tail func() string
	// stoppable is whether Esc would stop the running command.
	stoppable bool
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
	running := slices.ContainsFunc(queued, func(q event.Queued) bool {
		return q.Command != nil && q.Command.State == event.CommandRunning
	})
	if running || c.box == nil || width != c.width || th != c.th || session != c.session || !slices.EqualFunc(queued, c.queued, sameQueued) {
		c.queued, c.session, c.width, c.th = cloneQueued(queued), session, width, th
		if running {
			c.queued = nil
		}
		c.build(queued, spans, km)
	}
	noteRows := 0
	if c.note != nil {
		noteRows = 1 + len(c.note)
	}
	total := min(maxContent+2+noteRows, maxRows-1)
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

func (c *pendingCache) build(queued []event.Queued, spans func(string) []app.Span, km *keyMap) {
	c.box, c.boxes, c.note = nil, nil, nil
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
		for _, l := range commandRows(c.command(*q.Command), c.options(*q.Command, km), c.width, c.th) {
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
}

// command is the command as drawn: a running one shows its live tail.
func (c *pendingCache) command(cmd event.UserCommand) event.UserCommand {
	if cmd.State == event.CommandRunning && c.tail != nil {
		cmd.Output = c.tail()
	}
	return cmd
}

func (c *pendingCache) options(cmd event.UserCommand, km *keyMap) commandOptions {
	o := commandOptions{Lines: 5}
	switch cmd.State {
	case event.CommandExited, event.CommandStopped:
		o.Unread = true
	case event.CommandRunning:
		o.Elapsed = time.Since(cmd.Started)
		if ks := km.background.Keys(); len(ks) > 0 {
			o.BackgroundKey = ks[0]
		}
		if ks := km.interrupt.Keys(); len(ks) > 0 && c.stoppable {
			o.StopKey = ks[0]
		}
	}
	return o
}

// cut is the first rows of the queue that fit total, ending in a row that
// counts the rest. A cut inside a prompt box keeps the box's shade and closing
// row; one inside the last box keeps the note under it as well.
func (c *pendingCache) cut(total, noteRows int) []string {
	shaded := func(keep int, b [2]int, shown int) []string {
		more := fmt.Sprintf("  … %d more lines", len(c.box)-keep-1-shown)
		pad := strings.Repeat(" ", max(0, c.width-lipgloss.Width(more)))
		out := append([]string(nil), c.box[:keep]...)
		return append(out, shade(c.th).Foreground(c.th.dim).Render(more+pad), c.box[b[1]-1])
	}
	if noteRows > 0 {
		last := c.boxes[len(c.boxes)-1]
		if keep := total - 2 - noteRows; keep > last[0] && keep < last[1] {
			return append(shaded(keep, last, noteRows), c.box[last[1]:last[1]+noteRows]...)
		}
		if total-1 >= last[1]+noteRows {
			return c.plainCut(total)
		}
	}
	for _, b := range c.boxes {
		if keep := total - 2; keep > b[0] && keep < b[1] {
			return shaded(keep, b, 0)
		}
	}
	return c.plainCut(total)
}

func (c *pendingCache) plainCut(total int) []string {
	out := append([]string(nil), c.box[:total-1]...)
	return append(out, fg(c.th.dim).Render(fmt.Sprintf("  … %d more lines", len(c.box)-total+1)))
}

// sameQueued compares a command by value, not by pointer.
func sameQueued(a, b event.Queued) bool {
	if a.Text != b.Text || (a.Command == nil) != (b.Command == nil) {
		return false
	}
	return a.Command == nil || *a.Command == *b.Command
}

// cloneQueued copies the commands too, so a change to one the Controller
// handed out is seen.
func cloneQueued(queued []event.Queued) []event.Queued {
	out := slices.Clone(queued)
	for i, q := range out {
		if q.Command != nil {
			cmd := *q.Command
			out[i].Command = &cmd
		}
	}
	return out
}
