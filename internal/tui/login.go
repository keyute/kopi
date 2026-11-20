package tui

import (
	"strings"
	"unicode/utf8"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/event"
)

// secretMark is drawn in place of each character of a secret.
const secretMark = "•"

// newLoginDialog is the prompt for ev: its message as the title and its
// provider as the note. Options become one numbered choice each; otherwise
// the one option is an unlabelled text field, masked when ev asks for a secret.
func newLoginDialog(ev event.LoginRequested, km *keyMap, th theme) *loginDialog {
	d := &dialog{title: ev.Message, note: ev.Provider}
	l := &loginDialog{req: &ev, d: d}
	if len(ev.Options) > 0 {
		for _, o := range ev.Options {
			label := o.Label
			if o.Description != "" {
				label += " - " + o.Description
			}
			d.options = append(d.options, option{label: label})
		}
		return l
	}
	f := newField(th, km.newline, ev.Placeholder)
	f.secret = ev.Secret
	f.ta.Focus()
	d.options = []option{{field: &f}}
	return l
}

// loginDialog is the prompt for a pending login question.
type loginDialog struct {
	req *event.LoginRequested
	d   *dialog
}

// dialog is the login's own, which has nothing that depends on width.
func (l *loginDialog) dialog(int, theme) *dialog { return l.d }

// key answers when an option is picked: with the chosen option's ID or the
// typed text. Dismiss cancels the login. The prompt stays until its
// LoginEnded arrives.
func (l *loginDialog) key(m *model, msg tea.KeyPressMsg) (tea.Cmd, bool) {
	if key.Matches(msg, m.keys.dismiss) {
		m.c.Interrupt(app.Main)
		return nil, true
	}
	picked, cmd := l.d.key(msg, &m.keys)
	if picked {
		answer := l.d.text()
		if f := l.d.field(); f != nil && f.secret {
			answer = strings.NewReplacer("\r", "", "\n", "").Replace(answer)
		}
		if len(l.req.Options) > 0 {
			answer = l.req.Options[l.d.sel].ID
		}
		m.c.AnswerLogin(l.req.Token, answer)
		l.d.clear()
	}
	return cmd, true
}

// seen ends the prompt with its LoginEnded, which empties its fields.
func (l *loginDialog) seen(_ *model, msg tea.Msg) bool {
	if ev, ended := msg.(event.LoginEnded); ended && ev.Token == l.req.Token {
		l.d.clear()
		return false
	}
	return true
}

// clear empties every field, so an answered or ended prompt keeps no text.
func (d *dialog) clear() {
	for _, o := range d.options {
		if o.field != nil {
			o.field.reset()
		}
	}
}

// secretRow draws the draft of secret field f as one mark per character, the
// last width cells when it is longer, or its dim placeholder when empty.
func secretRow(f *composer, width int, th theme) string {
	if f.empty() {
		return fg(th.dim).Render(ansi.Truncate(f.ta.Placeholder, max(0, width), ""))
	}
	n := utf8.RuneCountInString(strings.ReplaceAll(f.value(), "\n", ""))
	return strings.Repeat(secretMark, min(n, max(0, width)))
}
