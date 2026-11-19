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
	path  string
	d     *dialog
}

func newPlanDialog(ev event.PlanRequested, km *keyMap, th theme) *planDialog {
	d := &dialog{title: "Approve this plan?", options: []option{
		{label: "Yes"},
		{label: rejectLabel(km), field: newFeedbackField(km, th)},
	}}
	parts := []string{ev.Path, binding(km.editPlan, "edit"), scrollHint(km), binding(km.interrupt, "dismiss")}
	d.note = strings.Join(slices.DeleteFunc(parts, isEmpty), " · ")
	return &planDialog{token: ev.Token, path: ev.Path, d: d}
}

// planRows draws d in place of the composer within maxRows, its field growing
// to maxField rows.
func planRows(d *dialog, width, maxField, maxRows int, th theme) []string {
	if maxRows <= 0 {
		return nil
	}
	return cutTop(d, d.rows(width, maxRows, max(1, maxField), th), maxRows)
}

// planKey goes to the dialog: Yes approves, No answers with the typed changes
// or, blank, dismisses like interrupt. Keys it has no use for are swallowed,
// and the page keys scroll the transcript, which shows the plan. The dialog
// stays until its PlanEnded arrives.
func (m *model) planKey(msg tea.KeyPressMsg) tea.Cmd {
	p, km := m.plan, &m.keys
	switch {
	case key.Matches(msg, km.interrupt):
		m.c.AnswerPlan(p.token, tool.PlanAnswer{})
		return nil
	case key.Matches(msg, km.editPlan):
		return m.openEditor(p.path, p.token)
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
	answer := tool.PlanAnswer{Approved: p.d.sel == 0}
	if text := p.d.text(); strings.TrimSpace(text) != "" {
		answer.Feedback = text
	}
	m.c.AnswerPlan(p.token, answer)
	return cmd
}

// endPlan drops the pending plan and, unless an approval is shown over it,
// its dialog.
func (m *model) endPlan() {
	if m.plan == nil {
		return
	}
	m.plan = nil
	if m.approval == nil {
		m.dialog = nil
	}
}
