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
		{Name: "plan", Summary: "switch to plan mode"},
		{Name: "execute", Summary: "switch to execute mode"},
		{Name: "status", Summary: "show status"},
		{Name: "new", Summary: "start a new session"},
		{Name: "resume", Summary: "resume a session"},
		{Name: "help", Summary: "list commands"},
		{Name: "quit", Summary: "exit kopi"},
	}
	c.args = map[string][]app.Candidate{"resume": {
		{Value: "abc123", Description: "2h ago  fix the bug"},
		{Value: "def456", Description: "1d ago  add tests"},
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
		assert.Equal(t, []string{"❭ abc123      2h ago  fix the bug", "  def456      1d ago  add tests"}, menuText(m))

		press(m, down)
		settle(m, press(m, enter))
		assert.Equal(t, []string{"/resume def456"}, c.submitted)
		assert.Empty(t, m.composer.value())
		assert.Zero(t, m.menuHeight())
	})
}

func TestEnterOnCommandRunsIt(t *testing.T) {
	m, c := newMenuModel(t, 60, 30)
	keys(m, "/pl")

	press(m, enter)

	assert.Equal(t, []string{"/plan"}, c.submitted)
	assert.Empty(t, m.composer.value())
	assert.Zero(t, m.menuHeight())
}

func TestNoMenuWhereNothingCompletes(t *testing.T) {
	for _, text := range []string{"/plan ", "//x", "hi\n/pl", "a@b", "hi"} {
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
}
