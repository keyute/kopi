package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/event"
)

// workingIndex is the working row's screen row with a one-line composer.
func workingIndex(m *model) int { return len(screen(m)) - 6 }

func TestPendingAreaSitsAboveWorkingRow(t *testing.T) {
	m, _ := newTestModel(t, 60, 30)
	send(m, event.TurnStart{Prompt: "go"})
	busy := m.transcriptHeight()

	send(m, event.QueueChanged{Pending: []string{"a", "b"}})

	s, w := screen(m), workingIndex(m)
	require.Equal(t, "⠋ Working… 0s", s[w])
	assert.Equal(t, []string{
		"",
		"", "❯ a", "", "  b", "",
		"",
		"  ↳ queued · ctrl+enter to send now · up to edit",
		"",
	}, s[w-9:w])
	raw := strings.Split(m.View().Content, "\n")
	for i := w - 8; i <= w-4; i++ {
		assert.Equal(t, 60, lipgloss.Width(raw[i]), "row %d is shaded full width", i)
	}
	for _, i := range []int{w - 9, w - 3, w - 1} {
		assert.Empty(t, raw[i], "row %d is unshaded", i)
	}
	assert.Len(t, s, 30)
	assert.Equal(t, busy-8, m.transcriptHeight())
	assert.Equal(t, "❯", s[m.composerTop()])

	send(m, event.QueueChanged{})
	assert.Equal(t, busy, m.transcriptHeight())
	assert.NotContains(t, strings.Join(screen(m), "\n"), "queued")
}

func TestPendingNoteNamesBoundKeys(t *testing.T) {
	for _, tc := range []struct {
		name string
		keys map[string][]string
		want string
	}{
		{"rebound", map[string][]string{"send-now": {"ctrl+s"}, "queue-edit": {"ctrl+e", "up"}}, "  ↳ queued · ctrl+s to send now · ctrl+e to edit"},
		{"no send now", map[string][]string{"queue-edit": {"up"}}, "  ↳ queued · up to edit"},
		{"no edit", map[string][]string{"send-now": {"ctrl+enter"}}, "  ↳ queued · ctrl+enter to send now"},
		{"neither", map[string][]string{}, "  ↳ queued"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := testOptions()
			opts.Keys = tc.keys
			m, _ := newTestModelWith(t, 60, 30, opts)
			send(m, event.TurnStart{Prompt: "go"}, event.QueueChanged{Pending: []string{"a"}})
			assert.Equal(t, tc.want, screen(m)[workingIndex(m)-2])
		})
	}
}

func TestSessionChangedClearsPending(t *testing.T) {
	m, _ := newTestModel(t, 60, 30)
	send(m, event.TurnStart{Prompt: "go"})
	busy := m.transcriptHeight()
	send(m, event.QueueChanged{Pending: []string{"a"}})
	require.Contains(t, screen(m), "❯ a")

	send(m, event.SessionChanged{SessionID: "s2"})

	assert.NotContains(t, screen(m), "❯ a")
	assert.Equal(t, busy, m.transcriptHeight())
}

func TestLongQueueIsCapped(t *testing.T) {
	m, _ := newTestModel(t, 60, 30)

	send(m, event.TurnStart{Prompt: "go"}, event.QueueChanged{Pending: []string{linesOf(30)}})

	s, w := screen(m), workingIndex(m)
	want := []string{"", "❯ line 0"}
	for i := 1; i < 9; i++ {
		want = append(want, fmt.Sprintf("  line %d", i))
	}
	want = append(want, "  … 21 more lines", "")
	assert.Equal(t, want, s[w-15:w-3], "the box holds a third of the height")
	assert.Len(t, s, 30)
}

func TestPendingBudgetTracksComposerGrowth(t *testing.T) {
	m, _ := newTestModel(t, 80, 24)
	send(m, event.TurnStart{Prompt: "go"}, event.QueueChanged{Pending: []string{linesOf(30)}})
	oldLines := &m.lines[0]
	for range 7 {
		press(m, tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModAlt})
		press(m, tea.KeyPressMsg{Code: 'a', Text: "a"})
		assert.Len(t, screen(m), 24)
		assert.Equal(t, 24-m.composer.height()-2, m.composerTop())
		assert.Contains(t, statusText(m), "plan")
	}
	assert.Equal(t, 8, m.composer.height())
	assert.Len(t, m.pending, 10)
	assert.Same(t, oldLines, &m.lines[0], "typing must not rebuild the transcript")
}

func TestPendingBudgetIncludesWrappedHelp(t *testing.T) {
	km := newKeyMap(testOptions().Keys)
	th := mocha()
	for _, width := range []int{20, 80} {
		for budget := 0; budget <= 20; budget++ {
			rows := pendingRows([]string{linesOf(30)}, &km, width, 8, budget, th)
			assert.LessOrEqual(t, len(rows), budget)
		}
	}
	assert.Empty(t, pendingRows([]string{"a"}, &km, 20, 8, 0, th))
	assert.Len(t, pendingRows([]string{linesOf(30)}, &km, 80, 8, 20, th), 13)
}

func TestPendingGeometryInputPaths(t *testing.T) {
	for _, path := range []string{"paste", "image", "history", "queue edit", "submit", "unsent", "deferred unsent", "cancel", "resize", "busy"} {
		t.Run(path, func(t *testing.T) {
			m, c := newTestModel(t, 80, 24)
			send(m, event.TurnStart{Prompt: "go"}, event.QueueChanged{Pending: []string{linesOf(30)}})
			switch path {
			case "paste":
				pasteText(m, linesOf(8))
			case "image":
				pasteText(m, strings.Repeat("a", 75))
				m.Update(imagePastedMsg{path: "synthetic.png"})
			case "history":
				m.composer.setHistory([]string{linesOf(8)})
				m.keys.queueEdit.SetEnabled(false)
				press(m, up)
			case "queue edit":
				c.queued = []event.Input{{Text: linesOf(8)}}
				press(m, up)
			case "submit":
				pasteText(m, linesOf(8))
				cmd := press(m, enter)
				require.NotNil(t, cmd)
				m.Update(cmd())
			case "unsent":
				send(m, event.TurnEnd{Unsent: []event.Input{{Text: linesOf(8)}}})
			case "deferred unsent":
				m.submitting = true
				send(m, event.TurnEnd{Unsent: []event.Input{{Text: linesOf(8)}}})
				m.Update(submitDoneMsg{})
			case "cancel":
				pasteText(m, linesOf(8))
				press(m, ctrlC)
			case "resize":
				m.Update(tea.WindowSizeMsg{Width: 20, Height: 24})
			case "busy":
				send(m, event.TurnEnd{})
			}
			budget := m.height - m.composer.height() - chromeRows
			if m.busy {
				budget -= busyRows
			}
			assert.Equal(t, pendingRows(m.queued, &m.keys, m.width, max(2, m.height/3), budget, m.th), m.pending)
			assert.Len(t, screen(m), m.height)
			assert.Equal(t, m.height-m.composer.height()-2, m.composerTop())
		})
	}
}

func TestFoldClickHitsTranscriptWhilePendingShown(t *testing.T) {
	m, _ := newTestModel(t, 60, 30)
	send(m, event.TurnStart{Prompt: "go"})
	for i := range 20 {
		send(m, event.Notice{Text: fmt.Sprintf("n%d", i)})
	}
	send(m, sealedGroup()...)
	send(m, event.QueueChanged{Pending: []string{"a", "b"}})

	clickText(t, m, "▸ Read 2 files")

	assert.Contains(t, screen(m), "▾ Read 2 files")
}
