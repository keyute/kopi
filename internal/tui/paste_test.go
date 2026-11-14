package tui

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

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
