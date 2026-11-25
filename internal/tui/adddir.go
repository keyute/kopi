package tui

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/keyute/kopi/internal/event"
)

// newDirDialog is the prompt for ev: whether to add its folder to the
// workspace, and whether to trust it.
func newDirDialog(ev event.DirRequested, km *keyMap) *dirDialog {
	d := &dialog{
		title: "Add this folder to the workspace?",
		options: []option{
			{label: "Yes, and trust this folder"},
			{label: "Yes, but don't trust it"},
			{label: "No, don't add"},
		},
		hints: []string{binding(km.submit, "choose"), binding(km.dismiss, "cancel")},
	}
	return &dirDialog{req: &ev, d: d}
}

// dirDialog is the prompt for a pending request to add a folder. It has no
// session to belong to: the request may come before the first one opens.
type dirDialog struct {
	req *event.DirRequested
	d   *dialog
	// body is the text drawn for bodyWidth and bodyTheme, kept across renders.
	body      []string
	bodyWidth int
	bodyTheme theme
}

// dialog is d with its body drawn for width and th.
func (a *dirDialog) dialog(width int, th theme) *dialog {
	if a.body == nil || a.bodyWidth != width || a.bodyTheme != th {
		a.body, a.bodyWidth, a.bodyTheme = a.lines(width-2, th), width, th
	}
	a.d.body = a.body
	return a.d
}

// lines is the body at width: every path and server is untrusted text.
func (a *dirDialog) lines(width int, th theme) []string {
	text, muted := fg(th.text), fg(th.muted)
	out := untrustedLines("", a.req.Dir, text.Bold(true), width)
	say := func(s string) { out = append(out, hang("", text, s, text, width)...) }
	list := func(head string, entries []string) {
		say(head)
		for _, e := range entries {
			out = append(out, untrustedLines("  ", e, muted, width)...)
		}
	}
	out = append(out, "")
	if len(a.req.Instructions) > 0 {
		list("Trusting it adds these instruction files to the model's instructions:", a.req.Instructions)
	} else {
		say("It has no instruction files.")
	}
	if !a.req.Saved {
		return out
	}
	if len(a.req.Servers)+len(a.req.Skills) == 0 {
		say("The answer is saved and also applies when Kopi starts in this folder.")
		return out
	}
	say("The answer is saved. When Kopi starts in this folder it also covers:")
	entry := func(prefix, text string) {
		out = append(out, untrustedLines("  "+prefix, text, muted, width)...)
	}
	for _, sv := range a.req.Servers {
		entry("MCP server ", sv)
	}
	for _, sk := range a.req.Skills {
		entry("skills in ", sk)
	}
	return out
}

// key answers when an option is picked, or no when dismissed. The prompt stays
// until its DirEnded arrives.
func (a *dirDialog) key(m *model, msg tea.KeyPressMsg) (tea.Cmd, bool) {
	if key.Matches(msg, m.keys.dismiss) {
		m.c.AnswerDir(a.req.Token, false, false)
		return nil, true
	}
	picked, cmd := a.d.key(msg, &m.keys)
	if picked {
		m.c.AnswerDir(a.req.Token, a.d.sel < 2, a.d.sel == 0)
	}
	return cmd, true
}

// seen ends the prompt with its DirEnded.
func (a *dirDialog) seen(_ *model, msg tea.Msg) bool {
	ev, ended := msg.(event.DirEnded)
	return !ended || ev.Token != a.req.Token
}
