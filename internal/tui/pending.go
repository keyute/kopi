package tui

import (
	"fmt"
	"slices"
	"strings"

	"charm.land/lipgloss/v2"
)

// pendingCache keeps the full rendered queue independent of the current
// composer height and the space left for it.
type pendingCache struct {
	queued []string
	width  int
	th     theme
	box    []string
	note   []string
}

// pendingRows draws the queued messages above the working row: one shaded
// box holding them all with a blank line between messages, a blank row, the
// note naming the keys that act on the queue and a blank row. maxContent caps
// the box's rows between its shaded blank rows; maxRows caps the entire area.
func pendingRows(queued []string, km *keyMap, width, maxContent, maxRows int, th theme) []string {
	var c pendingCache
	return c.rows(queued, km, width, maxContent, maxRows, th)
}

func (c *pendingCache) rows(queued []string, km *keyMap, width, maxContent, maxRows int, th theme) []string {
	if len(queued) == 0 {
		*c = pendingCache{}
		return nil
	}
	if c.box == nil || width != c.width || th != c.th || !slices.Equal(queued, c.queued) {
		c.queued, c.width, c.th = slices.Clone(queued), width, th
		c.box = shadedBox("❯ ", shade(th), strings.Join(queued, "\n\n"), width, th)
		note := "queued"
		if ks := km.sendNow.Keys(); len(ks) > 0 {
			note += " · " + ks[0] + " to send now"
		}
		if ks := km.queueEdit.Keys(); len(ks) > 0 {
			note += " · " + ks[0] + " to edit"
		}
		c.note = hang("  ↳ ", fg(th.dim), note, fg(th.dim), width)
	}
	maxContent = min(maxContent, maxRows-4-len(c.note))
	if maxContent < 1 {
		return nil
	}
	var box []string
	if hidden := len(c.box) - 2 - maxContent; hidden > 0 {
		more := fmt.Sprintf("  … %d more lines", hidden+1)
		pad := strings.Repeat(" ", max(0, width-lipgloss.Width(more)))
		box = append(box, c.box[:maxContent]...)
		box = append(box, shade(th).Foreground(th.dim).Render(more+pad), c.box[len(c.box)-1])
	} else {
		box = append(box, c.box...)
	}
	box = append(box, "")
	box = append(box, c.note...)
	return append(box, "")
}
