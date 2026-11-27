package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/keyute/kopi/internal/event"
)

// copyDialog is the /copy picker: the whole reply and each of its code blocks.
type copyDialog struct {
	d *dialog
	// texts is what each option copies.
	texts []string
	// labels is each option's label before it is cut to a width.
	labels []string
	width  int
}

// showCopy copies the last finished reply of the shown session, or opens the
// picker when it has code blocks.
func (m *model) showCopy() tea.Cmd {
	text := m.view().tr.LastReply()
	if text == "" {
		return m.flash("no reply to copy")
	}
	blocks := codeBlocks(text)
	if len(blocks) == 0 {
		return m.copy(text)
	}
	c := &copyDialog{
		d:      &dialog{title: "Copy", hints: []string{binding(m.keys.submit, "copy"), binding(m.keys.dismiss, "cancel")}},
		texts:  []string{text},
		labels: []string{"Whole reply · " + lineCount(text)},
	}
	for _, b := range blocks {
		label := lineCount(b.text) + " · " + firstLine(clean(b.text))
		if b.lang != "" {
			label = clean(b.lang) + " · " + label
		}
		c.texts, c.labels = append(c.texts, b.text), append(c.labels, label)
	}
	m.open(promptCopy, c)
	return nil
}

func lineCount(s string) string {
	if n := strings.Count(s, "\n") + 1; n > 1 {
		return fmt.Sprintf("%d lines", n)
	}
	return "1 line"
}

// firstLine is the first line of s that is not blank, trimmed.
func firstLine(s string) string {
	for l := range strings.SplitSeq(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

// dialog cuts each label to one row when the width changes.
func (c *copyDialog) dialog(width int, _ theme) *dialog {
	if c.width != width {
		c.width = width
		room := width - 2 - len(fmt.Sprintf("%d. ", len(c.labels)))
		c.d.options = c.d.options[:0]
		for _, l := range c.labels {
			c.d.options = append(c.d.options, option{label: cut(l, room)})
		}
	}
	return c.d
}

// key closes on dismiss and on a pick, which copies the picked row.
func (c *copyDialog) key(m *model, msg tea.KeyPressMsg) (tea.Cmd, bool) {
	if key.Matches(msg, m.keys.dismiss) {
		return nil, false
	}
	picked, cmd := c.d.key(msg, &m.keys)
	if picked {
		return m.copy(c.texts[c.d.sel]), false
	}
	return cmd, true
}

// seen ends the dialog with a session switch.
func (c *copyDialog) seen(_ *model, msg tea.Msg) bool {
	_, switched := msg.(event.SessionChanged)
	return !switched
}
