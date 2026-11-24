package tui

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/event"
	"github.com/keyute/kopi/internal/transcript"
)

var ctrlO = tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl}

// rowOf is the screen row showing text.
func rowOf(t *testing.T, m *model, text string) int {
	t.Helper()
	i := slices.Index(screen(m), text)
	require.GreaterOrEqual(t, i, 0, "%q is not on screen", text)
	return i
}

// clickRow is a left press and release at row y, a second after the previous
// press so it is never a double click. It must run in a synctest bubble.
func clickRow(m *model, y int) {
	time.Sleep(time.Second)
	m.Update(tea.MouseClickMsg{X: 4, Y: y, Button: tea.MouseLeft})
	m.Update(tea.MouseReleaseMsg{X: 4, Y: y})
}

func clickText(t *testing.T, m *model, text string) {
	t.Helper()
	clickRow(m, rowOf(t, m, text))
}

// sealedGroup is a group of two reads, a with output and b without, sealed
// by the reply after it.
func sealedGroup() []event.Event {
	return []event.Event{
		start("a"), okOut("a", "3 lines", "a1\na2\n"), start("b"), ok("b", "1 line"),
		event.TextDelta{Text: "Done."},
	}
}

func TestClickOpensAndClosesSealedGroup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestModel(t, 40, 30)
		send(m, sealedGroup()...)
		closed := []string{"", "▸ Read 2 files", "", "Done."}
		require.Equal(t, closed, body(m))

		clickText(t, m, "▸ Read 2 files")
		assert.Equal(t, []string{"", "▾ Read 2 files", "  ↳ a.go · 3 lines", "  ↳ b.go · 1 line", "", "Done."}, body(m))

		clickText(t, m, "▾ Read 2 files")
		assert.Equal(t, closed, body(m))
	})
}

func TestClickOnMemberTogglesItsOutputOnly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestModel(t, 40, 30)
		send(m, sealedGroup()...)
		clickText(t, m, "▸ Read 2 files")
		open := []string{"", "▾ Read 2 files", "  ↳ a.go · 3 lines", "  ↳ b.go · 1 line", "", "Done."}

		clickText(t, m, "  ↳ a.go · 3 lines")
		assert.Equal(t, []string{"", "▾ Read 2 files", "  ↳ a.go · 3 lines", "        a1", "        a2", "  ↳ b.go · 1 line", "", "Done."}, body(m))

		clickText(t, m, "  ↳ a.go · 3 lines")
		assert.Equal(t, open, body(m))

		clickText(t, m, "  ↳ b.go · 1 line")
		assert.Equal(t, open, body(m), "a member without output stays summary-only")
	})
}

func TestClosingThenReopeningShowsMembersCollapsed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestModel(t, 40, 30)
		send(m, sealedGroup()...)
		clickText(t, m, "▸ Read 2 files")
		clickText(t, m, "  ↳ a.go · 3 lines")

		clickText(t, m, "▾ Read 2 files")
		clickText(t, m, "▸ Read 2 files")

		assert.Equal(t, []string{"", "▾ Read 2 files", "  ↳ a.go · 3 lines", "  ↳ b.go · 1 line", "", "Done."}, body(m))
	})
}

func TestLiveGroupThatSealsForgetsExpansion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestModel(t, 40, 30)
		send(m, start("a"), okOut("a", "3 lines", "a1"), start("b"), ok("b", "1 line"))
		clickText(t, m, "  ↳ a.go · 3 lines")
		require.Equal(t, []string{"", "• Read 2 files", "  ↳ a.go · 3 lines", "        a1", "  ↳ b.go · 1 line"}, body(m))

		send(m, event.TextDelta{Text: "Done."})
		assert.Equal(t, []string{"", "▸ Read 2 files", "", "Done."}, body(m))

		clickText(t, m, "▸ Read 2 files")
		assert.Equal(t, []string{"", "▾ Read 2 files", "  ↳ a.go · 3 lines", "  ↳ b.go · 1 line", "", "Done."}, body(m))
	})
}

func TestClickOnSingleOkRowTogglesOutput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestModel(t, 40, 30)
		send(m, start("a"), okOut("a", "3 lines", "a1\na2"))

		clickText(t, m, "  ↳ 3 lines")
		assert.Equal(t, []string{"", "• a.go", "  ↳ 3 lines"}, body(m), "only the title line toggles")

		clickText(t, m, "• a.go")
		assert.Equal(t, []string{"", "• a.go", "  ↳ 3 lines", "      a1", "      a2"}, body(m))

		clickText(t, m, "• a.go")
		assert.Equal(t, []string{"", "• a.go", "  ↳ 3 lines"}, body(m))
	})
}

func TestRunningAndBodylessRowsDoNotToggle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestModel(t, 40, 30)
		send(m, start("a"), ok("a", "no output"), event.TextDelta{Text: "x"}, start("b"))
		want := []string{"", "• a.go", "  ↳ no output", "", "x", "", "• b.go"}
		require.Equal(t, want, body(m))

		clickText(t, m, "• a.go")
		clickText(t, m, "• b.go")

		assert.Equal(t, want, body(m))
	})
}

func TestOneLineFailureIsInert(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestModel(t, 40, 30)
		send(m, start("a"), event.ToolEnd{CallID: "a", IsError: true, Summary: "boom", Output: "boom"})
		want := []string{"", "• a.go", "  ↳ boom"}
		require.Equal(t, want, body(m))

		clickText(t, m, "• a.go")

		assert.Equal(t, want, body(m))
	})
}

func TestLongFailureOpensByClickAndByCtrlO(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestModel(t, 40, 40)
		var lines []string
		for i := 1; i <= 12; i++ {
			lines = append(lines, fmt.Sprintf("l%d", i))
		}
		out := strings.Join(lines, "\n")
		send(m, start("a"), event.ToolEnd{CallID: "a", IsError: true, Summary: "exit code 1 · 12 lines", Output: out, Body: out})
		collapsed := []string{"", "• a.go", "  ↳ exit code 1 · 12 lines", "      l1", "      l2", "      … 8 more lines", "      l11", "      l12"}
		require.Equal(t, collapsed, body(m))

		clickText(t, m, "  ↳ exit code 1 · 12 lines")
		assert.Equal(t, collapsed, body(m), "only the title line toggles")

		clickText(t, m, "• a.go")
		open := []string{"", "• a.go", "  ↳ exit code 1 · 12 lines"}
		for _, l := range lines {
			open = append(open, "      "+l)
		}
		assert.Equal(t, open, body(m))

		clickText(t, m, "• a.go")
		assert.Equal(t, collapsed, body(m))

		press(m, ctrlO)
		assert.Equal(t, open, body(m))
	})
}

func TestCtrlOShowsEverythingAndMakesClicksInert(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestModel(t, 40, 30)
		send(m, sealedGroup()...)
		send(m, start("c"), okOut("c", "1 line", "c1"))

		press(m, ctrlO)
		all := []string{
			"", "▾ Read 2 files", "  ↳ a.go · 3 lines", "        a1", "        a2", "  ↳ b.go · 1 line",
			"", "Done.",
			"", "• c.go", "  ↳ 1 line", "      c1",
		}
		assert.Equal(t, all, body(m))

		clickText(t, m, "▾ Read 2 files")
		clickText(t, m, "• c.go")
		assert.Equal(t, all, body(m), "clicks change nothing while everything is shown")

		press(m, ctrlO)
		assert.Equal(t, []string{"", "▸ Read 2 files", "", "Done.", "", "• c.go", "  ↳ 1 line"}, body(m))
	})
}

func TestCtrlOOffKeepsManuallyOpenGroupsButClearsExpansions(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestModel(t, 40, 30)
		send(m, sealedGroup()...)
		clickText(t, m, "▸ Read 2 files")

		press(m, ctrlO)
		press(m, ctrlO)

		assert.Equal(t, []string{"", "▾ Read 2 files", "  ↳ a.go · 3 lines", "  ↳ b.go · 1 line", "", "Done."}, body(m))
	})
}

func notices(m *model, n int) {
	for i := range n {
		send(m, event.Notice{Text: fmt.Sprintf("n%d", i)})
	}
}

func TestClickWhileFollowingKeepsClickedLineOnItsRow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestModel(t, 40, 20)
		notices(m, 10)
		send(m, sealedGroup()...)
		require.True(t, m.view().follow)
		row := rowOf(t, m, "▸ Read 2 files")

		clickRow(m, row)

		assert.Equal(t, "▾ Read 2 files", screen(m)[row])
		assert.False(t, m.view().follow)
		send(m, event.Notice{Text: "later"})
		assert.Equal(t, "▾ Read 2 files", screen(m)[row], "the view no longer follows the tail")
	})
}

func TestClicksOutsideTheTranscriptAreInert(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestModel(t, 40, 30)
		send(m, sealedGroup()...)
		want := body(m)
		rows := len(screen(m))

		for y := m.transcriptHeight(); y < rows; y++ {
			clickRow(m, y)
		}
		clickRow(m, rowOf(t, m, "▸ Read 2 files")-1)
		m.Update(tea.MouseClickMsg{Y: rowOf(t, m, "▸ Read 2 files"), Button: tea.MouseRight})

		assert.Equal(t, want, body(m))
	})
}

func TestClickAfterScrollingUpMapsToTheRightLine(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestModel(t, 40, 20)
		send(m, sealedGroup()...)
		notices(m, 20)
		require.NotContains(t, screen(m), "▸ Read 2 files")

		for !slices.Contains(screen(m), "▸ Read 2 files") {
			m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
		}
		clickText(t, m, "▸ Read 2 files")

		assert.Contains(t, screen(m), "▾ Read 2 files")
		assert.Contains(t, screen(m), "  ↳ a.go · 3 lines")
	})
}

func TestFoldsOfARowHaveStateOnceItIsNotRunning(t *testing.T) {
	f := newFolds()
	f.expanded["a"] = true
	for state, want := range map[transcript.State]bool{
		transcript.Running: false, transcript.OK: true, transcript.Failed: true, transcript.Unfinished: true,
		transcript.Denied: true, transcript.Rejected: true,
	} {
		got := f.of(transcript.Row{Call: transcript.Call{ID: "a", State: state}})
		if want {
			assert.Equal(t, []bool{true}, got.expanded, "state %d", state)
		} else {
			assert.Empty(t, got.expanded, "state %d", state)
		}
	}
}

func TestTaskResultFoldTogglesByClickAndCtrlO(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestModel(t, 60, 30)
		send(m, workflowEnd("completed", 1, 0, "", "result"))
		closed := []string{"", "• workflow finished · audit route handlers · 6m 3s", "  ↳ 1 agent"}
		require.Equal(t, closed, body(m))

		press(m, ctrlO)
		assert.Equal(t, append(closed, "      result"), body(m))
		press(m, ctrlO)
		assert.Equal(t, closed, body(m))

		clickText(t, m, "• workflow finished · audit route handlers · 6m 3s")
		assert.Equal(t, append(closed, "      result"), body(m))
	})
}

func TestTaskWithoutAResultHasNoFold(t *testing.T) {
	f := newFolds()
	f.all = true
	assert.Equal(t, blockFolds{}, f.of(transcript.Task{}))
	assert.Equal(t, blockFolds{}, f.of(transcript.Task{Result: " \n"}))
}

func TestUserCommandOutputOpensByClickAndCtrlO(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var out []string
		for i := 1; i <= 12; i++ {
			out = append(out, fmt.Sprintf("l%d", i))
		}
		m, _ := newTestModel(t, 40, 40)
		send(m, event.UserCommand{Command: "seq", State: event.CommandExited, Exit: "exit 0", Elapsed: 2 * time.Second, Output: strings.Join(out, "\n")})
		hint := "  ↳ exit 0 · 2s"
		assert.Contains(t, body(m), "      … 2 earlier lines")
		assert.NotContains(t, body(m), "      l1")

		clickText(t, m, hint)
		assert.Contains(t, body(m), "      l1")
		assert.NotContains(t, body(m), "      … 2 earlier lines")

		clickText(t, m, hint)
		assert.NotContains(t, body(m), "      l1")

		press(m, ctrlO)
		assert.Contains(t, body(m), "      l1")
	})
}
