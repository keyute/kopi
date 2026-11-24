package tui

import (
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
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

// testContentHeight stands in for maxComposerLines in capacity tests; a
// draft near 10,000 lines is slow to edit.
const testContentHeight = 30

// linesOf is n lines of text.
func linesOf(n int) string {
	ls := make([]string, n)
	for i := range ls {
		ls[i] = fmt.Sprintf("line %d", i)
	}
	return strings.Join(ls, "\n")
}

func TestComposerReplaceUsesRuneRange(t *testing.T) {
	for _, col := range []int{2, 3, 4} {
		t.Run(fmt.Sprintf("cursor=%d", col), func(t *testing.T) {
			m, _ := newTestModel(t, 60, 30)
			c := &m.composer
			c.ta.SetValue("é @界尾\nkeep")
			c.selectRange(selection{anchor: point{0, 1}, head: point{0, col}})
			c.hist.back = 1
			c.lastPaste = "previous"

			c.replace(2, 4, "猫 ")

			assert.Equal(t, "é 猫 尾\nkeep", c.value())
			assert.Equal(t, 0, c.ta.Line())
			assert.Equal(t, 4, c.ta.Column())
			assert.False(t, c.ta.HasSelection())
			assert.False(t, c.browsing())
			assert.Empty(t, c.lastPaste)
		})
	}
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

func TestEmptyPastePreservesCollapsedBlockAndImages(t *testing.T) {
	m, _ := newTestModel(t, 60, 30)
	c := &m.composer
	c.insertImage("/synthetic/draft.png")
	image := c.value()
	payload := linesOf(11)
	pasteText(m, payload)
	marker := c.lastPaste
	require.NotEmpty(t, marker)
	require.Equal(t, image+marker, c.value())
	before := c.blocks[marker]

	pasteText(m, "")

	assert.Equal(t, image+marker, c.value())
	assert.Contains(t, c.blocks, marker)
	assert.Equal(t, before, c.blocks[marker])
	text, imgs := c.expanded()
	assert.Equal(t, image+payload, text)
	assert.Equal(t, []string{"/synthetic/draft.png"}, imgs)
}

func TestRepeatedPasteExpandsSanitizedText(t *testing.T) {
	for _, tc := range []struct {
		name, text, want string
	}{
		{"tabs", strings.Repeat("\tline\n", 11), strings.Repeat("    line\n", 11)},
		{"controls and invalid runes", strings.Repeat("a\x00\x1b\u007f\u0085\ufffd\xffb\n", 11), strings.Repeat("ab\n", 11)},
		{"carriage returns", strings.Repeat("a\rb\n", 11), strings.Repeat("a\nb\n", 11)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTestModel(t, 60, 30)
			c := &m.composer
			c.ta.SetValue("prefix suffix")
			c.selectRange(selection{anchor: point{0, 7}, head: point{0, 7}})
			c.paste(tc.text)
			require.NotEmpty(t, c.blocks)

			c.paste(tc.text)

			assert.Equal(t, "prefix "+tc.want+"suffix", c.value())
			assert.Empty(t, c.blocks)
		})
	}
}

func TestRestoreSmallTextUsesTextareaSanitization(t *testing.T) {
	for _, text := range []string{
		"\tindented\n\t\tnested",
		"a\x00\x1b\u007f\u0085\ufffd\xffb",
		"a\rb\r\nc",
	} {
		t.Run(fmt.Sprintf("%q", text), func(t *testing.T) {
			m, _ := newTestModel(t, 60, 30)
			c := &m.composer
			c.ta.SetValue(text + "\n\ndraft")
			want := c.value()
			c.ta.SetValue("draft")

			c.restore([]event.Input{{Text: text}})

			assert.Equal(t, want, c.value())
			assert.Empty(t, c.blocks)
		})
	}
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
			synctest.Test(t, func(t *testing.T) {
				m, _ := newTestModel(t, 20, 30)
				typeText(m, tc.text)
				y := m.composerTop() + tc.row

				pressAt(m, tc.x, y)
				releaseAt(m, tc.x, y)
				time.Sleep(300 * time.Millisecond)
				pressAt(m, tc.x, y)
				cmd := releaseAt(m, tc.x, y)

				assertCopied(t, m, cmd, tc.word)
				press(m, tea.KeyPressMsg{Code: 'X', Text: "X"})
				assert.Equal(t, tc.typed, m.composer.value(), "typing replaces the word")
			})
		})
	}
}

// clickTimes presses n times in quick succession at (x, y) without
// releasing, so only the drag that follows copies. It must run in a synctest
// bubble.
func clickTimes(m *model, n, x, y int) {
	for range n {
		pressAt(m, x, y)
		time.Sleep(300 * time.Millisecond)
	}
}

func TestComposerDoubleClickOnSpaceIsPlainPress(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestModel(t, 40, 30)
		typeText(m, "hello world")

		clickTimes(m, 2, 7, m.composerTop())

		assert.Nil(t, releaseAt(m, 7, m.composerTop()))
		assert.False(t, m.composer.ta.HasSelection())
		assert.Equal(t, 5, m.composer.ta.Column())
	})
}

func TestComposerTripleClickSelectsLogicalLine(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestModel(t, 20, 30)
		typeText(m, "alpha beta gamma delta epsilon\nnext")
		y := m.composerTop() + 1

		clickTimes(m, 3, 5, y)
		cmd := releaseAt(m, 5, y)

		assertCopied(t, m, cmd, "alpha beta gamma delta epsilon")
		press(m, tea.KeyPressMsg{Code: 'X', Text: "X"})
		assert.Equal(t, "X\nnext", m.composer.value())
	})
}

func TestComposerDragAfterMultiClickExtendsByWholeUnits(t *testing.T) {
	for _, tc := range []struct {
		name               string
		text               string
		clicks             int
		x, row, toX, toRow int
		want               string
	}{
		{"words forwards", "alpha beta gamma", 2, 9, 0, 15, 0, "beta gamma"},
		{"words backwards", "alpha beta gamma", 2, 9, 0, 4, 0, "alpha beta"},
		{"lines forwards", "one two\nthree four\nfive", 3, 4, 1, 3, 2, "three four\nfive"},
		{"lines backwards", "one two\nthree four\nfive", 3, 4, 1, 3, 0, "one two\nthree four"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, _ := newTestModel(t, 40, 30)
				typeText(m, tc.text)
				top := m.composerTop()

				clickTimes(m, tc.clicks, tc.x, top+tc.row)
				moveTo(m, tc.toX, top+tc.toRow)
				cmd := releaseAt(m, tc.toX, top+tc.toRow)

				assertCopied(t, m, cmd, tc.want)
			})
		})
	}
}

func TestComposerTripleClickInScrolledInput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestModel(t, 40, 30)
		typeText(m, linesOf(20))
		off := m.composer.ta.ScrollYOffset()
		require.Positive(t, off)
		y := m.composerTop() + 1

		clickTimes(m, 3, 4, y)
		cmd := releaseAt(m, 4, y)

		assertCopied(t, m, cmd, fmt.Sprintf("line %d", off+1))
	})
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
	send(m, event.QueueChanged{Pending: []event.Queued{{Text: "a"}, {Text: "b"}}})
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

func TestComposerOverflowStaysBlockBacked(t *testing.T) {
	const cols = 18 // the textarea's width in a 20-column terminal
	// rows keeps every fixture over pasteMaxRunes too, so pasting collapses
	// the raw text by size, not by capacity.
	const rows = 250
	for _, tc := range []struct {
		name, text string
	}{
		{"logical lines", linesOf(rows + 1)},
		{"wrapped rows", strings.Repeat("w", cols*rows+1)},
		{"tab-indented logical lines", strings.Repeat("\tline\n", rows+1)},
		{"tab-expanded wrapped rows", strings.Repeat("\t", cols*rows/4+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTestModel(t, 20, 30)
			c := &m.composer
			c.ta.MaxContentHeight = rows
			require.Equal(t, cols, c.ta.Width())
			c.restore([]event.Input{{Text: tc.text}})
			assert.NotEqual(t, tc.text, c.value())
			text, imgs := c.expanded()
			assert.Equal(t, tc.text, text)
			assert.Empty(t, imgs)

			c.reset()
			c.paste(tc.text)
			mk := c.value()
			for range 2 {
				c.paste(tc.text)
				assert.Equal(t, mk, c.value(), "failed expansion leaves the marker intact")
				assert.Contains(t, c.blocks, mk)
				text, _ = c.expanded()
				assert.Equal(t, tc.text, text)
			}
		})
	}
}

func TestSmallPasteChecksCombinedCapacity(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)
	c := &m.composer
	c.ta.MaxContentHeight = testContentHeight
	draft := linesOf(testContentHeight - 2)
	c.ta.SetValue(draft)
	payload := linesOf(5)
	c.paste(payload)
	text, imgs := c.expanded()
	assert.Equal(t, draft+payload, text)
	assert.Empty(t, imgs)
}

func TestSmallPasteOverflowSanitizesAndMatchesOriginalClipboard(t *testing.T) {
	for _, tc := range []struct {
		name, payload, clean string
	}{
		{"tabs", "\talpha\n\tbeta\n\tgamma", "    alpha\n    beta\n    gamma"},
		{"controls", "a\x00\x1bb\nc\u007f\u0085d\ne\ufffd\xfff", "ab\ncd\nef"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTestModel(t, 60, 30)
			c := &m.composer
			draft := linesOf(4)
			c.ta.SetValue(draft)
			c.ta.MaxContentHeight = 5

			c.paste(tc.payload)

			marker := c.lastPaste
			require.NotEmpty(t, marker)
			require.Equal(t, draft+marker, c.value(), "only the insertion collapses")
			text, imgs := c.expanded()
			assert.Equal(t, draft+tc.clean, text)
			assert.Empty(t, imgs)

			c.paste(tc.payload)

			assert.Equal(t, draft+marker, c.value(), "failed expansion keeps the existing block")
			text, _ = c.expanded()
			assert.Equal(t, draft+tc.clean, text)

			c.ta.MaxContentHeight = 20
			c.paste(tc.payload)

			assert.Equal(t, draft+tc.clean, c.value(), "the original clipboard expands in place once it fits")
			assert.Empty(t, c.blocks)
		})
	}
}

func TestSmallPasteOverflowDoesNotMatchDifferentRawClipboard(t *testing.T) {
	for _, other := range []string{"ab\ncd\nef", "a\x1bb\ncd\nef"} {
		t.Run(fmt.Sprintf("%q", other), func(t *testing.T) {
			m, _ := newTestModel(t, 60, 30)
			c := &m.composer
			draft := linesOf(4)
			c.ta.SetValue(draft)
			c.ta.MaxContentHeight = 5
			payload := "a\x00b\ncd\nef"
			clean := "ab\ncd\nef"
			c.paste(payload)
			marker := c.lastPaste
			require.Equal(t, draft+marker, c.value())
			c.ta.MaxContentHeight = 20

			c.paste(other)

			assert.Equal(t, draft+marker+clean, c.value())
			text, _ := c.expanded()
			assert.Equal(t, draft+clean+clean, text)
			assert.Contains(t, c.blocks, marker)
		})
	}
}

func TestSmallPasteWrappedCapacityPreservesImagesAndSelection(t *testing.T) {
	m, _ := newTestModel(t, 20, 30)
	c := &m.composer
	c.insertImage("/synthetic/draft.png")
	image := c.value()
	c.paste(linesOf(11))
	block := c.lastPaste
	c.ta.MaxContentHeight = 5
	draft := image + block + "old"
	c.ta.SetValue(draft)
	require.Equal(t, draft, c.value())
	c.selectRange(selection{anchor: point{0, len([]rune(image + block))}, head: point{0, len([]rune(draft))}})
	payload := strings.Repeat("z", 90)
	c.paste(payload)
	text, imgs := c.expanded()
	assert.Equal(t, image+linesOf(11)+payload, text)
	assert.Equal(t, []string{"/synthetic/draft.png"}, imgs)
	assert.Contains(t, c.blocks, block)
}

func TestPasteMarkerAtCapacityCollapsesWholeDraft(t *testing.T) {
	m, _ := newTestModel(t, 20, 30)
	c := &m.composer
	draft := strings.Repeat("w", c.ta.Width()*4)
	c.ta.SetValue(draft)
	c.ta.MaxContentHeight = 5
	require.Equal(t, draft, c.value())
	payload := "\n1\n2\n3\n4"
	c.paste(payload)
	text, imgs := c.expanded()
	assert.Equal(t, draft+payload, text)
	assert.Empty(t, imgs)
	assert.NotContains(t, c.value(), draft)
}

func TestComposerCharacterSelectionEndpoints(t *testing.T) {
	for _, tc := range []struct {
		name, text     string
		x, y, toX, toY int
	}{
		{"backwards", "hello world", 10, 0, 3, 0},
		{"wrapped", "alpha beta gamma delta epsilon", 5, 1, 4, 0},
		{"scrolled", linesOf(20), 4, 1, 7, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTestModel(t, 20, 30)
			c := &m.composer
			c.ta.SetValue(tc.text)
			start := c.ta.PositionAt(tc.x, tc.y)
			end := c.ta.PositionAt(tc.toX, tc.toY)
			c.press(tc.x, tc.y, 1)
			assert.Equal(t, start.Row, c.ta.Line())
			assert.Equal(t, start.Col, c.ta.Column())
			c.drag(tc.toX, tc.toY)
			assert.Equal(t, end.Row, c.ta.Line())
			assert.Equal(t, end.Col, c.ta.Column())
			want := selection{anchor: point{start.Row, start.Col}, head: point{end.Row, end.Col}}
			lines := strings.Split(tc.text, "\n")
			from, to := want.ordered()
			selected := string([]rune(lines[from.line])[from.col:])
			if from.line == to.line {
				selected = string([]rune(lines[from.line])[from.col:to.col])
			} else {
				for i := from.line + 1; i < to.line; i++ {
					selected += "\n" + lines[i]
				}
				selected += "\n" + string([]rune(lines[to.line])[:to.col])
			}
			text, ok := c.release()
			assert.True(t, ok)
			assert.Equal(t, selected, text)
		})
	}
}

func TestRepeatedPasteChecksCombinedCapacity(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)
	c := &m.composer
	c.ta.MaxContentHeight = testContentHeight
	draft := linesOf(testContentHeight - 5)
	c.ta.SetValue(draft)
	payload := linesOf(11)
	c.paste(payload)
	before := c.value()
	marker := c.lastPaste

	c.paste(payload)

	assert.Equal(t, before, c.value())
	assert.Contains(t, c.blocks, marker)
	text, _ := c.expanded()
	assert.Equal(t, draft+payload, text)
}

func TestRestoreCombinedCapacityKeepsDraftAndImages(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)
	c := &m.composer
	c.ta.MaxContentHeight = testContentHeight
	c.insertImage("/synthetic/restored.png")
	restoredImage := c.value()
	c.reset()
	c.insertImage("/synthetic/draft.png")
	draftImage := c.value()
	c.paste(linesOf(11))
	inputs := []event.Input{
		{Text: linesOf(testContentHeight*3/5) + " " + restoredImage, Images: []string{"/synthetic/restored.png"}},
		{Text: linesOf(testContentHeight / 2)},
	}

	c.restore(inputs)

	text, imgs := c.expanded()
	assert.Equal(t, inputs[0].Text+"\n\n"+inputs[1].Text+"\n\n"+draftImage+linesOf(11), text)
	assert.Equal(t, []string{"/synthetic/restored.png", "/synthetic/draft.png"}, imgs)
	assert.Less(t, len(c.value()), 100)
}

func TestDuplicateMarkersKeepTheirBlocksUntilLastDeletion(t *testing.T) {
	for _, kind := range []string{"paste", "image"} {
		for _, direction := range []string{"backward", "forward"} {
			t.Run(kind+"/"+direction, func(t *testing.T) {
				m, _ := newTestModel(t, 60, 30)
				c := &m.composer
				if kind == "paste" {
					c.paste(linesOf(11))
				} else {
					c.insertImage("/synthetic/image.png")
				}
				mk := c.value()
				c.ta.SetValue(mk + mk)
				k := backspace
				if direction == "forward" {
					c.update(home)
					k = del
				}
				c.update(k)
				assert.Equal(t, mk, c.value())
				assert.Contains(t, c.blocks, mk)
				text, imgs := c.expanded()
				if kind == "paste" {
					assert.Equal(t, linesOf(11), text)
					assert.Empty(t, imgs)
				} else {
					assert.Equal(t, mk, text)
					assert.Equal(t, []string{"/synthetic/image.png"}, imgs)
				}
				c.update(k)
				assert.Empty(t, c.value())
				assert.NotContains(t, c.blocks, mk)
			})
		}
	}
}

func TestDuplicatePasteExpansionKeepsOtherOccurrence(t *testing.T) {
	m, _ := newTestModel(t, 60, 30)
	c := &m.composer
	payload := linesOf(11)
	c.paste(payload)
	mk := c.value()
	c.ta.SetValue(mk + mk)
	c.paste(payload)

	assert.Equal(t, mk+payload, c.value())
	assert.Contains(t, c.blocks, mk)
	text, _ := c.expanded()
	assert.Equal(t, payload+payload, text)
	c.update(tea.KeyPressMsg{Code: tea.KeyHome, Mod: tea.ModCtrl})
	c.update(del)
	assert.NotContains(t, c.blocks, mk)
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

func TestImageAtWrappedCapacityPreservesDraft(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprintf("selection=%t", replace), func(t *testing.T) {
			m, _ := newTestModel(t, 20, 30)
			c := &m.composer
			c.insertImage("/synthetic/existing.png")
			existing := c.value()
			payload := linesOf(11)
			c.paste(payload)
			marker := c.lastPaste
			prefix := existing + marker + strings.Repeat("w", c.ta.Width()*3)
			draft := prefix + "old suffix"
			c.ta.SetValue(draft)
			require.Equal(t, draft, c.value())
			c.ta.MaxContentHeight = c.ta.Height()
			pos := len([]rune(prefix))
			end := pos
			if replace {
				end += len("old")
			}
			c.selectRange(selection{anchor: point{0, pos}, head: point{0, end}})
			mk := fmt.Sprintf("[image #%d]", imageCount.Load()+1)

			c.insertImage("/synthetic/new.png")

			tail := "old suffix"
			if replace {
				tail = " suffix"
			}
			text, imgs := c.expanded()
			assert.Equal(t, existing+payload+strings.Repeat("w", c.ta.Width()*3)+mk+tail, text)
			assert.Equal(t, []string{"/synthetic/existing.png", "/synthetic/new.png"}, imgs)
			assert.Contains(t, c.blocks, marker)
			assert.Empty(t, c.lastPaste)
			assert.NotEqual(t, draft, c.value())
		})
	}
}
