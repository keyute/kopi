package tui

import (
	"fmt"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPasteDeliveryReturnsFocusFromBusyAgent(t *testing.T) {
	for _, kind := range []string{"terminal text", "clipboard text", "terminal image", "clipboard image"} {
		t.Run(kind, func(t *testing.T) {
			m, _ := focused(t, explorer("t1"))
			press(m, down)
			require.Equal(t, "t1", m.focus.id)
			m.composer.paste("draft ")
			shown := m.shown
			want := "draft pasted"
			var images []string
			var msg tea.Msg
			switch kind {
			case "terminal text":
				msg = tea.PasteMsg{Content: "pasted"}
			case "clipboard text":
				msg = clipboardTextMsg{text: "pasted"}
			case "terminal image", "clipboard image":
				path := writeImage(t, t.TempDir(), "a.png")
				images = []string{path}
				want = fmt.Sprintf("draft [image #%d]", imageCount.Load()+1)
				if kind == "terminal image" {
					msg = tea.PasteMsg{Content: path}
				} else {
					msg = imagePastedMsg{path: path}
				}
			}

			m.Update(msg)

			assert.Equal(t, agentFocus{}, m.focus)
			assert.Equal(t, shown, m.shown)
			text, imgs := m.composer.expanded()
			assert.Equal(t, want, text)
			assert.Equal(t, images, imgs)
		})
	}
}

func TestPastingImagePathInsertsImageBlock(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)
	dir := t.TempDir()
	a, b := writeImage(t, dir, "a.png"), writeImage(t, dir, "b.png")
	n := imageCount.Load() + 1

	typeText(m, "see ")
	pasteText(m, a)
	typeText(m, " and ")
	pasteText(m, `"`+b+`"`)

	want := fmt.Sprintf("see [image #%d] and [image #%d]", n, n+1)
	assert.Equal(t, want, m.composer.value())
	text, imgs := m.composer.expanded()
	assert.Equal(t, want, text, "image markers stay in the text")
	assert.Equal(t, []string{a, b}, imgs)
}

func TestImageNumbersKeepIncreasingAcrossReset(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)
	a := writeImage(t, t.TempDir(), "a.png")
	n := imageCount.Load() + 1
	pasteText(m, a)

	m.composer.reset()
	pasteText(m, a)

	assert.Equal(t, fmt.Sprintf("[image #%d]", n+1), m.composer.value())
}
