package tui

import (
	"fmt"
	"slices"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/event"
)

var ctrlO = tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl}

// rowOf is the screen row showing text.
func rowOf(t *testing.T, m *model, text string) int {
	t.Helper()
	i := slices.Index(screen(m), text)
	require.GreaterOrEqual(t, i, 0, "%q is not on screen", text)
	return i
}

func clickRow(m *model, y int) {
	m.Update(tea.MouseClickMsg{X: 4, Y: y, Button: tea.MouseLeft})
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
	m, _ := newTestModel(t, 40, 30)
	send(m, sealedGroup()...)
	closed := []string{"", "▸ Read 2 files", "", "Done."}
	require.Equal(t, closed, body(m))

	clickText(t, m, "▸ Read 2 files")
	assert.Equal(t, []string{"", "▾ Read 2 files", "  ↳ a.go · 3 lines", "  ↳ b.go · 1 line", "", "Done."}, body(m))

	clickText(t, m, "▾ Read 2 files")
	assert.Equal(t, closed, body(m))
}

func TestClickOnMemberTogglesItsOutputOnly(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)
	send(m, sealedGroup()...)
	clickText(t, m, "▸ Read 2 files")
	open := []string{"", "▾ Read 2 files", "  ↳ a.go · 3 lines", "  ↳ b.go · 1 line", "", "Done."}

	clickText(t, m, "  ↳ a.go · 3 lines")
	assert.Equal(t, []string{"", "▾ Read 2 files", "  ↳ a.go · 3 lines", "    a1", "    a2", "  ↳ b.go · 1 line", "", "Done."}, body(m))

	clickText(t, m, "  ↳ a.go · 3 lines")
	assert.Equal(t, open, body(m))

	clickText(t, m, "  ↳ b.go · 1 line")
	assert.Equal(t, open, body(m), "a member without output stays summary-only")
}

func TestClosingThenReopeningShowsMembersCollapsed(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)
	send(m, sealedGroup()...)
	clickText(t, m, "▸ Read 2 files")
	clickText(t, m, "  ↳ a.go · 3 lines")

	clickText(t, m, "▾ Read 2 files")
	clickText(t, m, "▸ Read 2 files")

	assert.Equal(t, []string{"", "▾ Read 2 files", "  ↳ a.go · 3 lines", "  ↳ b.go · 1 line", "", "Done."}, body(m))
}

func TestLiveGroupThatSealsForgetsExpansion(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)
	send(m, start("a"), okOut("a", "3 lines", "a1"), start("b"), ok("b", "1 line"))
	clickText(t, m, "  ↳ a.go · 3 lines")
	require.Equal(t, []string{"", "• Read 2 files", "  ↳ a.go · 3 lines", "    a1", "  ↳ b.go · 1 line"}, body(m))

	send(m, event.TextDelta{Text: "Done."})
	assert.Equal(t, []string{"", "▸ Read 2 files", "", "Done."}, body(m))

	clickText(t, m, "▸ Read 2 files")
	assert.Equal(t, []string{"", "▾ Read 2 files", "  ↳ a.go · 3 lines", "  ↳ b.go · 1 line", "", "Done."}, body(m))
}

func TestClickOnSingleOkRowTogglesOutput(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)
	send(m, start("a"), okOut("a", "3 lines", "a1\na2"))

	clickText(t, m, "  ↳ 3 lines")
	assert.Equal(t, []string{"", "• a.go", "  ↳ 3 lines"}, body(m), "only the title line toggles")

	clickText(t, m, "• a.go")
	assert.Equal(t, []string{"", "• a.go", "  ↳ 3 lines", "    a1", "    a2"}, body(m))

	clickText(t, m, "• a.go")
	assert.Equal(t, []string{"", "• a.go", "  ↳ 3 lines"}, body(m))
}

func TestRunningAndFailedRowsDoNotToggle(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)
	send(m, start("a"), event.ToolEnd{CallID: "a", IsError: true, Output: "boom"}, start("b"))
	want := []string{"", "• a.go", "  boom", "", "• b.go"}
	require.Equal(t, want, body(m))

	clickText(t, m, "• a.go")
	clickText(t, m, "• b.go")

	assert.Equal(t, want, body(m))
}

func TestCtrlOShowsEverythingAndMakesClicksInert(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)
	send(m, sealedGroup()...)
	send(m, start("c"), okOut("c", "1 line", "c1"))

	press(m, ctrlO)
	all := []string{
		"", "▾ Read 2 files", "  ↳ a.go · 3 lines", "    a1", "    a2", "  ↳ b.go · 1 line",
		"", "Done.",
		"", "• c.go", "  ↳ 1 line", "    c1",
	}
	assert.Equal(t, all, body(m))

	clickText(t, m, "▾ Read 2 files")
	clickText(t, m, "• c.go")
	assert.Equal(t, all, body(m), "clicks change nothing while everything is shown")

	press(m, ctrlO)
	assert.Equal(t, []string{"", "▸ Read 2 files", "", "Done.", "", "• c.go", "  ↳ 1 line"}, body(m))
}

func TestCtrlOOffKeepsManuallyOpenGroupsButClearsExpansions(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)
	send(m, sealedGroup()...)
	clickText(t, m, "▸ Read 2 files")

	press(m, ctrlO)
	press(m, ctrlO)

	assert.Equal(t, []string{"", "▾ Read 2 files", "  ↳ a.go · 3 lines", "  ↳ b.go · 1 line", "", "Done."}, body(m))
}

func notices(m *model, n int) {
	for i := range n {
		send(m, event.Notice{Text: fmt.Sprintf("n%d", i)})
	}
}

func TestClickWhileFollowingKeepsClickedLineOnItsRow(t *testing.T) {
	m, _ := newTestModel(t, 40, 20)
	notices(m, 10)
	send(m, sealedGroup()...)
	require.True(t, m.follow)
	row := rowOf(t, m, "▸ Read 2 files")

	clickRow(m, row)

	assert.Equal(t, "▾ Read 2 files", screen(m)[row])
	assert.False(t, m.follow)
	send(m, event.Notice{Text: "later"})
	assert.Equal(t, "▾ Read 2 files", screen(m)[row], "the view no longer follows the tail")
}

func TestClicksOutsideTheTranscriptAreInert(t *testing.T) {
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
}

func TestClickAfterScrollingUpMapsToTheRightLine(t *testing.T) {
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
}
