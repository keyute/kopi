package tui

import (
	"image/color"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/event"
)

// reverseVideo is an SGR sequence that starts with reverse video.
var reverseVideo = regexp.MustCompile(`\x1b\[7[;m]`)

func pressAt(m *model, x, y int) {
	m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
}

func moveTo(m *model, x, y int) tea.Cmd {
	_, cmd := m.Update(tea.MouseMotionMsg{X: x, Y: y, Button: tea.MouseLeft})
	return cmd
}

// releaseAt reports no button, as some terminals do.
func releaseAt(m *model, x, y int) tea.Cmd {
	_, cmd := m.Update(tea.MouseReleaseMsg{X: x, Y: y})
	return cmd
}

// fixedClock makes m's clock read *at.
func fixedClock(m *model) *time.Time {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return at }
	return &at
}

// runCopy runs a copy's clipboard writes, leaving its flash's tick unrun, and
// returns their messages.
func runCopy(t *testing.T, cmd tea.Cmd) []tea.Msg {
	t.Helper()
	require.NotNil(t, cmd)
	batch, ok := cmd().(tea.BatchMsg)
	require.True(t, ok)
	require.Len(t, batch, 2)
	writes, ok := batch[0]().(tea.BatchMsg)
	require.True(t, ok)
	var msgs []tea.Msg
	for _, c := range writes {
		msgs = append(msgs, c())
	}
	return msgs
}

func assertCopied(t *testing.T, m *model, cmd tea.Cmd, want string) {
	t.Helper()
	assert.Contains(t, runCopy(t, cmd), tea.SetClipboard(want)(), "copied through OSC 52")
	assert.Equal(t, []string{want}, m.clip.(*fakeClipboard).written, "and the system clipboard")
	assert.Contains(t, statusText(m), "Copied")
}

// reversedRows is the transcript's screen rows drawn with reverse video; the
// composer's cursor is reverse video too.
func reversedRows(m *model) []int {
	var out []int
	for i, r := range strings.Split(m.View().Content, "\n")[:m.transcriptHeight()] {
		if reverseVideo.MatchString(r) {
			out = append(out, i)
		}
	}
	return out
}

func TestSingleClickTogglesFoldAndCopiesNothing(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)
	send(m, sealedGroup()...)
	row := rowOf(t, m, "▸ Read 2 files")

	pressAt(m, 4, row)
	assert.Contains(t, screen(m), "▸ Read 2 files", "the fold toggles on release")
	cmd := releaseAt(m, 4, row)

	assert.Nil(t, cmd)
	assert.Contains(t, screen(m), "▾ Read 2 files")
	assert.True(t, m.sel.empty())
	assert.Empty(t, reversedRows(m))
	assert.Empty(t, m.clip.(*fakeClipboard).written)
}

func TestDragSelectsAcrossLinesAndCopies(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)
	send(m, event.Notice{Text: "first\nsecond"}, event.Notice{Text: "other"})
	row := rowOf(t, m, "κ first")

	pressAt(m, 2, row)
	assert.Nil(t, moveTo(m, 5, row+1))
	cmd := releaseAt(m, 5, row+1)

	assert.Equal(t, []int{row, row + 1}, reversedRows(m))
	assert.Contains(t, screen(m), "κ first", "the text is unchanged")
	assertCopied(t, m, cmd, "first\n  sec")

	press(m, tea.KeyPressMsg{Code: 'x', Text: "x"})
	assert.Empty(t, reversedRows(m))
}

func TestDragFoldHandleDoesNotToggle(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)
	send(m, sealedGroup()...)
	row := rowOf(t, m, "▸ Read 2 files")

	pressAt(m, 2, row)
	moveTo(m, 6, row)
	cmd := releaseAt(m, 6, row)

	assert.Contains(t, screen(m), "▸ Read 2 files")
	assertCopied(t, m, cmd, "Read")
}

func TestDoubleAndTripleClickSelectWordAndLine(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)
	at := fixedClock(m)
	send(m, event.Notice{Text: "alpha beta gamma"})
	row := rowOf(t, m, "κ alpha beta gamma")
	click := func() tea.Cmd {
		pressAt(m, 9, row)
		return releaseAt(m, 9, row)
	}

	require.Nil(t, click())
	*at = at.Add(300 * time.Millisecond)
	assertCopied(t, m, click(), "beta")

	*at = at.Add(300 * time.Millisecond)
	cmd := click()
	assert.Equal(t, "κ alpha beta gamma", strings.TrimRight(m.sel.text(m.lines), " "))
	runCopy(t, cmd)

	*at = at.Add(401 * time.Millisecond)
	assert.Nil(t, click(), "a press after 400 ms is a single click again")
	assert.Empty(t, reversedRows(m))
}

func TestDragAtTopEdgeAutoScrolls(t *testing.T) {
	m, _ := newTestModel(t, 40, 20)
	notices(m, 30)
	for range 3 {
		m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	}
	top := m.top()
	require.Positive(t, top)

	pressAt(m, 3, 2)
	tick := moveTo(m, 3, 0)
	require.NotNil(t, tick, "holding the top row starts the auto-scroll")
	assert.Nil(t, moveTo(m, 4, 0), "one tick chain at a time")
	assert.Equal(t, point{top, 4}, m.sel.head)

	_, cmd := m.Update(autoScrollMsg{})
	assert.NotNil(t, cmd, "it re-arms while held at the edge")
	assert.Equal(t, top-1, m.top())
	assert.Equal(t, point{top - 1, 4}, m.sel.head)
	assert.Equal(t, point{top + 2, 3}, m.sel.anchor)

	releaseAt(m, 4, 0)
	_, cmd = m.Update(autoScrollMsg{})
	assert.Nil(t, cmd)
	assert.Equal(t, top-1, m.top(), "a tick after release does nothing")
}

func TestTranscriptDragStaysInTheTranscript(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)
	send(m, event.Notice{Text: "first"})
	typeText(m, "draft")
	row := rowOf(t, m, "κ first")
	last := m.transcriptHeight() - 1

	pressAt(m, 0, row)
	moveTo(m, 30, m.composerTop())
	cmd := releaseAt(m, 30, m.composerTop())

	assert.Equal(t, point{m.top() + last, 30}, m.sel.head, "clamped to the last transcript row")
	assert.False(t, m.composer.ta.HasSelection())
	assert.Equal(t, "draft", m.composer.value())
	assertCopied(t, m, cmd, "κ first")
}

func TestComposerDragNeverSelectsTranscript(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)
	send(m, event.Notice{Text: "first"})
	typeText(m, "hello world")
	top := m.composerTop()

	pressAt(m, 7, top)
	moveTo(m, 3, rowOf(t, m, "κ first"))
	cmd := releaseAt(m, 3, 0)

	assert.True(t, m.sel.empty())
	assert.Empty(t, reversedRows(m))
	assert.True(t, m.composer.ta.HasSelection())
	assertCopied(t, m, cmd, "hello")
}

func TestPressesOutsideTranscriptAndComposerDoNothing(t *testing.T) {
	m, _ := newTestModel(t, 60, 30)
	send(m, event.TurnStart{Prompt: "go"})
	send(m, event.QueueChanged{Pending: []string{"a"}})
	typeText(m, "draft")
	top := m.composerTop()

	for _, y := range []int{m.transcriptHeight(), top - 4, top - 2, top - 1, top + 1, len(screen(m)) - 1} {
		pressAt(m, 3, y)
		moveTo(m, 5, y)
		assert.Nil(t, releaseAt(m, 5, y), "row %d", y)
		assert.True(t, m.sel.empty(), "row %d", y)
		assert.False(t, m.composer.ta.HasSelection(), "row %d", y)
	}
	assert.Equal(t, "draft", m.composer.value())
}

func TestWheelKeepsSelectionOnItsText(t *testing.T) {
	m, _ := newTestModel(t, 40, 20)
	notices(m, 30)
	row := rowOf(t, m, "κ n25")
	pressAt(m, 2, row)
	moveTo(m, 4, row)
	releaseAt(m, 4, row)
	require.Equal(t, []int{row}, reversedRows(m))

	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})

	moved := rowOf(t, m, "κ n25")
	assert.Equal(t, row+wheelStep, moved)
	assert.Equal(t, []int{moved}, reversedRows(m))
}

func TestSelectionClearsOnResizeFoldSessionAndTheme(t *testing.T) {
	for name, clear := range map[string]func(m *model){
		"resize":     func(m *model) { m.Update(tea.WindowSizeMsg{Width: 50, Height: 30}) },
		"expand all": func(m *model) { press(m, ctrlO) },
		"session":    func(m *model) { send(m, event.SessionChanged{}) },
		"theme":      func(m *model) { m.Update(tea.BackgroundColorMsg{Color: color.White}) },
	} {
		t.Run(name, func(t *testing.T) {
			m, _ := newTestModel(t, 40, 30)
			send(m, sealedGroup()...)
			row := rowOf(t, m, "Done.")
			pressAt(m, 0, row)
			moveTo(m, 3, row)
			releaseAt(m, 3, row)
			require.NotEmpty(t, reversedRows(m))

			clear(m)

			assert.True(t, m.sel.empty())
			assert.Empty(t, reversedRows(m))
		})
	}
}
