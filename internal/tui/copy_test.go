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
	send(m, event.TurnStart{Prompt: "go"}, event.TextDelta{Text: text}, event.TurnEnd{})
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
	assertCopied(t, m, cmd, "Hello **there**")
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
	t.Run("whole", func(t *testing.T) { pick(t, enter, strings.TrimSpace(text)) })
}

func TestCopyStandaloneIndentedCode(t *testing.T) {
	m, c := newTestModel(t, 52, 24)
	reply(m, "    first\n    second\n")
	runCopyCommand(m, c)
	require.NotNil(t, m.front())

	assertCopied(t, m, press(m, tea.KeyPressMsg{Code: '2', Text: "2"}), "first\nsecond")
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
