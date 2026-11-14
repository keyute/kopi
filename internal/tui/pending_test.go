package tui

import (
	"fmt"
	"strings"
	"testing"

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
