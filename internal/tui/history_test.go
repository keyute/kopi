package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func newHistoryModel(t *testing.T, entries ...string) *model {
	t.Helper()
	m, c := newTestModel(t, 60, 30)
	c.history = entries
	m.composer.setHistory(c.History())
	return m
}

func TestHistoryWalksOlderThenBackToDraft(t *testing.T) {
	m := newHistoryModel(t, "first", "second")
	typeText(m, "dr")
	press(m, home)

	for _, want := range []string{"second", "first", "first"} {
		assert.Nil(t, press(m, up))
		assert.Equal(t, want, m.composer.value())
	}
	for _, want := range []string{"second", "dr"} {
		assert.Nil(t, press(m, down))
		assert.Equal(t, want, m.composer.value())
	}
	press(m, down)
	assert.Equal(t, "dr", m.composer.value(), "browsing has stopped")
}

func TestHistoryNeedsColumnZeroOnADraft(t *testing.T) {
	m := newHistoryModel(t, "first")
	typeText(m, "dr")

	press(m, up)

	assert.Equal(t, "dr", m.composer.value())
}

func TestTypingWhileBrowsingKeepsShownText(t *testing.T) {
	m := newHistoryModel(t, "first", "second")
	press(m, up)

	typeText(m, "!")
	press(m, down)

	assert.Equal(t, "second!", m.composer.value())
}

func TestHistoryUpOffTopEdgeMovesCursor(t *testing.T) {
	m := newHistoryModel(t, "first")
	typeText(m, "one\ntwo")
	press(m, home)

	press(m, up)

	assert.Equal(t, "one\ntwo", m.composer.value())
	assert.Equal(t, 0, m.composer.ta.Line())
}

func TestMultiLineEntryBrowsesByRow(t *testing.T) {
	m := newHistoryModel(t, "old", "a\nb")
	press(m, up)
	assert.Equal(t, "a\nb", m.composer.value())

	press(m, up)
	assert.Equal(t, "a\nb", m.composer.value(), "the cursor moves to the first row")
	press(m, up)
	assert.Equal(t, "old", m.composer.value())
}
