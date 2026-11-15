package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

// pendingRows draws the queued messages above the working row: one shaded
// box holding them all with a blank line between messages, a blank row, the
// note naming the keys that act on the queue and a blank row. maxContent caps
// the box's rows between its shaded blank rows; maxRows caps the entire area.
func pendingRows(queued []string, km *keyMap, width, maxContent, maxRows int, th theme) []string {
	if len(queued) == 0 {
		return nil
	}
	note := "queued"
	if ks := km.sendNow.Keys(); len(ks) > 0 {
		note += " · " + ks[0] + " to send now"
	}
	if ks := km.queueEdit.Keys(); len(ks) > 0 {
		note += " · " + ks[0] + " to edit"
	}
	noteRows := hang("  ↳ ", fg(th.dim), note, fg(th.dim), width)
	maxContent = min(maxContent, maxRows-4-len(noteRows))
	if maxContent < 1 {
		return nil
	}
	box := shadedBox("❯ ", shade(th), strings.Join(queued, "\n\n"), width, th)
	if hidden := len(box) - 2 - maxContent; hidden > 0 {
		more := fmt.Sprintf("  … %d more lines", hidden+1)
		pad := strings.Repeat(" ", max(0, width-lipgloss.Width(more)))
		bottom := box[len(box)-1]
		box = append(box[:maxContent], shade(th).Foreground(th.dim).Render(more+pad), bottom)
	}
	box = append(box, "")
	box = append(box, noteRows...)
	return append(box, "")
}
