package tui

import (
	"errors"
	"testing"
	"testing/synctest"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/event"
)

var tab = tea.KeyPressMsg{Code: tea.KeyTab}

func newMenuModel(t *testing.T, width, height int) (*model, *fakeController) {
	t.Helper()
	m, c := newTestModel(t, width, height)
	c.commands = []app.Command{
		{Name: "plan", Summary: "switch to plan mode", Argument: true, Optional: true},
		{Name: "execute", Summary: "switch to execute mode"},
		{Name: "status", Summary: "show status"},
		{Name: "new", Summary: "start a new session"},
		{Name: "resume", Summary: "resume a session", Argument: true},
		{Name: "help", Summary: "list commands"},
		{Name: "quit", Summary: "exit kopi"},
	}
	c.args = map[string][]app.Candidate{"plan": {{Value: "open", Description: "edit the plan file"}}, "resume": {
		{Value: "abc123", Label: "2h ago", Description: "fix the bug"},
		{Value: "def456", Label: "1d ago", Description: "add tests"},
	}}
	c.files = []app.Candidate{{Value: "a.go"}, {Value: "dir/"}, {Value: "dir/b.go"}, {Value: "my file.txt"}}
	return m, c
}

// keys presses each rune of s through Update and returns their commands.
func keys(m *model, s string) tea.Cmd {
	var cmds []tea.Cmd
	for _, r := range s {
		cmds = append(cmds, press(m, tea.KeyPressMsg{Code: r, Text: string(r)}))
	}
	return tea.Batch(cmds...)
}

// settle runs cmd and feeds the completions it loads back in, dropping every
// other message. Run it inside synctest, where the cursor's blink timer fires
// at once.
func settle(m *model, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		for _, c := range msg {
			settle(m, c)
		}
	case candidatesMsg:
		_, next := m.Update(msg)
		settle(m, next)
	}
}

// menuText is the screen rows the menu takes, between the composer and the
// status line.
func menuText(m *model) []string {
	s := screen(m)
	return s[len(s)-1-m.menuHeight() : len(s)-1]
}

func TestSlashListsCommandsFiltersAndWraps(t *testing.T) {
	m, _ := newMenuModel(t, 60, 30)

	keys(m, "/")
	assert.Equal(t, []string{
		"❭ /plan       switch to plan mode",
		"  /execute    switch to execute mode",
		"  /status     show status",
		"  /new        start a new session",
		"  /resume     resume a session",
		"  1 of 7",
	}, menuText(m))
	assert.Equal(t, "❯ /", screen(m)[len(screen(m))-9], "the menu is below the composer")

	press(m, down)
	assert.Equal(t, "❭ /execute    switch to execute mode", menuText(m)[1])
	assert.Equal(t, "  2 of 7", menuText(m)[5], "the footer follows the selection")
	press(m, up)
	press(m, up)
	assert.Equal(t, []string{
		"  /status     show status",
		"  /new        start a new session",
		"  /resume     resume a session",
		"  /help       list commands",
		"❭ /quit       exit kopi",
		"  7 of 7",
	}, menuText(m), "up from the first wraps to the last")
	press(m, down)
	assert.Equal(t, "❭ /plan       switch to plan mode", menuText(m)[0], "down from the last wraps to the first")

	keys(m, "re")
	assert.Equal(t, []string{"❭ /resume     resume a session"}, menuText(m))
	assert.Equal(t, "/re", m.composer.value(), "menu keys never reach the draft")
}

func TestMenuTruncatesAndDropsDescriptionsWhenNarrow(t *testing.T) {
	m, _ := newMenuModel(t, 24, 30)
	keys(m, "/")
	assert.Equal(t, "❭ /plan       switch to…", menuText(m)[0])

	m.Update(tea.WindowSizeMsg{Width: 20, Height: 30})
	assert.Equal(t, []string{"❭ /plan", "  /execute", "  /status", "  /new", "  /resume", "  1 of 7"}, menuText(m))
}

func TestTabFillsCommandThenListsItsArguments(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := newMenuModel(t, 60, 30)
		keys(m, "/res")

		settle(m, press(m, tab))

		assert.Equal(t, "/resume ", m.composer.value())
		assert.Equal(t, []string{"❭ 2h ago      fix the bug", "  1d ago      add tests"}, menuText(m))

		press(m, down)
		settle(m, press(m, enter))
		assert.Equal(t, []string{"/resume def456"}, c.submitted)
		assert.Empty(t, m.composer.value())
		assert.Zero(t, m.menuHeight())
	})
}

func TestApprovalHidesMenuUntilItEnds(t *testing.T) {
	m, _ := newMenuModel(t, 60, 30)
	keys(m, "/")
	require.Positive(t, m.menuHeight())

	send(m, editApproval())
	assert.Zero(t, m.menuHeight())
	assert.Empty(t, menuText(m))
	assert.NotContains(t, screen(m), "❭ /plan       switch to plan mode")
	assert.Contains(t, screen(m), "❭ 1. Yes")

	send(m, event.ApprovalEnded{Token: 1})
	assert.Equal(t, "❭ /plan       switch to plan mode", menuText(m)[0])
}

func TestEnterOnCommandRunsIt(t *testing.T) {
	m, c := newMenuModel(t, 60, 30)
	keys(m, "/pl")

	press(m, enter)

	assert.Equal(t, []string{"/plan"}, c.submitted)
	assert.Empty(t, m.composer.value())
	assert.Zero(t, m.menuHeight())
}

func TestEnterOnCommandWithArgumentFillsItIn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := newMenuModel(t, 60, 30)
		keys(m, "/res")

		settle(m, press(m, enter))

		assert.Empty(t, c.submitted)
		assert.Equal(t, "/resume ", m.composer.value())
		assert.Equal(t, []string{"❭ 2h ago      fix the bug", "  1d ago      add tests"}, menuText(m))
	})
}

func TestNoMenuWhereNothingCompletes(t *testing.T) {
	for _, text := range []string{"/execute ", "//x", "hi\n/pl", "a@b", "hi"} {
		t.Run(text, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, c := newMenuModel(t, 60, 30)
				_, cmd := m.Update(tea.PasteMsg{Content: text})
				settle(m, cmd)

				assert.Zero(t, m.menuHeight())
				press(m, enter)
				assert.Equal(t, []string{text}, c.submitted)
			})
		})
	}
}

func TestOptionalArgumentMenu(t *testing.T) {
	run := func(t *testing.T, draft string) (*model, *fakeController) {
		m, c := newMenuModel(t, 60, 30)
		settle(m, keys(m, draft))
		return m, c
	}
	synctest.Test(t, func(t *testing.T) {
		m, c := run(t, "/pl")
		settle(m, press(m, tab))
		assert.Equal(t, "/plan ", m.composer.value())
		assert.Equal(t, []string{"  open        edit the plan file"}, menuText(m), "unlit")
		press(m, enter)
		assert.Equal(t, []string{"/plan "}, c.submitted, "enter sends the draft as typed")

		m, c = run(t, "/plan ")
		press(m, down)
		assert.Equal(t, []string{"❭ open        edit the plan file"}, menuText(m))
		press(m, enter)
		assert.Equal(t, []string{"/plan open"}, c.submitted)

		m, c = run(t, "/plan o")
		assert.Equal(t, []string{"❭ open        edit the plan file"}, menuText(m))
		press(m, enter)
		assert.Equal(t, []string{"/plan open"}, c.submitted)

		m, c = run(t, "/plan ")
		press(m, down)
		settle(m, keys(m, "o"))
		settle(m, press(m, backspace))
		assert.Equal(t, "/plan ", m.composer.value())
		assert.Equal(t, []string{"  open        edit the plan file"}, menuText(m), "an edit unlights the row")
		press(m, enter)
		assert.Equal(t, []string{"/plan "}, c.submitted)

		m, c = run(t, "/plan open")
		assert.Equal(t, []string{"  open        edit the plan file"}, menuText(m), "typed in full")
		press(m, tab)
		assert.Equal(t, []string{"❭ open        edit the plan file"}, menuText(m), "tab lights the row")
		assert.Equal(t, "/plan open", m.composer.value())
		assert.Empty(t, c.submitted)
	})
}

func TestEnterSendsAFullArgumentWithTheCursorBeforeIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := newMenuModel(t, 60, 30)
		settle(m, keys(m, "/resume def456"))
		for range len("def456") {
			press(m, left)
		}
		assert.Equal(t, []string{"  2h ago      fix the bug", "  1d ago      add tests"}, menuText(m), "unlit")

		press(m, enter)
		assert.Equal(t, []string{"/resume def456"}, c.submitted)
	})
}

func TestForwardDeleteToAFullArgumentUnlightsTheMenu(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := newMenuModel(t, 60, 30)
		settle(m, keys(m, "/resume ddef456"))
		for range len("ddef456") {
			press(m, left)
		}
		press(m, down)
		require.Equal(t, []string{"  2h ago      fix the bug", "❭ 1d ago      add tests"}, menuText(m))

		settle(m, press(m, del))
		assert.Equal(t, []string{"  2h ago      fix the bug", "  1d ago      add tests"}, menuText(m), "unlit")
		press(m, enter)
		assert.Equal(t, []string{"/resume def456"}, c.submitted)
	})
}

func TestUpDownAndTabLightAnUnlitMenu(t *testing.T) {
	unlit := func(t *testing.T) *model {
		m, _ := newMenuModel(t, 60, 30)
		settle(m, keys(m, "/resume def456"))
		for range len("def456") {
			press(m, left)
		}
		require.Equal(t, []string{"  2h ago      fix the bug", "  1d ago      add tests"}, menuText(m))
		return m
	}
	synctest.Test(t, func(t *testing.T) {
		m := unlit(t)
		press(m, down)
		assert.Equal(t, []string{"❭ 2h ago      fix the bug", "  1d ago      add tests"}, menuText(m))

		m = unlit(t)
		press(m, up)
		assert.Equal(t, []string{"  2h ago      fix the bug", "❭ 1d ago      add tests"}, menuText(m))

		m = unlit(t)
		press(m, tab)
		assert.Equal(t, []string{"❭ 2h ago      fix the bug", "  1d ago      add tests"}, menuText(m))
		assert.Equal(t, "/resume def456", m.composer.value(), "tab fills in only a lit row")
		press(m, tab)
		assert.Equal(t, "/resume abc123", m.composer.value())
	})
}

func TestReferenceMenu(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := newMenuModel(t, 60, 30)
		settle(m, keys(m, "see @"))
		assert.Equal(t, []string{"❭ a.go", "  dir/", "  dir/b.go", "  my file.txt"}, menuText(m))

		keys(m, "a")
		assert.Equal(t, []string{"❭ a.go"}, menuText(m))
		press(m, enter)
		assert.Equal(t, "see @a.go ", m.composer.value(), "enter fills in a file")
		assert.Empty(t, c.submitted)
		assert.Zero(t, m.menuHeight())

		settle(m, keys(m, "@d"))
		press(m, tab)
		assert.Equal(t, "see @a.go @dir/", m.composer.value())
		assert.Equal(t, []string{"❭ dir/b.go"}, menuText(m), "a directory lists its entries")

		m.composer.reset()
		settle(m, keys(m, "@my"))
		press(m, tab)
		assert.Equal(t, `@my\ file.txt `, m.composer.value())
		assert.Zero(t, m.menuHeight())
	})
}

// loads runs cmd and returns the completions it loads, in order, leaving them
// undelivered. Run it inside synctest.
func loads(cmd tea.Cmd) []candidatesMsg {
	if cmd == nil {
		return nil
	}
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		var out []candidatesMsg
		for _, c := range msg {
			out = append(out, loads(c)...)
		}
		return out
	case candidatesMsg:
		return []candidatesMsg{msg}
	}
	return nil
}

func deliver(m *model, msgs ...candidatesMsg) {
	for _, msg := range msgs {
		m.Update(msg)
	}
}

func newTypedMenuModel(t *testing.T) (*model, *fakeController) {
	t.Helper()
	m, c := newMenuModel(t, 60, 30)
	c.commands = append(c.commands, app.Command{Name: "add-dir", Argument: true, Typed: true})
	dirs := map[string][]string{
		"":                {"../kopi.sandbox/", "../pig/", "../tap/"},
		"../p":            {"../pig/", "../pix/"},
		"../pi":           {"../pig/"},
		"../pig":          {"../pig/"},
		"../pig/":         {"../pig/", "../pig/docs/", "../pig/internal/"},
		"../pig/docs/":    {"../pig/docs/"},
		"../pig/internal": {"../pig/internal/"},
	}
	c.typedArgs = map[string]func(string) []app.Candidate{"add-dir": func(typed string) []app.Candidate {
		var cs []app.Candidate
		for _, d := range dirs[typed] {
			cs = append(cs, app.Candidate{Value: d})
		}
		return cs
	}}
	return m, c
}

func TestTypedArgumentMenu(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := newTypedMenuModel(t)
		settle(m, keys(m, "/add-dir "))
		assert.Equal(t, []string{"❭ ../kopi.sandbox/", "  ../pig/", "  ../tap/"}, menuText(m))
		assert.Equal(t, []string{"add-dir "}, c.asks)

		settle(m, keys(m, "../pi"))
		assert.Equal(t, []string{"❭ ../pig/"}, menuText(m), "the reply is shown as given")
		assert.Equal(t, "add-dir ../pi", c.asks[len(c.asks)-1], "every change asks again")

		// A step fills in without running, and the next level is asked.
		cmd := press(m, tab)
		assert.Equal(t, "/add-dir ../pig/", m.composer.value())
		assert.Empty(t, c.submitted)
		assert.Equal(t, []string{"  ../pig/"}, menuText(m), "the rows stay, unlit, until the reply")

		press(m, tab)
		assert.Equal(t, "/add-dir ../pig/", m.composer.value(), "no row is taken while the reply is pending")

		settle(m, cmd)
		assert.Equal(t, "add-dir ../pig/", c.asks[len(c.asks)-1])
		assert.Equal(t, []string{"❭ ../pig/", "  ../pig/docs/", "  ../pig/internal/"}, menuText(m), "the Tab pressed while pending lights the typed directory")
		assert.Zero(t, m.menu.sel)
		assert.True(t, m.menu.picked, "the Tab pressed while pending is kept")

		press(m, down)
		settle(m, press(m, enter))
		assert.Equal(t, "/add-dir ../pig/docs/", m.composer.value(), "enter on a step fills it in")
		assert.Empty(t, c.submitted)
		assert.Equal(t, "add-dir ../pig/docs/", c.asks[len(c.asks)-1])
	})
}

func TestTypedArgumentMenuHoldsKeysWithoutRows(t *testing.T) {
	for _, afterEmpty := range []bool{false, true} {
		name := "initial"
		if afterEmpty {
			name = "after empty reply"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, c := newTypedMenuModel(t)
				c.history = []string{"previous prompt"}
				m.composer.setHistory(c.History())
				var cmd tea.Cmd
				if afterEmpty {
					settle(m, keys(m, "/add-dir x"))
					require.Empty(t, m.menu.shown)
					cmd = press(m, backspace)
				} else {
					cmd = keys(m, "/add-dir ")
				}
				require.True(t, m.menu.pending)
				require.Zero(t, m.menuHeight())
				for _, k := range []tea.KeyPressMsg{up, down, tab} {
					press(m, k)
					assert.Equal(t, "/add-dir ", m.composer.value())
					assert.False(t, m.composer.browsing())
				}
				require.Equal(t, []tea.KeyPressMsg{up, down, tab}, m.menu.held)
				settle(m, cmd)
				assert.Equal(t, "/add-dir ../kopi.sandbox/", m.composer.value())
				assert.False(t, m.composer.browsing())
				assert.Empty(t, m.menu.held)
				assert.Empty(t, c.submitted)
			})
		})
	}
}

func TestTypedArgumentMenuKeepsKeysPressedWhilePending(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTypedMenuModel(t)
		settle(m, keys(m, "/add-dir ../pig/"))
		press(m, backspace)
		cmd := keys(m, "/")
		press(m, up)
		press(m, tab)
		settle(m, cmd)

		assert.Equal(t, "/add-dir ../pig/internal/", m.composer.value(), "the held Up lit the last row and the held Tab filled it")
	})
}

func TestTypedArgumentMenuUpWhilePendingLightsTheRepliesLastRow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTypedMenuModel(t)
		settle(m, keys(m, "/add-dir ../pi"))
		cmd := keys(m, "g/")
		press(m, up)
		assert.Equal(t, []string{"  ../pig/"}, menuText(m), "the rows stay, unlit, until the reply")
		settle(m, cmd)
		assert.Equal(t, []string{"  ../pig/", "  ../pig/docs/", "❭ ../pig/internal/"}, menuText(m), "a longer reply")

		m, _ = newTypedMenuModel(t)
		settle(m, keys(m, "/add-dir ../pig/"))
		cmd = keys(m, "docs/")
		press(m, up)
		settle(m, cmd)
		assert.Equal(t, []string{"❭ ../pig/docs/"}, menuText(m), "a shorter reply")
	})
}

func TestTypedArgumentMenuDownWhilePendingLightsTheRepliesFirstRow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTypedMenuModel(t)
		settle(m, keys(m, "/add-dir ../pi"))
		cmd := keys(m, "g/")
		press(m, down)
		settle(m, cmd)
		assert.Equal(t, []string{"❭ ../pig/", "  ../pig/docs/", "  ../pig/internal/"}, menuText(m))
	})
}

func TestTypedArgumentMenuTabWhilePendingFillsTheRepliesFirstRow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := newTypedMenuModel(t)
		settle(m, keys(m, "/add-dir ../p"))
		cmd := keys(m, "i")
		press(m, tab)
		assert.Equal(t, "/add-dir ../pi", m.composer.value())
		settle(m, cmd)
		assert.Equal(t, "/add-dir ../pig/", m.composer.value())
		assert.Empty(t, c.submitted)
	})
}

func TestTypedArgumentMenuDropsKeysHeldForOlderText(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTypedMenuModel(t)
		settle(m, keys(m, "/add-dir ../pi"))
		cmd := keys(m, "g/")
		press(m, up)
		settle(m, tea.Batch(cmd, press(m, backspace)))

		assert.Empty(t, m.menu.held)
		assert.False(t, m.menu.picked)
		assert.Equal(t, []string{"  ../pig/"}, menuText(m))
	})
}

func TestTypedArgumentMenuDropsAStaleReply(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTypedMenuModel(t)
		settle(m, keys(m, "/add-dir ../pi"))
		older := loads(press(m, tea.KeyPressMsg{Code: 'g', Text: "g"}))
		newer := loads(press(m, tea.KeyPressMsg{Code: '/', Text: "/"}))
		require.Len(t, older, 1)
		require.Len(t, newer, 1)

		deliver(m, newer...)
		deliver(m, older...)

		assert.Equal(t, []string{"  ../pig/", "  ../pig/docs/", "  ../pig/internal/"}, menuText(m))
	})
}

func TestTypedArgumentMenuEnterSendsTheDraftWhilePendingAndWhenTypedInFull(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := newTypedMenuModel(t)
		settle(m, keys(m, "/add-dir ../pi"))
		keys(m, "g/")

		press(m, enter)
		assert.Equal(t, []string{"/add-dir ../pig/"}, c.submitted, "the row listed for older text is not taken")

		m, c = newTypedMenuModel(t)
		settle(m, keys(m, "/add-dir ../pig/"))
		press(m, enter)
		assert.Equal(t, []string{"/add-dir ../pig/"}, c.submitted, "a directory typed in full is added as typed")
	})
}

func TestTypedArgumentMenuEnterRunsADirectoryTypedWithoutItsSlash(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := newTypedMenuModel(t)
		settle(m, keys(m, "/add-dir ../pig"))
		press(m, enter)
		assert.Equal(t, []string{"/add-dir ../pig"}, c.submitted)

		m, c = newTypedMenuModel(t)
		settle(m, keys(m, "/add-dir ../pig"))
		press(m, tab)
		assert.Equal(t, "/add-dir ../pig", m.composer.value(), "the first Tab lights the row")
		press(m, tab)
		assert.Equal(t, "/add-dir ../pig/", m.composer.value())
		assert.Empty(t, c.submitted)
	})
}

func TestTypedArgumentMenuShowsNothingWhenTheLoadFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := newTypedMenuModel(t)
		c.typedErr = errors.New("boom")
		settle(m, keys(m, "/add-dir ../pig"))
		assert.Zero(t, m.menuHeight())
		assert.False(t, m.menu.pending)

		press(m, enter)
		assert.Equal(t, []string{"/add-dir ../pig"}, c.submitted)
	})
}

func TestListArgumentIsLoadedOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := newTypedMenuModel(t)
		settle(m, keys(m, "/resume d"))
		assert.Equal(t, []string{"❭ 1d ago      add tests"}, menuText(m))
		assert.Equal(t, []string{"resume "}, c.asks)
	})
}

func TestMidTokenReferenceCompletionKeepsFollowingLine(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := newMenuModel(t, 60, 30)
		m.composer.ta.SetValue("see @a\nkeep")
		m.composer.selectRange(selection{anchor: point{0, 5}, head: point{0, 5}})
		settle(m, m.followMenu())
		require.Equal(t, "❭ a.go", menuText(m)[0])

		press(m, enter)

		assert.Equal(t, "see @a.go \nkeep", m.composer.value())
		assert.Equal(t, 0, m.composer.ta.Line())
		assert.Equal(t, 10, m.composer.ta.Column())
		assert.Empty(t, c.submitted)
	})
}

func TestEscClosesMenuBeforeInterrupting(t *testing.T) {
	m, c := newMenuModel(t, 60, 30)
	c.running = true
	send(m, event.TurnStart{Prompt: "go"})
	keys(m, "/pl")
	require.Positive(t, m.menuHeight())

	press(m, escape)
	assert.Zero(t, m.menuHeight())
	assert.Zero(t, c.interrupts)
	press(m, left)
	press(m, tea.KeyPressMsg{Code: tea.KeyRight})
	assert.Zero(t, m.menuHeight(), "moving within the token keeps it closed")

	press(m, escape)
	assert.Equal(t, 1, c.interrupts)

	keys(m, "a")
	assert.Positive(t, m.menuHeight(), "a changed token reopens it")
}

func TestCancelWithMenuOpenKeepsItsMeaning(t *testing.T) {
	m, c := newMenuModel(t, 60, 30)
	keys(m, "/pl")

	press(m, ctrlC)
	assert.Empty(t, m.composer.value())
	assert.Zero(t, m.menuHeight())

	keys(m, "/")
	c.running = true
	press(m, ctrlC)
	assert.Equal(t, []string{"interrupt", "interrupt"}, c.calls, "idle cancel tries to interrupt first")
	assert.Equal(t, "/", m.composer.value(), "interrupting keeps the draft")
}

func TestTabWithoutMenuDoesNothing(t *testing.T) {
	m, _ := newMenuModel(t, 60, 30)
	keys(m, "hi")

	assert.Nil(t, press(m, tab))
	assert.Equal(t, "hi", m.composer.value())
}

func TestHistoryRecallKeepsMenuClosed(t *testing.T) {
	m, c := newMenuModel(t, 60, 30)
	c.history = []string{"older", "/plan"}
	m.composer.setHistory(c.History())

	press(m, up)
	assert.Equal(t, "/plan", m.composer.value())
	assert.Zero(t, m.menuHeight())

	press(m, up)
	assert.Equal(t, "older", m.composer.value())
}

func TestQueueEditWithoutMenu(t *testing.T) {
	m, c := newMenuModel(t, 60, 30)
	queueTwo(m, c)
	keys(m, "draft")

	press(m, up)

	assert.Equal(t, 1, c.takes)
	assert.Equal(t, "a\n\nb\n\ndraft", m.composer.value())
}

func TestStaleOrFailedLoadLeavesMenuEmpty(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, c := newMenuModel(t, 60, 30)
		keys(m, "@")
		m.Update(candidatesMsg{seq: m.menu.seq - 1, cs: c.files})
		assert.Zero(t, m.menuHeight(), "an older load is dropped")

		m.composer.reset()
		c.filesErr = errors.New("boom")
		settle(m, keys(m, " @"))
		assert.Zero(t, m.menuHeight())
	})
}

func TestShortTerminalKeepsComposerAndStatusLine(t *testing.T) {
	m, c := newMenuModel(t, 40, 10)
	queueTwo(m, c)
	keys(m, "/")

	s := screen(m)
	require.Len(t, s, 10)
	assert.Equal(t, statusText(m), s[9])
	assert.Contains(t, s[9], "plan")
	assert.Contains(t, s, "❯ /")
	assert.Equal(t, []string{"❭ /plan       switch to plan mode", "  /execute    switch to execute mode", "  1 of 7"}, menuText(m))

	press(m, up)
	assert.Zero(t, c.takes, "up moves the selection rather than editing the queue")
	assert.Equal(t, "/", m.composer.value())
	assert.Equal(t, "❭ /quit       exit kopi", menuText(m)[1])
}

func TestComposerDragHoldsMenuUntilRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newMenuModel(t, 60, 30)
		m.Update(tea.PasteMsg{Content: "@ one\n@ two"})
		require.Zero(t, m.menuHeight())
		top := m.composerTop()

		_, cmd := m.Update(tea.MouseClickMsg{X: 3, Y: top, Button: tea.MouseLeft})
		settle(m, cmd)
		assert.Equal(t, 1, m.composer.ta.Column(), "the press is inside the token")
		assert.Zero(t, m.menuHeight(), "the menu holds while the button is down")

		moveTo(m, 3, top+1)
		assert.Zero(t, m.menuHeight())
		assert.Equal(t, top, m.composerTop())
		assert.Equal(t, 1, m.composer.ta.Line(), "the drag maps to the composer row under the pointer")
		assert.Equal(t, 1, m.composer.ta.Column())

		settle(m, releaseAt(m, 3, top+1))
		s := screen(m)
		assert.Equal(t, "❭ a.go", menuText(m)[0], "release shows the menu for the final token")
		assert.Equal(t, "❯ @ one", s[m.composerTop()], "the menu is below the composer")
		assert.Equal(t, statusText(m), s[len(s)-1])
	})
}

func TestComposerDragKeepsOpenMenu(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newMenuModel(t, 80, 14)
		_, cmd := m.Update(tea.PasteMsg{Content: "@\n@"})
		settle(m, cmd)
		menu := menuText(m)
		require.NotEmpty(t, menu)
		top := m.composerTop()

		_, cmd = m.Update(tea.MouseClickMsg{X: 2, Y: top, Button: tea.MouseLeft})
		settle(m, cmd)
		assert.Zero(t, m.composer.ta.Line())
		assert.Zero(t, m.composer.ta.Column())
		assert.Equal(t, menu, menuText(m))

		settle(m, moveTo(m, 3, top+1))
		assert.Equal(t, top, m.composerTop())
		assert.Equal(t, 1, m.composer.ta.Line())
		assert.Equal(t, 1, m.composer.ta.Column())
		assert.True(t, m.composer.ta.HasSelection())
		assert.Equal(t, "@\n@", m.composer.ta.SelectedText())
		assert.Equal(t, menu, menuText(m), "the menu stays as it was at the press")
		s := screen(m)
		require.Len(t, s, 14)
		assert.Equal(t, "❯ @", s[top])

		assertCopied(t, m, releaseAt(m, 3, top+1), "@\n@")
		assert.Equal(t, "❭ a.go", menuText(m)[0])
		s = screen(m)
		require.Len(t, s, 14)
		assert.Equal(t, statusText(m), s[13])
	})
}

func TestComposerDragHeldMenuTakesNoKeys(t *testing.T) {
	for _, k := range []tea.KeyPressMsg{up, down, enter, tab} {
		t.Run(k.String(), func(t *testing.T) {
			m, c := newMenuModel(t, 60, 30)
			draft := "/pl"
			if k.Code == tea.KeyUp || k.Code == tea.KeyDown {
				draft = "/"
			}
			keys(m, draft)
			top := m.composerTop()
			pressAt(m, 2, top)
			moveTo(m, 2+len(draft), top)
			require.True(t, m.holding())
			require.Contains(t, menuText(m)[0], "❭ /plan", "the held menu stays visible")
			if draft == "/" {
				require.Greater(t, len(m.menu.shown), 1)
			}
			sel := m.menu.sel

			press(m, k)

			if k.Code == tea.KeyEnter {
				assert.Equal(t, []string{draft}, c.submitted, "enter submits the draft")
				return
			}
			assert.Empty(t, c.submitted)
			require.Equal(t, draft, m.composer.value())
			assert.Equal(t, sel, m.menu.sel, "held candidates cannot be navigated")

			releaseAt(m, 2+len(draft), top)
			require.False(t, m.holding())
			require.Contains(t, menuText(m)[0], "❭ /plan")
			press(m, tab)
			assert.Equal(t, "/plan ", m.composer.value(), "release restores completion")
		})
	}
}

func TestComposerDragHeldBackLoadAppearsOnRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newMenuModel(t, 60, 30)
		_, load := m.Update(tea.PasteMsg{Content: "@\n@"})
		require.True(t, m.menu.open)
		require.Empty(t, m.menu.shown)
		top := m.composerTop()

		pressAt(m, 3, top)
		settle(m, moveTo(m, 3, top+1))
		require.True(t, m.holding())
		require.Equal(t, 1, m.composer.ta.Line())
		require.Equal(t, 1, m.composer.ta.Column())
		before := screen(m)

		settle(m, load)

		assert.Equal(t, before, screen(m), "a load while held changes no rows")
		assert.Zero(t, m.menuHeight())

		releaseAt(m, 3, top+1)
		assert.False(t, m.holding())
		assert.Equal(t, "❭ a.go", menuText(m)[0])
		s := screen(m)
		require.Len(t, s, m.height)
		assert.Equal(t, statusText(m), s[m.height-1])
	})
}

func TestResizeCancelsComposerDrag(t *testing.T) {
	m, _ := newMenuModel(t, 60, 30)
	keys(m, "/")
	top := m.composerTop()
	pressAt(m, 3, top)
	moveTo(m, 2, top)
	require.True(t, m.composer.ta.HasSelection())
	require.Positive(t, m.menuHeight(), "the menu holds while the cursor is off the token")

	m.Update(tea.WindowSizeMsg{Width: 60, Height: 10})

	assert.Equal(t, drag{}, m.drag)
	assert.False(t, m.composer.ta.HasSelection())
	assert.True(t, m.sel.empty())
	assert.Zero(t, m.menuHeight(), "the menu follows the cursor once the hold ends")
	s := screen(m)
	require.Len(t, s, 10)
	assert.Equal(t, statusText(m), s[9])
	assert.Nil(t, moveTo(m, 3, m.composerTop()))
	assert.Nil(t, releaseAt(m, 3, m.composerTop()))
	assert.False(t, m.composer.ta.HasSelection())
	assert.Empty(t, m.clip.(*fakeClipboard).written)
	assert.NotContains(t, statusText(m), "Copied")
}

func TestLoadedMenuShrinksExhaustedPendingBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newMenuModel(t, 80, 14)
		send(m, event.QueueChanged{Pending: []event.Queued{{Text: linesOf(8)}}})
		cmd := keys(m, "@")
		require.False(t, m.view().busy)
		require.Zero(t, m.menuHeight(), "the async load has not arrived")
		pending := len(m.pending)
		require.Positive(t, pending)
		require.Equal(t, 14-m.composer.height()-chromeRows, pending, "the queue exhausts the available budget")
		s := screen(m)
		require.Len(t, s, 14)
		assert.Equal(t, statusText(m), s[13])

		settle(m, cmd)

		require.Positive(t, m.menuHeight())
		assert.Less(t, len(m.pending), pending)
		s = screen(m)
		require.Len(t, s, 14)
		assert.Equal(t, statusText(m), s[13])
	})
}

func TestMenuRowDragCopiesWithoutChangingMenu(t *testing.T) {
	m, _ := newMenuModel(t, 60, 30)
	keys(m, "/")
	menu, sel := menuText(m), m.menu.sel
	y := len(screen(m)) - 1 - m.menuHeight()

	assertCopied(t, m, dragRow(m, 2, 7, y), "/plan")

	assert.Equal(t, menu, menuText(m))
	assert.Equal(t, sel, m.menu.sel)
	assert.Equal(t, "/", m.composer.value())
	assert.False(t, m.holding())
}

func TestSkillListsAfterCommandsAndIsOnlyFilledIn(t *testing.T) {
	for name, key := range map[string]tea.KeyPressMsg{"enter": enter, "tab": tab} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m, c := newMenuModel(t, 60, 30)
				c.commands = append(c.commands, app.Command{Name: "skill:a", Summary: "does a", Skill: true})
				keys(m, "/")
				press(m, up)
				assert.Equal(t, "❭ /skill:a    does a", menuText(m)[len(menuText(m))-2])

				keys(m, "skill")
				settle(m, press(m, key))

				assert.Empty(t, c.submitted)
				assert.Equal(t, "/skill:a ", m.composer.value())
			})
		})
	}
}
