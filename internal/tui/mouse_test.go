package tui

import (
	"fmt"
	"image/color"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
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

// reversedRows is the screen rows drawn with reverse video, leaving out the
// fields' rows, where the cursor is reverse video too.
func reversedRows(m *model) []int {
	var out []int
	for i, r := range strings.Split(m.View().Content, "\n") {
		inField := slices.ContainsFunc(m.geo.fields, func(a fieldArea) bool { return i >= a.y && i < a.y+a.h })
		if !inField && reverseVideo.MatchString(r) {
			out = append(out, i)
		}
	}
	return out
}

// dragRow drags across row y from column x to toX and returns the release's
// command.
func dragRow(m *model, x, toX, y int) tea.Cmd {
	pressAt(m, x, y)
	moveTo(m, toX, y)
	return releaseAt(m, toX, y)
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
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestModel(t, 40, 30)
		send(m, event.Notice{Text: "alpha beta gamma"})
		row := rowOf(t, m, "κ alpha beta gamma")
		click := func() tea.Cmd {
			pressAt(m, 9, row)
			return releaseAt(m, 9, row)
		}

		require.Nil(t, click())
		time.Sleep(300 * time.Millisecond)
		assertCopied(t, m, click(), "beta")

		time.Sleep(300 * time.Millisecond)
		cmd := click()
		assert.Equal(t, "κ alpha beta gamma", strings.TrimRight(m.sel.text(m.view().lines), " "))
		runCopy(t, cmd)

		time.Sleep(401 * time.Millisecond)
		assert.Nil(t, click(), "a press after 400 ms is a single click again")
		assert.Empty(t, reversedRows(m))
	})
}

func TestDragAfterMultiClickExtendsByWholeUnits(t *testing.T) {
	for _, tc := range []struct {
		name               string
		clicks             int
		x, row, toX, toRow int
		want               string
	}{
		{"words forwards", 2, 9, 0, 15, 0, "beta gamma"},
		{"words backwards", 2, 9, 0, 3, 0, "alpha beta"},
		{"lines forwards", 3, 4, 0, 5, 1, "κ alpha beta gamma\n  second"},
		{"lines backwards", 3, 4, 1, 5, 0, "κ alpha beta gamma\n  second"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, _ := newTestModel(t, 40, 30)
				send(m, event.Notice{Text: "alpha beta gamma\nsecond"})
				first := rowOf(t, m, "κ alpha beta gamma")

				for range tc.clicks {
					pressAt(m, tc.x, first+tc.row)
					time.Sleep(300 * time.Millisecond)
				}
				moveTo(m, tc.toX, first+tc.toRow)
				cmd := releaseAt(m, tc.toX, first+tc.toRow)

				assertCopied(t, m, cmd, tc.want)
			})
		})
	}
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

func TestComposerClickReturnsFocusFromBusyAgent(t *testing.T) {
	for _, submit := range []bool{false, true} {
		t.Run(fmt.Sprintf("submit %t", submit), func(t *testing.T) {
			m, c := focused(t, explorer("t1"))
			press(m, down)
			require.Equal(t, "t1", m.focus.id)
			m.composer.paste("draft")
			m.arrange()
			shown := m.shown

			pressAt(m, 7, m.composerTop())
			releaseAt(m, 7, m.composerTop())
			assert.Equal(t, agentFocus{}, m.focus)
			if submit {
				press(m, enter)
				assert.Equal(t, []string{"draft"}, c.submitted)
				assert.Equal(t, []string{shown}, c.sessions)
			} else {
				press(m, keyX)
				assert.Equal(t, "draftx", m.composer.value())
				assert.Empty(t, c.calls)
				assert.Empty(t, c.stops)
			}
			assert.Equal(t, shown, m.shown)
		})
	}
}

func TestMouseOutsideComposerKeepsAgentFocus(t *testing.T) {
	m, _ := focused(t, explorer("t1"))
	press(m, down)
	want := m.focus

	pressAt(m, 0, 0)
	releaseAt(m, 0, 0)
	assert.Equal(t, want, m.focus)
	m.Update(tea.MouseClickMsg{X: 0, Y: m.composerTop(), Button: tea.MouseRight})
	assert.Equal(t, want, m.focus)
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

func TestPressesBelowTranscriptSelectRowsAsDrawn(t *testing.T) {
	m, _ := newTestModel(t, 60, 30)
	send(m, event.TurnStart{Prompt: "go"})
	send(m, event.QueueChanged{Pending: []string{"a"}})
	typeText(m, "draft")
	top := m.composerTop()

	for _, y := range []int{m.transcriptHeight(), top - 4, top - 2, top - 1, top + 1, len(screen(m)) - 1} {
		m.clip = &fakeClipboard{}
		want := strings.TrimRight(ansi.Cut(screen(m)[y], 3, 5), " ")
		cmd := dragRow(m, 3, 5, y)
		if want == "" {
			assert.Nil(t, cmd, "row %d", y)
		} else {
			assertCopied(t, m, cmd, want)
		}
		assert.False(t, m.composer.ta.HasSelection(), "row %d", y)
	}
	assert.Equal(t, "draft", m.composer.value())
}

func TestDialogPreviewRowsSelectAndCopy(t *testing.T) {
	for _, tc := range []struct {
		name          string
		clicks, x, to int
		want          string
	}{
		{"drag", 1, 2, 7, "- old"},
		{"double click", 2, 4, 4, "old"},
		{"triple click", 3, 4, 4, "  - old"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTestModel(t, 60, 30)
			send(m, event.TurnStart{}, editApproval())
			row := rowOf(t, m, "  - old")

			for range tc.clicks - 1 {
				pressAt(m, tc.x, row)
				releaseAt(m, tc.x, row)
			}
			assertCopied(t, m, dragRow(m, tc.x, tc.to, row), tc.want)
			assert.Equal(t, []int{row}, reversedRows(m))
		})
	}
}

func TestQueuedRowAndStatusLineCopy(t *testing.T) {
	m, c := newTestModel(t, 60, 30)
	queueTwo(m, c)
	last := len(screen(m)) - 1
	pressAt(m, 4, last)
	releaseAt(m, 4, last)
	pressAt(m, 4, last)
	assertCopied(t, m, releaseAt(m, 4, last), "model-1")
	assert.True(t, m.sel.empty(), "the flash redraws the status line")

	m.clip = &fakeClipboard{}
	assertCopied(t, m, dragRow(m, 0, 3, rowOf(t, m, "❯ a")), "❯ a")
}

func TestOneLiveSelection(t *testing.T) {
	type surface struct {
		pick func(t *testing.T, m *model)
		has  func(m *model) bool
	}
	transcript := surface{
		func(t *testing.T, m *model) { dragRow(m, 2, 7, rowOf(t, m, "κ first")) },
		func(m *model) bool { return m.selIn == regionTranscript && !m.sel.empty() },
	}
	composer := surface{
		func(_ *testing.T, m *model) { dragRow(m, 2, 7, m.composerTop()) },
		func(m *model) bool { return m.composer.ta.HasSelection() },
	}
	rows := surface{
		func(t *testing.T, m *model) { dragRow(m, 0, 3, rowOf(t, m, "❯ a")) },
		func(m *model) bool { return m.selIn == regionRows && !m.sel.empty() },
	}
	reason := surface{
		func(_ *testing.T, m *model) { a := m.geo.fields[0]; dragRow(m, a.x, a.x+5, a.y) },
		func(m *model) bool { return m.frontDialog().field().ta.HasSelection() },
	}
	for _, tc := range []struct {
		name          string
		first, second surface
		approval      bool
	}{
		{"transcript then composer", transcript, composer, false},
		{"composer then transcript", composer, transcript, false},
		{"rows then composer", rows, composer, false},
		{"composer then rows", composer, rows, false},
		{"rows then reason field", rows, reason, true},
		{"reason field then rows", reason, rows, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, c := newTestModel(t, 60, 30)
			send(m, event.Notice{Text: "first"})
			queueTwo(m, c)
			typeText(m, "hello world")
			if tc.approval {
				reasonField(t, m, "alpha beta")
			}

			tc.first.pick(t, m)
			require.True(t, tc.first.has(m))
			tc.second.pick(t, m)

			assert.False(t, tc.first.has(m))
			assert.True(t, tc.second.has(m))
		})
	}
}

// reasonField opens an approval, types draft into its reason field and
// returns where the field is drawn.
func reasonField(t *testing.T, m *model, draft string) fieldArea {
	t.Helper()
	send(m, editApproval())
	press(m, digit('3'))
	keys(m, draft)
	require.Len(t, m.geo.fields, 1)
	return m.geo.fields[0]
}

func TestReasonFieldDragCopies(t *testing.T) {
	m, _ := newTestModel(t, 60, 30)
	a := reasonField(t, m, "alpha beta")
	require.True(t, strings.HasPrefix(screen(m)[a.y], "❭ 3. No (esc)  alpha beta"))

	assertCopied(t, m, dragRow(m, a.x, a.x+5, a.y), "alpha")
	assert.Equal(t, "alpha", m.frontDialog().field().ta.SelectedText())
	assert.True(t, m.sel.empty())
}

func TestReasonFieldSelectionIsReplaced(t *testing.T) {
	for name, replace := range map[string]func(m *model){
		"typing": func(m *model) { keys(m, "gamma") },
		"paste":  func(m *model) { pasteText(m, "gamma") },
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, _ := newTestModel(t, 60, 30)
				a := reasonField(t, m, "alpha beta")

				clickTimes(m, 2, a.x+7, a.y)
				releaseAt(m, a.x+7, a.y)
				require.Equal(t, "beta", m.frontDialog().field().ta.SelectedText())
				replace(m)

				assert.Equal(t, "alpha gamma", m.frontDialog().text())
			})
		})
	}
}

func TestPressOnUnselectedFieldSelectsItsOption(t *testing.T) {
	m, _ := newTestModel(t, 60, 30)
	send(m, editApproval())
	require.Len(t, m.geo.fields, 1)
	a := m.geo.fields[0]
	require.Equal(t, "  3. No (esc)  What should change?", screen(m)[a.y])

	pressAt(m, a.x, a.y)
	releaseAt(m, a.x, a.y)

	assert.Equal(t, 2, m.frontDialog().sel)
	assert.True(t, strings.HasPrefix(screen(m)[a.y], "❭ 3. No (esc)"))
	keys(m, "hi")
	assert.Equal(t, "hi", m.frontDialog().text(), "the cursor is in the field")
}

func TestApprovalEndedDuringReasonDragCopiesNothing(t *testing.T) {
	m, _ := newTestModel(t, 60, 30)
	a := reasonField(t, m, "alpha beta")
	pressAt(m, a.x, a.y)
	moveTo(m, a.x+5, a.y)

	send(m, event.ApprovalEnded{Token: 1})

	assert.Nil(t, moveTo(m, a.x+7, a.y))
	assert.Nil(t, releaseAt(m, a.x+7, a.y))
	assert.Empty(t, m.clip.(*fakeClipboard).written)
}

func TestRowSelectionClearsWhenItsRowsChange(t *testing.T) {
	m, c := newTestModel(t, 60, 30)
	queueTwo(m, c)
	row := rowOf(t, m, "❯ a")
	dragRow(m, 0, 3, row)
	require.Equal(t, []int{row}, reversedRows(m))

	send(m, event.QueueChanged{Pending: []string{"c", "b"}})

	require.Equal(t, row, rowOf(t, m, "❯ c"), "the rows keep their count")
	assert.True(t, m.sel.empty())
	assert.Empty(t, reversedRows(m))
}

func TestRowSelectionClearsWhenTheWorkingRowTakesItsPlace(t *testing.T) {
	m, _ := newTestModel(t, 40, 14)
	send(m, event.QueueChanged{Pending: []string{linesOf(8), "b"}})
	n := len(m.below)
	y := m.transcriptHeight() + 1 + len(m.pending) - busyRows
	dragRow(m, 0, 10, y)
	require.Equal(t, regionRows, m.selIn)

	send(m, event.TurnStart{Prompt: "go"})

	require.Len(t, m.below, n, "the busy rows are paid for by queue rows")
	require.True(t, strings.HasPrefix(screen(m)[y], "⠋ Working"))
	assert.True(t, m.sel.empty())
}

func TestRowSelectionSurvivesSpinnerTicks(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, m *model, c *fakeController){
		"dialog preview": func(t *testing.T, m *model, _ *fakeController) {
			send(m, event.TurnStart{}, editApproval())
			dragRow(m, 2, 7, rowOf(t, m, "  - old"))
		},
		"across the working row": func(t *testing.T, m *model, c *fakeController) {
			queueTwo(m, c)
			from := rowOf(t, m, "  ↳ queued · ctrl+enter to send now · up to edit")
			pressAt(m, 2, from)
			moveTo(m, 0, from+3)
			releaseAt(m, 0, from+3)
		},
	} {
		t.Run(name, func(t *testing.T) {
			m, c := newTestModel(t, 60, 30)
			setup(t, m, c)
			reversed := reversedRows(m)
			require.NotEmpty(t, reversed)
			working := screen(m)[m.geo.transcript+len(m.pending)+1]

			m.Update(m.spinner.Tick())

			require.NotEqual(t, working, screen(m)[m.geo.transcript+len(m.pending)+1], "the spinner moved")
			assert.Equal(t, reversed, reversedRows(m))
		})
	}
}

func TestDragStaysInItsSurface(t *testing.T) {
	t.Run("rows", func(t *testing.T) {
		m, c := newTestModel(t, 60, 30)
		send(m, event.Notice{Text: "first"})
		queueTwo(m, c)
		row := rowOf(t, m, "❯ a")

		pressAt(m, 2, row)
		moveTo(m, 1, rowOf(t, m, "κ first"))

		assert.Equal(t, regionRows, m.selIn)
		assert.Equal(t, point{0, 1}, m.sel.head, "clamped to the first lower row")
		for _, r := range reversedRows(m) {
			assert.GreaterOrEqual(t, r, m.transcriptHeight())
		}
	})
	t.Run("composer", func(t *testing.T) {
		m, _ := newTestModel(t, 40, 30)
		typeText(m, "hello world")
		last := len(screen(m)) - 1

		pressAt(m, 2, m.composerTop())
		moveTo(m, 7, last)
		cmd := releaseAt(m, 7, last)

		assert.True(t, m.sel.empty())
		assertCopied(t, m, cmd, "hello world")
	})
}

func TestWheelInvalidatesHeldFoldClickOnlyWhenViewportMoves(t *testing.T) {
	for _, scrolls := range []bool{false, true} {
		t.Run(fmt.Sprint(scrolls), func(t *testing.T) {
			m, _ := newTestModel(t, 40, 30)
			if scrolls {
				notices(m, 30)
			}
			send(m, sealedGroup()...)
			row := rowOf(t, m, "▸ Read 2 files")
			pressAt(m, 4, row)
			top := m.top()
			m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
			if scrolls {
				require.NotEqual(t, top, m.top())
				assert.Equal(t, drag{}, m.drag)
			} else {
				require.Equal(t, top, m.top())
			}
			assert.Nil(t, releaseAt(m, 4, row))
			if scrolls {
				m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
				assert.Contains(t, screen(m), "▸ Read 2 files")
			} else {
				assert.Contains(t, screen(m), "▾ Read 2 files")
			}
			assert.Empty(t, m.clip.(*fakeClipboard).written)
		})
	}
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

func TestActiveTranscriptDragClearsWhenCoordinatesChange(t *testing.T) {
	for name, invalidate := range map[string]func(*model){
		"resize":     func(m *model) { m.Update(tea.WindowSizeMsg{Width: 50, Height: 30}) },
		"theme":      func(m *model) { m.Update(tea.BackgroundColorMsg{Color: color.White}) },
		"session":    func(m *model) { send(m, event.SessionChanged{}) },
		"expand all": func(m *model) { press(m, ctrlO) },
		"fold":       func(m *model) { m.holdTop(m.view().folds.toggleAll) },
		"key":        func(m *model) { press(m, left) },
	} {
		t.Run(name, func(t *testing.T) {
			m, _ := newTestModel(t, 40, 30)
			notices(m, 30)
			pressAt(m, 2, 2)
			require.NotNil(t, moveTo(m, 6, 0))
			require.False(t, m.sel.empty())
			require.True(t, m.ticking)

			invalidate(m)

			assert.Equal(t, drag{}, m.drag)
			assert.True(t, m.sel.empty())
			top := m.top()
			_, cmd := m.Update(autoScrollMsg{})
			assert.Nil(t, cmd, "a late tick cannot restart the drag")
			assert.Equal(t, top, m.top())
			assert.False(t, m.ticking)
			assert.Nil(t, moveTo(m, 10, 1))
			assert.Nil(t, releaseAt(m, 10, 1))
			assert.True(t, m.sel.empty())
			assert.Empty(t, reversedRows(m))
			assert.Empty(t, m.clip.(*fakeClipboard).written)
		})
	}
}

func TestTranscriptDragWhenCandidatesConsumeViewport(t *testing.T) {
	for _, height := range []int{10, 12} {
		for _, clicks := range []int{2, 3} {
			t.Run(fmt.Sprintf("height%d/clicks%d", height, clicks), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					m, c := newMenuModel(t, 40, height)
					c.files = append(c.files, c.files[0])
					notices(m, 30)
					load := keys(m, "@")
					require.True(t, m.menu.open)
					require.Zero(t, m.menuHeight())
					m.scroll(-len(m.view().lines))
					require.Zero(t, m.top())
					for range clicks {
						pressAt(m, 8, 2)
					}
					require.Equal(t, clicks, m.drag.count)
					tick := moveTo(m, 10, m.transcriptHeight()-1)
					require.NotNil(t, tick)
					require.True(t, m.ticking)
					require.False(t, m.sel.empty())

					settle(m, load)

					require.Equal(t, 5, m.menuHeight())
					require.Zero(t, m.top())
					if height == 10 {
						require.Zero(t, m.transcriptHeight())
						require.NotPanics(t, func() { assert.Nil(t, moveTo(m, 12, 0)) })
						assert.Equal(t, drag{}, m.drag)
						assert.True(t, m.sel.empty())
						_, cmd := m.Update(tick())
						assert.Nil(t, cmd, "a pending tick cannot restart the drag")
						assert.False(t, m.ticking)
						assert.Zero(t, m.top())
						assert.Nil(t, releaseAt(m, 12, 0))
						assert.Empty(t, m.clip.(*fakeClipboard).written)
					} else {
						require.Positive(t, m.transcriptHeight())
						assert.Equal(t, regionTranscript, m.drag.region)
						assert.Nil(t, moveTo(m, 12, 0))
						assert.False(t, m.sel.empty())
						assert.NotNil(t, releaseAt(m, 12, 0))
						_, cmd := m.Update(tick())
						assert.Nil(t, cmd)
						assert.False(t, m.ticking)
					}
				})
			})
		}
	}
}

func TestActiveTranscriptDragClearsWhenLinesAboveChange(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)
	send(m, start("a"), okOut("a", "3 lines", "a1\na2\n"), start("b"), ok("b", "1 line"))
	row := rowOf(t, m, "  ↳ b.go · 1 line")
	pressAt(m, 4, row)
	moveTo(m, 8, row)

	send(m, event.TextDelta{Text: "Done."})

	assert.Equal(t, drag{}, m.drag)
	assert.Nil(t, moveTo(m, 10, row))
	assert.Nil(t, releaseAt(m, 10, row))
	assert.True(t, m.sel.empty())
	assert.Empty(t, m.clip.(*fakeClipboard).written)
}

func TestSelectionClearsWhenLinesAboveChange(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)
	send(m, start("a"), okOut("a", "3 lines", "a1\na2\n"), start("b"), ok("b", "1 line"))
	row := rowOf(t, m, "  ↳ b.go · 1 line")
	pressAt(m, 4, row)
	moveTo(m, 8, row)
	releaseAt(m, 8, row)
	require.Equal(t, []int{row}, reversedRows(m))

	send(m, event.TextDelta{Text: "Done."})

	assert.True(t, m.sel.empty())
	assert.Empty(t, reversedRows(m))
}

func TestSelectionSurvivesBlockAppendedBelow(t *testing.T) {
	m, _ := newTestModel(t, 40, 30)
	send(m, event.Notice{Text: "first"})
	row := rowOf(t, m, "κ first")
	pressAt(m, 2, row)
	moveTo(m, 7, row)
	releaseAt(m, 7, row)

	send(m, event.Notice{Text: "other"})

	assert.Equal(t, "first", m.sel.text(m.view().lines))
	assert.Equal(t, []int{row}, reversedRows(m))
}

func TestWheelOverAQuestionDialogScrollsItsPreview(t *testing.T) {
	var lines []string
	for i := range 20 {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	m, _ := newTestModel(t, 90, 20)
	notices(m, 30)
	send(m, event.QuestionRequested{Token: 7, Questions: previewQuestion(strings.Join(lines, "\n\n"))})
	top := m.top()
	require.Contains(t, strings.Join(screen(m), "\n"), "more lines")

	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown, X: 10, Y: m.geo.composerTop})
	assert.Contains(t, strings.Join(screen(m), "\n"), "earlier lines", "the wheel over the dialog scrolls the preview")
	assert.Equal(t, top, m.top(), "and not the transcript")

	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp, X: 10, Y: m.geo.composerTop})
	assert.NotContains(t, strings.Join(screen(m), "\n"), "earlier lines")

	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp, X: 10, Y: 0})
	assert.Equal(t, top-wheelStep, m.top(), "above the dialog it scrolls the transcript")
	assert.NotContains(t, strings.Join(screen(m), "\n"), "earlier lines")
}

func TestWheelOverAnApprovalAboveAQuestionScrollsTheTranscript(t *testing.T) {
	m, _ := newTestModel(t, 90, 20)
	notices(m, 30)
	send(m, event.QuestionRequested{Token: 7, Questions: previewQuestion("x")})
	send(m, event.ApprovalRequested{Title: "Bash"})
	require.NotNil(t, m.approval)
	top := m.top()
	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp, X: 10, Y: m.geo.composerTop})
	assert.Equal(t, top-wheelStep, m.top())
}

func TestWheelOverACutApprovalScrollsItsPreview(t *testing.T) {
	m, _ := newTestModel(t, 40, 20)
	notices(m, 30)
	send(m, longApproval(50))
	top := m.top()

	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown, X: 10, Y: m.geo.composerTop})
	assert.Equal(t, wheelStep, m.approvalDialog.scroll.off)
	assert.Equal(t, top, m.top(), "and not the transcript")

	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp, X: 10, Y: m.geo.composerTop})
	assert.Zero(t, m.approvalDialog.scroll.off)

	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp, X: 10, Y: 0})
	assert.Equal(t, top-wheelStep, m.top(), "above the dialog it scrolls the transcript")
	assert.Zero(t, m.approvalDialog.scroll.off)
}

func TestWheelOverAnApprovalWhosePreviewFitsScrollsTheTranscript(t *testing.T) {
	m, _ := newTestModel(t, 40, 20)
	notices(m, 30)
	send(m, editApproval())
	top := m.top()

	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp, X: 10, Y: m.geo.composerTop})
	assert.Equal(t, top-wheelStep, m.top())
	assert.Zero(t, m.approvalDialog.scroll.off)
}
