package tui

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newHistoryModel(t *testing.T, entries ...string) *model {
	t.Helper()
	m, c := newTestModel(t, 60, 30)
	c.history = entries
	m.composer.setHistory(c.History())
	return m
}

func TestHistoryOverflowPreservesEntryAndDraft(t *testing.T) {
	for _, text := range []string{linesOf(testContentHeight + 1), strings.Repeat("w", 20*testContentHeight+1)} {
		m := newHistoryModel(t, text, "newer")
		c := &m.composer
		c.ta.MaxContentHeight = testContentHeight
		c.resize(20, 30)
		c.paste(linesOf(11))
		c.insertImage("/synthetic/draft.png")
		draft := c.value()
		want, images := c.expanded()
		c.update(home)
		assert.True(t, c.historyPrev())
		c.update(home)
		assert.True(t, c.historyPrev())
		got, imgs := c.expanded()
		assert.Less(t, len(c.value()), 100)
		assert.Equal(t, text, got)
		assert.Empty(t, imgs, "history carries no image metadata")
		assert.True(t, c.historyNext())
		assert.Equal(t, "newer", c.value())
		assert.True(t, c.historyNext())
		assert.Equal(t, draft, c.value())
		got, imgs = c.expanded()
		assert.Equal(t, want, got)
		assert.Equal(t, images, imgs)
	}
}

func TestHistoryReturnCollapsesOverflowingSavedDraftWithImages(t *testing.T) {
	m := newHistoryModel(t, "entry")
	c := &m.composer
	c.resize(20, 30)
	c.paste(linesOf(11))
	c.insertImage("/synthetic/draft.png")
	c.ta.SetValue(c.value() + strings.Repeat("w", c.ta.Width()*4))
	want, images := c.expanded()
	c.ta.MaxContentHeight = 5
	c.update(home)
	assert.True(t, c.historyPrev())
	assert.Equal(t, "entry", c.value())

	assert.True(t, c.historyNext())

	got, imgs := c.expanded()
	assert.Equal(t, want, got)
	assert.Equal(t, images, imgs)
	assert.Less(t, len(c.value()), 100)
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

func TestRecalledCommandShowsSwallowedAndDownReturnsACommandDraft(t *testing.T) {
	m := newHistoryModel(t, "!ls")
	typeText(m, "!pwd")
	m.syncInput(false)
	require.True(t, m.composer.bang)
	press(m, home)

	press(m, up)

	assert.Equal(t, "!ls", m.composer.value())
	assert.True(t, m.composer.bang)
	assert.Equal(t, "ls", m.composer.ta.Value())
	assert.True(t, m.composer.browsing(), "the swallow does not stop browsing")

	press(m, down)

	assert.Equal(t, "!pwd", m.composer.value())
	assert.True(t, m.composer.bang)
	assert.Equal(t, "pwd", m.composer.ta.Value())
}
