package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeClipboard struct {
	text     string
	err      error
	image    []byte
	imageErr error
	written  []string
}

func (f *fakeClipboard) ReadText() (string, error) { return f.text, f.err }

func (f *fakeClipboard) ReadImage(context.Context) ([]byte, error) { return f.image, f.imageErr }

func (f *fakeClipboard) WriteText(text string) error {
	f.written = append(f.written, text)
	return nil
}

var ctrlV = tea.KeyPressMsg{Code: 'v', Mod: tea.ModCtrl}

func TestPasteKeyInsertsClipboardText(t *testing.T) {
	m, _ := newTestModel(t, 30, 20)
	m.clip = &fakeClipboard{text: "from clipboard"}
	typeText(m, "> ")

	cmd := press(m, ctrlV)

	require.NotNil(t, cmd)
	assert.Equal(t, "> ", m.composer.value(), "the clipboard is read off the update loop")
	m.Update(cmd())
	assert.Equal(t, "> from clipboard", m.composer.value())
}

func TestComposerClipboardCompletionDuringApprovalIsDiscarded(t *testing.T) {
	m, _ := newTestModel(t, 60, 30)
	m.clip = &fakeClipboard{text: "composer clipboard"}
	typeText(m, "draft")
	cmd := press(m, ctrlV)
	require.NotNil(t, cmd)

	send(m, editApproval())
	press(m, digit('3'))
	keys(m, "feedback")
	m.Update(cmd())

	assert.Equal(t, "draft", m.composer.value())
	assert.Equal(t, "feedback", m.frontDialog().text())
}

func TestApprovalClipboardWithoutTextChangesNothing(t *testing.T) {
	for _, cb := range []*fakeClipboard{{err: errors.New("no clipboard")}, {}} {
		m, _ := newTestModel(t, 60, 30)
		m.clip = cb
		send(m, editApproval())
		press(m, digit('3'))
		keys(m, "feedback")

		cmd := press(m, ctrlV)

		require.NotNil(t, cmd)
		assert.Nil(t, cmd())
		assert.Equal(t, "feedback", m.frontDialog().text())
		assert.Empty(t, m.composer.value())
	}
}

func TestPasteKeyWithoutTextChangesNothing(t *testing.T) {
	for _, cb := range []*fakeClipboard{{err: errors.New("no clipboard")}, {}} {
		m, _ := newTestModel(t, 30, 20)
		m.clip = cb
		typeText(m, "draft")

		cmd := press(m, ctrlV)

		require.NotNil(t, cmd)
		assert.Nil(t, cmd())
		assert.Equal(t, "draft", m.composer.value())
	}
}

func TestPasteKeySavesClipboardImageAsBlock(t *testing.T) {
	m, _ := newTestModel(t, 30, 20)
	m.imageDir = filepath.Join(t.TempDir(), "images")
	m.clip = &fakeClipboard{image: []byte("png bytes"), text: "ignored"}
	n := imageCount.Load() + 1

	cmd := press(m, ctrlV)
	require.NotNil(t, cmd)
	m.Update(cmd())

	assert.Equal(t, fmt.Sprintf("[image #%d]", n), m.composer.value())
	_, imgs := m.composer.expanded()
	require.Len(t, imgs, 1)
	assert.Equal(t, m.imageDir, filepath.Dir(imgs[0]))
	data, err := os.ReadFile(imgs[0])
	require.NoError(t, err)
	assert.Equal(t, []byte("png bytes"), data)
	fi, err := os.Stat(imgs[0])
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
}

func TestPasteKeyImageFailureFlashes(t *testing.T) {
	m, _ := newTestModel(t, 40, 20)
	m.clip = &fakeClipboard{imageErr: errors.New("boom"), text: "ignored"}
	typeText(m, "draft")

	cmd := press(m, ctrlV)
	require.NotNil(t, cmd)
	m.Update(cmd())

	assert.Equal(t, "draft", m.composer.value())
	assert.Contains(t, statusText(m), "paste failed")
}

func TestParsePNGf(t *testing.T) {
	png, err := parsePNGf([]byte("«data PNGf89504E470D0A»\n"))
	require.NoError(t, err)
	assert.Equal(t, []byte{0x89, 'P', 'N', 'G', '\r', '\n'}, png)

	png, err = parsePNGf(nil)
	require.NoError(t, err)
	assert.Nil(t, png)

	for _, garbage := range []string{"missing value", "«data TIFF00»", "«data PNGfzz»"} {
		_, err = parsePNGf([]byte(garbage))
		assert.Error(t, err, garbage)
	}
}

func TestListsPNG(t *testing.T) {
	assert.True(t, listsPNG([]byte("TARGETS\ntext/plain\nimage/png\n")))
	assert.False(t, listsPNG([]byte("text/plain\nimage/png-ish\n")))
	assert.False(t, listsPNG(nil))
}
