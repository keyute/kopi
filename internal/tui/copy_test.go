package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/keyute/kopi/internal/app"
	"github.com/keyute/kopi/internal/event"
)

func reply(m *model, text string) {
	send(m, event.TurnStart{Prompt: "go"}, event.TextDelta{Text: text}, event.ReplyEnd{Text: text}, event.TurnEnd{})
}

func runCopyCommand(m *model, c *fakeController) tea.Cmd {
	c.submitErr = app.ErrCopy
	return submitText(m, "/copy")
}

func TestCopyWithNoReplyFlashes(t *testing.T) {
	m, c := newTestModel(t, 60, 20)

	runCopyCommand(m, c)

	assert.Nil(t, m.front())
	assert.Contains(t, strings.Join(screen(m), "\n"), "no reply to copy")
	assert.Empty(t, m.clip.(*fakeClipboard).written)
}

func TestCopyOfAReplyWithoutCodeCopiesItWhole(t *testing.T) {
	m, c := newTestModel(t, 60, 20)
	reply(m, "\nHello **there**\n")

	cmd := runCopyCommand(m, c)

	assert.Nil(t, m.front())
	assertCopied(t, m, cmd, "\nHello **there**\n")
}

func TestCopyOfAReplyWithBlocksPicksOne(t *testing.T) {
	text := "Intro\n\n```go\nfunc main() {\n}\n```\n\n```sh\ntask test\n```\n\n```\n{\"name\": \"kopi\",\n\n  \"x\": 1}\n```"
	pick := func(t *testing.T, k tea.KeyPressMsg, want string) {
		m, c := newTestModel(t, 52, 24)
		reply(m, text)
		runCopyCommand(m, c)
		require.NotNil(t, m.front())

		s := strings.Join(screen(m), "\n")
		for _, row := range []string{
			"  Copy",
			"❭ 1. Whole reply · 16 lines",
			"  2. go · 2 lines · func main() {",
			"  3. sh · 1 line · task test",
			"  4. 3 lines · {\"name\": \"kopi\",",
			"  enter to copy · esc to cancel",
		} {
			assert.Contains(t, s, row)
		}
		assertCopied(t, m, press(m, k), want)
		assert.Nil(t, m.front())
	}
	t.Run("digit", func(t *testing.T) { pick(t, tea.KeyPressMsg{Code: '3', Text: "3"}, "task test") })
	t.Run("enter", func(t *testing.T) {
		m, c := newTestModel(t, 52, 24)
		reply(m, text)
		runCopyCommand(m, c)
		press(m, tea.KeyPressMsg{Code: tea.KeyDown})
		assertCopied(t, m, press(m, enter), "func main() {\n}")
	})
	t.Run("whole", func(t *testing.T) { pick(t, enter, text) })
}

func TestCopyStandaloneIndentedCode(t *testing.T) {
	m, c := newTestModel(t, 52, 24)
	reply(m, "    first\n    second\n")
	runCopyCommand(m, c)
	require.NotNil(t, m.front())

	assertCopied(t, m, press(m, tea.KeyPressMsg{Code: '2', Text: "2"}), "first\nsecond")
}

func TestCopyWholeReplyWithLeadingIndentedCode(t *testing.T) {
	m, c := newTestModel(t, 52, 24)
	text := "    first\n    second\n\nDone.\n"
	reply(m, text)
	runCopyCommand(m, c)
	require.NotNil(t, m.front())

	assertCopied(t, m, press(m, enter), text)
}

func TestCopyPickerClosesOnlyForItsReplacedTarget(t *testing.T) {
	for _, session := range []string{"main", "child"} {
		t.Run(session, func(t *testing.T) {
			m, c := newTestModel(t, 52, 24)
			if session == "child" {
				m.viewOf("child")
				m.show("child")
			}
			text := "```sh\necho old\n```"
			if session == "main" {
				reply(m, text)
			} else {
				send(m, event.Session{ID: "child", Event: event.ReplyEnd{Text: text}})
			}
			runCopyCommand(m, c)
			require.NotNil(t, m.front())
			send(m, event.TextDelta{Text: "streaming"}, event.Session{ID: "other", Event: event.CopyTarget{}}, event.Notice{Text: "notice"})
			require.NotNil(t, m.front())
			if session == "main" {
				send(m, event.CopyTarget{Text: text})
			} else {
				send(m, event.Session{ID: "child", Event: event.CopyTarget{Text: text}})
			}
			require.NotNil(t, m.front())
			if session == "main" {
				send(m, event.CopyTarget{Text: "earlier"})
			} else {
				send(m, event.Session{ID: "child", Event: event.CopyTarget{}})
			}
			assert.Nil(t, m.front())
			press(m, enter)
			assert.Empty(t, m.clip.(*fakeClipboard).written)
		})
	}
}

func TestCopyPickerClosesWhenPlanShowsMain(t *testing.T) {
	m, c := newTestModel(t, 52, 24)
	m.viewOf("child")
	m.show("child")
	send(m, event.Session{ID: "child", Event: event.ReplyEnd{Text: "```sh\necho old\n```"}})
	runCopyCommand(m, c)
	require.NotNil(t, m.front())

	send(m, event.PlanRequested{Token: 1, Plan: "# plan"})
	assert.Equal(t, app.Main, m.shown)
	send(m, event.PlanEnded{Token: 1})
	assert.Nil(t, m.front())
	press(m, enter)
	assert.Empty(t, m.clip.(*fakeClipboard).written)
}

func TestCopyEscapeCopiesNothing(t *testing.T) {
	m, c := newTestModel(t, 52, 24)
	reply(m, "```sh\ntask test\n```")
	runCopyCommand(m, c)

	press(m, escape)

	assert.Nil(t, m.front())
	assert.Empty(t, m.clip.(*fakeClipboard).written)
}

func TestCopyLabelStaysOneRow(t *testing.T) {
	m, c := newTestModel(t, 30, 24)
	reply(m, "```sh\n"+strings.Repeat("long ", 30)+"\n```")
	runCopyCommand(m, c)

	var row string
	for _, r := range screen(m) {
		if strings.HasPrefix(r, "  2. sh · 1 line") {
			row = r
		}
	}
	require.NotEmpty(t, row)
	assert.True(t, strings.HasSuffix(row, "…"), row)
	assert.LessOrEqual(t, len([]rune(row)), 30)
}

func TestCopyLanguageLabelWithTabStaysOneRow(t *testing.T) {
	m, c := newTestModel(t, 30, 24)
	reply(m, "```long\tlanguage\nbody\n```")
	runCopyCommand(m, c)
	require.NotNil(t, m.front())

	var row string
	for _, r := range screen(m) {
		if strings.HasPrefix(r, "  2. long") {
			row = r
		}
	}
	require.NotEmpty(t, row)
	assert.NotContains(t, row, "\t")
	assert.True(t, strings.HasSuffix(row, "…"), row)
	assert.LessOrEqual(t, len([]rune(row)), 30)
	assertCopied(t, m, press(m, tea.KeyPressMsg{Code: '2', Text: "2"}), "body")
}

func TestCopyLabelWithTabsStaysOneRowAndCopiesTheTabs(t *testing.T) {
	m, c := newTestModel(t, 30, 24)
	block := "NAME\t= kopi\t" + strings.Repeat("x\t", 20)
	reply(m, "```\n"+block+"\n```")
	runCopyCommand(m, c)

	var row string
	for _, r := range screen(m) {
		if strings.HasPrefix(r, "  2. 1 line") {
			row = r
		}
	}
	require.NotEmpty(t, row)
	assert.True(t, strings.HasSuffix(row, "…"), row)
	assert.LessOrEqual(t, len([]rune(row)), 30)
	assertCopied(t, m, press(m, tea.KeyPressMsg{Code: '2', Text: "2"}), block)
}
