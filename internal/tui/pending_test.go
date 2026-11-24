package tui

import (
	"fmt"
	"image/color"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/skill"
)

// workingIndex is the working row's screen row with a one-line composer.
func workingIndex(m *model) int { return len(screen(m)) - 6 }

func TestPendingAreaSitsAboveWorkingRow(t *testing.T) {
	m, _ := newTestModel(t, 60, 30)
	send(m, event.TurnStart{Prompt: "go"})
	busy := m.transcriptHeight()

	send(m, event.QueueChanged{Pending: []event.Queued{{Text: "a"}, {Text: "b"}}})

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
			send(m, event.TurnStart{Prompt: "go"}, event.QueueChanged{Pending: []event.Queued{{Text: "a"}}})
			assert.Equal(t, tc.want, screen(m)[workingIndex(m)-2])
		})
	}
}

func TestQueueEditIgnoresQueuedCommands(t *testing.T) {
	m, c := newTestModel(t, 60, 30)
	c.history = []string{"older"}
	m.composer.setHistory(c.History())
	send(m, event.TurnStart{Prompt: "go"}, event.QueueChanged{Pending: []event.Queued{{Command: &event.UserCommand{Command: "ls", State: event.CommandQueued}}}})

	press(m, up)

	assert.Zero(t, c.takes)
	assert.Equal(t, "older", m.composer.value())
}

func TestSessionChangedClearsPending(t *testing.T) {
	m, _ := newTestModel(t, 60, 30)
	send(m, event.TurnStart{Prompt: "go"})
	busy := m.transcriptHeight()
	send(m, event.QueueChanged{Pending: []event.Queued{{Text: "a"}}})
	require.Contains(t, screen(m), "❯ a")

	send(m, event.SessionChanged{SessionID: "s2"})

	assert.NotContains(t, screen(m), "❯ a")
	assert.Equal(t, busy, m.transcriptHeight())
}

func TestLongQueueIsCapped(t *testing.T) {
	m, _ := newTestModel(t, 60, 30)

	send(m, event.TurnStart{Prompt: "go"}, event.QueueChanged{Pending: []event.Queued{{Text: linesOf(30)}}})

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
	send(m, event.TurnStart{Prompt: "go"}, event.QueueChanged{Pending: []event.Queued{{Text: linesOf(30)}}})
	oldLines := &m.view().lines[0]
	for range 7 {
		press(m, tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModAlt})
		press(m, tea.KeyPressMsg{Code: 'a', Text: "a"})
		assert.Len(t, screen(m), 24)
		assert.Equal(t, 24-m.composer.height()-2, m.composerTop())
		assert.Contains(t, statusText(m), "plan")
	}
	assert.Equal(t, 8, m.composer.height())
	assert.Len(t, m.pending, 10)
	assert.Same(t, oldLines, &m.view().lines[0], "typing must not rebuild the transcript")
}

func TestPendingBudgetIncludesWrappedHelp(t *testing.T) {
	km := newKeyMap(testOptions().Keys)
	th := mocha()
	for _, width := range []int{20, 80} {
		for budget := 0; budget <= 20; budget++ {
			rows := pendingRows([]event.Queued{{Text: linesOf(30)}}, "", nil, &km, width, 8, budget, th)
			assert.LessOrEqual(t, len(rows), budget)
		}
	}
	assert.Empty(t, pendingRows([]event.Queued{{Text: "a"}}, "", nil, &km, 20, 8, 0, th))
	assert.Len(t, pendingRows([]event.Queued{{Text: linesOf(30)}}, "", nil, &km, 80, 8, 20, th), 13)
}

func TestPendingGeometryInputPaths(t *testing.T) {
	for _, path := range []string{"paste", "image", "history", "queue edit", "submit", "unsent", "cancel", "resize", "busy"} {
		t.Run(path, func(t *testing.T) {
			m, c := newTestModel(t, 80, 24)
			send(m, event.TurnStart{Prompt: "go"}, event.QueueChanged{Pending: []event.Queued{{Text: linesOf(30)}}})
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
				press(m, enter)
			case "unsent":
				send(m, event.TurnEnd{Unsent: []event.Input{{Text: linesOf(8)}}})
			case "cancel":
				pasteText(m, linesOf(8))
				press(m, ctrlC)
			case "resize":
				m.Update(tea.WindowSizeMsg{Width: 20, Height: 24})
			case "busy":
				send(m, event.TurnEnd{})
			}
			budget := m.height - m.composer.height() - chromeRows
			if m.view().busy {
				budget -= busyRows
			}
			assert.Equal(t, pendingRows(m.view().queued, "", nil, &m.keys, m.width, max(2, m.height/3), budget, m.th), m.pending)
			assert.Len(t, screen(m), m.height)
			assert.Equal(t, m.height-m.composer.height()-2, m.composerTop())
		})
	}
}

func TestPendingCacheReusesRenderedQueue(t *testing.T) {
	m, _ := newTestModel(t, 80, 30)
	send(m, event.TurnStart{Prompt: "go"}, event.QueueChanged{Pending: []event.Queued{{Text: linesOf(100)}}})
	box, note := &m.pendingCache.box[0], &m.pendingCache.note[0]
	for name, update := range map[string]func(){
		"typing":          func() { press(m, tea.KeyPressMsg{Code: 'a', Text: "a"}) },
		"unrelated event": func() { send(m, event.Notice{Text: "notice"}) },
		"same content":    func() { send(m, event.QueueChanged{Pending: []event.Queued{{Text: linesOf(100)}}}) },
		"height":          func() { m.Update(tea.WindowSizeMsg{Width: 80, Height: 24}) },
		"composer growth": func() { press(m, tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModAlt}) },
	} {
		t.Run(name, func(t *testing.T) {
			update()
			assert.Same(t, box, &m.pendingCache.box[0])
			assert.Same(t, note, &m.pendingCache.note[0])
			budget := m.height - m.composer.height() - chromeRows - busyRows
			assert.Equal(t, pendingRows(m.view().queued, "", nil, &m.keys, m.width, max(2, m.height/3), budget, m.th), m.pending)
		})
	}
}

func TestPendingCacheInvalidatesRenderedQueue(t *testing.T) {
	for name, update := range map[string]func(*model){
		"content":         func(m *model) { send(m, event.QueueChanged{Pending: []event.Queued{{Text: "changed"}}}) },
		"mutated content": func(m *model) { m.view().queued[0].Text = "changed"; m.arrange() },
		"width":           func(m *model) { m.Update(tea.WindowSizeMsg{Width: 40, Height: 30}) },
		"theme":           func(m *model) { m.Update(tea.BackgroundColorMsg{Color: color.White}) },
	} {
		t.Run(name, func(t *testing.T) {
			m, _ := newTestModel(t, 80, 30)
			send(m, event.QueueChanged{Pending: []event.Queued{{Text: linesOf(100)}}})
			box, note := &m.pendingCache.box[0], &m.pendingCache.note[0]

			update(m)

			assert.NotSame(t, box, &m.pendingCache.box[0])
			assert.NotSame(t, note, &m.pendingCache.note[0])
			budget := m.height - m.composer.height() - chromeRows
			assert.Equal(t, pendingRows(m.view().queued, "", nil, &m.keys, m.width, max(2, m.height/3), budget, m.th), m.pending)
			send(m, event.QueueChanged{})
			assert.Empty(t, m.pending)
			assert.Empty(t, m.pendingCache.box)
		})
	}
}

func TestPendingCacheBudgetDoesNotAlterFullBox(t *testing.T) {
	var c pendingCache
	km, th := newKeyMap(testOptions().Keys), mocha()
	queued := []event.Queued{{Text: linesOf(30)}}
	full := c.rows(queued, "", nil, &km, 80, 100, 100, th)
	box := append([]string(nil), c.box...)
	for budget := 0; budget < 40; budget++ {
		assert.LessOrEqual(t, len(c.rows(queued, "", nil, &km, 80, 8, budget, th)), budget)
		assert.Equal(t, box, c.box)
	}
	assert.Equal(t, full, c.rows(queued, "", nil, &km, 80, 100, 100, th))
}

func TestFoldClickHitsTranscriptWhilePendingShown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestModel(t, 60, 30)
		send(m, event.TurnStart{Prompt: "go"})
		for i := range 20 {
			send(m, event.Notice{Text: fmt.Sprintf("n%d", i)})
		}
		send(m, sealedGroup()...)
		send(m, event.QueueChanged{Pending: []event.Queued{{Text: "a"}, {Text: "b"}}})

		clickText(t, m, "▸ Read 2 files")

		assert.Contains(t, screen(m), "▾ Read 2 files")
	})
}

func TestPendingRowsStyleSkillTokens(t *testing.T) {
	km, th := newKeyMap(testOptions().Keys), mocha()
	set, _ := skill.NewSet("skill", []skill.Skill{{Name: "a"}}, nil)
	spans := func(text string) []app.Span { return app.Spans(text, set, false, false) }

	rows := pendingRows([]event.Queued{{Text: "run /skill:a"}}, "", spans, &km, 40, 8, 20, th)

	assert.Contains(t, rows[1], spanStyle(th, app.KindCommand).Render("/skill:a"))
}

func queuedCommand(command string, state event.CommandState) event.Queued {
	return event.Queued{Command: &event.UserCommand{Command: command, State: state, Exit: "exit 0", Elapsed: 12 * time.Second}}
}

// pendingText is the steering section as drawn, trimmed.
func pendingText(m *model) []string {
	out := make([]string, len(m.pending))
	for i, r := range m.pending {
		out[i] = strings.TrimRight(ansi.Strip(r), " ")
	}
	return out
}

func TestPendingDrawsTheQueueInOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := newTestModel(t, 60, 100)
		c.commandOutput = "tail"
		running := queuedCommand("sleep 9", event.CommandRunning)
		running.Command.Started = time.Now()
		send(m, event.QueueChanged{Pending: []event.Queued{
			queuedCommand("ls", event.CommandExited), {Text: "a"}, {Text: "b"}, running, queuedCommand("pwd", event.CommandQueued),
		}})

		assert.Equal(t, []string{
			"! ls", "", "",
			"  ↳ exit 0 · 12s · unread",
			"", "",
			"❯ a", "", "  b", "",
			"",
			"  ↳ queued · ctrl+enter to send now · up to edit",
			"", "",
			"! sleep 9", "", "",
			"  ↳ running · ctrl+b to background · esc to stop",
			"    tail",
			"", "",
			"! pwd", "", "",
			"  ↳ queued",
			"",
		}, pendingText(m)[1:])
	})
}

func TestPendingNoteIsAbsentWithoutQueuedPrompts(t *testing.T) {
	m, _ := newTestModel(t, 60, 30)
	send(m, event.QueueChanged{Pending: []event.Queued{queuedCommand("ls", event.CommandQueued)}})

	assert.NotContains(t, strings.Join(pendingText(m), "\n"), "to send now")
	assert.Equal(t, "", pendingText(m)[len(m.pending)-1])
}

func TestPendingRunningCommandShowsClockTailAndStopKey(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := newTestModel(t, 60, 100)
		c.commandOutput = linesOf(8)
		running := queuedCommand("make", event.CommandRunning)
		running.Command.Started = time.Now()
		send(m, event.QueueChanged{Pending: []event.Queued{running}})
		s := strings.Join(pendingText(m), "\n")
		assert.Contains(t, s, "↳ running · ctrl+b to background · esc to stop")
		assert.Contains(t, s, "… 3 earlier lines\n    line 3\n    line 4\n    line 5\n    line 6\n    line 7")

		time.Sleep(12 * time.Second)
		m.arrange()
		assert.Contains(t, strings.Join(pendingText(m), "\n"), "↳ running 12s · ctrl+b to background")

		send(m, event.TurnStart{Prompt: "go"})
		s = strings.Join(pendingText(m), "\n")
		assert.Contains(t, s, "↳ running 12s · ctrl+b to background\n")
		assert.NotContains(t, s, "esc to stop")
	})
}

func TestTickWantedWhileACommandRuns(t *testing.T) {
	m, _ := newTestModel(t, 60, 30)
	require.False(t, m.tickWanted())

	_, cmd := m.Update(eventsMsg{event.QueueChanged{Pending: []event.Queued{queuedCommand("make", event.CommandRunning)}}})
	assert.True(t, m.tickWanted())
	assert.NotNil(t, cmd)

	send(m, event.QueueChanged{Pending: []event.Queued{queuedCommand("make", event.CommandQueued)}})
	assert.False(t, m.tickWanted())
	send(m, event.QueueChanged{})
	assert.False(t, m.tickWanted())
}

func TestPendingCapEndsWithMoreLines(t *testing.T) {
	m, _ := newTestModel(t, 60, 30)
	var q []event.Queued
	for range 4 {
		q = append(q, queuedCommand("ls", event.CommandExited))
	}
	q = append(q, event.Queued{Text: "tail prompt"})
	send(m, event.TurnStart{Prompt: "go"}, event.QueueChanged{Pending: q})

	rows := pendingText(m)
	require.Len(t, rows, 10+2+2+1, "a third of the height, its two shaded rows, the note and the trailing row")
	assert.Equal(t, "  … 16 more lines", rows[len(rows)-2])
	assert.Empty(t, rows[len(rows)-1])
	assert.Len(t, screen(m), 30)
	assert.Equal(t, "❯", screen(m)[m.composerTop()])
}

func TestPendingCapInsideLastBoxKeepsTheNote(t *testing.T) {
	m, _ := newTestModel(t, 60, 30)
	send(m, event.TurnStart{Prompt: "go"}, event.QueueChanged{Pending: []event.Queued{{Text: linesOf(30)}, queuedCommand("ls", event.CommandQueued)}})

	rows := pendingText(m)
	require.Len(t, rows, 10+2+2+1)
	assert.Equal(t, []string{"  … 27 more lines", "", "", "  ↳ queued · ctrl+enter to send now · up to edit", ""}, rows[len(rows)-5:])
}
