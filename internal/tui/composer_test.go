package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/event"
)

var (
	backspace = tea.KeyPressMsg{Code: tea.KeyBackspace}
	del       = tea.KeyPressMsg{Code: tea.KeyDelete}
	home      = tea.KeyPressMsg{Code: tea.KeyHome}
	selectAll = tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl}
)

// linesOf is n lines of text.
func linesOf(n int) string {
	ls := make([]string, n)
	for i := range ls {
		ls[i] = fmt.Sprintf("line %d", i)
	}
	return strings.Join(ls, "\n")
}

func TestPasteCollapsesOverTheLimits(t *testing.T) {
	for _, tc := range []struct {
		name, text, want string
	}{
		{"10 lines", linesOf(10), linesOf(10)},
		{"11 lines", linesOf(11), "[paste #1 +11 lines]"},
		{"10 lines and a final newline", linesOf(10) + "\n", linesOf(10) + "\n"},
		{"11 lines and a final newline", linesOf(11) + "\n", "[paste #1 +11 lines]"},
		{"1000 runes", strings.Repeat("é", 1000), strings.Repeat("é", 1000)},
		{"1001 runes", strings.Repeat("é", 1001), "[paste #1 1001 chars]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTestModel(t, 40, 30)
			pasteText(m, tc.text)
			assert.Equal(t, tc.want, m.composer.value())
			text, imgs := m.composer.expanded()
			assert.Equal(t, tc.text, text)
			assert.Empty(t, imgs)
		})
	}
}

func TestPasteNormalisesLineEndings(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)
	pasteText(m, strings.ReplaceAll(linesOf(11), "\n", "\r\n"))

	assert.Equal(t, "[paste #1 +11 lines]", m.composer.value())
	text, _ := m.composer.expanded()
	assert.Equal(t, linesOf(11), text)
}

func TestPasteBlocksNumberUpAndRestartAfterReset(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)
	pasteText(m, linesOf(11))
	typeText(m, " and ")
	pasteText(m, linesOf(12))

	assert.Equal(t, "[paste #1 +11 lines] and [paste #2 +12 lines]", m.composer.value())
	text, _ := m.composer.expanded()
	assert.Equal(t, linesOf(11)+" and "+linesOf(12), text)

	m.composer.reset()
	pasteText(m, linesOf(11))
	assert.Equal(t, "[paste #1 +11 lines]", m.composer.value())
}

func TestRepeatedPasteExpandsThenCollapsesAgain(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)
	typeText(m, "> ")
	pasteText(m, linesOf(11))
	require.Equal(t, "> [paste #1 +11 lines]", m.composer.value())

	pasteText(m, linesOf(11))
	assert.Equal(t, "> "+linesOf(11), m.composer.value(), "the second paste expands the block in place")
	assert.Empty(t, m.composer.blocks)

	pasteText(m, linesOf(11))
	assert.Equal(t, "> "+linesOf(11)+"[paste #2 +11 lines]", m.composer.value())
}

func TestInputBetweenIdenticalPastesKeepsBothBlocks(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)
	pasteText(m, linesOf(11))
	press(m, tea.KeyPressMsg{Code: 'x', Text: "x"})
	pasteText(m, linesOf(11))

	assert.Equal(t, "[paste #1 +11 lines]x[paste #2 +11 lines]", m.composer.value())
}

func TestBackspaceAndDeleteRemoveMarkersWhole(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)
	img := writeImage(t, t.TempDir(), "a.png")
	n := imageCount.Load() + 1
	typeText(m, "a")
	pasteText(m, linesOf(11))
	pasteText(m, img)
	typeText(m, "b")
	require.Equal(t, fmt.Sprintf("a[paste #1 +11 lines][image #%d]b", n), m.composer.value())

	press(m, backspace)
	assert.Equal(t, fmt.Sprintf("a[paste #1 +11 lines][image #%d]", n), m.composer.value(), "elsewhere one rune goes")
	press(m, backspace)
	assert.Equal(t, "a[paste #1 +11 lines]", m.composer.value())
	press(m, backspace)
	assert.Equal(t, "a", m.composer.value())

	m.composer.reset()
	typeText(m, "a")
	pasteText(m, linesOf(11))
	pasteText(m, img)
	press(m, home)
	press(m, del)
	assert.Equal(t, fmt.Sprintf("[paste #1 +11 lines][image #%d]", n+1), m.composer.value())
	press(m, del)
	assert.Equal(t, fmt.Sprintf("[image #%d]", n+1), m.composer.value())
	press(m, del)
	assert.Empty(t, m.composer.value())
}

func TestEditedMarkerIsPlainText(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)
	img := writeImage(t, t.TempDir(), "a.png")
	for _, paste := range []string{linesOf(11), img} {
		m.composer.reset()
		pasteText(m, paste)
		mk := m.composer.value()
		press(m, left)
		press(m, backspace)

		edited := mk[:len(mk)-2] + "]"
		require.Equal(t, edited, m.composer.value())
		text, imgs := m.composer.expanded()
		assert.Equal(t, edited, text)
		assert.Empty(t, imgs)
	}
}

func TestPasteReplacesSelection(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)
	typeText(m, "old draft")
	press(m, selectAll)

	pasteText(m, linesOf(11))

	assert.Equal(t, "[paste #1 +11 lines]", m.composer.value())
}

func TestRestoreKeepsDraftBlocks(t *testing.T) {
	m, _ := newTestModel(t, 60, 30)
	m.composer.insertImage("/tmp/a.png")
	mk := fmt.Sprintf("[image #%d]", imageCount.Load())
	m.composer.reset()
	pasteText(m, linesOf(11))

	m.composer.restore([]event.Input{{Text: "a " + mk, Images: []string{"/tmp/a.png"}}})

	assert.Equal(t, "a "+mk+"\n\n[paste #1 +11 lines]", m.composer.value())
	text, imgs := m.composer.expanded()
	assert.Equal(t, "a "+mk+"\n\n"+linesOf(11), text)
	assert.Equal(t, []string{"/tmp/a.png"}, imgs)
}

func TestComposerDragSelectsAndCopies(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)
	typeText(m, "hello world")
	top := m.composerTop()

	pressAt(m, 2, top)
	moveTo(m, 7, top)
	require.True(t, m.composer.ta.HasSelection())
	cmd := releaseAt(m, 7, top)

	assertCopied(t, m, cmd, "hello")
	press(m, tea.KeyPressMsg{Code: 'X', Text: "X"})
	assert.Equal(t, "X world", m.composer.value(), "typing replaces the selection")
}

func TestComposerDoubleClickSelectsWord(t *testing.T) {
	for _, tc := range []struct {
		name, text  string
		x, row      int
		word, typed string
	}{
		{"first row", "alpha beta gamma", 9, 0, "beta", "alpha X gamma"},
		{"wrapped row", "alpha beta gamma delta epsilon", 5, 1, "delta", "alpha beta gamma X epsilon"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTestModel(t, 20, 30)
			at := fixedClock(m)
			typeText(m, tc.text)
			y := m.composerTop() + tc.row

			pressAt(m, tc.x, y)
			releaseAt(m, tc.x, y)
			*at = at.Add(300 * time.Millisecond)
			pressAt(m, tc.x, y)
			cmd := releaseAt(m, tc.x, y)

			assertCopied(t, m, cmd, tc.word)
			press(m, tea.KeyPressMsg{Code: 'X', Text: "X"})
			assert.Equal(t, tc.typed, m.composer.value(), "typing replaces the word")
		})
	}
}

func TestComposerClickPlacesCursor(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)
	typeText(m, "hello world")

	pressAt(m, 5, m.composerTop())
	assert.Nil(t, releaseAt(m, 5, m.composerTop()))

	assert.False(t, m.composer.ta.HasSelection())
	assert.Equal(t, 3, m.composer.ta.Column(), "the gutter is two columns")
	typeText(m, "_")
	assert.Equal(t, "hel_lo world", m.composer.value())
}

func TestComposerClickHitsRightRowWhilePendingShown(t *testing.T) {
	m, _ := newTestModel(t, 60, 30)
	send(m, event.TurnStart{Prompt: "go"})
	send(m, event.QueueChanged{Pending: []string{"a", "b"}})
	typeText(m, "abc\ndef")

	pressAt(m, 3, m.composerTop()+1)
	releaseAt(m, 3, m.composerTop()+1)

	assert.Equal(t, 1, m.composer.ta.Line())
	assert.Equal(t, 1, m.composer.ta.Column())
}

func TestComposerClickIsOtherInputBetweenPastes(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)
	pasteText(m, linesOf(11))

	pressAt(m, 30, m.composerTop())
	releaseAt(m, 30, m.composerTop())
	pasteText(m, linesOf(11))

	assert.Equal(t, "[paste #1 +11 lines][paste #2 +11 lines]", m.composer.value())
}

func TestRestoreRegistersOnlyMarkersThatWereImages(t *testing.T) {
	m, _ := newTestModel(t, 60, 30)
	typeText(m, "see [image #999] ")
	m.composer.insertImage("/tmp/b.png")
	real := fmt.Sprintf("[image #%d]", imageCount.Load())
	require.Equal(t, "see [image #999] "+real, m.composer.value())

	text, imgs := m.composer.expanded()
	m.composer.reset()
	m.composer.restore([]event.Input{{Text: text, Images: imgs}})

	assert.Equal(t, block{image: "/tmp/b.png"}, m.composer.blocks[real])
	assert.NotContains(t, m.composer.blocks, "[image #999]", "a typed marker stays plain text")
}
