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

// dialog is the plan's own, which has nothing that depends on width.
func (p *planDialog) dialog(int, theme) *dialog { return p.d }

// key answers: Yes approves, No answers with the typed changes, which the
// Controller treats as a dismissal like the dismiss key when blank. Keys it has no
// use for are swallowed. The dialog stays until its PlanEnded arrives.
func (p *planDialog) key(m *model, msg tea.KeyPressMsg) (tea.Cmd, bool) {
	km := &m.keys
	switch {
	case key.Matches(msg, km.dismiss):
		m.c.AnswerPlan(p.token, tool.PlanAnswer{})
		return nil, true
	case key.Matches(msg, km.editPlan):
		return m.openEditor(m.c.PlanFile()), true
	}
	picked, cmd := p.d.key(msg, km)
	if picked {
		m.c.AnswerPlan(p.token, tool.PlanAnswer{Approved: p.d.sel == 0, Feedback: p.d.text()})
	}
	return cmd, true
}

// owns reports a repeat of the dialog's own request.
func (p *planDialog) owns(ev event.Event) bool {
	r, ok := ev.(event.PlanRequested)
	return ok && r.Token == p.token
}

// seen ends the dialog with its PlanEnded. A repeat of its own request keeps it,
// and what was typed in it; the editor's exit has the Controller read the plan
// again.
func (p *planDialog) seen(m *model, msg tea.Msg) bool {
	switch ev := msg.(type) {
	case event.PlanEnded:
		return ev.Token != p.token
	case editorDoneMsg:
		m.c.RefreshPlan(p.token)
	}
	return true
}
