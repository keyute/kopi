package tui

import (
	"context"
	"errors"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/keyute/kopi/internal/event"
)

// btwDialog is the /btw dialog, drawn in the composer's slot: the question and,
// once its SideQuestionEnded arrives, the answer or the error.
type btwDialog struct {
	d        *dialog
	km       *keyMap
	id       int
	question string
	answer   string
	err      error
	ended    bool
	// body is the rows for bodyWidth and bodyTheme, kept across renders.
	body      []string
	bodyWidth int
	bodyTheme theme
}

func newBtwDialog(ev event.SideQuestionAsked, km *keyMap) *btwDialog {
	return &btwDialog{d: &dialog{title: "Side question"}, km: km, id: ev.ID, question: ev.Question}
}

// key dismisses: while the answer is pending that also stops the request.
// Every other key is swallowed.
func (b *btwDialog) key(m *model, msg tea.KeyPressMsg) (tea.Cmd, bool) {
	if !key.Matches(msg, m.keys.dismiss) {
		return nil, true
	}
	if !b.ended {
		m.c.StopSideQuestion(b.id)
	}
	return nil, false
}

// seen fills the dialog with its own SideQuestionEnded; a session switch or a
// newer question ends it. A cancelled request leaves it as it is, for the
// switch or the newer question that cancelled it to close.
func (b *btwDialog) seen(_ *model, msg tea.Msg) bool {
	switch ev := msg.(type) {
	case event.SessionChanged, event.SideQuestionAsked:
		return false
	case event.SideQuestionEnded:
		if ev.ID == b.id && !errors.Is(ev.Err, context.Canceled) {
			b.answer, b.err, b.ended, b.bodyWidth = ev.Answer, ev.Err, true, 0
		}
	}
	return true
}

func (b *btwDialog) dialog(width int, th theme) *dialog {
	if b.bodyWidth != width || b.bodyTheme != th {
		b.body, b.bodyWidth, b.bodyTheme = b.rows(width-2, th), width, th
	}
	b.d.body = b.body
	if b.ended {
		b.d.note, b.d.hints = "", []string{binding(b.km.dismiss, "close")}
	} else {
		b.d.note, b.d.hints = "Answering…", []string{binding(b.km.dismiss, "cancel")}
	}
	return b.d
}

// rows is the question, a blank row, then the answer or the error once there
// is one; while it is pending the blank row sets the note off.
func (b *btwDialog) rows(width int, th theme) []string {
	muted := fg(th.muted)
	rows := []string{""}
	rows = append(rows, hang("", muted, b.question, muted, width)...)
	rows = append(rows, "")
	switch {
	case !b.ended:
	case b.err != nil:
		rows = append(rows, hang("", fg(th.error), b.err.Error(), fg(th.error), width)...)
	default:
		rows = append(rows, newMarkdown(width, th).lines("", b.answer)...)
	}
	return rows
}
