package tui

// recall is the composer's prompt history browsing.
type recall struct {
	// entries are oldest first.
	entries []string
	// back counts entries back from the newest to the one shown; zero while
	// not browsing.
	back int
	// saved is the draft from before browsing started.
	saved string
}

func (c *composer) setHistory(entries []string) {
	c.hist.entries = entries
	c.hist.back = min(c.hist.back, len(entries))
}

// stopBrowsing keeps what is shown as the draft.
func (c *composer) stopBrowsing() { c.hist.back = 0 }

// atTop reports whether the cursor is on the draft's first visual row.
func (c *composer) atTop() bool {
	return c.ta.Line() == 0 && c.ta.LineInfo().RowOffset == 0
}

// atBottom reports whether the cursor is on the draft's last visual row.
func (c *composer) atBottom() bool {
	li := c.ta.LineInfo()
	return c.ta.Line() == c.ta.LineCount()-1 && li.RowOffset >= li.Height-1
}

// historyPrev shows the next older entry and reports whether it acted. It
// acts at the top edge only, and unless already browsing only on an empty
// draft or at column 0; at the oldest entry it stays there.
func (c *composer) historyPrev() bool {
	h := &c.hist
	if len(h.entries) == 0 || !c.atTop() || (h.back == 0 && !c.empty() && c.ta.Column() != 0) {
		return false
	}
	if h.back == 0 {
		h.saved = c.ta.Value()
	}
	if h.back < len(h.entries) {
		h.back++
		c.showHistory(h.entries[len(h.entries)-h.back])
	}
	return true
}

// historyNext shows the next newer entry, and after the newest the saved
// draft; it acts only while browsing with the cursor on the last row.
func (c *composer) historyNext() bool {
	h := &c.hist
	if h.back == 0 || !c.atBottom() {
		return false
	}
	h.back--
	if h.back == 0 {
		trial := c.ta
		trial.SetValue(h.saved)
		if trial.Value() != sanitizedText(h.saved) {
			text, imgs := c.expand(h.saved)
			trial.SetValue(c.collapse(text, imgs))
		}
		c.ta = trial
	} else {
		c.showHistory(h.entries[len(h.entries)-h.back])
	}
	return true
}

func (c *composer) showHistory(text string) {
	trial := c.ta
	trial.SetValue(text)
	if trial.Value() != sanitizedText(text) {
		trial.SetValue(c.collapse(text, nil))
	}
	c.ta = trial
}
