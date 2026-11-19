package tui

import (
	"slices"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/tool"
)

// planDialog is the ExitPlanMode dialog, drawn in the composer's slot. The plan
// itself is a transcript block above it.
type planDialog struct {
	token uint64
	d     *dialog
}

func newPlanDialog(ev event.PlanRequested, km *keyMap, th theme) *planDialog {
	d := &dialog{title: "Approve this plan?", options: []option{
		{label: "Yes"},
		{label: rejectLabel(km), field: newFeedbackField(km, th)},
	}}
	parts := []string{ev.Path, binding(km.editPlan, "edit"), scrollHint(km), binding(km.interrupt, "dismiss")}
	d.note = strings.Join(slices.DeleteFunc(parts, isEmpty), " · ")
	return &planDialog{token: ev.Token, d: d}
}

// planRows draws d in place of the composer within maxRows, its field growing
// to maxField rows. Both rules surround a window containing the selection.
func planRows(d *dialog, width, maxField, maxRows int, th theme) []string {
	if maxRows <= 0 {
		d.areas = nil
		return nil
	}
	rows := d.rows(width, maxRows, max(1, maxField), th)
	if len(rows) <= maxRows {
		return rows
	}
	if maxRows < 3 {
		return squeeze(d, rows, maxRows)
	}
	room := maxRows - 2
	start := max(1, min(d.optionStarts[d.sel], len(rows)-1-room))
	return clip(d, rows, 1, start, start+room)
}

// squeeze is rows for a dialog that overflows maxRows of 2 or fewer: both
// rules when there are two, else the top rows.
func squeeze(d *dialog, rows []string, maxRows int) []string {
	d.areas = nil
	if maxRows == 2 {
		return []string{rows[0], rows[len(rows)-1]}
	}
	return rows[:maxRows]
}

// clip is the first head rows of rows, rows[start:end] and the last row, with
// d's field areas moved to the window and those outside it dropped.
func clip(d *dialog, rows []string, head, start, end int) []string {
	var areas []fieldArea
	for _, fa := range d.areas {
		top, bottom := max(fa.y, start), min(fa.y+fa.h, end)
		if top < bottom {
			fa.y, fa.h = top-start+head, bottom-top
			areas = append(areas, fa)
		}
	}
	d.areas = areas
	return append(append(slices.Clone(rows[:head]), rows[start:end]...), rows[len(rows)-1])
}

// planKey goes to the dialog: Yes approves, No answers with the typed changes,
// which the Controller treats as a dismissal like interrupt when blank. Keys it has no use for are swallowed,
// and the page keys scroll the transcript, which shows the plan. The dialog
// stays until its PlanEnded arrives.
func (m *model) planKey(msg tea.KeyPressMsg) tea.Cmd {
	p, km := m.plan, &m.keys
	switch {
	case key.Matches(msg, km.interrupt):
		m.c.AnswerPlan(p.token, tool.PlanAnswer{})
		return nil
	case key.Matches(msg, km.editPlan):
		return m.openEditor(m.c.PlanFile())
	case key.Matches(msg, km.paste) && p.d.field() != nil:
		return readClipboardText(m.clip, p.d.field())
	case key.Matches(msg, km.pageUp):
		m.scroll(-m.transcriptHeight())
		return nil
	case key.Matches(msg, km.pageDown):
		m.scroll(m.transcriptHeight())
		return nil
	}
	picked, cmd := p.d.key(msg, km)
	if !picked {
		return cmd
	}
	m.c.AnswerPlan(p.token, tool.PlanAnswer{Approved: p.d.sel == 0, Feedback: p.d.text()})
	return cmd
}
